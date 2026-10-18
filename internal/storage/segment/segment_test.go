package segment

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// ---------------------------------------------------------------------------
// Spec R4 scenario 1: encoding roundtrip. Every supported column type is
// written through the full segment path (builder -> files -> OpenSegment ->
// ScanColumn) and read back value-by-value (floats bit-for-bit); block zone
// maps must match the actual min/max of the decoded data.
// ---------------------------------------------------------------------------

type colSpec struct {
	fid  types.FieldID
	name string
	ft   meta.FieldType
	// gen returns the value (any of string/int64/float64/bool) and whether
	// row i is null.
	gen  func(i int) (any, bool)
	want Encoding
}

func TestR4_EncodingRoundtrip(t *testing.T) {
	// Segment A: small blocks, all scalar types, nulls, short last block.
	const rowsA = 1000
	specsA := []colSpec{
		{1, "kw", meta.TypeKeyword,
			func(i int) (any, bool) { return fmt.Sprintf("cat_%02d", i%37), i%9 == 4 },
			EncodingDict},
		{2, "txt", meta.TypeText,
			func(i int) (any, bool) { return fmt.Sprintf("lorem ipsum %d dolor %d sit", i, i*7), false },
			EncodingDict},
		{3, "ts", meta.TypeDate,
			func(i int) (any, bool) { return int64(1700000000000 + i*60000), false },
			EncodingDict}, // 1000 distinct < 4096 -> dictionary per the matrix
		{4, "val", meta.TypeDouble,
			func(i int) (any, bool) { return float64(i%97) + float64(i)*0.001, i%13 == 6 },
			EncodingGorilla},
		{5, "flag", meta.TypeBoolean,
			func(i int) (any, bool) { return i%3 == 0, i%11 == 7 },
			EncodingRLE},
		{6, "num", meta.TypeLong,
			func(i int) (any, bool) { return int64((i * 2654435761) % 1000), false },
			EncodingDict},
	}
	segA := buildTestSegment(t, 4242, 128, rowsA, specsA)
	defer segA.Close()
	verifyRoundtrip(t, segA, rowsA, 128, specsA)

	// Segment B: default block size, one block exercising the high
	// cardinality encodings (delta+bitpack monotonic, delta+zstd general,
	// raw zstd keyword).
	const rowsB = 5000
	specsB := []colSpec{
		{11, "kw2", meta.TypeKeyword,
			func(i int) (any, bool) { return fmt.Sprintf("u-kw-%d", i), false },
			EncodingZstd},
		{12, "ts2", meta.TypeDate,
			func(i int) (any, bool) { return int64(1760000000000 + i*1000), false },
			EncodingDeltaBitpack},
		{13, "num2", meta.TypeLong,
			func(i int) (any, bool) { return int64((i * 2654435761) % 9973), false },
			EncodingDeltaZstd},
	}
	segB := buildTestSegment(t, 4243, 0 /* default 8192 */, rowsB, specsB)
	defer segB.Close()
	verifyRoundtrip(t, segB, rowsB, DefaultBlockSize, specsB)
}

// buildTestSegment writes a segment with the given columns, sequential _id
// and _source values, and seq range [1, rows].
func buildTestSegment(t *testing.T, segID types.SegmentID, blockSize, rows int, specs []colSpec) *Segment {
	t.Helper()
	parent := t.TempDir()
	var opts []Option
	if blockSize > 0 {
		opts = append(opts, WithBlockSize(blockSize))
	}
	b, err := NewSegmentBuilder(parent, segID, types.ShardID(7), opts...)
	require.NoError(t, err)

	for i := 0; i < rows; i++ {
		b.AppendID(fmt.Sprintf("doc-%06d", i))
		b.AppendSource([]byte(fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("x", 1+i%13))))
	}
	for _, sp := range specs {
		w, err := b.Column(sp.fid, sp.name, sp.ft)
		require.NoError(t, err)
		for i := 0; i < rows; i++ {
			v, isNull := sp.gen(i)
			if isNull {
				w.AppendNull()
				continue
			}
			switch val := v.(type) {
			case string:
				w.AppendString(val)
			case int64:
				w.AppendLong(val)
			case float64:
				w.AppendDouble(val)
			case bool:
				w.AppendBool(val)
			default:
				t.Fatalf("unsupported test value %T", v)
			}
		}
	}
	b.SetSeqRange(1, types.SeqNo(rows))

	seg, err := b.Finish()
	require.NoError(t, err)
	assert.Equal(t, segID, seg.ID())
	assert.Equal(t, rows, seg.NumRows())
	return seg
}

