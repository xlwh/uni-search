package esapi

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// PITManager manages Point-In-Time snapshots (design 03 §8, 04 §6.4).
// Each PIT pins a shard snapshot so segment merges cannot delete the
// pinned segments (D6: tombstone + background GC). PITs have a TTL
// (default 5 minutes) and are evicted via LRU when the capacity is
// exceeded (default 10000).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.4
type PITManager struct {
	mu        sync.Mutex
	pits      map[string]*pitEntry
	ttl       time.Duration
	maxPITs   int
}

type pitEntry struct {
	id        string
	index     string
	snapshot  types.Snapshot
	createdAt time.Time
	expiresAt time.Time
	lastUsed  time.Time
}

// NewPITManager creates a PIT manager with the default TTL (5 min)
// and capacity (10000).
func NewPITManager() *PITManager {
	return &PITManager{
		pits:    make(map[string]*pitEntry),
		ttl:     5 * time.Minute,
		maxPITs: 10000,
	}
}

// Create creates a new PIT for the given index and snapshot.
func (m *PITManager) Create(index string, snap types.Snapshot) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Generate a random PIT ID.
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("pit: generate id: %w", err)
	}
	pitID := base64.URLEncoding.EncodeToString(buf[:])

	now := time.Now()
	m.pits[pitID] = &pitEntry{
		id:        pitID,
		index:     index,
		snapshot:  snap,
		createdAt: now,
		expiresAt: now.Add(m.ttl),
		lastUsed:  now,
	}

	// LRU eviction if over capacity.
	if len(m.pits) > m.maxPITs {
		m.evictOldest()
	}

	return pitID, nil
}

// Snapshot returns the snapshot for a PIT, refreshing its TTL.
func (m *PITManager) Snapshot(pitID string) (types.Snapshot, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.pits[pitID]
	if !ok {
		return types.Snapshot{}, "", fmt.Errorf("pit: %s not found", pitID)
	}
	if time.Now().After(entry.expiresAt) {
		delete(m.pits, pitID)
		return types.Snapshot{}, "", fmt.Errorf("pit: %s expired", pitID)
	}

	// Refresh TTL.
	entry.lastUsed = time.Now()
	entry.expiresAt = time.Now().Add(m.ttl)
	return entry.snapshot, entry.index, nil
}

// Delete removes a PIT.
func (m *PITManager) Delete(pitID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.pits[pitID]; !ok {
		return fmt.Errorf("pit: %s not found", pitID)
	}
	delete(m.pits, pitID)
	return nil
}

// Cleanup removes expired PITs. Called periodically by a background
// goroutine or on each access.
func (m *PITManager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for id, entry := range m.pits {
		if now.After(entry.expiresAt) {
			delete(m.pits, id)
		}
	}
}

// Count returns the number of active PITs.
func (m *PITManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pits)
}

// evictOldest removes the PIT with the oldest lastUsed time.
func (m *PITManager) evictOldest() {
	var oldestID string
	var oldestTime time.Time
	first := true
	for id, entry := range m.pits {
		if first || entry.lastUsed.Before(oldestTime) {
			oldestID = id
			oldestTime = entry.lastUsed
			first = false
		}
	}
	if oldestID != "" {
		delete(m.pits, oldestID)
	}
}

// PITResult is the ES-style PIT creation response.
type PITResult struct {
	ID string `json:"id"`
}
