package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/memtable"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/internal/storage/wal"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// Shard is the per-shard aggregate root (design 01 §10): it owns the
// MemTable, the WAL, the live SegmentSet, the DeleteTracker and the
// FlushWorker. Write path: append-only with seq_no assigned by the WAL
// group commit. Read path: Snapshot returns an MVCC view of the
// SegmentSet plus the memtable watermark.
//
// Lifecycle: Open recovers on-disk state (opens existing segments,
// replays the WAL past the last SegmentFlushed marker); Close flushes
// frozen tables and closes the WAL.
type Shard struct {
	shardID   types.ShardID
	dataDir   string
	segDir    string // <dataDir>/segments/{shard_id}
	w         *wal.LocalWAL
	mt        *memtable.MemTable
	dt        *DeleteTracker
	setHolder *segmentSetHolder
	flusher   *FlushWorker

	// applied tracks the WAL-applied watermark (every Write advances
	// it). The MVCC snapshot uses max(memtable visible, applied) so
	// pure-delete batches (which append nothing to the memtable) still
	// surface their tombstone's visibility (design 01 §8).
	applied atomic.Uint64

	// mappingFn returns the live field definitions; the flusher uses
	// it to label columns. The mapping itself is owned by internal/meta
	// (T3); the Shard just asks for a snapshot of it.
	mappingFn func() map[types.FieldID]*meta.FieldDef

	// applyInsert turns an Insert WAL payload (document JSON) into a
	// memtable.Row. The caller wires this from the ES-compat layer
	// (T17); tests inject a trivial implementation.
	applyInsert func(payload []byte, seq types.SeqNo) (memtable.Row, error)
}

// ShardConfig carries the dependencies Open needs. applyInsert and
// mappingFn are required; dataDir must exist.
type ShardConfig struct {
	ShardID     types.ShardID
	DataDir     string
	ApplyInsert func(payload []byte, seq types.SeqNo) (memtable.Row, error)
	MappingFn   func() map[types.FieldID]*meta.FieldDef
	Memtable    *memtable.Config
	WALOpts     []wal.Option
}

// OpenShard recovers or creates a shard at cfg.DataDir/shards/{id}/.
// Recovery:
//  1. Open the WAL (it truncates bad tails and tracks flushedUpTo).
//  2. Open every published seg_{id} directory in segments/{id}/,
//     load their HNSW indexes, publish them into the SegmentSet.
//  3. Replay WAL entries with seq > flushedUpTo: Inserts/Updates go
//     into a fresh memtable; Deletes are recorded in the tracker.
//
// Recovery is idempotent: a second Open after a clean Close replays
// the same WAL tail and ends up with the same in-memory state.
func OpenShard(ctx context.Context, cfg ShardConfig) (*Shard, error) {
	if cfg.ApplyInsert == nil || cfg.MappingFn == nil {
		return nil, fmt.Errorf("storage: ApplyInsert and MappingFn are required")
	}
	shardDir := filepath.Join(cfg.DataDir, "shards", fmt.Sprintf("%d", cfg.ShardID))
	segDir := filepath.Join(shardDir, "segments", fmt.Sprintf("%d", cfg.ShardID))
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, err
	}
	w, err := wal.Open(shardDir, cfg.ShardID, cfg.WALOpts...)
	if err != nil {
		return nil, err
	}
	mt := memtable.New(cfg.ShardID, memtable.Config{})
	dt := NewDeleteTracker()
	sh := &segmentSetHolder{}

	s := &Shard{
		shardID:     cfg.ShardID,
		dataDir:     shardDir,
		segDir:      segDir,
		w:           w,
		mt:          mt,
		dt:          dt,
		setHolder:   sh,
		mappingFn:   cfg.MappingFn,
		applyInsert: cfg.ApplyInsert,
	}
	s.flusher = NewFlushWorker(cfg.ShardID, segDir, w, mt, dt, sh, cfg.MappingFn)

	if err := s.recoverSegments(ctx); err != nil {
		w.Close()
		return nil, err
	}
	if err := s.recoverWAL(ctx); err != nil {
		w.Close()
		return nil, err
	}
	return s, nil
}

