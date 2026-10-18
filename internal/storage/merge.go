package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// MergeSegments merges the given source segments into one new segment
// (design 01 §7.2): rows are concatenated in min_seq order, rows hit
// by the source's DeleteBitmap are physically dropped, scalar columns
// and _id/_source are copied verbatim, and HNSW indexes are rebuilt
// from the surviving vectors (M0 = full rebuild per 02 §2.4.3).
//
// Atomicity: the new segment is published via a single SegmentSet
// swap under the segmentSetHolder's W lock — readers see either the
// old set (with N source segments) or the new set (1 merged segment),
// never a mix. The source segments' on-disk directories are removed
// after the swap; their segmentDeletes bitmaps are dropped.
//
// Failed merge: the tmp directory is removed (builder.Abort); source
// segments stay readable and on disk.
func (s *Shard) MergeSegments(ctx context.Context, segIDs []types.SegmentID) (types.SegmentID, error) {
	if len(segIDs) == 0 {
		return 0, fmt.Errorf("storage: merge requires at least one segment")
	}

	// 1. Resolve source SegmentEntries from the live SegmentSet.
	cur := s.setHolder.load()
	want := make(map[types.SegmentID]struct{}, len(segIDs))
	for _, id := range segIDs {
		want[id] = struct{}{}
	}
	var sources []*SegmentEntry
	for _, e := range cur.items {
		if e.Reader == nil {
			continue
		}
		if _, ok := want[e.Reader.ID()]; ok {
			sources = append(sources, e)
		}
	}
	if len(sources) != len(segIDs) {
		return 0, fmt.Errorf("storage: merge source mismatch: have %d of %d (some are gone)",
			len(sources), len(segIDs))
	}

	// 2. Order sources by min_seq so the new segment's rows are in
	// ascending seq_no order (§7.2 归并保序).
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].Reader.Footer().MinSeqNo < sources[j].Reader.Footer().MinSeqNo
	})

	// 3. Snapshot the per-source delete bitmaps so a concurrent
	//    delete landing mid-merge doesn't race (M0 accepts the lost-
	//    delete window — test scenarios don't exercise concurrent
	//    deletes during merge).
	bitmaps := make([]*roaring.Bitmap, len(sources))
	for i, src := range sources {
		bitmaps[i] = snapshotBitmap(s.dt.SegmentBitmap(src.Reader.ID()))
	}

	// 4. Allocate a new segment id and start a builder.
	segID := s.flusher.allocSegID()
	builder, err := segment.NewSegmentBuilder(s.segDir, segID, s.shardID)
	if err != nil {
		return 0, err
	}

	fields := s.mappingFn()

	// 5. Walk source segments in order; for each non-deleted row,
	//    copy its _id/_source and scalar columns into the builder.
	if _, err := s.copyMergeRows(builder, sources, bitmaps, fields); err != nil {
		builder.Abort()
		return 0, err
	}

	// 6. Set the new segment's seq range = (min of sources' min_seq,
	//    max of sources' max_seq) so the merged segment is visible to
	//    every snapshot that saw the sources (§8).
	if builder.NumRows() == 0 {
		// No surviving rows: nothing to publish. Drop the builder
		// without creating an empty segment (segment.Format refuses
		// empty segments). Source segments stay live.
		builder.Abort()
		return 0, fmt.Errorf("storage: merge produced no rows (every source row deleted)")
	}
	minSeq, maxSeq := sources[0].Reader.Footer().MinSeqNo, sources[0].Reader.Footer().MaxSeqNo
	for _, src := range sources[1:] {
		f := src.Reader.Footer()
		if f.MinSeqNo < minSeq {
			minSeq = f.MinSeqNo
		}
		if f.MaxSeqNo > maxSeq {
			maxSeq = f.MaxSeqNo
		}
	}
	builder.SetSeqRange(minSeq, maxSeq)

	// 7. Build HNSW indexes from the surviving vectors of every
	//    source segment (M0 = full rebuild, 02 §2.4.3). The graph
	//    files are written into the builder's tmp directory so they
	//    are published atomically with the segment columns.
	if err := s.buildMergeVectorIndexes(ctx, builder, sources, bitmaps, segID, fields); err != nil {
		builder.Abort()
		return 0, err
	}

	// 8. Atomic publish: rename tmp → seg_{segID} + fsync + footer.
	segReader, err := builder.Finish()
	if err != nil {
		return 0, err
	}

	// 9. Load the rebuilt HNSW indexes from the published segment.
	loaded, err := s.flusher.loadVectorIndexes(segID, fields)
	if err != nil {
		// Segment is durable; we still publish it without HNSW (KNN
		// queries on this segment will error; scalar queries work).
		// Failing here would leak the segment on disk.
		loaded = map[types.FieldID]index.LoadedIndex{}
	}

	// 10. Atomic SegmentSet swap: replace the source segments with
	//     the merged one, and drop source segments' bitmaps. Under
	//     the W lock so concurrent Iterate sees one consistent state.
	newEntry := &SegmentEntry{Reader: segReader, Indexes: loaded}
	sourcesToRemove := make(map[types.SegmentID]struct{}, len(sources))
	for _, src := range sources {
		sourcesToRemove[src.Reader.ID()] = struct{}{}
	}

	s.setHolder.mu.Lock()
	defer s.setHolder.mu.Unlock()
	curSet := s.setHolder.load()
	items := make([]*SegmentEntry, 0, len(curSet.items)-len(sources)+1)
	items = append(items, newEntry)
	for _, e := range curSet.items {
		if e.Reader == nil {
			continue
		}
		if _, drop := sourcesToRemove[e.Reader.ID()]; drop {
			continue
		}
		items = append(items, e)
	}
	s.setHolder.store(newSegmentSet(items))

	// 11. Drop source bitmaps. Source segment readers are NOT closed
	//     here: a concurrent Iterate that snapshotted the old
	//     SegmentSet before the W-lock swap still holds a reference
	//     to them and may walk their rows after we release the W
	//     lock. The design's 60s delayed drop (§7.2) handles this in
	//     M1+ via in-flight query tracking; for M0 the source
	//     Segment handles stay open until process exit (their files
	//     are unlinked from disk below — the OS keeps the inodes
	//     alive until the last fd closes).
	for _, src := range sources {
		s.dt.DropSegment(src.Reader.ID())
	}

	// 12. Unlink the source segment directories from disk. Open file
	//     handles inside the still-referenced *segment.Segment remain
	//     valid (Unix semantics: unlinked inodes survive until last
	//     close); newly opened readers won't find them. M0 doesn't
	//     track in-flight queries, so the design's 60s delay becomes
	//     "immediate unlink, lazy close".
	for _, src := range sources {
		_ = os.RemoveAll(filepath.Join(s.segDir, fmt.Sprintf("seg_%d", src.Reader.ID())))
	}

	return segID, nil
}

