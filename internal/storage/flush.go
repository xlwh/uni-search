package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/memtable"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/internal/storage/wal"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// flushLimiter caps the global number of concurrent flush operations
// across all shards to min(4, CPU/2) (design 01 §5 point 4).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.3
var flushLimiter = make(chan struct{}, maxConcurrentFlushes())

func maxConcurrentFlushes() int {
	n := runtime.NumCPU() / 2
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}

// FlushWorker publishes immutable segments from frozen MemTables (design
// 01 §5). Per-shard flush concurrency is 1 (§5 point 4); the worker
// encodes columns through the segment builder, builds HNSW indexes for
// dense_vector columns into the segment's vec.idx/ subdirectory, then
// atomically renames the tmp directory into place, appends a
// SegmentFlushed marker to the WAL and drops the frozen table.
type FlushWorker struct {
	shardID   types.ShardID
	segDir    string // <data-dir>/segments/{shard_id}
	wal       *wal.LocalWAL
	memtable  *memtable.MemTable
	deletes   *DeleteTracker
	setHolder *segmentSetHolder

	// segSeq generates segment IDs (snowflake-shaped:
	// timestamp_ms << 20 | counter).
	segSeq atomic.Uint64

	// mappingFn returns the current field definitions for the shard.
	mappingFn func() map[types.FieldID]*meta.FieldDef
}

// NewFlushWorker constructs a flush worker. segDir is the directory under
// which seg_{id} directories live for this shard (already shard-scoped;
// the storage layer computes it as <data>/shards/<id>/segments/<id>).
func NewFlushWorker(
	shardID types.ShardID,
	segDir string,
	w *wal.LocalWAL,
	mt *memtable.MemTable,
	dt *DeleteTracker,
	sh *segmentSetHolder,
	mappingFn func() map[types.FieldID]*meta.FieldDef,
) *FlushWorker {
	return &FlushWorker{
		shardID:   shardID,
		segDir:    segDir,
		wal:       w,
		memtable:  mt,
		deletes:   dt,
		setHolder: sh,
		mappingFn: mappingFn,
	}
}

// FlushOnce flushes the oldest frozen table. os.ErrNotExist signals no
// pending work.
func (f *FlushWorker) FlushOnce(ctx context.Context) error {
	frozen := f.memtable.Frozen()
	if len(frozen) == 0 {
		return os.ErrNotExist
	}
	return f.flushTable(ctx, frozen[0])
}

// FlushAll flushes every frozen table currently pending.
func (f *FlushWorker) FlushAll(ctx context.Context) error {
	for {
		if err := f.FlushOnce(ctx); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
	}
}

// Run polls MaybeFreeze + FlushOnce every tick until ctx is canceled.
func (f *FlushWorker) Run(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.memtable.MaybeFreeze()
			for {
				if err := f.FlushOnce(ctx); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						break
					}
					return
				}
			}
		}
	}
}