// recoverSegments opens every published seg_{id} directory in segDir
// and publishes them into the SegmentSet. Half-built segments (seg_*.tmp)
// are discarded: their SegmentBuilder never reached the rename step.
func (s *Shard) recoverSegments(ctx context.Context) error {
	entries, err := os.ReadDir(s.segDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var segs []*SegmentEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "seg_") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		idStr := strings.TrimPrefix(name, "seg_")
		_, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}
		full := filepath.Join(s.segDir, name)
		sr, err := segment.OpenSegment(full)
		if err != nil {
			return fmt.Errorf("storage: open segment %s: %w", name, err)
		}
		entry := &SegmentEntry{Reader: sr, Indexes: map[types.FieldID]index.LoadedIndex{}}
		// Load HNSW indexes for dense_vector fields.
		fields := s.mappingFn()
		for fid, fd := range fields {
			if fd.Type != meta.TypeDenseVector {
				continue
			}
			vecDir := filepath.Join(full, "vec.idx")
			if _, err := os.Stat(vecDir); err != nil {
				continue
			}
			ix, err := index.NewIndex(index.KindHNSW)
			if err != nil {
				return err
			}
			li, err := ix.Load(&segIndexInput{dir: vecDir})
			if err != nil {
				return fmt.Errorf("storage: load HNSW for seg %s field %d: %w", name, fid, err)
			}
			entry.Indexes[fid] = li
		}
		segs = append(segs, entry)
	}
	if len(segs) > 0 {
		s.setHolder.store(newSegmentSet(segs))
	}
	return nil
}

// recoverWAL replays WAL entries past flushedUpTo into the memtable +
// delete tracker. The first SegmentFlushed marker whose payload <=
// flushedUpTo is skipped (already covered by segments on disk).
// Delete/Update entries dispatch by where the _id currently lives: in
// a published segment → segmentDeletes bitmap; in the memtable →
// memDeletes tombstone with seq.
//
// The applied watermark is advanced BEFORE each Delete/Update is
// dispatched so applyDelete's locateIDInSegment sees the segment as
// visible (segment max_seq <= VisibleSeqNo). Without this pre-advance,
// a Delete targeting a row that was just flushed would miss the
// segment (its max_seq > 0 == applied) and tombstone in memDeletes
// instead, where Iterate's segment path never checks it.
func (s *Shard) recoverWAL(ctx context.Context) error {
	from := s.w.FlushedUpTo() + 1
	err := s.w.Replay(ctx, from, func(e wal.Entry) error {
		// Advance applied to this entry's seq before dispatching
		// deletes so segments with max_seq <= seq are visible to
		// locateIDInSegment.
		if cur := types.SeqNo(s.applied.Load()); e.SeqNo > cur {
			s.applied.Store(uint64(e.SeqNo))
		}
		switch e.Type {
		case wal.EntryInsert:
			row, err := s.applyInsert(e.InsertDoc(), e.SeqNo)
			if err != nil {
				return err
			}
			return s.mt.Append(row)
		case wal.EntryDelete:
			s.applyDelete(e.DeleteID(), e.SeqNo)
			return nil
		case wal.EntryUpdate:
			s.applyDelete(e.UpdateID(), e.SeqNo)
			row, err := s.applyInsert(e.UpdateDoc(), e.SeqNo)
			if err != nil {
				return err
			}
			return s.mt.Append(row)
		case wal.EntrySegmentFlushed, wal.EntryMetadataTick:
			return nil
		}
		return nil
	})
	return err
}

// applyDelete dispatches a Delete by where the _id currently lives
// (§6.2): in a published segment → segmentDeletes (immediate effect);
// in the memtable or unseen → memDeletes with seq (MVCC-aware).
func (s *Shard) applyDelete(id string, seq types.SeqNo) {
	if segID, rowID, ok := s.locateIDInSegment(context.Background(), id); ok {
		s.dt.DeleteInSegment(segID, rowID)
		return
	}
	s.dt.DeleteInMemtable(id, seq)
}

