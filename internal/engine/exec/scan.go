// Package exec scan.go implements the §3.3 Scan operator: streams
// a stored column block by block, applies R2 zone-map block skip
// and the BitmapFilter predicate per row (R1/R4 pushdowns land
// here), and emits 4096-row ColumnBatches. MVCC visibility is
// enforced at the segment level by SegmentReader.ScanColumn (the
// segment is pruned when footer.MaxSeqNo > Snapshot.ReadUpTo).
package exec

import (
	"context"
	"fmt"
	"strconv"

	"github.com/RoaringBitmap/roaring"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// ZoneMapPredicate is the R2 block-skip predicate: given the typed
// zone map of the next block, Overlaps reports whether the block
// could contain rows matching the predicate. Returning false makes
// Scan Skip the block (zero decode). The predicate is the runtime
// form of a RangeQuery/TermQuery after R2 materialization.
type ZoneMapPredicate interface {
	Overlaps(zm segment.TypedZoneMap) bool
}

// ScanOp streams a single column from one segment into 4096-row
// ColumnBatches. It is the leaf of most pipelines; the optimizer
// (R1/R2) installs a ZoneMapPredicate for range scans and a bitmap
// filter for DeleteBitmap-aware scans (01 §6.2 vectorized
// roaring.Contains). MVCC visibility is delegated to the storage
// layer (SegmentReader.ScanColumn prunes the segment when
// footer.MaxSeqNo > Snapshot.ReadUpTo).
//
// Each Next call allocates a fresh batch via NewBatch so the caller
// may retain the returned batch without interference from the next
// pull — operators downstream must Release batches back to the pool
// when done.
type ScanOp struct {
	seg     segment.SegmentReader
	field   types.FieldID
	snap    types.Snapshot
	pred    ZoneMapPredicate
	bitfilt func(rowID uint32) bool // BitmapFilter predicate (DeleteBitmap hit = false)

	iter     segment.ColumnIterator
	rowsLeft int // rows in current decoded block left to drain
	cur      *segment.BlockData
	curSeg   uint64    // segment id for DocIDs
	col      ColumnVec // pre-loaded typed lane
}

// NewScan constructs a ScanOp. The SegmentReader is opened lazily on
// the first Next call. segID is the segment id used to construct
// GlobalDocIDs; the caller can derive it from seg.ID().
func NewScan(seg segment.SegmentReader, field types.FieldID, snap types.Snapshot, segID uint64) *ScanOp {
	return &ScanOp{
		seg:    seg,
		field:  field,
		snap:   snap,
		curSeg: segID,
	}
}

// SetZoneMapPredicate installs the R2 block-skip predicate. Must be
// called before the first Next; the optimizer wires it during plan
// finalization.
func (s *ScanOp) SetZoneMapPredicate(p ZoneMapPredicate) { s.pred = p }

// SetBitmapFilter installs the BitmapFilter predicate (01 §6.2):
// rows for which the predicate returns false are excluded from the
// emitted batch (the DeleteBitmap makes them invisible).
func (s *ScanOp) SetBitmapFilter(f func(rowID uint32) bool) { s.bitfilt = f }

// Next implements Operator. It blocks until at least one row is
// available, draining blocks as needed and skipping zone-map-
// non-overlapping blocks entirely. Returns io.EOF at end of column.
// A fresh ColumnBatch is allocated per call so callers may retain
// the returned batch safely; downstream operators must Release it.
func (s *ScanOp) Next(ctx context.Context) (*ColumnBatch, error) {
	if s.iter == nil {
		it, err := s.seg.ScanColumn(ctx, s.field, s.snap)
		if err != nil {
			return nil, fmt.Errorf("exec.Scan: open column: %w", err)
		}
		s.iter = it
		s.col.FieldType = s.iter.FieldType()
	}
	out := NewBatch(1)
	c := &out.Cols[0]
	c.FieldType = s.col.FieldType
	c.Name = strconv.Itoa(int(s.field))
	for out.Rows < BatchSize {
		if s.rowsLeft == 0 {
			if err := s.advanceBlock(ctx); err != nil {
				if out.Rows > 0 {
					return out, nil
				}
				Release(out)
				return nil, err
			}
		}
		if s.cur == nil {
			// Empty iterator (segment pruned by MVCC).
			if out.Rows > 0 {
				return out, nil
			}
			Release(out)
			return nil, ErrEOF
		}
		s.drainRows(out)
	}
	return out, nil
}

// advanceBlock advances to the next block, honoring the zone-map
// predicate. Returns io.EOF when the column is exhausted. When the
// segment is pruned by MVCC (emptyColumnIterator), Meta().NumRows is
// 0 and the function signals end-of-stream with no error.
func (s *ScanOp) advanceBlock(ctx context.Context) error {
	for {
		if s.iter.Done() {
			return ErrEOF
		}
		m := s.iter.Meta()
		// Empty block / MVCC-pruned segment: nothing to decode.
		if m.NumRows == 0 {
			if _, err := s.iter.Next(ctx); err != nil {
				return err
			}
			continue
		}
		if s.pred != nil && !s.pred.Overlaps(m.Zone) {
			s.iter.Skip()
			continue
		}
		bd, err := s.iter.Next(ctx)
		if err != nil {
			return err
		}
		s.cur = bd
		s.rowsLeft = bd.Rows
		return nil
	}
}

// drainRows copies as many rows as fit into the supplied batch,
// applying the BitmapFilter predicate. The current block's typed
// lane decides which slice we copy into.
func (s *ScanOp) drainRows(out *ColumnBatch) {
	if s.cur == nil || s.rowsLeft == 0 {
		return
	}
	startIdx := s.cur.Rows - s.rowsLeft
	c := &out.Cols[0]
	switch s.col.FieldType {
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		longs := s.cur.Longs()
		for i := startIdx; i < s.cur.Rows && out.Rows < BatchSize; i++ {
			if s.bitfilt != nil && !s.bitfilt(uint32(i)) {
				s.rowsLeft--
				continue
			}
			if !out.AppendRow(s.curSeg, uint32(i)) {
				break
			}
			c.Longs = append(c.Longs, longs[i])
			s.rowsLeft--
		}
	case meta.TypeDouble:
		doubles := s.cur.Doubles()
		for i := startIdx; i < s.cur.Rows && out.Rows < BatchSize; i++ {
			if s.bitfilt != nil && !s.bitfilt(uint32(i)) {
				s.rowsLeft--
				continue
			}
			if !out.AppendRow(s.curSeg, uint32(i)) {
				break
			}
			c.Doubles = append(c.Doubles, doubles[i])
			s.rowsLeft--
		}
	case meta.TypeKeyword, meta.TypeText:
		strs := s.cur.Strings()
		for i := startIdx; i < s.cur.Rows && out.Rows < BatchSize; i++ {
			if s.bitfilt != nil && !s.bitfilt(uint32(i)) {
				s.rowsLeft--
				continue
			}
			if !out.AppendRow(s.curSeg, uint32(i)) {
				break
			}
			c.Strings = append(c.Strings, strs[i])
			s.rowsLeft--
		}
	case meta.TypeBoolean:
		bools := s.cur.Bools()
		for i := startIdx; i < s.cur.Rows && out.Rows < BatchSize; i++ {
			if s.bitfilt != nil && !s.bitfilt(uint32(i)) {
				s.rowsLeft--
				continue
			}
			if !out.AppendRow(s.curSeg, uint32(i)) {
				break
			}
			c.Bools = append(c.Bools, bools[i])
			s.rowsLeft--
		}
	}
}

// Close implements Operator. The segment handle is owned by the
// caller (the storage engine's segment set), so Scan only drops its
// iterator reference; per-call batches are owned by the caller and
// must be Released downstream.
func (s *ScanOp) Close() {
	s.iter = nil
	s.cur = nil
	s.rowsLeft = 0
}

// SetSelFromBitmap materializes the batch's Sel roaring bitmap from
// the bitmap filter's output (R4 pre-filter / 01 §6.2 vectorized
// roaring.Contains path). Helper used by ScanOp when the filter
// returns a bitmap rather than a per-row predicate.
func SetSelFromBitmap(b *ColumnBatch, rows int, bm *roaring.Bitmap) {
	if b == nil || bm == nil {
		return
	}
	if b.Sel == nil {
		b.Sel = roaring.New()
	}
	b.Sel.Clear()
	b.Sel.And(bm)
}