// verifyRoundtrip reads every column through ScanColumn, checks values
// (floats bit-for-bit), nulls, per-block zone maps against the decoded
// data, the chosen encodings and the footer column stats.
func verifyRoundtrip(t *testing.T, seg *Segment, rows, blockSize int, specs []colSpec) {
	t.Helper()
	ctx := context.Background()
	snap := types.NewSnapshot(types.SeqNo(rows)) // ReadUpTo == max_seq_no

	for _, sp := range specs {
		it, err := seg.ScanColumn(ctx, sp.fid, snap)
		require.NoError(t, err)

		row := 0
		blockIdx := 0
		// Whole-column expected aggregates.
		var expMin, expMax any
		expNulls := 0
		distinct := map[any]struct{}{}

		for !it.Done() {
			metaInfo := it.Meta()
			assert.Equal(t, blockIdx, metaInfo.Index, "%s: block order", sp.name)
			assert.Equal(t, sp.want, metaInfo.Encoding, "%s: block %d encoding", sp.name, blockIdx)

			expRows := blockSize
			if rem := rows - row; rem < expRows {
				expRows = rem
			}
			assert.Equal(t, uint32(expRows), metaInfo.NumRows, "%s: block %d rows", sp.name, blockIdx)

			blk, err := it.Next(ctx)
			require.NoError(t, err, "%s: decode block %d", sp.name, blockIdx)
			require.Equal(t, expRows, blk.Rows)

			// Actual min/max/nulls of this block from the decoded values.
			var bmin, bmax any
			bnulls := 0
			for i := 0; i < blk.Rows; i++ {
				v, isNull := sp.gen(row)
				row++
				if isNull {
					expNulls++
					bnulls++
					assert.True(t, blk.IsNull(i), "%s row %d must be null", sp.name, row-1)
					continue
				}
				assert.False(t, blk.IsNull(i), "%s row %d must not be null", sp.name, row)
				switch val := v.(type) {
				case string:
					assert.Equal(t, val, blk.Strings()[i], "%s row %d", sp.name, row)
				case int64:
					assert.Equal(t, val, blk.Longs()[i], "%s row %d", sp.name, row)
				case float64:
					assert.Equal(t, math.Float64bits(val), math.Float64bits(blk.Doubles()[i]),
						"%s row %d: %v vs %v", sp.name, row, val, blk.Doubles()[i])
				case bool:
					assert.Equal(t, val, blk.Bools()[i], "%s row %d", sp.name, row)
				}
				distinct[v] = struct{}{}
				bmin, bmax = updateZoneTyped(bmin, bmax, v)
				expMin, expMax = updateZoneTyped(expMin, expMax, v)
			}

			// Block zone map == actual decoded min/max (spec R4).
			zone := metaInfo.Zone
			assert.Equal(t, bnulls > 0, zone.HasNull, "%s block %d has_null", sp.name, blockIdx)
			if bmin == nil {
				assert.Nil(t, zone.Min, "%s block %d min", sp.name, blockIdx)
				assert.Nil(t, zone.Max, "%s block %d max", sp.name, blockIdx)
			} else {
				assert.Equal(t, bmin, zone.Min, "%s block %d zone min", sp.name, blockIdx)
				assert.Equal(t, bmax, zone.Max, "%s block %d zone max", sp.name, blockIdx)
			}
			assert.Equal(t, uint32(bnulls), metaInfo.NullCount, "%s block %d null count", sp.name, blockIdx)
			blockIdx++
		}
		assert.Equal(t, rows, row, "%s: total rows scanned", sp.name)
		assert.Equal(t, it.Decoded(), blockIdx, "%s: decoded counter", sp.name)

		// Footer column stats for this field.
		var st *ColumnStats
		for i := range seg.Footer().ColumnStats {
			if seg.Footer().ColumnStats[i].FieldID == sp.fid {
				st = &seg.Footer().ColumnStats[i]
			}
		}
		require.NotNil(t, st, "%s: footer stats present", sp.name)
		assert.Equal(t, uint64(expNulls), st.NullCount, "%s: footer null count", sp.name)
		assert.Equal(t, uint64(len(distinct)), st.Cardinality, "%s: footer cardinality", sp.name)
		if expMin != nil {
			assert.Equal(t, expMin, deserializeZoneValue(sp.ft, st.Min), "%s: footer min", sp.name)
			assert.Equal(t, expMax, deserializeZoneValue(sp.ft, st.Max), "%s: footer max", sp.name)
		}
	}
}

