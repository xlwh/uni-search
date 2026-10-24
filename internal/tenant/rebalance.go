// Package tenant rebalance.go implements the M2 rebalance protocol
// and dedicated/cold tenant modes (design 05-distributed.md §3.3, §5).
// Rebalance is a three-phase migration: snapshot pull → incremental
// sync → atomic route switch. Dedicated mode gives a tenant exclusive
// Shard Node access; cold mode pauses segment loading.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.9
package tenant

import (
	"fmt"
	"sync"
	"time"
)

// RebalanceCoordinator manages shard migration between nodes.
type RebalanceCoordinator struct {
	mu        sync.Mutex
	active    map[string]*RebalanceState  // shard ID → state
	// Hooks for actual data migration (design 05 §3.3).
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.6
	pullSnapshot  func(shardID, source, target string) error
	syncIncremental func(shardID, source, target string) error
	switchRoute   func(shardID string) error
}

// RebalanceState tracks one in-progress migration.
type RebalanceState struct {
	ShardID     string
	SourceNode  string
	TargetNode  string
	Phase       RebalancePhase
	StartedAt   time.Time
}

// RebalancePhase enumerates the three-phase migration.
type RebalancePhase int

const (
	RebalancePhaseSnapshot RebalancePhase = iota  // target pulls snapshot from source
	RebalancePhaseIncremental                     // target syncs incremental WAL
	RebalancePhaseComplete                        // route table atomically switched
)

// NewRebalanceCoordinator creates a rebalance coordinator.
func NewRebalanceCoordinator() *RebalanceCoordinator {
	return &RebalanceCoordinator{
		active: make(map[string]*RebalanceState),
	}
}

// StartMigration begins a three-phase shard migration.
func (rc *RebalanceCoordinator) StartMigration(shardID, sourceNode, targetNode string) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if _, exists := rc.active[shardID]; exists {
		return fmt.Errorf("rebalance: shard %s already migrating", shardID)
	}
	rc.active[shardID] = &RebalanceState{
		ShardID:    shardID,
		SourceNode: sourceNode,
		TargetNode: targetNode,
		Phase:      RebalancePhaseSnapshot,
		StartedAt:  time.Now(),
	}
	return nil
}

// SetHooks injects data migration callbacks (design 05 §3.3).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.6
func (rc *RebalanceCoordinator) SetHooks(pullSnapshot, syncIncremental, switchRoute func(string, string, string) error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.pullSnapshot = pullSnapshot
	rc.syncIncremental = syncIncremental
	rc.switchRoute = func(shardID string) error {
		// switchRoute only needs shardID, wrap to match signature.
		if switchRoute != nil {
			return switchRoute(shardID, "", "")
		}
		return nil
	}
}

// AdvancePhase moves the migration to the next phase, calling the
// appropriate hook for data migration.
func (rc *RebalanceCoordinator) AdvancePhase(shardID string) error {
	rc.mu.Lock()
	state, ok := rc.active[shardID]
	if !ok {
		rc.mu.Unlock()
		return fmt.Errorf("rebalance: no active migration for %s", shardID)
	}
	if state.Phase >= RebalancePhaseComplete {
		rc.mu.Unlock()
		return fmt.Errorf("rebalance: already complete for %s", shardID)
	}
	// Execute the hook for the current phase before advancing.
	var hookErr error
	switch state.Phase {
	case RebalancePhaseSnapshot:
		if rc.pullSnapshot != nil {
			hookErr = rc.pullSnapshot(shardID, state.SourceNode, state.TargetNode)
		}
	case RebalancePhaseIncremental:
		if rc.syncIncremental != nil {
			hookErr = rc.syncIncremental(shardID, state.SourceNode, state.TargetNode)
		}
	}
	if hookErr != nil {
		rc.mu.Unlock()
		return fmt.Errorf("rebalance: phase %d hook failed: %w", state.Phase, hookErr)
	}
	state.Phase++
	if state.Phase == RebalancePhaseComplete {
		if rc.switchRoute != nil {
			rc.switchRoute(shardID)
		}
		delete(rc.active, shardID)
	}
	rc.mu.Unlock()
	return nil
}

