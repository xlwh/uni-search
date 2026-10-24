// Package simd phase1_test.go tests the new Phase 1 kernels:
// DotI8F32 (int8 × float32 mixed dot product for SQ8) and
// HammingDistanceBatch (batch Hamming distance for RaBitQ).
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#7.2
package simd

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- DotI8F32 tests --------------------------------------------------------

// naiveDotI8F32 computes the dot product of a float32 query against
// int8 codes decoded as unsigned uint8 levels, using float64 reference.
func naiveDotI8F32(q []float32, code []int8) float64 {
	var sum float64
	for i := range q {
		sum += float64(q[i]) * float64(uint8(code[i]))
	}
	return sum
}

func TestDotI8F32_Correctness(t *testing.T) {
	dims := []int{0, 1, 7, 8, 9, 127, 128, 129, 767, 768, 769}
	for _, n := range dims {
		t.Run("dims_"+itoa(n), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(n), 42))
			q := make([]float32, n)
			code := make([]int8, n)
			for i := range q {
				q[i] = rng.Float32()
				code[i] = int8(rng.IntN(256))
			}
			if n == 0 {
				require.Equal(t, float32(0), DotI8F32(q, code),
					"DotI8F32 of empty vectors must be 0")
				return
			}
			got := DotI8F32(q, code)
			want := naiveDotI8F32(q, code)
			rel := math.Abs(float64(got)-want) / math.Abs(want)
			require.Lessf(t, rel, 1e-5,
				"DotI8F32 dims=%d: kernel %v vs naive %v: rel err %g", n, got, want, rel)
		})
	}
}

func TestDotI8F32_ExactKnownValues(t *testing.T) {
	// Hand-computed: q=[1,2,3], code=[0,128,255] → 1*0 + 2*128 + 3*255 = 0+256+765 = 1021
	q := []float32{1, 2, 3}
	code := []int8{0, -128, -1} // uint8: 0, 128, 255
	got := DotI8F32(q, code)
	require.InDeltaf(t, 1021.0, got, 1e-3, "DotI8F32 known values")
}

func TestDotI8F32_LengthMismatchPanics(t *testing.T) {
	q := []float32{1, 2, 3}
	code := []int8{1, 2}
	require.Panics(t, func() { DotI8F32(q, code) }, "DotI8F32 must panic on length mismatch")
	require.Panics(t, func() { DotI8F32(q, code[:1]) }, "DotI8F32 must panic on length mismatch")
}

func TestDotI8F32_PointerBound(t *testing.T) {
	require.NotNil(t, DotI8F32, "DotI8F32 must be bound")
}

// --- HammingDistanceBatch tests --------------------------------------------

func TestHammingDistanceBatch_Correctness(t *testing.T) {
	// Query bitmap: 0xFF 0x0F 0x00 (3 bytes)
	qBits := []byte{0xFF, 0x0F, 0x00}
	// Two candidates:
	//   cand 0: 0xFF 0x0F 0x00 → Hamming = 0 (identical)
	//   cand 1: 0x00 0xF0 0xFF → XOR: 0xFF 0xFF 0xFF → Hamming = 8+8+8 = 24
	codesFlat := []byte{
		0xFF, 0x0F, 0x00, // cand 0
		0x00, 0xF0, 0xFF, // cand 1
	}
	codeOffsets := []int32{0, 3}
	ids := []int32{0, 1}
	out := make([]int, 2)
	HammingDistanceBatch(qBits, codesFlat, codeOffsets, ids, out)
	assert.Equal(t, 0, out[0], "Hamming distance for identical bitmaps")
	assert.Equal(t, 24, out[1], "Hamming distance for complement bitmaps")
}

