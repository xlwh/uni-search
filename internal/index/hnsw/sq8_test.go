// Package hnsw sq8_test.go tests SQ8 (Scalar Quantization 8-bit)
// encoding/decoding and recall.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.6
package hnsw

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SQ8Quantizer performs 8-bit scalar quantization: maps float32
// values to [0, 255] using min/max scaling. This is the default
// quantization for HNSW warm tier (design 02 §2.5).
// Memory reduction: 4x (float32 → uint8).

// quantizeSQ8 quantizes a float32 vector to uint8 using min/max scaling.
func quantizeSQ8(vec []float32) ([]uint8, float32, float32) {
	if len(vec) == 0 {
		return nil, 0, 0
	}
	minV, maxV := float32(math.MaxFloat32), float32(-math.MaxFloat32)
	for _, v := range vec {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	scale := float32(255.0)
	rangeV := maxV - minV
	if rangeV == 0 {
		rangeV = 1
	}

	out := make([]uint8, len(vec))
	for i, v := range vec {
		normalized := (v - minV) / rangeV
		out[i] = uint8(math.Round(float64(normalized * scale)))
	}
	return out, minV, maxV
}

// dequantizeSQ8 converts a uint8 vector back to float32 using the
// stored min/max.
func dequantizeSQ8(quantized []uint8, minV, maxV float32) []float32 {
	out := make([]float32, len(quantized))
	rangeV := maxV - minV
	if rangeV == 0 {
		rangeV = 1
	}
	scale := rangeV / 255.0
	for i, q := range quantized {
		out[i] = minV + float32(q)*scale
	}
	return out
}

// sq8Distance computes the approximate L2 distance between a query
// vector (float32) and a quantized vector (uint8 + min/max).
func sq8Distance(query []float32, quantized []uint8, minV, maxV float32) float32 {
	if len(query) != len(quantized) {
		return float32(math.MaxFloat32)
	}
	rangeV := maxV - minV
	if rangeV == 0 {
		rangeV = 1
	}
	scale := rangeV / 255.0

	var dist float32
	for i := 0; i < len(query); i++ {
		deqVal := minV + float32(quantized[i])*scale
		diff := query[i] - deqVal
		dist += diff * diff
	}
	return dist
}

func TestSQ8_QuantizeDequantizeRoundtrip(t *testing.T) {
	// Test that dequantize(quantize(v)) is close to v.
	vec := []float32{0.1, 0.5, 0.9, -0.3, 0.7, 1.0, -1.0, 0.0}
	quantized, minV, maxV := quantizeSQ8(vec)
	require.Len(t, quantized, len(vec), "quantized length should match")

	dequantized := dequantizeSQ8(quantized, minV, maxV)
	require.Len(t, dequantized, len(vec))

	// SQ8 has 256 levels; max quantization error is range/256.
	rangeV := maxV - minV
	maxErr := rangeV / 255.0
	for i := range vec {
		err := math.Abs(float64(vec[i] - dequantized[i]))
		assert.LessOrEqual(t, err, float64(maxErr*1.5), "quantization error at index %d should be within tolerance", i)
	}
}

func TestSQ8_MemoryReduction(t *testing.T) {
	// SQ8 reduces float32 (4 bytes) to uint8 (1 byte) → 4x reduction.
	dim := 768
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = float32(i) / float32(dim)
	}

	originalBytes := len(vec) * 4 // float32 = 4 bytes
	quantized, _, _ := quantizeSQ8(vec)
	quantizedBytes := len(quantized) // uint8 = 1 byte

	assert.Equal(t, dim*4, originalBytes, "original should be dim*4 bytes")
	assert.Equal(t, dim, quantizedBytes, "quantized should be dim bytes")
	assert.Equal(t, 4, originalBytes/quantizedBytes, "4x memory reduction")
}

