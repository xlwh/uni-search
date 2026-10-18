package segment

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// xorshift64 deterministic PRNG for reproducible pseudo-random test data.
type testRng struct{ s uint64 }

func (r *testRng) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}

// --- helpers -------------------------------------------------------------

// mkLongBlock builds a BlockData long vector from (value, null) pairs; a
// nil nulls slice means no nulls.
func mkLongBlock(vals []int64, nulls []bool) *BlockData {
	d := &BlockData{longs: make([]int64, 0, len(vals))}
	for i, v := range vals {
		if len(nulls) > 0 && nulls[i] {
			d.appendNull()
			continue
		}
		d.longs = append(d.longs, v)
		d.Rows++
	}
	return d
}

func mkDoubleBlock(vals []float64, nulls []bool) *BlockData {
	d := &BlockData{doubles: make([]float64, 0, len(vals))}
	for i, v := range vals {
		if len(nulls) > 0 && nulls[i] {
			d.appendNull()
			continue
		}
		d.doubles = append(d.doubles, v)
		d.Rows++
	}
	return d
}

func mkStringBlock(vals []string, nulls []bool) *BlockData {
	d := &BlockData{strs: make([]string, 0, len(vals))}
	for i, v := range vals {
		if len(nulls) > 0 && nulls[i] {
			d.appendNull()
			continue
		}
		d.strs = append(d.strs, v)
		d.Rows++
	}
	return d
}

func mkBoolBlock(vals []bool, nulls []bool) *BlockData {
	d := &BlockData{bools: make([]bool, 0, len(vals))}
	for i, v := range vals {
		if len(nulls) > 0 && nulls[i] {
			d.appendNull()
			continue
		}
		d.bools = append(d.bools, v)
		d.Rows++
	}
	return d
}

// roundtrip runs one encoder through encodeBlock/decodeBlock and returns
// the decoded block.
func roundtrip(t *testing.T, ft meta.FieldType, enc Encoding, d *BlockData) *BlockData {
	t.Helper()
	payload, err := encodeBlock(ft, enc, d)
	require.NoError(t, err)
	got, err := decodeBlock(ft, enc, payload, d.Rows)
	require.NoError(t, err)
	require.Equal(t, d.Rows, got.Rows)
	require.Equal(t, d.nullCount, got.nullCount)
	return got
}

func nullsOf(n int, at func(i int) bool) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = at(i)
	}
	return out
}

// --- bitpack / bitstream primitives ---------------------------------------

func TestR4_BitpackRoundtripAllWidths(t *testing.T) {
	rng := &testRng{s: 0xC0FFEE}
	for w := 0; w <= 64; w++ {
		const n = 100
		vals := make([]uint64, n)
		for i := range vals {
			vals[i] = rng.next() & bitMask(w)
		}
		enc := bitpackEncode(vals, w)
		dec := bitpackDecode(enc, n, w)
		assert.Equal(t, vals, dec, "width %d", w)
	}
}

func TestR4_BitpackWidthBoundaries(t *testing.T) {
	// Width 0 packs no bits at all.
	assert.Empty(t, bitpackEncode([]uint64{1, 2, 3}, 0))
	assert.Equal(t, []uint64{0, 0}, bitpackDecode(nil, 2, 0))

	// Width 64 keeps full values including the sign bit.
	vals := []uint64{^uint64(0), 0x8000000000000000, 0}
	dec := bitpackDecode(bitpackEncode(vals, 64), 3, 64)
	assert.Equal(t, vals, dec)

	// Zigzag edges: 0 -> 0, 1 -> 2, -1 -> 1, MinInt64 -> MaxUint64.
	assert.Equal(t, uint64(0), zigzag(0))
	assert.Equal(t, uint64(2), zigzag(1))
	assert.Equal(t, uint64(1), zigzag(-1))
	assert.Equal(t, uint64(^uint64(0)), zigzag(math.MinInt64))
	assert.Equal(t, int64(-1), unzigzag(zigzag(-1)))
	assert.Equal(t, int64(math.MinInt64), unzigzag(zigzag(math.MinInt64)))
	assert.Equal(t, int64(math.MaxInt64), unzigzag(zigzag(math.MaxInt64)))
}

func TestR4_MSBBitstreamRoundtrip(t *testing.T) {
	rng := &testRng{s: 42}
	var w msbBitWriter
	type pair struct {
		nbits int
		v     uint64
	}
	var seq []pair
	for i := 0; i < 1000; i++ {
		nbits := int(rng.next()%64) + 1
		v := rng.next() & bitMask(nbits)
		w.write(v, nbits)
		seq = append(seq, pair{nbits, v})
	}
	r := &msbBitReader{buf: w.bytes()}
	for i, p := range seq {
		assert.Equal(t, p.v, r.read(p.nbits), "index %d", i)
	}
	// Past the stream end reads deliver zero bits (gorilla pads with 0).
	assert.Equal(t, uint64(0), r.read(8))
}

