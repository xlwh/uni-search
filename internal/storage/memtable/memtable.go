// Package memtable implements UniSearch's write-visible columnar in-memory
// table (design 01-storage-engine §2). Documents append into an active
// Table; the visible watermark advances with every append so same-shard
// queries read them immediately without any flush step. When the active
// table crosses the size or age threshold it moves into the frozen list
// (double-buffered, at most MaxFrozen tables) where the flush worker (T6)
// consumes it; a full frozen list applies write backpressure (429).
//
// The memtable is deliberately decoupled from the WAL (design 01 §3.3):
// seq numbers are assigned by the write path before rows arrive here.
package memtable

import (
	"fmt"
	"sync"
	"time"

	apierrors "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// Freeze thresholds and backpressure bound (design 01 §2.2).
const (
	DefaultMaxActiveBytes int64         = 512 << 20 // freeze active above 512MB
	DefaultMaxActiveAge   time.Duration = 30 * time.Second
	DefaultMaxFrozen      int           = 2 // frozen tables before write backpressure
)

// Config carries the §2.2 freeze/backpressure thresholds. Zero fields fall
// back to the Default* values, so Config{} selects the design defaults.
type Config struct {
	MaxActiveBytes int64         // freeze when active bytes exceed this
	MaxActiveAge   time.Duration // freeze when active is older than this
	MaxFrozen      int           // frozen tables held before writes are rejected
}

func (c Config) withDefaults() Config {
	if c.MaxActiveBytes <= 0 {
		c.MaxActiveBytes = DefaultMaxActiveBytes
	}
	if c.MaxActiveAge <= 0 {
		c.MaxActiveAge = DefaultMaxActiveAge
	}
	if c.MaxFrozen <= 0 {
		c.MaxFrozen = DefaultMaxFrozen
	}
	return c
}

// MemTable is the per-shard write buffer (design 01 §2.1): an active Table
// receiving appends plus a frozen list awaiting segment flush. Appends are
// visible to readers immediately through the visible watermark (§8); rows
// are never mutated after append, so frozen tables are safe to read while
// the flush worker encodes them.
type MemTable struct {
	shardID types.ShardID
	cfg     Config

	mu      sync.RWMutex
	active  *Table
	frozen  []*Table // oldest first; Append keeps len(frozen) < cfg.MaxFrozen
	visible types.SeqNo
}

// New creates an empty MemTable for the given shard.
func New(shardID types.ShardID, cfg Config) *MemTable {
	return &MemTable{
		shardID: shardID,
		cfg:     cfg.withDefaults(),
		active:  newTable(),
	}
}

// ShardID returns the owning shard.
func (m *MemTable) ShardID() types.ShardID { return m.shardID }

// Append appends a single row. Write is visible once it returns.
func (m *MemTable) Append(row Row) error {
	return m.AppendBatch(row)
}

// AppendBatch appends rows in order under one lock acquisition. Rows carry
// seq numbers assigned by the write path (WAL group commit); the visible
// watermark advances to the highest appended seq. A rejected batch returns
// the 429 backpressure error without appending anything; on a mid-batch
// validation error the rows before it stay appended (the caller retries
// from the failed row onward).
func (m *MemTable) AppendBatch(rows ...Row) error {
	if len(rows) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.frozen) >= m.cfg.MaxFrozen {
		return errFrozenFull(m.shardID, len(m.frozen))
	}
	for i := range rows {
		m.maybeFreezeLocked()
		if err := m.active.appendRow(&rows[i]); err != nil {
			return err
		}
		if rows[i].SeqNo > m.visible {
			m.visible = rows[i].SeqNo
		}
	}
	return nil
}

// maybeFreezeLocked moves active into frozen when a §2.2 threshold fired
// and starts a fresh active table. It is a no-op while the frozen list is
// full; the Append admission check guarantees writes are rejected then, so
// the active table stops growing.
func (m *MemTable) maybeFreezeLocked() {
	a := m.active
	if a == nil || a.rows == 0 {
		return
	}
	if a.bytes.Load() > m.cfg.MaxActiveBytes || time.Since(a.created) > m.cfg.MaxActiveAge {
		m.frozen = append(m.frozen, a)
		m.active = newTable()
	}
}