func TestHammingDistanceBatch_LargeBitmask(t *testing.T) {
	// Test with 768-bit (96-byte) bitmaps to match RaBitQ at dim=768
	nBytes := 96
	rng := rand.New(rand.NewPCG(768, 1))
	qBits := make([]byte, nBytes)
	for i := range qBits {
		qBits[i] = byte(rng.IntN(256))
	}
	// 4 candidates, 2 identical to query, 2 random
	codesFlat := make([]byte, 4*nBytes)
	copy(codesFlat[0:nBytes], qBits)       // cand 0: identical
	randFill(rng, codesFlat[nBytes:2*nBytes]) // cand 1: random
	copy(codesFlat[2*nBytes:3*nBytes], qBits)  // cand 2: identical
	randFill(rng, codesFlat[3*nBytes:4*nBytes]) // cand 3: random

	codeOffsets := []int32{0, int32(nBytes), int32(2 * nBytes), int32(3 * nBytes)}
	ids := []int32{0, 1, 2, 3}
	out := make([]int, 4)
	HammingDistanceBatch(qBits, codesFlat, codeOffsets, ids, out)

	// Identical candidates must have distance 0
	assert.Equal(t, 0, out[0], "identical bitmap should have Hamming distance 0")
	assert.Equal(t, 0, out[2], "identical bitmap should have Hamming distance 0")
	// Random candidates must have non-zero distance
	assert.Greater(t, out[1], 0, "random bitmap should have non-zero Hamming distance")
	assert.Greater(t, out[3], 0, "random bitmap should have non-zero Hamming distance")
	// Verify against naive computation
	for i, id := range ids {
		off := int(codeOffsets[id])
		expected := naiveHamming(qBits, codesFlat[off:off+nBytes])
		assert.Equal(t, expected, out[i], "Hamming distance mismatch for cand %d", id)
	}
}

func TestHammingDistanceBatch_EmptyQuery(t *testing.T) {
	qBits := []byte{}
	codesFlat := []byte{}
	codeOffsets := []int32{0}
	ids := []int32{0}
	out := make([]int, 1)
	HammingDistanceBatch(qBits, codesFlat, codeOffsets, ids, out)
	assert.Equal(t, 0, out[0], "Hamming distance for empty bitmaps must be 0")
}

func TestHammingDistanceBatch_PointerBound(t *testing.T) {
	require.NotNil(t, HammingDistanceBatch, "HammingDistanceBatch must be bound")
}

// --- Benchmarks -------------------------------------------------------------

func BenchmarkDotI8F32(b *testing.B) {
	rng := rand.New(rand.NewPCG(768, 1))
	n := 768
	q := make([]float32, n)
	code := make([]int8, n)
	for i := range q {
		q[i] = rng.Float32()
		code[i] = int8(rng.IntN(256))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = DotI8F32(q, code)
	}
}

func BenchmarkHammingDistanceBatch(b *testing.B) {
	nBytes := 96 // 768 bits
	rng := rand.New(rand.NewPCG(768, 1))
	qBits := make([]byte, nBytes)
	for i := range qBits {
		qBits[i] = byte(rng.IntN(256))
	}
	const nCands = 16
	codesFlat := make([]byte, nCands*nBytes)
	randFill(rng, codesFlat)
	codeOffsets := make([]int32, nCands)
	for i := range codeOffsets {
		codeOffsets[i] = int32(i * nBytes)
	}
	ids := make([]int32, nCands)
	for i := range ids {
		ids[i] = int32(i)
	}
	out := make([]int, nCands)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		HammingDistanceBatch(qBits, codesFlat, codeOffsets, ids, out)
	}
}

// --- Helpers ----------------------------------------------------------------

func randFill(rng *rand.Rand, buf []byte) {
	for i := range buf {
		buf[i] = byte(rng.IntN(256))
	}
}

func naiveHamming(a, b []byte) int {
	dist := 0
	for i := 0; i+8 <= len(a); i += 8 {
		x := binary.LittleEndian.Uint64(a[i:]) ^ binary.LittleEndian.Uint64(b[i:])
		dist += int(popcount64(x))
	}
	for i := len(a) &^ 7; i < len(a); i++ {
		x := a[i] ^ b[i]
		for x != 0 {
			dist++
			x &= x - 1
		}
	}
	return dist
}

func popcount64(x uint64) int {
	count := 0
	for x != 0 {
		count++
		x &= x - 1
	}
	return count
}

// itoa is a minimal int-to-string to avoid strconv import in test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