// --- dictionary encoder ----------------------------------------------------

func TestR4_EncoderDictString(t *testing.T) {
	vals := []string{}
	nulls := []bool{}
	rng := &testRng{s: 7}
	for i := 0; i < 300; i++ {
		vals = append(vals, fmt.Sprintf("term-%d", rng.next()%37))
		nulls = append(nulls, i%13 == 5)
	}
	d := mkStringBlock(vals, nulls)
	got := roundtrip(t, meta.TypeKeyword, EncodingDict, d)
	for i := range vals {
		assert.Equal(t, nulls[i], got.IsNull(i), "row %d", i)
		if !nulls[i] {
			assert.Equal(t, vals[i], got.Strings()[i], "row %d", i)
		}
	}

	// All-null block and single-distinct block.
	allNull := mkStringBlock([]string{"", "", ""}, []bool{true, true, true})
	got = roundtrip(t, meta.TypeKeyword, EncodingDict, allNull)
	assert.Equal(t, 3, got.NullCount())

	single := mkStringBlock([]string{"x", "x", "x"}, nil)
	got = roundtrip(t, meta.TypeKeyword, EncodingDict, single)
	assert.Equal(t, []string{"x", "x", "x"}, got.Strings())
}

func TestR4_EncoderDictLong(t *testing.T) {
	vals := []int64{}
	nulls := []bool{}
	for i := 0; i < 200; i++ {
		vals = append(vals, int64(i%51)-25)
		nulls = append(nulls, i%9 == 2)
	}
	d := mkLongBlock(vals, nulls)
	got := roundtrip(t, meta.TypeLong, EncodingDict, d)
	for i := range vals {
		assert.Equal(t, nulls[i], got.IsNull(i), "row %d", i)
		if !nulls[i] {
			assert.Equal(t, vals[i], got.Longs()[i], "row %d", i)
		}
	}

	// Date columns share the long vector path.
	dd := mkLongBlock([]int64{1700000000000, 1700000000001}, nil)
	got = roundtrip(t, meta.TypeDate, EncodingDict, dd)
	assert.Equal(t, []int64{1700000000000, 1700000000001}, got.Longs())
}

// --- delta + bitpack / delta + zstd ---------------------------------------

func TestR4_EncoderDeltaBitpack(t *testing.T) {
	// Monotonic timestamps with nulls scattered in.
	vals := []int64{}
	nulls := []bool{}
	for i := 0; i < 500; i++ {
		vals = append(vals, 1700000000000+int64(i)*60000)
		nulls = append(nulls, i%17 == 3)
	}
	got := roundtrip(t, meta.TypeDate, EncodingDeltaBitpack, mkLongBlock(vals, nulls))
	for i := range vals {
		assert.Equal(t, nulls[i], got.IsNull(i), "row %d", i)
		if !nulls[i] {
			assert.Equal(t, vals[i], got.Longs()[i], "row %d", i)
		}
	}

	// Random deltas, null-first row, single row, all-null.
	rng := &testRng{s: 99}
	vals = nil
	nulls = nil
	for i := 0; i < 256; i++ {
		vals = append(vals, int64(rng.next()%1_000_000)-500_000)
		nulls = append(nulls, i%7 == 1)
	}
	got = roundtrip(t, meta.TypeLong, EncodingDeltaBitpack, mkLongBlock(vals, nulls))
	for i := range vals {
		if !nulls[i] {
			assert.Equal(t, vals[i], got.Longs()[i], "row %d", i)
		}
	}

	got = roundtrip(t, meta.TypeLong, EncodingDeltaBitpack, mkLongBlock([]int64{42}, []bool{false}))
	assert.Equal(t, []int64{42}, got.Longs())

	got = roundtrip(t, meta.TypeLong, EncodingDeltaBitpack, mkLongBlock([]int64{7, 8}, []bool{true, false}))
	assert.Equal(t, []int64{0, 8}, got.Longs())
	assert.True(t, got.IsNull(0))

	got = roundtrip(t, meta.TypeLong, EncodingDeltaBitpack, mkLongBlock([]int64{1, 1, 1}, []bool{true, true, true}))
	assert.Equal(t, 3, got.NullCount())
}