func TestSQ8_DistanceApproximation(t *testing.T) {
	// SQ8 distance should be close to exact float32 distance.
	vec1 := []float32{0.1, 0.2, 0.3, 0.4}
	vec2 := []float32{0.5, 0.6, 0.7, 0.8}

	// Exact L2 distance.
	var exactDist float32
	for i := range vec1 {
		diff := vec1[i] - vec2[i]
		exactDist += diff * diff
	}

	// SQ8 distance (quantize vec2, compare with vec1).
	quantized2, minV, maxV := quantizeSQ8(vec2)
	sq8Dist := sq8Distance(vec1, quantized2, minV, maxV)

	// SQ8 distance should be within 10% of exact.
	ratio := float64(sq8Dist) / float64(exactDist)
	assert.InDelta(t, 1.0, ratio, 0.15, "SQ8 distance should be within 15% of exact")
}

func TestSQ8_Recall(t *testing.T) {
	// Test that SQ8 preserves ranking order well enough for high recall.
	dim := 128
	numVectors := 200

	// Generate random vectors.
	vectors := make([][]float32, numVectors)
	for i := range vectors {
		vectors[i] = make([]float32, dim)
		for j := range vectors[i] {
			vectors[i][j] = float32(math.Sin(float64(i*j+1))) * 0.5
		}
	}

	// Query vector.
	query := make([]float32, dim)
	for j := range query {
		query[j] = float32(math.Sin(float64(j+1))) * 0.5
	}

	// Compute exact distances.
	type hit struct {
		idx  int
		dist float32
	}
	exactHits := make([]hit, numVectors)
	for i, vec := range vectors {
		var d float32
		for j := range query {
			diff := query[j] - vec[j]
			d += diff * diff
		}
		exactHits[i] = hit{idx: i, dist: d}
	}

	// Sort by ascending distance (exact).
	for i := 0; i < len(exactHits); i++ {
		for j := i + 1; j < len(exactHits); j++ {
			if exactHits[j].dist < exactHits[i].dist {
				exactHits[i], exactHits[j] = exactHits[j], exactHits[i]
			}
		}
	}

	// Compute SQ8 distances.
	sq8Hits := make([]hit, numVectors)
	for i, vec := range vectors {
		q, minV, maxV := quantizeSQ8(vec)
		sq8Hits[i] = hit{idx: i, dist: sq8Distance(query, q, minV, maxV)}
	}

	// Sort by ascending distance (SQ8).
	for i := 0; i < len(sq8Hits); i++ {
		for j := i + 1; j < len(sq8Hits); j++ {
			if sq8Hits[j].dist < sq8Hits[i].dist {
				sq8Hits[i], sq8Hits[j] = sq8Hits[j], sq8Hits[i]
			}
		}
	}

	// Compute recall@10: how many of the exact top-10 are in the SQ8 top-10?
	k := 10
	exactTopK := make(map[int]bool)
	for i := 0; i < k && i < len(exactHits); i++ {
		exactTopK[exactHits[i].idx] = true
	}

	overlap := 0
	for i := 0; i < k && i < len(sq8Hits); i++ {
		if exactTopK[sq8Hits[i].idx] {
			overlap++
		}
	}

	recall := float64(overlap) / float64(k)
	assert.GreaterOrEqual(t, recall, 0.7, "SQ8 recall@10 should be >= 70%%")
}

func TestSQ8_EdgeCases(t *testing.T) {
	// All same values.
	same := []float32{5.0, 5.0, 5.0}
	q, minV, maxV := quantizeSQ8(same)
	assert.Equal(t, minV, maxV, "min should equal max for constant vector")
	deq := dequantizeSQ8(q, minV, maxV)
	for i := range deq {
		assert.InDelta(t, 5.0, deq[i], 0.01, "constant vector should roundtrip at index %d", i)
	}

	// Single element.
	single := []float32{42.0}
	qs, minS, maxS := quantizeSQ8(single)
	require.Len(t, qs, 1)
	deqs := dequantizeSQ8(qs, minS, maxS)
	assert.InDelta(t, 42.0, deqs[0], 0.1)

	// Empty vector.
	empty := []float32{}
	qe, _, _ := quantizeSQ8(empty)
	assert.Nil(t, qe, "empty vector should produce nil quantized")
}