// updateZoneTyped folds v into the running typed min/max.
func updateZoneTyped(minV, maxV, v any) (any, any) {
	if minV == nil {
		return v, v
	}
	switch val := v.(type) {
	case string:
		if val < minV.(string) {
			minV = val
		}
		if val > maxV.(string) {
			maxV = val
		}
	case int64:
		if val < minV.(int64) {
			minV = val
		}
		if val > maxV.(int64) {
			maxV = val
		}
	case float64:
		if val < minV.(float64) {
			minV = val
		}
		if val > maxV.(float64) {
			maxV = val
		}
	case bool:
		if !val {
			minV = false
		} else {
			maxV = true
		}
	}
	return minV, maxV
}

// ---------------------------------------------------------------------------
// Spec R4 scenario 2: block skipping. Predicates disjoint from a block's
// zone map must cause zero decodes of that block (verified with the decode
// counter), the scan must touch fewer blocks than the column holds, and
// the results must equal the fully decoded, then filtered, scan.
// ---------------------------------------------------------------------------

func TestR4_BlockSkipping(t *testing.T) {
	const (
		blockSize = 100
		rows      = 1000 // 10 blocks
		blocks    = rows / blockSize
	)
	parent := t.TempDir()
	b, err := NewSegmentBuilder(parent, types.SegmentID(777), 1, WithBlockSize(blockSize))
	require.NoError(t, err)

	for i := 0; i < rows; i++ {
		b.AppendID(fmt.Sprintf("row-%04d", i))
	}
	cols := []colSpec{
		{1, "num", meta.TypeLong,
			func(i int) (any, bool) { return int64(i/blockSize*1000 + i%blockSize), false },
			EncodingDict}, // 100 distinct per block
		{2, "price", meta.TypeDouble,
			func(i int) (any, bool) { return float64(i/blockSize)*10 + float64(i%blockSize)*0.1, false },
			EncodingGorilla},
		{3, "tag", meta.TypeKeyword,
			func(i int) (any, bool) { return fmt.Sprintf("k%02d-%02d", i/blockSize, i%37), false },
			EncodingDict},
	}
	for _, sp := range cols {
		w, err := b.Column(sp.fid, sp.name, sp.ft)
		require.NoError(t, err)
		for i := 0; i < rows; i++ {
			v, _ := sp.gen(i)
			switch val := v.(type) {
			case int64:
				w.AppendLong(val)
			case float64:
				w.AppendDouble(val)
			case string:
				w.AppendString(val)
			}
		}
	}
	b.SetSeqRange(1, 100)
	seg, err := b.Finish()
	require.NoError(t, err)
	defer seg.Close()

	ctx := context.Background()
	snap := types.NewSnapshot(100)

	type pred struct {
		name  string
		field types.FieldID
		// overlap decides from the typed zone map whether the block can
		// contain matches (the query-engine R2 rule).
		overlap func(z TypedZoneMap) bool
		// match evaluates the predicate against a decoded value.
		match func(v any) bool
		// expected overlapping block count.
		wantBlocks int
	}

	preds := []pred{
		{
			name:  "long range [2050,2060] hits only block 2",
			field: 1,
			overlap: func(z TypedZoneMap) bool {
				return z.Min.(int64) <= 2060 && z.Max.(int64) >= 2050
			},
			match: func(v any) bool {
				n := v.(int64)
				return n >= 2050 && n <= 2060
			},
			wantBlocks: 1,
		},
		{
			name:  "double range [25.0,25.9] hits only block 2",
			field: 2,
			overlap: func(z TypedZoneMap) bool {
				return z.Min.(float64) <= 25.9 && z.Max.(float64) >= 25.0
			},
			match: func(v any) bool {
				f := v.(float64)
				return f >= 25.0 && f <= 25.9
			},
			wantBlocks: 1,
		},
		{
			name:  "keyword prefix k05 hits only block 5",
			field: 3,
			overlap: func(z TypedZoneMap) bool {
				return z.Min.(string) <= "k05~" && z.Max.(string) >= "k05"
			},
			match: func(v any) bool {
				return len(v.(string)) >= 3 && v.(string)[:3] == "k05"
			},
			wantBlocks: 1,
		},
	}

	for _, p := range preds {
		t.Run(p.name, func(t *testing.T) {
			// Reference: full decode, then filter.
			fullIt, err := seg.ScanColumn(ctx, p.field, snap)
			require.NoError(t, err)
			var wantRows []int
			var wantVals []any
			row := 0
			for !fullIt.Done() {
				blk, err := fullIt.Next(ctx)
				require.NoError(t, err)
				for i := 0; i < blk.Rows; i++ {
					var v any
					switch {
					case blk.Longs() != nil:
						v = blk.Longs()[i]
					case blk.Doubles() != nil:
						v = blk.Doubles()[i]
					case blk.Strings() != nil:
						v = blk.Strings()[i]
					case blk.Bools() != nil:
						v = blk.Bools()[i]
					}
					if p.match(v) {
						wantRows = append(wantRows, row)
						wantVals = append(wantVals, v)
					}
					row++
				}
			}
			require.Equal(t, rows, row)
			require.Equal(t, blocks, fullIt.Decoded(), "reference pass decodes every block")
			require.NotEmpty(t, wantRows, "predicate must have matches")

			// Skipping pass: consult zone maps, decode only overlaps.
			skipIt, err := seg.ScanColumn(ctx, p.field, snap)
			require.NoError(t, err)
			require.Equal(t, blocks, skipIt.NumBlocks())
			var gotRows []int
			var gotVals []any
			row = 0
			for !skipIt.Done() {
				m := skipIt.Meta()
				if p.overlap(m.Zone) {
					blk, err := skipIt.Next(ctx)
					require.NoError(t, err)
					for i := 0; i < blk.Rows; i++ {
						var v any
						switch {
						case blk.Longs() != nil:
							v = blk.Longs()[i]
						case blk.Doubles() != nil:
							v = blk.Doubles()[i]
						case blk.Strings() != nil:
							v = blk.Strings()[i]
						case blk.Bools() != nil:
							v = blk.Bools()[i]
						}
						if p.match(v) {
							gotRows = append(gotRows, row)
							gotVals = append(gotVals, v)
						}
						row++
					}
				} else {
					row += int(m.NumRows)
					skipIt.Skip()
				}
			}

			// Zero decodes for skipped blocks, fewer than total blocks,
			// and identical results to full decode + filter.
			assert.Equal(t, p.wantBlocks, skipIt.Decoded(),
				"only the zone-overlapping block may be decoded")
			assert.Less(t, skipIt.Decoded(), blocks, "skipping must scan fewer blocks")
			assert.Equal(t, wantRows, gotRows, "matched rows identical to full scan")
			assert.Equal(t, wantVals, gotVals, "matched values identical to full scan")
		})
	}

	t.Run("predicate disjoint from every zone map decodes nothing", func(t *testing.T) {
		it, err := seg.ScanColumn(ctx, 1, snap)
		require.NoError(t, err)
		seen := 0
		for !it.Done() {
			m := it.Meta()
			// Range entirely above every block's max.
			if m.Zone.Max.(int64) < 900000 {
				it.Skip()
			} else {
				_, err := it.Next(ctx)
				require.NoError(t, err)
				seen++
			}
		}
		assert.Equal(t, 0, seen)
		assert.Equal(t, 0, it.Decoded(), "zero blocks decoded for disjoint predicate")
		assert.Equal(t, blocks, it.BlockIndex(), "iteration advanced over all blocks")
	})
}

