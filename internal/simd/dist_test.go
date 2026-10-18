package simd

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Naive per-element reference implementations. -------------------------
//
// The references accumulate in float64, which is the ground truth required
// by the R2 accuracy budget: a float32 sequential loop is itself off by
// more than 1e-6 relative at dims 768, so comparing kernel against a
// float32 naive loop would measure noise, not correctness.

// naiveDotF64 computes the dot product element by element in float64.
func naiveDotF64(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

// naiveL2F64 computes the squared Euclidean distance element by element in
// float64.
func naiveL2F64(a, b []float32) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return sum
}

// randVec returns n uniform-random float32 values in [0, 1) drawn from a
// deterministic PCG stream so failures are reproducible. All-positive data
// keeps the reference dot product well conditioned: with mixed-sign data
// the exact cancellation of partial sums can push the *inherent* float32
// rounding error far past 1e-6 relative for any implementation, including
// a perfect one (see the exact-patterns test below for signed operands).
func randVec(rng *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = rng.Float32()
	}
	return v
}

// relError returns |got-want| / |want|. Callers must guarantee want != 0
// (empty vectors are asserted to be exactly 0 instead).
func relError(got float32, want float64) float64 {
	return math.Abs(float64(got)-want) / math.Abs(want)
}

// requireCloseRel asserts the R2 accuracy budget: the kernel result must be
// within 1e-6 relative error of the naive reference.
func requireCloseRel(t *testing.T, label string, got float32, want float64) {
	t.Helper()
	rel := relError(got, want)
	require.Lessf(t, rel, 1e-6, "%s: kernel %v vs naive %v: rel err %g", label, got, want, rel)
}

// TestR2_DistanceCorrectness pins the R2 acceptance scenario: for random
// vectors at production dims (128/768/1536) and at every boundary length
// around the 8x unroll (0/1/15/16/17/31/32/33, per 00-overview.md §3.2),
// DotF32 and L2F32 must match the naive per-element reference within 1e-6
// relative error, and DotF32Batch16 must match 16 individual DotF32 calls.
func TestR2_DistanceCorrectness(t *testing.T) {
	const pairsPerDim = 16

	tests := []struct {
		name string
		n    int // vector length
	}{
		{name: "dims-128", n: 128},
		{name: "dims-768", n: 768},
		{name: "dims-1536", n: 1536},
		{name: "len-0", n: 0},
		{name: "len-1", n: 1},
		{name: "len-15", n: 15},
		{name: "len-16", n: 16},
		{name: "len-17", n: 17},
		{name: "len-31", n: 31},
		{name: "len-32", n: 32},
		{name: "len-33", n: 33},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(tt.n), 1))

			if tt.n == 0 {
				// Empty vectors yield exact zeros, not just values close
				// to zero (relative error is undefined against a zero
				// reference, hence the exact assertions).
				a := randVec(rng, 0)
				b := randVec(rng, 0)
				require.Equal(t, float32(0), DotF32(a, b), "DotF32 of empty vectors must be 0")
				require.Equal(t, float32(0), L2F32(a, b), "L2F32 of empty vectors must be 0")
			}
			for p := 0; p < pairsPerDim && tt.n > 0; p++ {
				a := randVec(rng, tt.n)
				b := randVec(rng, tt.n)
				requireCloseRel(t, fmt.Sprintf("DotF32 %s pair %d", tt.name, p),
					DotF32(a, b), naiveDotF64(a, b))
				requireCloseRel(t, fmt.Sprintf("L2F32 %s pair %d", tt.name, p),
					L2F32(a, b), naiveL2F64(a, b))
			}

			// Batch16 must agree with scoring the same candidates one by
			// one. out is deliberately longer than 16 to pin the contract
			// that trailing elements are untouched.
			q := randVec(rng, tt.n)
			cands := randVec(rng, batchWidth*tt.n)
			out := make([]float32, batchWidth+4)
			copy(out[batchWidth:], []float32{1, 2, 3, 4}) // sentinels
			DotF32Batch16(q, cands, tt.n, out)
			if tt.n == 0 {
				for i := 0; i < batchWidth; i++ {
					require.Equal(t, float32(0), out[i],
						"DotF32Batch16 with dims=0 must produce zeros")
				}
			} else {
				for i := 0; i < batchWidth; i++ {
					want := DotF32(q, cands[i*tt.n:(i+1)*tt.n])
					requireCloseRel(t, fmt.Sprintf("Batch16 %s cand %d", tt.name, i),
						out[i], float64(want))
				}
			}
			assert.Equal(t, []float32{1, 2, 3, 4}, out[batchWidth:],
				"DotF32Batch16 must not write past out[15]")
		})
	}
}