// GetState returns the migration state.
func (rc *RebalanceCoordinator) GetState(shardID string) (*RebalanceState, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	state, ok := rc.active[shardID]
	return state, ok
}

// --- Dedicated mode ---

// DedicatedAllocator manages dedicated shard node assignment.
type DedicatedAllocator struct {
	mu          sync.Mutex
	assignments map[string]string  // tenant ID → exclusive node ID
}

// NewDedicatedAllocator creates a dedicated allocator.
func NewDedicatedAllocator() *DedicatedAllocator {
	return &DedicatedAllocator{
		assignments: make(map[string]string),
	}
}

// Assign gives a tenant exclusive access to a node.
func (da *DedicatedAllocator) Assign(tenantID, nodeID string) error {
	da.mu.Lock()
	defer da.mu.Unlock()
	// Check if node is already assigned to another tenant.
	for t, n := range da.assignments {
		if n == nodeID && t != tenantID {
			return fmt.Errorf("dedicated: node %s already assigned to tenant %s", nodeID, t)
		}
	}
	da.assignments[tenantID] = nodeID
	return nil
}

// GetNode returns the exclusive node for a tenant.
func (da *DedicatedAllocator) GetNode(tenantID string) (string, bool) {
	da.mu.Lock()
	defer da.mu.Unlock()
	node, ok := da.assignments[tenantID]
	return node, ok
}

// Release frees a tenant's exclusive node assignment.
func (da *DedicatedAllocator) Release(tenantID string) {
	da.mu.Lock()
	defer da.mu.Unlock()
	delete(da.assignments, tenantID)
}

// --- Cold mode ---

// ColdManager manages tenant cold (pause) mode. Cold mode unloads
// segments from memory (mmap close) while preserving metadata.
// Wakeup reopens segments (seconds-level, page cache warmup optional).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.7
type ColdManager struct {
	mu            sync.Mutex
	coldTenants    map[string]*ColdState
	closeSegment   func(segmentID string) error // mmap close callback
	reopenSegment  func(segmentID string) error // mmap reopen callback
}

// ColdState tracks a paused tenant's state.
type ColdState struct {
	TenantID  string
	PausedAt  time.Time
	SegmentIDs []string  // segments that were unloaded
}

// NewColdManager creates a cold manager.
func NewColdManager() *ColdManager {
	return &ColdManager{
		coldTenants: make(map[string]*ColdState),
	}
}

// SetCallbacks injects segment close/reopen hooks (design 05 §5.2).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.7
func (cm *ColdManager) SetCallbacks(closeFn, reopenFn func(string) error) {
	cm.closeSegment = closeFn
	cm.reopenSegment = reopenFn
}

// Pause unloads a tenant's segments from memory (mmap close).
func (cm *ColdManager) Pause(tenantID string, segmentIDs []string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if _, exists := cm.coldTenants[tenantID]; exists {
		return fmt.Errorf("cold: tenant %s already paused", tenantID)
	}
	// Close each segment's mmap (release memory).
	if cm.closeSegment != nil {
		for _, segID := range segmentIDs {
			_ = cm.closeSegment(segID) // best-effort: ignore individual errors
		}
	}
	cm.coldTenants[tenantID] = &ColdState{
		TenantID:   tenantID,
		PausedAt:   time.Now(),
		SegmentIDs: segmentIDs,
	}
	return nil
}

// Wakeup reloads a paused tenant's segments (mmap reopen).
func (cm *ColdManager) Wakeup(tenantID string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	state, ok := cm.coldTenants[tenantID]
	if !ok {
		return fmt.Errorf("cold: tenant %s not paused", tenantID)
	}
	// Reopen each segment's mmap (reload from disk).
	if cm.reopenSegment != nil {
		for _, segID := range state.SegmentIDs {
			_ = cm.reopenSegment(segID) // best-effort
		}
	}
	delete(cm.coldTenants, tenantID)
	return nil
}

// IsPaused returns true if the tenant is in cold mode.
func (cm *ColdManager) IsPaused(tenantID string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	_, ok := cm.coldTenants[tenantID]
	return ok
}