// locateIDInSegment walks the live SegmentSet for _id; returns the
// (segment id, row id) of the first segment containing it. Returns
// ok=false if no segment has the id (caller then knows the id, if
// present, lives in the memtable).
func (s *Shard) locateIDInSegment(ctx context.Context, id string) (types.SegmentID, types.RowID, bool) {
	set := s.setHolder.load()
	for _, e := range set.items {
		if e.Reader == nil {
			continue
		}
		// §8 snapshot prune: skip segments whose max_seq exceeds the
		// current watermark (they're not yet visible).
		if e.Reader.Footer().MaxSeqNo > s.VisibleSeqNo() {
			continue
		}
		rid, ok, err := e.Reader.LookupID(ctx, id)
		if err != nil || !ok {
			continue
		}
		return e.Reader.ID(), rid, true
	}
	return 0, 0, false
}

// Write applies a batch atomically: one WAL group commit assigns seqs,
// then memtable rows are appended under the same lock. A pure delete
// batch (no doc payload) records memtable tombstones only.
//
// The batch may mix Inserts, Deletes, Updates; entries are written in
// the order given. UPDATE is encoded as a single WAL record at the
// assigned seq so its DELETE+INSERT pair shares the seq_no (§6.2).
func (s *Shard) Write(ctx context.Context, batch []WriteOp) (types.SeqNo, error) {
	if len(batch) == 0 {
		return s.mt.VisibleSeqNo(), nil
	}
	entries := make([]wal.Entry, 0, len(batch))
	for _, op := range batch {
		switch op.Kind {
		case WriteInsert:
			entries = append(entries, wal.NewInsert(op.Doc))
		case WriteDelete:
			entries = append(entries, wal.NewDelete(op.ID))
		case WriteUpdate:
			entries = append(entries, wal.NewUpdate(op.ID, op.Doc))
		default:
			return 0, fmt.Errorf("storage: unknown write op kind %d", op.Kind)
		}
	}
	seq, err := s.w.Append(ctx, entries...)
	if err != nil {
		return 0, err
	}
	// Apply to memtable/tracker. Each entry's SeqNo has been assigned
	// by Append and written back into the slice.
	for i, op := range batch {
		e := entries[i]
		switch op.Kind {
		case WriteInsert:
			row, err := s.applyInsert(op.Doc, e.SeqNo)
			if err != nil {
				return seq, err
			}
			if err := s.mt.Append(row); err != nil {
				return seq, err
			}
		case WriteDelete:
			// Dispatch by where the _id currently lives (§6.2).
			s.applyDelete(op.ID, e.SeqNo)
		case WriteUpdate:
			// Hide the old version (segment: immediate; memtable:
			// tombstone with MVCC), then append the new one at the
			// same seq — the strict-inequality rule leaves the new
			// version visible for memtable updates.
			s.applyDelete(op.ID, e.SeqNo)
			row, err := s.applyInsert(op.Doc, e.SeqNo)
			if err != nil {
				return seq, err
			}
			if err := s.mt.Append(row); err != nil {
				return seq, err
			}
		}
	}
	// Advance the applied watermark so pure-delete batches surface
	// their tombstone's visibility in the next Snapshot.
	if cur := types.SeqNo(s.applied.Load()); seq > cur {
		s.applied.Store(uint64(seq))
	}
	return seq, nil
}

// WriteAsync is like Write but uses AppendAsync (design 01 §3.2:
// refresh=false): WAL entries are queued for background fsync without
// blocking. The memtable is still updated synchronously so the write
// is immediately visible to queries. Use this when refresh=false to
// avoid waiting for fsync on every write.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.8
func (s *Shard) WriteAsync(ctx context.Context, batch []WriteOp) (types.SeqNo, error) {
	if len(batch) == 0 {
		return s.mt.VisibleSeqNo(), nil
	}
	entries := make([]wal.Entry, 0, len(batch))
	for _, op := range batch {
		switch op.Kind {
		case WriteInsert:
			entries = append(entries, wal.NewInsert(op.Doc))
		case WriteDelete:
			entries = append(entries, wal.NewDelete(op.ID))
		case WriteUpdate:
			entries = append(entries, wal.NewUpdate(op.ID, op.Doc))
		default:
			return 0, fmt.Errorf("storage: unknown write op kind %d", op.Kind)
		}
	}
	seq, err := s.w.AppendAsync(ctx, entries...)
	if err != nil {
		return 0, err
	}
	for i, op := range batch {
		e := entries[i]
		switch op.Kind {
		case WriteInsert:
			row, err := s.applyInsert(op.Doc, e.SeqNo)
			if err != nil {
				return seq, err
			}
			if err := s.mt.Append(row); err != nil {
				return seq, err
			}
		case WriteDelete:
			s.applyDelete(op.ID, e.SeqNo)
		case WriteUpdate:
			s.applyDelete(op.ID, e.SeqNo)
			row, err := s.applyInsert(op.Doc, e.SeqNo)
			if err != nil {
				return seq, err
			}
			if err := s.mt.Append(row); err != nil {
				return seq, err
			}
		}
	}
	if cur := types.SeqNo(s.applied.Load()); seq > cur {
		s.applied.Store(uint64(seq))
	}
	return seq, nil
}