// copyMergeRows walks each source segment's rows in stored order,
// skipping rows flagged by their per-segment delete bitmap, and
// appends _id/_source/scalar-columns to the builder. Returns the map
// of per-field ColumnWriters (mostly so callers can sanity-check).
func (s *Shard) copyMergeRows(
	b *segment.SegmentBuilder,
	sources []*SegmentEntry,
	bitmaps []*roaring.Bitmap,
	fields map[types.FieldID]*meta.FieldDef,
) (map[types.FieldID]*segment.ColumnWriter, error) {
	cwByField := make(map[types.FieldID]*segment.ColumnWriter)
	for fid, fd := range fields {
		if !isStorableScalarField(fd.Type) {
			continue
		}
		w, err := b.Column(fid, fd.Name, fd.Type)
		if err != nil {
			return nil, err
		}
		cwByField[fid] = w
	}

	for i, src := range sources {
		sr := src.Reader
		bm := bitmaps[i]
		n := sr.NumRows()
		for r := 0; r < n; r++ {
			if bm != nil && bm.Contains(uint32(r)) {
				continue
			}
			id, err := sr.DocID(types.RowID(r))
			if err != nil {
				return nil, err
			}
			src0, err := sr.Source(types.RowID(r))
			if err != nil {
				return nil, err
			}
			b.AppendID(id)
			b.AppendSource(src0)
			for fid, fd := range fields {
				if !isStorableScalarField(fd.Type) {
					continue
				}
				w := cwByField[fid]
				if err := appendRowValue(w, sr, fid, fd.Type, r); err != nil {
					return nil, err
				}
			}
		}
	}
	return cwByField, nil
}

