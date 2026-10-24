package storage

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/index/inverted"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// merge_indexes.go builds inverted and scalar indexes during segment
// merge, reading surviving rows from source segments. This closes the
// gap where MergeSegments only rebuilt HNSW (vector) indexes but left
// inv.idx/ and scalar.idx/ empty, making text search and scalar filtering
// fail on merged segments.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.2

// ---------------------------------------------------------------------------
// Merge column providers (read from source segments, skip deleted rows)
// ---------------------------------------------------------------------------

// mergeTextSource is one source segment's text column for inverted index.
type mergeTextSource struct {
	sr     *segment.Segment
	fid    types.FieldID
	bitmap *roaring.Bitmap
}

// mergeTextColumnProvider implements the text column interface for
// inverted index building over source segments' surviving rows.
type mergeTextColumnProvider struct {
	field   string
	sources []mergeTextSource
}

func (p *mergeTextColumnProvider) Field() string { return p.field }
func (p *mergeTextColumnProvider) Dim() int      { return 0 }
func (p *mergeTextColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	return nil
}

func (p *mergeTextColumnProvider) Rows() int64 {
	var n int64
	for _, src := range p.sources {
		n += int64(src.sr.NumRows())
		if src.bitmap != nil {
			n -= int64(src.bitmap.GetCardinality())
		}
	}
	return n
}

// ForEachText yields (rowID, text) pairs in surviving-row order.
// rowID is the new segment's sequential row ID (0-based).
func (p *mergeTextColumnProvider) ForEachText(fn func(rowID int32, text []byte) error) error {
	var newRowID int32
	for _, src := range p.sources {
		cr, err := src.sr.Column(src.fid)
		if err != nil {
			// Column not in this source: all rows are null for this
			// field, but still count them for row ID alignment.
			for r := 0; r < src.sr.NumRows(); r++ {
				if src.bitmap != nil && src.bitmap.Contains(uint32(r)) {
					continue
				}
				newRowID++
			}
			continue
		}
		it := cr.NewIterator()
		rowOffset := 0
		for !it.Done() {
			bd, err := it.Next(context.Background())
			if err != nil {
				return err
			}
			strs := bd.Strings()
			for i := 0; i < bd.Rows; i++ {
				globalRow := rowOffset + i
				if src.bitmap != nil && src.bitmap.Contains(uint32(globalRow)) {
					continue
				}
				if !bd.IsNull(i) && i < len(strs) && strs[i] != "" {
					if err := fn(newRowID, []byte(strs[i])); err != nil {
						return err
					}
				}
				newRowID++
			}
			rowOffset += bd.Rows
		}
	}
	return nil
}

// mergeLongSource is one source segment's long column for BKD index.
type mergeLongSource struct {
	sr     *segment.Segment
	fid    types.FieldID
	bitmap *roaring.Bitmap
}

// mergeLongColumnProvider implements the long column interface for
// BKD index building over source segments' surviving rows.
type mergeLongColumnProvider struct {
	field   string
	sources []mergeLongSource
}

func (p *mergeLongColumnProvider) Field() string { return p.field }
func (p *mergeLongColumnProvider) Dim() int      { return 0 }
func (p *mergeLongColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	return nil
}

func (p *mergeLongColumnProvider) Rows() int64 {
	var n int64
	for _, src := range p.sources {
		n += int64(src.sr.NumRows())
		if src.bitmap != nil {
			n -= int64(src.bitmap.GetCardinality())
		}
	}
	return n
}

func (p *mergeLongColumnProvider) ForEachLong(fn func(rowID int32, v int64) error) error {
	var newRowID int32
	for _, src := range p.sources {
		cr, err := src.sr.Column(src.fid)
		if err != nil {
			for r := 0; r < src.sr.NumRows(); r++ {
				if src.bitmap != nil && src.bitmap.Contains(uint32(r)) {
					continue
				}
				newRowID++
			}
			continue
		}
		it := cr.NewIterator()
		rowOffset := 0
		for !it.Done() {
			bd, err := it.Next(context.Background())
			if err != nil {
				return err
			}
			longs := bd.Longs()
			for i := 0; i < bd.Rows; i++ {
				globalRow := rowOffset + i
				if src.bitmap != nil && src.bitmap.Contains(uint32(globalRow)) {
					continue
				}
				if !bd.IsNull(i) && i < len(longs) {
					if err := fn(newRowID, longs[i]); err != nil {
						return err
					}
				}
				newRowID++
			}
			rowOffset += bd.Rows
		}
	}
	return nil
}