// MaybeFreeze applies the §2.2 thresholds (bytes/age) without appending.
// The flush worker may poll it so an idle shard still freezes on time.
func (m *MemTable) MaybeFreeze() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.frozen) < m.cfg.MaxFrozen {
		m.maybeFreezeLocked()
	}
}

// Freeze moves the active table into the frozen list unconditionally
// (the ?refresh=true path in design 01 §3.2 maps to this). It fails with
// the 429 backpressure error while the frozen list is full.
func (m *MemTable) Freeze() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.frozen) >= m.cfg.MaxFrozen {
		return errFrozenFull(m.shardID, len(m.frozen))
	}
	if m.active != nil && m.active.rows > 0 {
		m.frozen = append(m.frozen, m.active)
		m.active = newTable()
	}
	return nil
}

// Frozen returns the frozen tables, oldest first, for the flush worker to
// consume. The tables stay readable through MemTable queries until the
// flush worker removes them after publishing the new segment (design 01 §5
// orders segment publication before frozen removal).
func (m *MemTable) Frozen() []*Table {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Table, len(m.frozen))
	copy(out, m.frozen)
	return out
}

// NumFrozen reports the frozen table count.
func (m *MemTable) NumFrozen() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.frozen)
}

// RemoveFrozen removes the given table from the frozen list after its
// segment has been published. It reports whether the table was found.
func (m *MemTable) RemoveFrozen(t *Table) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, f := range m.frozen {
		if f == t {
			m.frozen = append(m.frozen[:i], m.frozen[i+1:]...)
			return true
		}
	}
	return false
}

// Snapshot returns the current visibility watermark as an MVCC read view
// (design 01 §8): rows appended at seq <= ReadUpTo are visible.
func (m *MemTable) Snapshot() types.Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return types.NewSnapshot(m.visible)
}

// VisibleSeqNo returns the visibility watermark (§8).
func (m *MemTable) VisibleSeqNo() types.SeqNo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.visible
}

// ActiveBytes reports the active table's memory estimate.
func (m *MemTable) ActiveBytes() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active.bytes.Load()
}

// ActiveRows reports the active table's row count.
func (m *MemTable) ActiveRows() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active.rows
}

// Lookup returns the newest row visible to snap for the given _id,
// searching the active table first, then frozen tables newest-first.
// Superseded versions of an _id stay readable through older snapshots.
func (m *MemTable) Lookup(id string, snap types.Snapshot) (RowView, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if rid, ok := m.active.lookupVisible(id, snap); ok {
		return m.active.RowView(rid), true
	}
	for i := len(m.frozen) - 1; i >= 0; i-- {
		if rid, ok := m.frozen[i].lookupVisible(id, snap); ok {
			return m.frozen[i].RowView(rid), true
		}
	}
	return RowView{}, false
}

// Iterate visits every row visible to snap across frozen tables (oldest
// first) and then the active table, in append order. Rows superseded by a
// later append of the same _id are still visited here; version filtering
// belongs to the delete tracker on the query path (design 01 §6.2).
// Callbacks run outside the memtable lock; returning false stops iteration.
func (m *MemTable) Iterate(snap types.Snapshot, fn func(RowView) bool) {
	m.mu.RLock()
	views := make([]RowView, 0, m.active.rows+8)
	for _, t := range m.frozen {
		views = t.collect(snap, views)
	}
	views = m.active.collect(snap, views)
	m.mu.RUnlock()
	for _, v := range views {
		if !fn(v) {
			return
		}
	}
}

// errFrozenFull builds the §2.2 write-backpressure error: a 429
// es_rejected_execution_exception-style APIError so clients retry with
// backoff. Assembled here because internal/errors only ships constructors
// for error types its current consumers produce; APIError fields are the
// public contract.
func errFrozenFull(shard types.ShardID, frozen int) *apierrors.APIError {
	return &apierrors.APIError{
		Status: 429, // HTTP Too Many Requests
		Type:   "es_rejected_execution_exception",
		Reason: fmt.Sprintf(
			"memtable for shard %d has %d frozen tables pending flush; write rejected, retry with backoff",
			shard, frozen),
	}
}