// TestR2_ExactIntegerPatterns uses ±1 vectors whose arithmetic is exactly
// representable in float32, including negative operands: the kernels must
// return the mathematically exact values.
func TestR2_ExactIntegerPatterns(t *testing.T) {
	const n = 17
	a := make([]float32, n)
	neg := make([]float32, n)
	for i := range a {
		a[i] = float32(1 - 2*(i%2)) // +1, -1, +1, ...
		neg[i] = -a[i]
	}

	require.Equal(t, float32(n), DotF32(a, a), "dot of ±1 vector with itself: all products +1")
	require.Equal(t, float32(-n), DotF32(a, neg), "dot with negated vector: all products -1")
	require.Equal(t, float32(0), L2F32(a, a), "L2 of identical vectors is exactly 0")
	require.Equal(t, float32(4*n), L2F32(a, neg), "opposite ±1 vectors: 17 differences of ±2, squares 4")
}

// TestR2_LengthMismatchPanics pins the shape contract of DotF32 and L2F32:
// mismatched lengths must panic loudly instead of returning NaN or
// silently truncating to the shorter vector.
func TestR2_LengthMismatchPanics(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{1, 2, 3, 4}

	require.Panics(t, func() { DotF32(a, b) }, "DotF32 must panic when len(a) < len(b)")
	require.Panics(t, func() { DotF32(b, a) }, "DotF32 must panic when len(a) > len(b)")
	require.Panics(t, func() { L2F32(a, b) }, "L2F32 must panic when len(a) < len(b)")
	require.Panics(t, func() { L2F32(b, a) }, "L2F32 must panic when len(a) > len(b)")
}

// TestR2_Batch16ShapeContractPanics pins the shape contract of
// DotF32Batch16: q must match dims, cands must hold 16*dims floats, and out
// must have at least 16 slots.
func TestR2_Batch16ShapeContractPanics(t *testing.T) {
	const dims = 4
	q := make([]float32, dims)
	cands := make([]float32, batchWidth*dims)
	out := make([]float32, batchWidth)

	t.Run("q shorter than dims", func(t *testing.T) {
		require.Panics(t, func() { DotF32Batch16(q[:dims-1], cands, dims, out) })
	})
	t.Run("q longer than dims", func(t *testing.T) {
		require.Panics(t, func() { DotF32Batch16(append(q, 9), cands, dims, out) })
	})
	t.Run("cands shorter than 16*dims", func(t *testing.T) {
		require.Panics(t, func() { DotF32Batch16(q, cands[:batchWidth*dims-1], dims, out) })
	})
	t.Run("out shorter than 16", func(t *testing.T) {
		require.Panics(t, func() { DotF32Batch16(q, cands, dims, out[:batchWidth-1]) })
	})
}

// TestR2_PointersBound guards against a future refactor leaving the public
// pointers nil, which would turn every query into a runtime panic.
func TestR2_PointersBound(t *testing.T) {
	require.NotNil(t, DotF32)
	require.NotNil(t, L2F32)
	require.NotNil(t, DotF32Batch16)
}

// --- Benchmarks (observability only, no gate) ------------------------------

// benchSink prevents the compiler from optimizing the benchmark loops away.
var benchSink float32

func BenchmarkR2_DotF32(b *testing.B) {
	for _, dims := range []int{128, 768} {
		b.Run(fmt.Sprintf("dims-%d", dims), func(b *testing.B) {
			rng := rand.New(rand.NewPCG(uint64(dims), 42))
			a := randVec(rng, dims)
			c := randVec(rng, dims)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchSink = DotF32(a, c)
			}
		})
	}
}

func BenchmarkR2_L2F32(b *testing.B) {
	for _, dims := range []int{128, 768} {
		b.Run(fmt.Sprintf("dims-%d", dims), func(b *testing.B) {
			rng := rand.New(rand.NewPCG(uint64(dims), 43))
			a := randVec(rng, dims)
			c := randVec(rng, dims)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchSink = L2F32(a, c)
			}
		})
	}
}

func BenchmarkR2_DotF32Batch16(b *testing.B) {
	for _, dims := range []int{128, 768} {
		b.Run(fmt.Sprintf("dims-%d", dims), func(b *testing.B) {
			rng := rand.New(rand.NewPCG(uint64(dims), 44))
			q := randVec(rng, dims)
			cands := randVec(rng, batchWidth*dims)
			out := make([]float32, batchWidth)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				DotF32Batch16(q, cands, dims, out)
			}
			benchSink = out[0]
		})
	}
}