// mergeBytesSource is one source segment's keyword column for Bitmap index.
type mergeBytesSource struct {
	sr     *segment.Segment
	fid    types.FieldID
	bitmap *roaring.Bitmap
}

// mergeBytesColumnProvider implements the bytes column interface for
// Bitmap index building over source segments' surviving rows.
type mergeBytesColumnProvider struct {
	field   string
	sources []mergeBytesSource
}

func (p *mergeBytesColumnProvider) Field() string { return p.field }
func (p *mergeBytesColumnProvider) Dim() int      { return 0 }
func (p *mergeBytesColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	return nil
}

func (p *mergeBytesColumnProvider) Rows() int64 {
	var n int64
	for _, src := range p.sources {
		n += int64(src.sr.NumRows())
		if src.bitmap != nil {
			n -= int64(src.bitmap.GetCardinality())
		}
	}
	return n
}

func (p *mergeBytesColumnProvider) ForEachValue(fn func(rowID int32, v []byte) error) error {
	var newRowID int32
	for _, src := range p.sources {
		cr, err := src.sr.Column(src.fid)
		if err != nil {
			for r := 0; r < src.sr.NumRows(); r++ {
				if src.bitmap != nil && src.bitmap.Contains(uint32(r)) {
					continue
				}
				newRowID++
			}
			continue
		}
		it := cr.NewIterator()
		rowOffset := 0
		for !it.Done() {
			bd, err := it.Next(context.Background())
			if err != nil {
				return err
			}
			strs := bd.Strings()
			for i := 0; i < bd.Rows; i++ {
				globalRow := rowOffset + i
				if src.bitmap != nil && src.bitmap.Contains(uint32(globalRow)) {
					continue
				}
				if !bd.IsNull(i) && i < len(strs) && strs[i] != "" {
					if err := fn(newRowID, []byte(strs[i])); err != nil {
						return err
					}
				}
				newRowID++
			}
			rowOffset += bd.Rows
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Build functions (write index files into builder's tmp directory)
// ---------------------------------------------------------------------------

// buildMergeInvertedIndexes builds inverted indexes for text fields
// from surviving rows across all source segments. @spec: 02_spec.md#2.6
//
// F1: Tries to merge existing posting lists via MergeWithOptions first,
// falling back to full rebuild (ix.Build) if the merge path fails or
// source indexes are not loaded.
func (s *Shard) buildMergeInvertedIndexes(
	ctx context.Context,
	sources []*SegmentEntry,
	bitmaps []*roaring.Bitmap,
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) error {
	tmpBase := filepath.Join(s.segDir, fmt.Sprintf("seg_%d.tmp", segID), "inv.idx")
	for fid, fd := range fields {
		if fd.Type != meta.TypeText {
			continue
		}
		dir := filepath.Join(tmpBase, fmt.Sprintf("%d", fid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}

		// Try the merge path: if all sources have loaded inverted indexes,
		// merge posting lists directly instead of re-tokenizing text.
		if s.tryMergeInvertedIndex(ctx, sources, bitmaps, fid, fd.Name, dir) {
			continue // merge succeeded
		}

		// Fallback: full rebuild via re-tokenization.
		slog.Warn("inverted index merge unavailable, falling back to build",
			"seg_id", segID, "field_id", fid, "field", fd.Name)
		var textSources []mergeTextSource
		for i, src := range sources {
			textSources = append(textSources, mergeTextSource{
				sr:     src.Reader,
				fid:    fid,
				bitmap: bitmaps[i],
			})
		}
		if len(textSources) == 0 {
			continue
		}
		provider := &mergeTextColumnProvider{field: fd.Name, sources: textSources}
		ix, err := index.NewIndex(index.KindInverted)
		if err != nil {
			if err == index.ErrUnknownKind {
				continue
			}
			return err
		}
		if err := ix.Build(ctx, provider, &segIndexOutput{dir: dir}); err != nil {
			return fmt.Errorf("storage: build merge inverted index seg %d field %d: %w", segID, fid, err)
		}
	}
	return nil
}

// tryMergeInvertedIndex attempts to merge posting lists from loaded source
// indexes. Returns true if the merge succeeded, false if it should fall back
// to full rebuild. @spec: 02_spec.md#2.5
func (s *Shard) tryMergeInvertedIndex(
	ctx context.Context,
	sources []*SegmentEntry,
	bitmaps []*roaring.Bitmap,
	fid types.FieldID,
	fieldName string,
	dir string,
) bool {
	// Check that all sources have the inverted index loaded.
	var loaded []index.LoadedIndex
	var numRows []uint32
	var deletes []*roaring.Bitmap
	for i, src := range sources {
		li, ok := src.Indexes[fid]
		if !ok || li == nil {
			return false // source index not loaded, need fallback
		}
		loaded = append(loaded, li)
		numRows = append(numRows, uint32(src.Reader.NumRows()))
		deletes = append(deletes, bitmaps[i])
	}
	if len(loaded) == 0 {
		return false
	}

	if err := inverted.MergeLoaded(ctx, loaded, deletes, numRows,
		&segIndexOutput{dir: dir}, fieldName, true); err != nil {
		slog.Warn("inverted index merge failed, falling back to build",
			"field_id", fid, "field", fieldName, "error", err)
		return false
	}
	return true
}

// buildMergeScalarIndexes builds BKD/Bitmap indexes for scalar fields
// from surviving rows across all source segments.
func (s *Shard) buildMergeScalarIndexes(
	ctx context.Context,
	sources []*SegmentEntry,
	bitmaps []*roaring.Bitmap,
	segID types.SegmentID,
	fields map[types.FieldID]*meta.FieldDef,
) error {
	tmpBase := filepath.Join(s.segDir, fmt.Sprintf("seg_%d.tmp", segID), "scalar.idx")
	for fid, fd := range fields {
		var kind index.IndexKind
		switch fd.Type {
		case meta.TypeLong, meta.TypeDate, meta.TypeDouble, meta.TypeFloat, meta.TypeIP, meta.TypeGeoPoint:
			kind = index.KindBKD
		case meta.TypeKeyword:
			kind = index.KindBitmap
		default:
			continue
		}

		dir := filepath.Join(tmpBase, fmt.Sprintf("%d", fid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}

		var provider index.ColumnProvider
		switch fd.Type {
		case meta.TypeLong, meta.TypeDate, meta.TypeIP:
			var longSources []mergeLongSource
			for i, src := range sources {
				longSources = append(longSources, mergeLongSource{
					sr:     src.Reader,
					fid:    fid,
					bitmap: bitmaps[i],
				})
			}
			provider = &mergeLongColumnProvider{field: fd.Name, sources: longSources}
		case meta.TypeKeyword:
			var bytesSources []mergeBytesSource
			for i, src := range sources {
				bytesSources = append(bytesSources, mergeBytesSource{
					sr:     src.Reader,
					fid:    fid,
					bitmap: bitmaps[i],
				})
			}
			provider = &mergeBytesColumnProvider{field: fd.Name, sources: bytesSources}
		default:
			continue
		}

		ix, err := index.NewIndex(kind)
		if err != nil {
			if err == index.ErrUnknownKind {
				continue
			}
			return err
		}
		if err := ix.Build(ctx, provider, &segIndexOutput{dir: dir}); err != nil {
			return fmt.Errorf("storage: build merge scalar index seg %d field %d: %w", segID, fid, err)
		}
	}
	return nil
}