func (f *FlushWorker) flushTable(ctx context.Context, t *memtable.Table) error {
	// Acquire global flush slot (design 01 §5: multi-shard concurrency
	// = min(4, CPU/2)). Per-shard concurrency is already 1 (single
	// FlushWorker per shard), this limits cross-shard parallelism.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.3
	select {
	case flushLimiter <- struct{}{}:
		defer func() { <-flushLimiter }()
	case <-ctx.Done():
		return ctx.Err()
	}

	if t.NumRows() == 0 {
		f.memtable.RemoveFrozen(t)
		return nil
	}
	segID := f.allocSegID()
	if err := os.MkdirAll(f.segDir, 0o755); err != nil {
		return err
	}
	builder, err := segment.NewSegmentBuilder(f.segDir, segID, f.shardID)
	if err != nil {
		return err
	}

	fields := f.mappingFn()

	// 1. Encode scalar columns.
	totalRows := t.NumRows()
	for _, fid := range t.FieldIDs() {
		fd, ok := fields[fid]
		if !ok {
			continue
		}
		cb := t.Column(fid)
		if cb == nil {
			continue // vector column, handled in step 4
		}
		col, err := builder.Column(fid, fd.Name, fd.Type)
		if err != nil {
			builder.Abort()
			return err
		}
		if err := copyScalarColumn(col, cb, fd.Type, totalRows); err != nil {
			builder.Abort()
			return err
		}
	}

	// 2. Append _id and _source for every row.
	for i := 0; i < t.NumRows(); i++ {
		builder.AppendID(t.ID(types.RowID(i)))
		builder.AppendSource(t.Source(types.RowID(i)))
	}

	// 3. MVCC seq range.
	if t.NumRows() > 0 {
		minSeq, maxSeq := t.SeqNo(0), t.SeqNo(0)
		for i := 1; i < t.NumRows(); i++ {
			s := t.SeqNo(types.RowID(i))
			if s < minSeq {
				minSeq = s
			}
			if s > maxSeq {
				maxSeq = s
			}
		}
		builder.SetSeqRange(minSeq, maxSeq)
	}

	// 4. Build HNSW graph files into tmpDir/vec.idx/. The files live
	// inside the builder's tmp directory so Finish() publishes them
	// atomically with the segment columns (§5 atomicity).
	if err := f.buildVectorIndexes(ctx, builder, t, segID, fields); err != nil {
		builder.Abort()
		return err
	}

	// 4b. Build inverted indexes for text fields into tmpDir/inv.idx/.
	// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.6
	if err := f.buildInvertedIndexes(ctx, t, segID, fields); err != nil {
		builder.Abort()
		return err
	}

	// 4c. Build scalar indexes (BKD/Bitmap) for numeric/keyword fields
	// into tmpDir/scalar.idx/.
	if err := f.buildScalarIndexes(ctx, t, segID, fields); err != nil {
		builder.Abort()
		return err
	}

	// 5. Promote memDeletes tombstones for rows in this table into
	// segmentDeletes[segID]. A tombstone at seq D hides the row at
	// seq R iff D > R (§6.2/§8); the strict inequality preserves the
	// UPDATE same-seq semantics (new value visible). Promoted tombstones
	// are removed from memDeletes so recovery + Iterate stay consistent.
	promoted := f.promoteTombstones(t, segID)

	// 6. Atomic publish (rename tmp → final + fsync + footer). After
	// this returns, the segment and its vec.idx/ subdirectory are
	// durably visible at seg_{segID}/.
	segReader, err := builder.Finish()
	if err != nil {
		return err
	}
	_ = promoted // segmentDeletes already updated in-place

	// 7. Load all indexes from the published segment directory. The
	// Load must happen after Finish because segIndexInput points at
	// the final (post-rename) location.
	loaded, err := f.loadVectorIndexes(segID, fields)
	if err != nil {
		return err
	}
	invLoaded, err := f.loadInvertedIndexes(segID, fields)
	if err != nil {
		return err
	}
	scalarLoaded, err := f.loadScalarIndexes(segID, fields)
	if err != nil {
		return err
	}
	// Merge all index types into one map.
	for fid, li := range invLoaded {
		loaded[fid] = li
	}
	for fid, li := range scalarLoaded {
		loaded[fid] = li
	}

	// 8. Publish into SegmentSet (atomic.Pointer swap) AND remove the
	// now-redundant frozen memtable under one W lock so a concurrent
	// Iterate never observes the row in both places at once. Without
	// this pairing a reader could collect memtable views (still holding
	// the frozen table) right after the segment was published, then
	// visit the segment rows too — yielding duplicate rows.
	f.publishSegmentAndRemoveFrozen(&SegmentEntry{Reader: segReader, Indexes: loaded}, t)

	// 9. WAL SegmentFlushed marker — recovery skips the flushed prefix.
	maxSeq := segReader.Footer().MaxSeqNo
	if _, err := f.wal.Append(ctx, wal.NewSegmentFlushed(maxSeq)); err != nil {
		// The segment is durable on disk; the missing marker only means
		// recovery will replay these WAL entries (inserts land in a new
		// memtable, deletes tombstone again — idempotent).
		_ = err
	}

	// 10. Drop the flushed table.
	// (Already removed in step 8 under the publish lock.)
	return nil
}

// promoteTombstones walks the frozen table's rows and migrates any
// memDeletes tombstone that targets one of them into segmentDeletes.
// Returns the count of promoted tombstones (mostly for tests).
func (f *FlushWorker) promoteTombstones(t *memtable.Table, segID types.SegmentID) int {
	if t.NumRows() == 0 {
		return 0
	}
	count := 0
	for i := 0; i < t.NumRows(); i++ {
		id := t.ID(types.RowID(i))
		rowSeq := t.SeqNo(types.RowID(i))
		// Check via the tracker; we cannot peek the tombstone seq
		// directly without a getter, so use the existing helper.
		if delSeq := f.deletes.MemDeleteSeq(id); delSeq > 0 {
			if delSeq > rowSeq {
				f.deletes.PromoteMemDelete(id, segID, types.RowID(i))
				count++
			}
		}
	}
	return count
}