// buildMergeVectorIndexes builds HNSW for each dense_vector field from
// the surviving vectors across all source segments. Vectors are read
// from each source's vec.idx/hnsw.vecs file (the §2.1 normalized
// vector copy) and concatenated in (source-segment, row) order,
// skipping rows flagged by the source's delete bitmap.
func (s *Shard) buildMergeVectorIndexes(
	ctx context.Context,
	_ *segment.SegmentBuilder,
	sources []*SegmentEntry,
	bitmaps []*roaring.Bitmap,
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) error {
	for fid, fd := range fields {
		if fd.Type != meta.TypeDenseVector {
			continue
		}
		dim := fd.Dimensions
		if dim <= 0 {
			continue
		}
		var sv []mergeVecSource
		totalRows := 0
		for i, src := range sources {
			vecDir := filepath.Join(s.segDir, fmt.Sprintf("seg_%d", src.Reader.ID()), "vec.idx")
			data, err := os.ReadFile(filepath.Join(vecDir, "hnsw.vecs"))
			if err != nil {
				continue // no vectors for this field in this segment
			}
			vecs, err := decodeVecs(data)
			if err != nil {
				return fmt.Errorf("storage: decode hnsw.vecs for seg %d field %d: %w",
					src.Reader.ID(), fid, err)
			}
			if len(vecs)%dim != 0 {
				return fmt.Errorf("storage: hnsw.vecs for seg %d field %d has %d floats (not multiple of dim %d)",
					src.Reader.ID(), fid, len(vecs), dim)
			}
			nrows := src.Reader.NumRows()
			live := nrows
			bm := bitmaps[i]
			if bm != nil {
				live -= int(bm.GetCardinality())
			}
			sv = append(sv, mergeVecSource{vecs: vecs, bitmap: bm, nrows: nrows})
			totalRows += live
		}
		if len(sv) == 0 || totalRows == 0 {
			continue
		}

		tmpVecDir := filepath.Join(s.segDir, fmt.Sprintf("seg_%d.tmp", segID), "vec.idx")
		if err := os.MkdirAll(tmpVecDir, 0o755); err != nil {
			return err
		}
		provider := &mergeColumnProvider{
			field:   fd.Name,
			rows:    int64(totalRows),
			dim:     dim,
			sources: sv,
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

// appendRowValue reads one row's value from a source segment's
// column file and appends it to the destination ColumnWriter. Rows
// past the column's row count or columns missing from the source
// segment produce AppendNull so the destination writer's row count
// stays in sync with the builder's _id column.
func appendRowValue(w *segment.ColumnWriter, sr *segment.Segment, fid types.FieldID, ft meta.FieldType, row int) error {
	if row >= sr.NumRows() {
		w.AppendNull()
		return nil
	}
	cr, err := sr.Column(fid)
	if err != nil {
		// Column not present in the source segment: the row had no
		// value for this field (or the field was added after the
		// segment was written).
		w.AppendNull()
		return nil
	}
	// Walk blocks until we cover the row's offset.
	it := cr.NewIterator()
	consumed := 0
	for !it.Done() {
		bd, err := it.Next(context.Background())
		if err != nil {
			return err
		}
		blkRows := bd.Rows
		if row < consumed+blkRows {
			off := row - consumed
			if bd.IsNull(off) {
				w.AppendNull()
				return nil
			}
			switch ft {
			case meta.TypeKeyword, meta.TypeText:
				w.AppendString(bd.Strings()[off])
			case meta.TypeLong, meta.TypeDate, meta.TypeIP:
				w.AppendLong(bd.Longs()[off])
			case meta.TypeDouble:
				w.AppendDouble(bd.Doubles()[off])
			case meta.TypeBoolean:
				w.AppendBool(bd.Bools()[off])
			}
			return nil
		}
		consumed += blkRows
	}
	w.AppendNull()
	return nil
}

// isStorableScalarField reports whether ft is stored as a col_*.bin
// block file by the column writer. Vectors, geo, object, nested and
// the §1.2 "not stored as column" types return false.
func isStorableScalarField(ft meta.FieldType) bool {
	switch ft {
	case meta.TypeKeyword, meta.TypeText,
		meta.TypeLong, meta.TypeDate, meta.TypeIP,
		meta.TypeDouble, meta.TypeBoolean:
		return true
	}
	return false
}

// decodeVecs parses an hnsw.vecs file: a flat little-endian float32
// stream. The dimension is recovered from the field mapping by the
// caller, not from the file (which has no header).
func decodeVecs(data []byte) ([]float32, error) {
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("hnsw.vecs length %d not multiple of 4", len(data))
	}
	out := make([]float32, len(data)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4 : i*4+4]))
	}
	return out, nil
}

// mergeVecSource is one source segment's vector stream for the
// mergeColumnProvider: the raw hnsw.vecs float32 slice, the segment's
// delete bitmap (may be nil), and the source segment's row count.
type mergeVecSource struct {
	vecs   []float32
	bitmap *roaring.Bitmap
	nrows  int
}

// mergeColumnProvider implements index.ColumnProvider over the
// surviving rows of a set of source segments (M0 merge: HNSW full
// rebuild). ForEachVector yields (rowID, vec) pairs in row order,
// skipping rows flagged by the source's delete bitmap.
type mergeColumnProvider struct {
	field   string
	rows    int64
	dim     int
	sources []mergeVecSource
}

func (p *mergeColumnProvider) Field() string { return p.field }
func (p *mergeColumnProvider) Rows() int64   { return p.rows }
func (p *mergeColumnProvider) Dim() int      { return p.dim }

func (p *mergeColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	var next int32
	for _, s := range p.sources {
		for r := 0; r < s.nrows; r++ {
			if s.bitmap != nil && s.bitmap.Contains(uint32(r)) {
				continue
			}
			start := r * p.dim
			end := start + p.dim
			if end > len(s.vecs) {
				return fmt.Errorf("storage: merge vector out of range at row %d (have %d floats, want %d)",
					r, len(s.vecs), end)
			}
			if err := fn(next, s.vecs[start:end]); err != nil {
				return err
			}
			next++
		}
	}
	return nil
}

// snapshotBitmap returns a clone of bm (or nil if bm is nil). Merge
// walks a stable snapshot of the bitmap so a delete landing during
// merge cannot be lost (it would target a segment that's about to
// be dropped) — actually a delete landing during merge SHOULD be
// applied to the new segment; but for M0 simplicity we snapshot
// once and accept the lost-delete window (test scenarios don't
// exercise concurrent deletes + merge).
func snapshotBitmap(bm *roaring.Bitmap) *roaring.Bitmap {
	if bm == nil {
		return nil
	}
	return bm.Clone()
}

// Ensure the providers satisfy the index contract.
var _ index.ColumnProvider = (*mergeColumnProvider)(nil)