// Snapshot returns the MVCC read view (memtable watermark + live
// SegmentSet) for query execution (design 01 §8). The watermark is
// max(memtable visible, applied) so pure-delete batches surface their
// tombstone's visibility (the memtable's own visible only advances on
// appends).
func (s *Shard) Snapshot() (types.Snapshot, *SegmentSet) {
	mt := s.mt.VisibleSeqNo()
	applied := types.SeqNo(s.applied.Load())
	if applied > mt {
		mt = applied
	}
	return types.NewSnapshot(mt), s.setHolder.load()
}

// VisibleSeqNo returns the current MVCC watermark.
func (s *Shard) VisibleSeqNo() types.SeqNo {
	mt := s.mt.VisibleSeqNo()
	applied := types.SeqNo(s.applied.Load())
	if applied > mt {
		return applied
	}
	return mt
}

// IsSegmentRowDeleted reports whether (segID,rowID) has been deleted
// via the DeleteTracker (§6.2). Exposed so the query execution path
// can filter deleted rows from segment scan results.
func (s *Shard) IsSegmentRowDeleted(segID types.SegmentID, rowID types.RowID) bool {
	return s.dt.IsSegmentRowDeleted(segID, rowID)
}

// Iterate visits every visible row across the memtable and the live
// segments. The memtable path uses MemTable.Iterate; segment rows
// are visited via ScanColumn over the _id column (which is dense by
// construction). DeleteTracker filters both sources.
//
// For M0 testing simplicity, segments are iterated by their id column.
// The query engine (T15) will use the columnar Scan path directly.
//
// Iterate holds the segmentSetHolder's R lock across both the segment
// set snapshot and the memtable view collection so a concurrent flush
// cannot publish a segment and remove the now-redundant frozen memtable
// in between (which would otherwise yield duplicate rows). Callbacks
// run outside the lock so user-supplied fn cannot deadlock the flusher.
func (s *Shard) Iterate(snap types.Snapshot, fn func(rowID types.RowID, segID types.SegmentID, id string, seq types.SeqNo) bool) error {
	s.setHolder.mu.RLock()
	set := s.setHolder.load()
	var views []memtable.RowView
	s.mt.Iterate(snap, func(v memtable.RowView) bool {
		views = append(views, v)
		return true
	})
	s.setHolder.mu.RUnlock()

	// 1. Memtable rows.
	for _, v := range views {
		if s.dt.IsMemDeleted(v.ID, v.SeqNo, snap) {
			continue
		}
		if !fn(v.RowID, 0, v.ID, v.SeqNo) {
			return nil
		}
	}
	// 2. Segment rows.
	for _, e := range set.items {
		sr := e.Reader
		// §8 snapshot prune: skip segments with max_seq > ReadUpTo.
		if sr.Footer().MaxSeqNo > snap.ReadUpTo {
			continue
		}
		bm := s.dt.SegmentBitmap(sr.ID())
		n := sr.NumRows()
		for r := 0; r < n; r++ {
			if bm != nil && bm.Contains(uint32(r)) {
				continue
			}
			id, err := sr.DocID(types.RowID(r))
			if err != nil {
				return err
			}
			if !fn(types.RowID(r), sr.ID(), id, 0) {
				return nil
			}
		}
	}
	return nil
}

