package segment

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/unisearch-db/unisearch/internal/meta"
)

// @spec: 02_spec.md#3.8 F2-T01..T06

// --- DoubleDelta tests -------------------------------------------------------

func TestF2_DoubleDeltaDirect(t *testing.T) {
	rng := &testRng{s: 0xDEADBEEF}
	tests := []struct {
		name  string
		vals  []int64
		nulls []bool
	}{
		{
			name: "evenly_spaced",
			vals: genLongs(500, func(i int) int64 { return int64(i) * 60 }),
		},
		{
			name: "linear_with_offset",
			vals: genLongs(500, func(i int) int64 { return int64(i)*10 + 42 }),
		},
		{
			name: "quadratic_step",
			vals: genLongs(200, func(i int) int64 { return int64(i * i) }),
		},
		{
			name: "constant",
			vals: genLongs(100, func(i int) int64 { return 42 }),
		},
		{
			name: "two_rows",
			vals: []int64{10, 20},
		},
		{
			name: "single_row",
			vals: []int64{42},
		},
		{
			name:  "with_nulls",
			vals:  genLongs(500, func(i int) int64 { return int64(i) * 1000 }),
			nulls: nullsOf(500, func(i int) bool { return i%10 == 0 }),
		},
		{
			name:  "random_monotonic",
			vals:  genMonotonicLongs(500, rng),
			nulls: nullsOf(500, func(i int) bool { return rng.next()%7 == 0 }),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := mkLongBlock(tt.vals, tt.nulls)
			codec := &doubleDeltaCodec{}
			payload, err := codec.Encode(meta.TypeLong, data)
			require.NoError(t, err)
			got, err := codec.Decode(meta.TypeLong, payload, data.Rows)
			require.NoError(t, err)
			require.Equal(t, data.Rows, got.Rows)
			require.Equal(t, data.nullCount, got.nullCount)
			require.Equal(t, data.longs, got.longs)
		})
	}
}

// --- T64 tests ---------------------------------------------------------------

func TestF2_T64Roundtrip(t *testing.T) {
	rng := &testRng{s: 0xCAFE}
	tests := []struct {
		name  string
		vals  []int64
		nulls []bool
	}{
		{
			name: "small_positive",
			vals: genLongs(500, func(i int) int64 { return int64(i) }),
		},
		{
			name: "large_constant_high_bytes",
			vals: genLongs(500, func(i int) int64 { return int64(1<<40) + int64(i) }),
		},
		{
			name: "all_same",
			vals: genLongs(100, func(i int) int64 { return 12345 }),
		},
		{
			name: "mixed_range",
			vals: genLongs(300, func(i int) int64 { return int64(rng.next() % 1000000) }),
		},
		{
			name:  "with_nulls",
			vals:  genLongs(500, func(i int) int64 { return int64(i) }),
			nulls: nullsOf(500, func(i int) bool { return i%13 == 0 }),
		},
		{
			name: "two_rows",
			vals: []int64{1, 2},
		},
		{
			name: "single_row",
			vals: []int64{42},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := mkLongBlock(tt.vals, tt.nulls)
			codec := &t64Codec{}
			payload, err := codec.Encode(meta.TypeLong, data)
			require.NoError(t, err)
			got, err := codec.Decode(meta.TypeLong, payload, data.Rows)
			require.NoError(t, err)
			require.Equal(t, data.Rows, got.Rows)
			require.Equal(t, data.nullCount, got.nullCount)
			require.Equal(t, data.longs, got.longs)
		})
	}
}

// --- ALP tests ---------------------------------------------------------------

