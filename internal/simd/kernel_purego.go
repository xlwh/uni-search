// Package simd kernel_purego.go contains the pure Go kernel function
// implementations. This file has NO build tag so the functions are
// available both as the default binding (dot_purego.go init) and as the
// fallback under the simd_asm tag (dispatch_*.go).
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.3
package simd

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

// batchWidth is the fixed candidate count processed per DotF32Batch16 call.
const batchWidth = 16

// dotF32PureGo computes the dot product with an 8x-unrolled loop.
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
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return ((s0 + s1) + (s2 + s3) + (s4 + s5)) + (s6 + s7)
}

// l2F32PureGo computes the squared Euclidean distance with an 8x-unrolled loop.
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
	for ; i < n; i++ {
		d := a[i] - b[i]
		s0 += d * d
	}
	return ((s0 + s1) + (s2 + s3) + (s4 + s5)) + (s6 + s7)
}

// dotF32Batch16PureGo scores batchWidth candidates against q.
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

// dotI8F32PureGo computes the dot product of a float32 query against an
// int8 code row decoded as unsigned uint8 levels (the SQ8 distance kernel).
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.3
func dotI8F32PureGo(q []float32, code []int8) float32 {
	if len(q) != len(code) {
		panic(fmt.Sprintf("simd: DotI8F32: length mismatch: len(q)=%d, len(code)=%d", len(q), len(code)))
	}
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	n := len(q)
	i := 0
	for ; i+8 <= n; i += 8 {
		s0 += q[i+0] * float32(uint8(code[i+0]))
		s1 += q[i+1] * float32(uint8(code[i+1]))
		s2 += q[i+2] * float32(uint8(code[i+2]))
		s3 += q[i+3] * float32(uint8(code[i+3]))
		s4 += q[i+4] * float32(uint8(code[i+4]))
		s5 += q[i+5] * float32(uint8(code[i+5]))
		s6 += q[i+6] * float32(uint8(code[i+6]))
		s7 += q[i+7] * float32(uint8(code[i+7]))
	}
	for ; i < n; i++ {
		s0 += q[i] * float32(uint8(code[i]))
	}
	return ((s0 + s1) + (s2 + s3) + (s4 + s5)) + (s6 + s7)
}

// hammingDistanceBatchPureGo computes Hamming distances using math/bits
// OnesCount64, which the Go compiler maps to hardware POPCNT on amd64.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.3
func hammingDistanceBatchPureGo(qBits []byte, codesFlat []byte, codeOffsets []int32, ids []int32, out []int) {
	nBytes := len(qBits)
	if nBytes == 0 {
		for i := range ids {
			out[i] = 0
		}
		return
	}
	if len(out) < len(ids) {
		panic(fmt.Sprintf("simd: HammingDistanceBatch: len(out)=%d < len(ids)=%d", len(out), len(ids)))
	}
	for i, id := range ids {
		off := int(codeOffsets[id])
		if off < 0 || off+nBytes > len(codesFlat) {
			out[i] = -1
			continue
		}
		b := codesFlat[off : off+nBytes]
		dist := 0
		j := 0
		for ; j+8 <= nBytes; j += 8 {
			x := binary.LittleEndian.Uint64(qBits[j:]) ^ binary.LittleEndian.Uint64(b[j:])
			dist += bits.OnesCount64(x)
		}
		for ; j < nBytes; j++ {
			dist += bits.OnesCount8(qBits[j] ^ b[j])
		}
		out[i] = dist
	}
}

// dotF16F32PureGo computes the dot product of a float32 query against
// a float16 vector stored as uint16 values. Each uint16 is converted
// to float32 via IEEE 754 half-precision decoding before multiplication.
//
// @spec: ./archive/2026-10-23-qdrant-features-import/02_spec.md#2.4
func dotF16F32PureGo(q []float32, docF16 []uint16) float32 {
	if len(q) != len(docF16) {
		panic(fmt.Sprintf("simd: DotF16F32: length mismatch: len(q)=%d, len(docF16)=%d", len(q), len(docF16)))
	}
	var s float32
	for i := range q {
		s += q[i] * f16ToF32SIMD(docF16[i])
	}
	return s
}

// f16ToF32SIMD converts IEEE 754 half-precision to float32.
// Inlined here to avoid a dependency on internal/types.
func f16ToF32SIMD(h uint16) float32 {
	sign := uint32(h>>15) & 1
	exp := uint32(h>>10) & 0x1F
	mant := uint32(h) & 0x3FF

	switch {
	case exp == 0 && mant == 0:
		return math32frombits(sign << 31)
	case exp == 0:
		e := int32(-14)
		for mant&0x400 == 0 {
			mant <<= 1
			e--
		}
		mant &= 0x3FF
		return math32frombits(sign<<31 | uint32(e+112+15)<<23 | mant<<13)
	case exp == 0x1F:
		return math32frombits(sign<<31 | 0x7F800000 | mant<<13)
	default:
		return math32frombits(sign<<31 | (exp+112)<<23 | mant<<13)
	}
}

// math32frombits wraps math.Float32frombits for readability.
func math32frombits(b uint32) float32 {
	return math.Float32frombits(b)
}