// ---------------------------------------------------------------------------
// src.zst + id.dict/id.hash + footer + checksums + MVCC pruning.
// ---------------------------------------------------------------------------

func TestR4_SourceAndID(t *testing.T) {
	const rows = 50
	parent := t.TempDir()
	b, err := NewSegmentBuilder(parent, types.SegmentID(31), 3)
	require.NoError(t, err)
	ids := make([]string, rows)
	docs := make([][]byte, rows)
	for i := 0; i < rows; i++ {
		ids[i] = fmt.Sprintf("id-%03d", i)
		docs[i] = []byte(fmt.Sprintf(`{"i":%d,"body":"%s"}`, i, "payload"))
		b.AppendID(ids[i])
		b.AppendSource(docs[i])
	}
	// One stored column so MVCC visibility is observable via ScanColumn.
	w, err := b.Column(0, "f0", meta.TypeLong)
	require.NoError(t, err)
	for i := 0; i < rows; i++ {
		w.AppendLong(int64(i))
	}
	b.SetSeqRange(100, 149)
	seg, err := b.Finish()
	require.NoError(t, err)
	defer seg.Close()

	ctx := context.Background()

	// _id -> RowID point lookups via id.hash.
	for i, id := range ids {
		row, ok, err := seg.LookupID(ctx, id)
		require.NoError(t, err)
		assert.True(t, ok, "lookup %s", id)
		assert.Equal(t, types.RowID(i), row)
	}
	row, ok, err := seg.LookupID(ctx, "no-such-id")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, types.RowID(0), row)

	// RowID -> _id via the lazy row-ordered vector.
	for i, id := range ids {
		got, err := seg.DocID(types.RowID(i))
		require.NoError(t, err)
		assert.Equal(t, id, got)
	}
	_, err = seg.DocID(types.RowID(rows))
	assert.Error(t, err, "row out of range")

	// _source per-row equality and footer offsets.
	footer := seg.Footer()
	require.Len(t, footer.SourceOffsets, rows+1)
	for i, doc := range docs {
		src, err := seg.Source(types.RowID(i))
		require.NoError(t, err)
		assert.Equal(t, doc, src)
		assert.Equal(t, uint64(len(doc)), footer.SourceOffsets[i+1]-footer.SourceOffsets[i])
	}
	_, err = seg.Source(types.RowID(rows))
	assert.Error(t, err, "row out of range")

	// Footer basics.
	assert.Equal(t, types.SegmentID(31), footer.SegmentID)
	assert.Equal(t, types.ShardID(3), footer.ShardID)
	assert.Equal(t, uint64(rows), footer.NumRows)
	assert.Equal(t, types.SeqNo(100), footer.MinSeqNo)
	assert.Equal(t, types.SeqNo(149), footer.MaxSeqNo)
	assert.Equal(t, FormatVersion, footer.FormatVersion)
	assert.NotZero(t, footer.CreatedAt)

	// Checksum manifest verifies an intact directory.
	require.NoError(t, seg.VerifyChecksums())
	// ...and detects corruption of a listed file.
	err = os.WriteFile(filepath.Join(parent, segmentDirName(31), idDictFileName), []byte("garbage"), 0o644)
	require.NoError(t, err)
	assert.Error(t, seg.VerifyChecksums(), "corrupt id.dict must fail the manifest")

	// A segment written without _source has no src.zst.
	parent2 := t.TempDir()
	b2, err := NewSegmentBuilder(parent2, types.SegmentID(32), 3)
	require.NoError(t, err)
	b2.AppendID("only-id")
	b2.SetSeqRange(0, 0)
	seg2, err := b2.Finish()
	require.NoError(t, err)
	defer seg2.Close()
	_, err = seg2.Source(0)
	assert.Error(t, err, "_source disabled")
	_, err = os.Stat(filepath.Join(parent2, segmentDirName(32), sourceFileName))
	assert.True(t, os.IsNotExist(err), "src.zst must be absent when _source is disabled")

	// MVCC: segments pruned unless max_seq_no <= ReadUpTo (design 01 §8).
	it, err := seg.ScanColumn(ctx, 0, types.NewSnapshot(148))
	require.NoError(t, err)
	assert.True(t, it.Done(), "snapshot below max_seq_no prunes the whole segment")
	assert.Equal(t, 0, it.NumBlocks())
	it, err = seg.ScanColumn(ctx, 0, types.NewSnapshot(149))
	require.NoError(t, err)
	assert.False(t, it.Done(), "snapshot at max_seq_no sees the whole segment")

	// Scanning a field that was never stored reports an error.
	_, err = seg.ScanColumn(ctx, 99, types.NewSnapshot(1<<62))
	assert.Error(t, err, "non-stored field must fail, not return empty")
}