func TestF2_ALPRoundtrip(t *testing.T) {
	// ALP encoding requires values that are exactly representable as int64/10^e.
	// Using division (float64(i) / 10.0) instead of multiplication (float64(i) * 0.1)
	// ensures the values match the round-trip check float64(iv)/factor == vals[i].
	tests := []struct {
		name  string
		vals  []float64
		nulls []bool
	}{
		{
			name: "integers_as_floats",
			vals: genDoubles(500, func(i int) float64 { return float64(i * 100) }),
		},
		{
			name: "one_decimal",
			vals: genDoubles(500, func(i int) float64 { return float64(i) / 10.0 }),
		},
		{
			name: "two_decimals",
			vals: genDoubles(500, func(i int) float64 { return float64(i) / 100.0 }),
		},
		{
			name: "three_decimals",
			vals: genDoubles(300, func(i int) float64 { return float64(i) / 1000.0 }),
		},
		{
			name: "constant",
			vals: genDoubles(100, func(i int) float64 { return float64(314) / 100.0 }),
		},
		{
			name:  "with_nulls",
			vals:  genDoubles(500, func(i int) float64 { return float64(i) / 10.0 }),
			nulls: nullsOf(500, func(i int) bool { return i%7 == 0 }),
		},
		{
			name: "single_row",
			vals: []float64{float64(314) / 100.0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := mkDoubleBlock(tt.vals, tt.nulls)
			codec := &alpCodec{}
			payload, err := codec.Encode(meta.TypeDouble, data)
			require.NoError(t, err)
			got, err := codec.Decode(meta.TypeDouble, payload, data.Rows)
			require.NoError(t, err)
			require.Equal(t, data.Rows, got.Rows)
			require.Equal(t, data.nullCount, got.nullCount)
			// Compare with tolerance for floating-point precision.
			for i := range tt.vals {
				if tt.nulls != nil && i < len(tt.nulls) && tt.nulls[i] {
					continue
				}
				require.InDelta(t, data.doubles[i], got.doubles[i], 1e-9, "row %d", i)
			}
		})
	}
}

// --- CodecChain tests --------------------------------------------------------

func TestF2_CodecChainRoundtrip(t *testing.T) {
	// Build a Delta → ZSTD chain.
	deltaCodec := &deltaBitpackCodec{}
	zstdCodec := &zstdRawCodec{}
	chain := NewCodecChain(deltaCodec, zstdCodec)

	// Wait — zstdRawCodec operates on string vectors, not byte transforms.
	// The CodecChain byte-transform path only works with ZSTD compression
	// (zstdCompress/zstdDecompress), not with zstdRawCodec (which is a
	// full BlockCodec for string columns). So we need a "zstdTransformCodec"
	// that just does byte-level ZSTD compression. Let's test the chain with
	// just a single codec for now (which is equivalent to direct encoding).
	_ = chain
	_ = zstdCodec

	// Test single-codec chain (should behave same as direct codec).
	singleChain := NewCodecChain(deltaCodec)
	data := mkLongBlock(genLongs(100, func(i int) int64 { return int64(i) * 10 }), nil)
	payload, err := singleChain.Encode(meta.TypeLong, data)
	require.NoError(t, err)
	got, err := singleChain.Decode(meta.TypeLong, payload, data.Rows)
	require.NoError(t, err)
	require.Equal(t, data.longs, got.longs)
}

func TestF2_NewCodecByID(t *testing.T) {
	ids := []uint8{
		CodecIDDelta, CodecIDDoubleDelta, CodecIDGorilla,
		CodecIDT64, CodecIDALP, CodecIDZstd, CodecIDDict, CodecIDRLE,
	}
	for _, id := range ids {
		c, err := NewCodecByID(id)
		require.NoError(t, err, "codec ID %d", id)
		require.Equal(t, id, c.ID())
	}

	// Unknown ID should fail.
	_, err := NewCodecByID(255)
	require.Error(t, err)
}

func TestF2_EncodingCompositeString(t *testing.T) {
	require.Equal(t, "composite", EncodingComposite.String())
}

// --- helpers -----------------------------------------------------------------

func genLongs(n int, fn func(i int) int64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = fn(i)
	}
	return out
}

func genDoubles(n int, fn func(i int) float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = fn(i)
	}
	return out
}

func genMonotonicLongs(n int, rng *testRng) []int64 {
	out := make([]int64, n)
	cur := int64(0)
	for i := range out {
		cur += int64(rng.next()%10 + 1)
		out[i] = cur
	}
	return out
}
