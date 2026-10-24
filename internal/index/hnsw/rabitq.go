// Package hnsw rabitq.go implements the RaBitQ 1-bit quantization
// for the warm+ tier (design 02-index-engine.md §2.5). RaBitQ reduces
// vector memory to 1/32 of float32 while maintaining ≥95% recall via
// binary quantization + rerank from the original vectors.
//
// Phase 2 optimization (vsag algo integration): projection matrix flattened
// from [][]float32 to []float32 for cache-friendly access; HammingDistance
// uses math/bits.OnesCount64 for hardware POPCNT acceleration; Quantize
// loop order transposed for sequential memory access.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.2
package hnsw

import (
	"encoding/binary"
	"math"
	"math/bits"
	"math/rand"
)

// RaBitQuantizer performs 1-bit quantization of float32 vectors.
// The quantized representation uses a random projection matrix and
// binary sign quantization, achieving ~32× memory reduction.
type RaBitQuantizer struct {
	dim   int     // vector dimensionality
	proj  []float32 // flattened random projection matrix (dim*nBits, contiguous)
	nBits int     // number of projection bits (typically dim)
}

// NewRaBitQuantizer creates a quantizer with the given dimension.
// The projection matrix is seeded deterministically for reproducibility.
func NewRaBitQuantizer(dim int) *RaBitQuantizer {
	if dim <= 0 {
		dim = 768
	}
	nBits := dim
	q := &RaBitQuantizer{
		dim:   dim,
		nBits: nBits,
		proj:  make([]float32, dim*nBits), // single contiguous allocation
	}
	// Generate random projection matrix (Gaussian distribution).
	// Stored in row-major: proj[i*nBits+j] is the (i,j) entry.
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < dim; i++ {
		base := i * nBits
		for j := 0; j < nBits; j++ {
			// Box-Muller transform for Gaussian random.
			u1 := rng.Float64()
			u2 := rng.Float64()
			z := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
			q.proj[base+j] = float32(z) / float32(math.Sqrt(float64(nBits)))
		}
	}
	return q
}

// Quantize converts a float32 vector to a binary bitmap.
// Each bit is the sign of the projected dot product.
//
// Phase 2 optimization: loop order transposed — outer iterates over dim
// (sequential proj access), inner over nBits — making proj[i*nBits+j]
// access contiguous in the inner loop for cache line efficiency.
func (q *RaBitQuantizer) Quantize(vec []float32) []byte {
	if len(vec) != q.dim {
		return nil
	}
	nBytes := (q.nBits + 7) / 8
	out := make([]byte, nBytes)
	// Accumulate projection dots in a flat array (cache-friendly).
	dots := make([]float32, q.nBits)
	for i := 0; i < q.dim; i++ {
		vi := vec[i]
		base := i * q.nBits
		for j := 0; j < q.nBits; j++ {
			dots[j] += vi * q.proj[base+j]
		}
	}
	for j := 0; j < q.nBits; j++ {
		if dots[j] > 0 {
			out[j/8] |= 1 << (j % 8)
		}
	}
	return out
}

// HammingDistance computes the Hamming distance between two binary
// quantized vectors. Lower distance = higher similarity.
//
// Phase 2 optimization: uses math/bits.OnesCount64 for hardware POPCNT
// acceleration (the Go compiler maps OnesCount64 to the POPCNT instruction
// on amd64 when available). Processes 8 bytes at a time, falling back to
// OnesCount8 for the tail.
func HammingDistance(a, b []byte) int {
	if len(a) != len(b) {
		return -1
	}
	dist := 0
	i := 0
	for ; i+8 <= len(a); i += 8 {
		x := binary.LittleEndian.Uint64(a[i:]) ^ binary.LittleEndian.Uint64(b[i:])
		dist += bits.OnesCount64(x)
	}
	for ; i < len(a); i++ {
		dist += bits.OnesCount8(a[i] ^ b[i])
	}
	return dist
}

// ApproximateDotProduct estimates the dot product from Hamming distance.
// The estimate is based on the RaBitQ theoretical bound: the expected
// dot product is proportional to (nBits - 2*hamming) / nBits.
func ApproximateDotProduct(hamming, nBits int) float32 {
	if nBits <= 0 {
		return 0
	}
	return float32(nBits-2*hamming) / float32(nBits)
}

// QuantizedBatch quantizes a batch of vectors.
func (q *RaBitQuantizer) QuantizeBatch(vectors [][]float32) [][]byte {
	out := make([][]byte, len(vectors))
	for i, v := range vectors {
		out[i] = q.Quantize(v)
	}
	return out
}

// MemorySavings returns the memory reduction ratio (float32 → binary).
// For dim=768: 768*4 bytes → 768/8=96 bytes → 32× reduction.
func (q *RaBitQuantizer) MemorySavings() int {
	return 32 // 1/32 memory (float32 → 1-bit)
}