func TestR4_EncoderDeltaZstd(t *testing.T) {
	rng := &testRng{s: 1234}
	vals := []int64{}
	nulls := []bool{}
	for i := 0; i < 300; i++ {
		vals = append(vals, int64(rng.next()%(1<<40))-(1<<39))
		nulls = append(nulls, i%11 == 4)
	}
	got := roundtrip(t, meta.TypeLong, EncodingDeltaZstd, mkLongBlock(vals, nulls))
	for i := range vals {
		assert.Equal(t, nulls[i], got.IsNull(i), "row %d", i)
		if !nulls[i] {
			assert.Equal(t, vals[i], got.Longs()[i], "row %d", i)
		}
	}
}

// --- gorilla ----------------------------------------------------------------

func TestR4_EncoderGorilla(t *testing.T) {
	cases := []struct {
		name  string
		vals  []float64
		nulls []bool
	}{
		{
			name: "smooth-sensor",
			vals: func() []float64 {
				out := []float64{}
				for i := 0; i < 400; i++ {
					out = append(out, 20.0+float64(i)*0.01+float64(i%7)*0.001)
				}
				return out
			}(),
			nulls: nullsOf(400, func(i int) bool { return i%19 == 8 }),
		},
		{
			name:  "wild-jumps",
			vals:  []float64{1e-300, -4.25e9, 0, math.MaxFloat64, -0.5, 1e-300, 1e-300, 3.14159},
			nulls: nullsOf(8, func(i int) bool { return i == 3 }),
		},
		{
			name:  "constants-and-duplicates",
			vals:  []float64{7.5, 7.5, 7.5, -1.25, 7.5, -1.25, -1.25},
			nulls: nil,
		},
		{
			name:  "nulls-first-and-last",
			vals:  []float64{0, 9.75, 0, 0},
			nulls: []bool{true, false, true, true},
		},
		{
			name:  "single",
			vals:  []float64{-2.5},
			nulls: nil,
		},
		{
			name:  "all-null",
			vals:  []float64{0, 0, 0},
			nulls: []bool{true, true, true},
		},
		{
			name: "random-floats",
			vals: func() []float64 {
				rng := &testRng{s: 0xBEEF}
				out := []float64{}
				for i := 0; i < 512; i++ {
					out = append(out, math.Float64frombits(rng.next()))
				}
				return out
			}(),
			nulls: nullsOf(512, func(i int) bool { return i%13 == 6 }),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mkDoubleBlock(tc.vals, tc.nulls)
			got := roundtrip(t, meta.TypeDouble, EncodingGorilla, d)
			for i := range tc.vals {
				isNull := len(tc.nulls) > 0 && tc.nulls[i]
				assert.Equal(t, isNull, got.IsNull(i), "row %d", i)
				if !isNull {
					// Spec R4: floats compare bit-for-bit.
					assert.Equal(t,
						math.Float64bits(tc.vals[i]),
						math.Float64bits(got.Doubles()[i]),
						"row %d: %v vs %v", i, tc.vals[i], got.Doubles()[i])
				}
			}
		})
	}

	// Negative zero must survive (distinct bit pattern).
	got := roundtrip(t, meta.TypeDouble, EncodingGorilla, mkDoubleBlock([]float64{0.0, math.Copysign(0, -1)}, nil))
	assert.Equal(t, math.Float64bits(0.0), math.Float64bits(got.Doubles()[0]))
	assert.Equal(t, math.Float64bits(math.Copysign(0, -1)), math.Float64bits(got.Doubles()[1]))
}

// --- RLE ---------------------------------------------------------------------

func TestR4_EncoderRLE(t *testing.T) {
	vals := []bool{}
	for i := 0; i < 500; i++ {
		vals = append(vals, (i/64)%2 == 0) // long alternating runs
	}
	nulls := nullsOf(500, func(i int) bool { return i%31 == 7 })
	got := roundtrip(t, meta.TypeBoolean, EncodingRLE, mkBoolBlock(vals, nulls))
	for i := range vals {
		assert.Equal(t, nulls[i], got.IsNull(i), "row %d", i)
		if !nulls[i] {
			assert.Equal(t, vals[i], got.Bools()[i], "row %d", i)
		}
	}

	got = roundtrip(t, meta.TypeBoolean, EncodingRLE, mkBoolBlock([]bool{true, false, true}, nil))
	assert.Equal(t, []bool{true, false, true}, got.Bools())
}

// --- raw zstd fallback ---------------------------------------------------------

