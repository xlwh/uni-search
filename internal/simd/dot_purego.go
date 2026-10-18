//go:build !simd_asm

package simd

import "fmt"

// Pure Go baseline kernels: 8x-unrolled loops with eight independent
// accumulators (the fallback path of design 00-overview.md §3.2). Eight
// lanes give the scheduler room to hide multiply-add latency, and the
// smaller partial sums are also numerically more accurate than a single
// accumulator.
//
// Replacement scheme: avo-generated assembly kernels (amd64 AVX2+FMA /
// arm64 NEON) will live in files guarded by the "simd_asm" build tag; this
// file carries the inverse tag, so enabling the tag swaps out every kernel
// in one step. Any implementation that binds to the public pointers must
// honor the contracts documented on them in dist.go and pass the contract
// tests in dist_test.go unchanged.

// batchWidth is the fixed candidate count processed per DotF32Batch16 call.
const batchWidth = 16

func init() {
	DotF32 = dotF32PureGo
	L2F32 = l2F32PureGo
	DotF32Batch16 = dotF32Batch16PureGo
}

// dotF32PureGo computes the dot product with an 8x-unrolled loop. It
// panics on length mismatch and returns 0 for empty vectors (the contract
// documented on DotF32).
func dotF32PureGo(a, b []float32) float32 {
	if len(a) != len(b) {
		panic(fmt.Sprintf("simd: DotF32: length mismatch: len(a)=%d, len(b)=%d", len(a), len(b)))
	}
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	n := len(a)
	i := 0
	for ; i+8 <= n; i += 8 {
		s0 += a[i+0] * b[i+0]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
		s4 += a[i+4] * b[i+4]
		s5 += a[i+5] * b[i+5]
		s6 += a[i+6] * b[i+6]
		s7 += a[i+7] * b[i+7]
	}
	for ; i < n; i++ { // tail: the n%8 leftover lanes
		s0 += a[i] * b[i]
	}
	return ((s0 + s1) + (s2 + s3) + (s4 + s5)) + (s6 + s7)
}

// l2F32PureGo computes the squared Euclidean distance with an 8x-unrolled
// loop. It panics on length mismatch and returns 0 for empty vectors (the
// contract documented on L2F32).
func l2F32PureGo(a, b []float32) float32 {
	if len(a) != len(b) {
		panic(fmt.Sprintf("simd: L2F32: length mismatch: len(a)=%d, len(b)=%d", len(a), len(b)))
	}
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	n := len(a)
	i := 0
	for ; i+8 <= n; i += 8 {
		d0 := a[i+0] - b[i+0]
		s0 += d0 * d0
		d1 := a[i+1] - b[i+1]
		s1 += d1 * d1
		d2 := a[i+2] - b[i+2]
		s2 += d2 * d2
		d3 := a[i+3] - b[i+3]
		s3 += d3 * d3
		d4 := a[i+4] - b[i+4]
		s4 += d4 * d4
		d5 := a[i+5] - b[i+5]
		s5 += d5 * d5
		d6 := a[i+6] - b[i+6]
		s6 += d6 * d6
		d7 := a[i+7] - b[i+7]
		s7 += d7 * d7
	}
	for ; i < n; i++ { // tail: the n%8 leftover lanes
		d := a[i] - b[i]
		s0 += d * d
	}
	return ((s0 + s1) + (s2 + s3) + (s4 + s5)) + (s6 + s7)
}

// dotF32Batch16PureGo scores batchWidth candidates against q by delegating
// to dotF32PureGo, which guarantees bit-identical results with individual
// DotF32 calls (the contract documented on DotF32Batch16).
func dotF32Batch16PureGo(q []float32, cands []float32, dims int, out []float32) {
	if len(q) != dims {
		panic(fmt.Sprintf("simd: DotF32Batch16: len(q)=%d but dims=%d", len(q), dims))
	}
	if len(cands) < batchWidth*dims {
		panic(fmt.Sprintf("simd: DotF32Batch16: cands must hold at least %d floats (16*dims), got %d",
			batchWidth*dims, len(cands)))
	}
	if len(out) < batchWidth {
		panic(fmt.Sprintf("simd: DotF32Batch16: len(out) must be >= 16, got %d", len(out)))
	}
	for i := 0; i < batchWidth; i++ {
		off := i * dims
		out[i] = dotF32PureGo(q, cands[off:off+dims])
	}
}