// publishSegmentAndRemoveFrozen atomically (a) appends the new segment
// to the live SegmentSet and (b) removes the now-flushed frozen memtable
// under the segmentSetHolder's W lock. Iterate holds the R lock across
// "snapshot segment set + collect memtable views", so this paired
// update is observed as a single transition: readers either see the
// old set (no segment) + the frozen memtable, or the new set (segment
// published) with the frozen memtable already gone — never both at
// once (which would yield duplicate rows) or neither (which would
// yield missing rows).
func (f *FlushWorker) publishSegmentAndRemoveFrozen(e *SegmentEntry, t *memtable.Table) {
	f.setHolder.mu.Lock()
	defer f.setHolder.mu.Unlock()
	cur := f.setHolder.load()
	items := make([]*SegmentEntry, 0, len(cur.items)+1)
	items = append(items, cur.items...)
	items = append(items, e)
	f.setHolder.store(newSegmentSet(items))
	f.memtable.RemoveFrozen(t)
}

// buildVectorIndexes writes the HNSW graph files for each dense_vector
// field present in the frozen table into tmpDir/vec.idx/ (the
// SegmentBuilder's tmpDir, computed deterministically — naming matches
// segment.builder: parentDir/seg_{segID}.tmp/). When Finish renames
// tmpDir → seg_{segID}, the vec.idx/ subdir is moved atomically with it.
//
// The index files are NOT loaded yet — Load must run after Finish so
// segIndexInput points at the post-rename location. Use
// loadVectorIndexes to obtain LoadedIndex handles.
func (f *FlushWorker) buildVectorIndexes(
	ctx context.Context,
	_ *segment.SegmentBuilder,
	t *memtable.Table,
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) error {
	for _, fid := range t.FieldIDs() {
		fd, ok := fields[fid]
		if !ok || fd.Type != meta.TypeDenseVector {
			continue
		}
		vb := t.Vector(fid)
		if vb == nil || len(vb.Dense) == 0 {
			continue
		}
		if err := assertDenseVectorCoverage(vb, t.NumRows(), fid); err != nil {
			return err
		}
		provider, err := newMemtableColumnProvider(fd.Name, int64(t.NumRows()), vb)
		if err != nil {
			return err
		}
		tmpVecDir := filepath.Join(f.segDir, fmt.Sprintf("seg_%d.tmp", segID), "vec.idx")
		if err := os.MkdirAll(tmpVecDir, 0o755); err != nil {
			return err
		}
		ix, err := index.NewIndex(index.KindHNSW)
		if err != nil {
			return err
		}
		if err := ix.Build(ctx, provider, &segIndexOutput{dir: tmpVecDir}); err != nil {
			return err
		}
	}
	return nil
}

// loadVectorIndexes opens the published HNSW indexes from
// seg_{segID}/vec.idx/. Called after builder.Finish() so the directory
// is durably visible; returns one LoadedIndex per dense_vector field
// that has a vec.idx/ subdirectory.
func (f *FlushWorker) loadVectorIndexes(
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) (map[types.FieldID]index.LoadedIndex, error) {
	loaded := make(map[types.FieldID]index.LoadedIndex)
	for fid, fd := range fields {
		if fd.Type != meta.TypeDenseVector {
			continue
		}
		vecDir := filepath.Join(f.segDir, fmt.Sprintf("seg_%d", segID), "vec.idx")
		if _, err := os.Stat(vecDir); err != nil {
			continue
		}
		ix, err := index.NewIndex(index.KindHNSW)
		if err != nil {
			return nil, err
		}
		li, err := ix.Load(&segIndexInput{dir: vecDir})
		if err != nil {
			return nil, fmt.Errorf("storage: load HNSW for seg %d field %d: %w", segID, fid, err)
		}
		loaded[fid] = li
	}
	return loaded, nil
}