// LookupID resolves _id to (segment, rowID) or (memtable row) visible
// to snap. Returns false if the id is not present or has been deleted.
func (s *Shard) LookupID(ctx context.Context, id string, snap types.Snapshot) (LookupResult, bool) {
	if v, ok := s.mt.Lookup(id, snap); ok {
		if s.dt.IsMemDeleted(v.ID, v.SeqNo, snap) {
			return LookupResult{}, false
		}
		return LookupResult{InMemtable: true, RowID: v.RowID, SeqNo: v.SeqNo}, true
	}
	set := s.setHolder.load()
	for _, e := range set.items {
		sr := e.Reader
		if sr.Footer().MaxSeqNo > snap.ReadUpTo {
			continue
		}
		rid, ok, err := sr.LookupID(ctx, id)
		if err != nil || !ok {
			continue
		}
		if s.dt.IsSegmentRowDeleted(sr.ID(), rid) {
			return LookupResult{}, false
		}
		return LookupResult{SegmentID: sr.ID(), RowID: rid}, true
	}
	return LookupResult{}, false
}

// FlushAll flushes every frozen table (used by tests and the explicit
// ?refresh=wait_for path).
func (s *Shard) FlushAll(ctx context.Context) error {
	s.mt.Freeze()
	return s.flusher.FlushAll(ctx)
}

// Run starts the background flush loop; blocks until ctx is canceled.
func (s *Shard) Run(ctx context.Context) {
	s.flusher.Run(ctx, 0)
}

// Close flushes pending tables and closes the WAL.
func (s *Shard) Close() error {
	ctx := context.Background()
	_ = s.FlushAll(ctx)
	set := s.setHolder.load()
	for _, e := range set.items {
		for _, li := range e.Indexes {
			li.Close()
		}
		if e.Reader != nil {
			e.Reader.Close()
		}
	}
	return s.w.Close()
}

// GetDocSource returns the _source and seq_no for a document by _id,
// checking both the memtable and segments. Returns found=false if the
// id is not present or has been deleted.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.7
func (s *Shard) GetDocSource(ctx context.Context, id string, snap types.Snapshot) ([]byte, types.SeqNo, bool) {
	// Check memtable first.
	if v, ok := s.mt.Lookup(id, snap); ok {
		if s.dt.IsMemDeleted(v.ID, v.SeqNo, snap) {
			return nil, 0, false
		}
		return v.Source, v.SeqNo, true
	}
	// Check segments.
	set := s.setHolder.load()
	for _, e := range set.items {
		if e.Reader == nil || e.Reader.Footer().MaxSeqNo > snap.ReadUpTo {
			continue
		}
		rid, ok, err := e.Reader.LookupID(ctx, id)
		if err != nil || !ok {
			continue
		}
		if s.dt.IsSegmentRowDeleted(e.Reader.ID(), rid) {
			return nil, 0, false
		}
		source, err := e.Reader.Source(rid)
		if err != nil {
			return nil, 0, false
		}
		return source, 0, true
	}
	return nil, 0, false
}

// IterateMemtable visits every visible row in the active memtable only
// (not segments). The callback receives the row's _id, seq_no, and
// _source. Used by the embedded engine's memtable search path (D1/D4).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.5
func (s *Shard) IterateMemtable(snap types.Snapshot, fn func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool) {
	s.mt.Iterate(snap, func(v memtable.RowView) bool {
		if s.dt.IsMemDeleted(v.ID, v.SeqNo, snap) {
			return true // skip deleted, continue iteration
		}
		return fn(v.RowID, v.ID, v.SeqNo, v.Source)
	})
}

// WriteOpKind tags the kind of one write operation in a Write batch.
type WriteOpKind int

const (
	WriteInsert WriteOpKind = iota
	WriteDelete
	WriteUpdate
)

// WriteOp is one operation in a Shard.Write batch.
type WriteOp struct {
	Kind WriteOpKind
	ID   string // required for Delete/Update
	Doc  []byte // required for Insert/Update
}

// LookupResult is the outcome of an _id point lookup.
type LookupResult struct {
	InMemtable bool
	SegmentID  types.SegmentID
	RowID      types.RowID
	SeqNo      types.SeqNo
}

// ErrNoWork signals "nothing to flush" (returned by FlushOnce).
var ErrNoWork = errors.New("storage: no frozen tables to flush")