func TestR4_EncoderZstdRaw(t *testing.T) {
	rng := &testRng{s: 555}
	vals := []string{}
	nulls := []bool{}
	for i := 0; i < 600; i++ {
		vals = append(vals, fmt.Sprintf("distinct-keyword-value-%d", rng.next()))
		nulls = append(nulls, i%21 == 9)
	}
	got := roundtrip(t, meta.TypeKeyword, EncodingZstd, mkStringBlock(vals, nulls))
	for i := range vals {
		assert.Equal(t, nulls[i], got.IsNull(i), "row %d", i)
		if !nulls[i] {
			assert.Equal(t, vals[i], got.Strings()[i], "row %d", i)
		}
	}
}

// --- selection matrix -----------------------------------------------------------

func TestR4_EncodingSelectionMatrix(t *testing.T) {
	// Direct matrix assertions (design 01 §4.2).
	assert.Equal(t, EncodingDict, chooseEncoding(meta.TypeKeyword, blockStats{cardinality: 4095}))
	assert.Equal(t, EncodingZstd, chooseEncoding(meta.TypeKeyword, blockStats{cardinality: 4096}))
	assert.Equal(t, EncodingDict, chooseEncoding(meta.TypeText, blockStats{cardinality: 10}))

	assert.Equal(t, EncodingDict, chooseEncoding(meta.TypeLong, blockStats{cardinality: 100}))
	assert.Equal(t, EncodingDict, chooseEncoding(meta.TypeDate, blockStats{cardinality: 4095}))
	assert.Equal(t, EncodingDeltaBitpack, chooseEncoding(meta.TypeLong, blockStats{cardinality: 5000, monotonic: true}))
	assert.Equal(t, EncodingDeltaZstd, chooseEncoding(meta.TypeLong, blockStats{cardinality: 5000}))
	assert.Equal(t, EncodingDeltaBitpack, chooseEncoding(meta.TypeDate, blockStats{cardinality: 8192, monotonic: true}))
	assert.Equal(t, EncodingDeltaZstd, chooseEncoding(meta.TypeIP, blockStats{cardinality: 8192}))

	assert.Equal(t, EncodingGorilla, chooseEncoding(meta.TypeDouble, blockStats{cardinality: 8192}))
	assert.Equal(t, EncodingGorilla, chooseEncoding(meta.TypeDouble, blockStats{cardinality: 3}))
	assert.Equal(t, EncodingRLE, chooseEncoding(meta.TypeBoolean, blockStats{cardinality: 2}))

	// End-to-end: the writer picks the same encodings block by block.
	type exp struct {
		ft   meta.FieldType
		n    int
		want Encoding
		gen  func(w *ColumnWriter, i int)
	}
	cases := []exp{
		{
			ft: meta.TypeKeyword, n: 5000, want: EncodingZstd,
			gen: func(w *ColumnWriter, i int) { w.AppendString(fmt.Sprintf("unique-kw-%d", i)) },
		},
		{
			ft: meta.TypeDate, n: 5000, want: EncodingDeltaBitpack,
			gen: func(w *ColumnWriter, i int) { w.AppendLong(1700000000000 + int64(i)*1000) },
		},
		{
			ft: meta.TypeLong, n: 5000, want: EncodingDeltaZstd,
			gen: func(w *ColumnWriter, i int) {
				w.AppendLong(int64((i * 2654435761) % 1000000)) // distinct, non-monotonic
			},
		},
		{
			ft: meta.TypeLong, n: 5000, want: EncodingDict,
			gen: func(w *ColumnWriter, i int) { w.AppendLong(int64(i % 64)) },
		},
		{
			ft: meta.TypeDouble, n: 5000, want: EncodingGorilla,
			gen: func(w *ColumnWriter, i int) { w.AppendDouble(float64(i) * 1.0000001) },
		},
		{
			ft: meta.TypeBoolean, n: 5000, want: EncodingRLE,
			gen: func(w *ColumnWriter, i int) { w.AppendBool(i%128 < 64) },
		},
	}
	for i, tc := range cases {
		w := NewColumnWriter(types.FieldID(i), fmt.Sprintf("f%d", i), tc.ft)
		for r := 0; r < tc.n; r++ {
			tc.gen(w, r)
		}
		cf, err := w.Finish()
		require.NoError(t, err)
		require.NotEmpty(t, cf.Header.Blocks)
		for _, bi := range cf.Header.Blocks {
			assert.Equal(t, tc.want, bi.Encoding, "%s block encoding", tc.ft)
		}
		// And the file reads back with the right shape.
		r, err := ReadColumnFile(cf.Data)
		require.NoError(t, err)
		it := r.NewIterator()
		require.False(t, it.Done())
		assert.Equal(t, tc.want, it.Meta().Encoding)
	}
}
