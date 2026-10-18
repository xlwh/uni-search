// Package storage implements the per-shard storage root (design 01
// §5-§8): the DeleteTracker that maintains MVCC-visible deletes, the
// atomic SegmentSet that publishes segments for queries, and the
// FlushWorker that publishes immutable segments from frozen MemTables.
// Sub-packages segment/memtable/wal are the building blocks; this
// package owns the per-shard orchestration only.
package storage

import (
	"sync"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// DeleteTracker maintains per-segment row deletions and per-_id memtable
// deletions so the query path can filter deleted rows without rewriting
// data (design 01 §6.2). Deletes are effective immediately for any
// snapshot whose ReadUpTo >= the delete seq_no (no async window).
//
// MVCC rule for memtable deletes (§6.2 + §8): a row at seq R is hidden
// from snapshot snap iff there is a tombstone at seq D with
// D <= snap.ReadUpTo and D > R. The strict inequality is what makes
// UPDATE = DELETE + INSERT at the same seq visible as the new value:
// the inserted row at seq D is not hidden by the tombstone at D.
type DeleteTracker struct {
	mu sync.RWMutex
	// segmentDeletes carries per-segment row bitmaps (§6.1).
	segmentDeletes map[types.SegmentID]*roaring.Bitmap
	// memDeletes carries _id -> tombstone seq for rows still in a
	// MemTable. Promoted into segmentDeletes when the row is flushed.
	memDeletes map[string]types.SeqNo
}

// NewDeleteTracker returns an empty tracker.
func NewDeleteTracker() *DeleteTracker {
	return &DeleteTracker{
		segmentDeletes: make(map[types.SegmentID]*roaring.Bitmap),
		memDeletes:     make(map[string]types.SeqNo),
	}
}

// DeleteInSegment marks (segID,rowID) deleted (§6.2 "若在段内"). The
// bitmap is created on first use.
func (d *DeleteTracker) DeleteInSegment(segID types.SegmentID, rowID types.RowID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	bm, ok := d.segmentDeletes[segID]
	if !ok {
		bm = roaring.New()
		d.segmentDeletes[segID] = bm
	}
	bm.Add(uint32(rowID))
}

// DeleteInMemtable records a tombstone at seq for _id (§6.2 "若在
// MemTable"). Idempotent: a later call with a smaller seq does not lower
// the recorded tombstone, so retried deletes at the same seq are safe.
func (d *DeleteTracker) DeleteInMemtable(id string, seq types.SeqNo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cur, ok := d.memDeletes[id]; !ok || seq > cur {
		d.memDeletes[id] = seq
	}
}

// IsSegmentRowDeleted reports whether (segID,rowID) is deleted.
func (d *DeleteTracker) IsSegmentRowDeleted(segID types.SegmentID, rowID types.RowID) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	bm, ok := d.segmentDeletes[segID]
	if !ok {
		return false
	}
	return bm.Contains(uint32(rowID))
}

// SegmentBitmap returns the roaring bitmap for segID (nil if none). The
// returned bitmap aliases the tracker's storage; callers must not mutate.
func (d *DeleteTracker) SegmentBitmap(segID types.SegmentID) *roaring.Bitmap {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.segmentDeletes[segID]
}

// IsMemDeleted reports whether an _id row at rowSeq is hidden by a
// memtable tombstone in the given snapshot (§6.2/§8).
func (d *DeleteTracker) IsMemDeleted(id string, rowSeq types.SeqNo, snap types.Snapshot) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	del, ok := d.memDeletes[id]
	if !ok {
		return false
	}
	return del <= snap.ReadUpTo && del > rowSeq
}

// PromoteMemDelete moves an _id's memtable tombstone into the segment
// bitmap when the row is flushed into segID at rowID. Called by
// FlushWorker after a frozen table is encoded.
func (d *DeleteTracker) PromoteMemDelete(id string, segID types.SegmentID, rowID types.RowID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.memDeletes, id)
	bm, ok := d.segmentDeletes[segID]
	if !ok {
		bm = roaring.New()
		d.segmentDeletes[segID] = bm
	}
	bm.Add(uint32(rowID))
}

// DropSegment removes the segment bitmap after a merge replaces the
// segment (design 01 §7.2).
func (d *DeleteTracker) DropSegment(segID types.SegmentID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.segmentDeletes, segID)
}

// MemDeleteSeq returns the tombstone seq recorded for id (0 if none).
// Used by recovery to replay deletes consistently.
func (d *DeleteTracker) MemDeleteSeq(id string) types.SeqNo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.memDeletes[id]
}