// buildInvertedIndexes writes inverted index files for each text field
// present in the frozen table into tmpDir/inv.idx/{fieldID}/. Mirrors
// buildVectorIndexes: the index files live inside the builder's tmp
// directory so Finish() publishes them atomically.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.6
func (f *FlushWorker) buildInvertedIndexes(
	ctx context.Context,
	t *memtable.Table,
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) error {
	tmpBase := filepath.Join(f.segDir, fmt.Sprintf("seg_%d.tmp", segID), "inv.idx")
	for _, fid := range t.FieldIDs() {
		fd, ok := fields[fid]
		if !ok {
			continue
		}
		if fd.Type != meta.TypeText {
			continue
		}
		cb := t.Column(fid)
		if cb == nil || len(cb.Strings) == 0 {
			continue
		}
		dir := filepath.Join(tmpBase, fmt.Sprintf("%d", fid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		provider := newMemtableTextColumnProvider(fd.Name, int64(t.NumRows()), cb)
		ix, err := index.NewIndex(index.KindInverted)
		if err != nil {
			if errors.Is(err, index.ErrUnknownKind) {
				continue // index kind not registered, skip
			}
			return err
		}
		if err := ix.Build(ctx, provider, &segIndexOutput{dir: dir}); err != nil {
			return fmt.Errorf("storage: build inverted index seg %d field %d: %w", segID, fid, err)
		}
	}
	return nil
}

// buildScalarIndexes writes scalar index files (BKD/Bitmap) for each
// numeric/keyword field into tmpDir/scalar.idx/{fieldID}/.
func (f *FlushWorker) buildScalarIndexes(
	ctx context.Context,
	t *memtable.Table,
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) error {
	tmpBase := filepath.Join(f.segDir, fmt.Sprintf("seg_%d.tmp", segID), "scalar.idx")
	for _, fid := range t.FieldIDs() {
		fd, ok := fields[fid]
		if !ok {
			continue
		}
		cb := t.Column(fid)
		if cb == nil {
			continue
		}
		var kind index.IndexKind
		switch fd.Type {
		case meta.TypeLong, meta.TypeDate, meta.TypeDouble, meta.TypeFloat, meta.TypeIP, meta.TypeGeoPoint:
			kind = index.KindBKD
		case meta.TypeKeyword:
			kind = index.KindBitmap
		case meta.TypeBoolean:
			// Boolean values are stored in cb.Bools, not cb.Strings.
			// The bitmap index builder uses memtableBytesColumnProvider
			// which reads cb.Strings, so we skip boolean bitmap index
			// and rely on column data + zone map for filtering.
			continue
		default:
			continue
		}
		dir := filepath.Join(tmpBase, fmt.Sprintf("%d", fid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		var provider index.ColumnProvider
		switch fd.Type {
		case meta.TypeLong:
			provider = newMemtableLongColumnProvider(fd.Name, int64(t.NumRows()), cb)
		case meta.TypeDate:
			// Date values may be stored as int64 (epoch millis) or as
			// strings (e.g., "2026-01-15"). If strings, cb.Longs is empty
			// and the Long provider would panic. Skip BKD index for
			// string-stored dates; column data + zone map still works.
			if len(cb.Longs) == 0 {
				continue
			}
			provider = newMemtableLongColumnProvider(fd.Name, int64(t.NumRows()), cb)
		case meta.TypeDouble, meta.TypeFloat:
			provider = newMemtableDoubleColumnProvider(fd.Name, int64(t.NumRows()), cb)
		case meta.TypeKeyword:
			provider = newMemtableBytesColumnProvider(fd.Name, int64(t.NumRows()), cb)
		default:
			continue
		}
		ix, err := index.NewIndex(kind)
		if err != nil {
			if errors.Is(err, index.ErrUnknownKind) {
				continue // index kind not registered, skip
			}
			return err
		}
		if err := ix.Build(ctx, provider, &segIndexOutput{dir: dir}); err != nil {
			return fmt.Errorf("storage: build scalar index seg %d field %d: %w", segID, fid, err)
		}
	}
	return nil
}

// loadInvertedIndexes opens published inverted indexes from
// seg_{segID}/inv.idx/{fieldID}/. Returns one LoadedIndex per text
// field that has an inv.idx subdirectory.
func (f *FlushWorker) loadInvertedIndexes(
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) (map[types.FieldID]index.LoadedIndex, error) {
	loaded := make(map[types.FieldID]index.LoadedIndex)
	for fid, fd := range fields {
		if fd.Type != meta.TypeText {
			continue
		}
		dir := filepath.Join(f.segDir, fmt.Sprintf("seg_%d", segID), "inv.idx", fmt.Sprintf("%d", fid))
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		ix, err := index.NewIndex(index.KindInverted)
		if err != nil {
			if errors.Is(err, index.ErrUnknownKind) {
				continue // index kind not registered, skip
			}
			return nil, err
		}
		li, err := ix.Load(&segIndexInput{dir: dir})
		if err != nil {
			return nil, fmt.Errorf("storage: load inverted index seg %d field %d: %w", segID, fid, err)
		}
		loaded[fid] = li
	}
	return loaded, nil
}

// loadScalarIndexes opens published scalar indexes (BKD/Bitmap) from
// seg_{segID}/scalar.idx/{fieldID}/.
func (f *FlushWorker) loadScalarIndexes(
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) (map[types.FieldID]index.LoadedIndex, error) {
	loaded := make(map[types.FieldID]index.LoadedIndex)
	for fid, fd := range fields {
		switch fd.Type {
		case meta.TypeLong, meta.TypeDate, meta.TypeDouble, meta.TypeFloat, meta.TypeIP, meta.TypeGeoPoint:
			// BKD
		case meta.TypeKeyword, meta.TypeBoolean:
			// Bitmap
		default:
			continue
		}
		dir := filepath.Join(f.segDir, fmt.Sprintf("seg_%d", segID), "scalar.idx", fmt.Sprintf("%d", fid))
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		var kind index.IndexKind
		switch fd.Type {
		case meta.TypeLong, meta.TypeDate, meta.TypeDouble, meta.TypeFloat, meta.TypeIP, meta.TypeGeoPoint:
			kind = index.KindBKD
		case meta.TypeKeyword, meta.TypeBoolean:
			kind = index.KindBitmap
		}
		ix, err := index.NewIndex(kind)
		if err != nil {
			if errors.Is(err, index.ErrUnknownKind) {
				continue // index kind not registered, skip
			}
			return nil, err
		}
		li, err := ix.Load(&segIndexInput{dir: dir})
		if err != nil {
			return nil, fmt.Errorf("storage: load scalar index seg %d field %d: %w", segID, fid, err)
		}
		loaded[fid] = li
	}
	return loaded, nil
}

// ColumnWriter, dispatching by FieldType and padding rows that have no
// entry (sparse column) with AppendNull. GeoPoint is not a storable
// block column in M0 (column_writer.go) — it lives in the BKD index
// only, so we skip it here.
func copyScalarColumn(w *segment.ColumnWriter, cb *memtable.ColumnBuilder, ft meta.FieldType, totalRows int) error {
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
		return emitSparse(w, totalRows, cb.RowIDs, func(i int) { w.AppendString(cb.Strings[i]) })
	case meta.TypeLong, meta.TypeIP:
		return emitSparse(w, totalRows, cb.RowIDs, func(i int) { w.AppendLong(cb.Longs[i]) })
	case meta.TypeDate:
		// Date values may be stored as int64 (epoch millis) or as
		// strings (e.g., "2026-01-15") depending on how they were
		// parsed from JSON. Dispatch accordingly.
		if len(cb.Longs) > 0 {
			return emitSparse(w, totalRows, cb.RowIDs, func(i int) { w.AppendLong(cb.Longs[i]) })
		}
		return emitSparse(w, totalRows, cb.RowIDs, func(i int) {
			w.AppendLong(parseDateToMillis(cb.Strings[i]))
		})
	case meta.TypeDouble, meta.TypeFloat:
		return emitSparse(w, totalRows, cb.RowIDs, func(i int) { w.AppendDouble(cb.Doubles[i]) })
	case meta.TypeBoolean:
		return emitSparse(w, totalRows, cb.RowIDs, func(i int) { w.AppendBool(cb.Bools[i]) })
	case meta.TypeGeoPoint, meta.TypeDenseVector, meta.TypeSparseVector,
		meta.TypeObject, meta.TypeNested:
		return nil
	default:
		return nil
	}
}

// parseDateToMillis converts a date string to epoch milliseconds.
// Supports common formats: RFC3339, "2006-01-02", "2006-01-02T15:04:05".
func parseDateToMillis(s string) int64 {
	// Try RFC3339 first (most common in ES).
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	// Try date-only format.
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UnixMilli()
	}
	// Try datetime without timezone.
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t.UnixMilli()
	}
	// Fallback: return 0 for unparseable dates.
	return 0
}

// emitSparse walks positions 0..totalRows-1, calling emit(i) when the
// memtable builder has a value at row RowIDs[i] (in order), else
// AppendNull. The memtable ColumnBuilder keeps RowIDs ascending so a
// single forward pass is enough; misordered RowIDs (write-path bug)
// surface as a returned error.
func emitSparse(w *segment.ColumnWriter, totalRows int, rowIDs []types.RowID, emit func(i int)) error {
	want := 0
	for i, rid := range rowIDs {
		if int(rid) < want {
			return fmt.Errorf("storage: column RowIDs out of order at %d (row %d, want >= %d)",
				i, rid, want)
		}
		for int(rid) > want {
			w.AppendNull()
			want++
		}
		emit(i)
		want++
	}
	for want < totalRows {
		w.AppendNull()
		want++
	}
	return nil
}

// allocSegID returns a process-unique segment id: timestamp_ms (44 bits)
// shifted left by 20 with a 20-bit counter — snowflake-shaped, fits in
// uint64.
func (f *FlushWorker) allocSegID() types.SegmentID {
	ms := uint64(time.Now().UnixMilli())
	counter := f.segSeq.Add(1)
	return types.SegmentID((ms << 20) | (counter & 0xFFFFF))
}