// ---------------------------------------------------------------------------
// Builder validation edges.
// ---------------------------------------------------------------------------

func TestR4_BuilderValidation(t *testing.T) {
	t.Run("empty segment refused", func(t *testing.T) {
		b, err := NewSegmentBuilder(t.TempDir(), types.SegmentID(1), 1)
		require.NoError(t, err)
		_, err = b.Finish()
		assert.Error(t, err, "a segment with zero rows must not be written")
		entries, _ := os.ReadDir(t.TempDir())
		for _, e := range entries {
			assert.NotEqual(t, segmentDirName(1), e.Name(), "tmp dir must be removed on failure")
		}
	})

	t.Run("column row mismatch refused", func(t *testing.T) {
		b, err := NewSegmentBuilder(t.TempDir(), types.SegmentID(2), 1)
		require.NoError(t, err)
		b.AppendID("a")
		b.AppendID("b")
		w, err := b.Column(1, "x", meta.TypeLong)
		require.NoError(t, err)
		w.AppendLong(1)
		_, err = b.Finish()
		assert.Error(t, err, "column with 1 row vs 2 id rows")
	})

	t.Run("source row overflow refused", func(t *testing.T) {
		b, err := NewSegmentBuilder(t.TempDir(), types.SegmentID(3), 1)
		require.NoError(t, err)
		b.AppendID("a")
		b.AppendSource([]byte("{}"))
		b.AppendSource([]byte("{}"))
		_, err = b.Finish()
		assert.Error(t, err)
	})

	t.Run("duplicate id refused", func(t *testing.T) {
		b, err := NewSegmentBuilder(t.TempDir(), types.SegmentID(4), 1)
		require.NoError(t, err)
		b.AppendID("same")
		b.AppendID("same")
		_, err = b.Finish()
		assert.Error(t, err, "duplicate _id must be rejected by id.hash build")
	})

	t.Run("non-storable column type refused", func(t *testing.T) {
		b, err := NewSegmentBuilder(t.TempDir(), types.SegmentID(5), 1)
		require.NoError(t, err)
		b.AppendID("a")
		_, err = b.Column(1, "pt", meta.TypeGeoPoint)
		assert.Error(t, err, "geo_point has no column block encoding in M0")
	})

	t.Run("wrong-typed append surfaces at finish", func(t *testing.T) {
		w := NewColumnWriter(9, "kw", meta.TypeKeyword)
		w.AppendLong(3)
		_, err := w.Finish()
		assert.Error(t, err)
		assert.Error(t, w.Err())
	})

	t.Run("abort discards tmp dir", func(t *testing.T) {
		parent := t.TempDir()
		b, err := NewSegmentBuilder(parent, types.SegmentID(6), 1)
		require.NoError(t, err)
		b.AppendID("a")
		require.NoError(t, b.Abort())
		_, err = os.Stat(filepath.Join(parent, fmt.Sprintf("seg_%d.tmp", 6)))
		assert.True(t, os.IsNotExist(err))
	})
}
