// Package tenant implements the M1 multi-tenant shared-shard resource
// management (design 05-distributed §5). It provides per-tenant
// resource groups with four-dimensional quotas (storage bytes, vector
// count, query QPS, write TPS) and admission control via query
// concurrency semaphores and write token buckets. Quota violations
// return HTTP 429 with a tenant-identified error body.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.10
package tenant

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Quota is the four-dimensional per-tenant resource limit.
type Quota struct {
	StorageBytes int64
	VectorCount  int64
	QueryQPS     int
	WriteTPS     int
}

// ResourceGroup tracks per-tenant resource usage against a Quota.
type ResourceGroup struct {
	mu             sync.Mutex
	tenantID       string
	quota          Quota
	storageUsed    atomic.Int64
	vectorCount    atomic.Int64
	querySemaphore chan struct{}
	writeTokens    chan struct{}
	queryCount     atomic.Int64
	writeCount     atomic.Int64
	lastReset      time.Time
}

// NewResourceGroup creates a resource group for a tenant with the
// given quota. The query semaphore limits concurrent queries; the
// write token bucket limits write TPS.
func NewResourceGroup(tenantID string, q Quota) *ResourceGroup {
	rg := &ResourceGroup{
		tenantID:  tenantID,
		quota:     q,
		lastReset: time.Now(),
	}
	if q.QueryQPS > 0 {
		rg.querySemaphore = make(chan struct{}, q.QueryQPS)
	}
	if q.WriteTPS > 0 {
		rg.writeTokens = make(chan struct{}, q.WriteTPS)
		// Pre-fill the token bucket.
		for i := 0; i < q.WriteTPS; i++ {
			rg.writeTokens <- struct{}{}
		}
	}
	return rg
}

// AcquireQuery tries to acquire a query slot. Returns ErrQuotaExceeded
// if the concurrent query limit is reached.
func (rg *ResourceGroup) AcquireQuery() error {
	if rg.querySemaphore == nil {
		return nil
	}
	select {
	case rg.querySemaphore <- struct{}{}:
		rg.queryCount.Add(1)
		return nil
	default:
		return ErrQuotaExceeded(rg.tenantID, "query_concurrency")
	}
}

// ReleaseQuery releases a query slot.
func (rg *ResourceGroup) ReleaseQuery() {
	if rg.querySemaphore != nil {
		<-rg.querySemaphore
	}
}

// AcquireWrite tries to acquire a write token. Returns ErrQuotaExceeded
// if the write TPS limit is reached.
func (rg *ResourceGroup) AcquireWrite() error {
	if rg.writeTokens == nil {
		rg.writeCount.Add(1)
		return nil
	}
	select {
	case <-rg.writeTokens:
		rg.writeCount.Add(1)
		return nil
	default:
		return ErrQuotaExceeded(rg.tenantID, "write_tps")
	}
}

// AddStorage adds bytes to the storage usage counter.
func (rg *ResourceGroup) AddStorage(bytes int64) {
	rg.storageUsed.Add(bytes)
}

// AddVectors adds to the vector count counter.
func (rg *ResourceGroup) AddVectors(count int64) {
	rg.vectorCount.Add(count)
}

// CheckStorage returns ErrQuotaExceeded if storage is over quota.
func (rg *ResourceGroup) CheckStorage() error {
	if rg.quota.StorageBytes <= 0 {
		return nil
	}
	if rg.storageUsed.Load() > rg.quota.StorageBytes {
		return ErrQuotaExceeded(rg.tenantID, "storage_bytes")
	}
	return nil
}

// CheckVectors returns ErrQuotaExceeded if vector count is over quota.
func (rg *ResourceGroup) CheckVectors() error {
	if rg.quota.VectorCount <= 0 {
		return nil
	}
	if rg.vectorCount.Load() > rg.quota.VectorCount {
		return ErrQuotaExceeded(rg.tenantID, "vector_count")
	}
	return nil
}

// RefillWriteTokens refills the write token bucket at the configured
// TPS rate. Called periodically by a background goroutine.
func (rg *ResourceGroup) RefillWriteTokens() {
	if rg.writeTokens == nil {
		return
	}
	for {
		select {
		case rg.writeTokens <- struct{}{}:
		default:
			return
		}
	}
}

// TenantID returns the tenant identifier.
func (rg *ResourceGroup) TenantID() string { return rg.tenantID }

// Quota returns the current quota settings for this resource group.
func (rg *ResourceGroup) Quota() Quota {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.quota
}

// Usage returns the current resource usage (storage bytes, vector count).
func (rg *ResourceGroup) Usage() (storageUsed int64, vectorCount int64) {
	return rg.storageUsed.Load(), rg.vectorCount.Load()
}

// Manager manages all tenant resource groups.
type Manager struct {
	mu       sync.RWMutex
	groups   map[string]*ResourceGroup
	refillTicker *time.Ticker
	done     chan struct{}
}

// NewManager creates a tenant manager. Starts a background goroutine
// to refill write token buckets every second.
func NewManager() *Manager {
	m := &Manager{
		groups: make(map[string]*ResourceGroup),
		done:   make(chan struct{}),
	}
	m.refillTicker = time.NewTicker(time.Second)
	go func() {
		for {
			select {
			case <-m.refillTicker.C:
				m.RefillAll()
			case <-m.done:
				return
			}
		}
	}()
	return m
}

// Register creates or updates a tenant's resource group.
func (m *Manager) Register(tenantID string, q Quota) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.groups[tenantID] = NewResourceGroup(tenantID, q)
}

// Get returns the resource group for a tenant, or nil if not found.
func (m *Manager) Get(tenantID string) *ResourceGroup {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.groups[tenantID]
}

// Close stops the background refiller.
func (m *Manager) Close() {
	close(m.done)
	m.refillTicker.Stop()
}

// List returns all tenant resource groups. The returned slice is a
// snapshot; modifying it does not affect the manager's internal state.
func (m *Manager) List() []*ResourceGroup {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*ResourceGroup, 0, len(m.groups))
	for _, rg := range m.groups {
		result = append(result, rg)
	}
	return result
}

// Delete removes a tenant's resource group. Returns true if the tenant
// existed and was removed, false otherwise.
func (m *Manager) Delete(tenantID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[tenantID]; !ok {
		return false
	}
	delete(m.groups, tenantID)
	return true
}

// RefillAll refills write tokens for all tenants.
func (m *Manager) RefillAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rg := range m.groups {
		rg.RefillWriteTokens()
	}
}

// QuotaExceededError is the 429 error returned when a tenant exceeds quota.
type QuotaExceededError struct {
	TenantID string
	Dimension string
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("quota_exceeded_exception: tenant %q exceeded %s quota", e.TenantID, e.Dimension)
}

// ErrQuotaExceeded creates a QuotaExceededError.
func ErrQuotaExceeded(tenantID, dimension string) *QuotaExceededError {
	return &QuotaExceededError{TenantID: tenantID, Dimension: dimension}
}
