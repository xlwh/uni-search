// Package hnsw pq_test.go tests Product Quantization training, encoding,
// LUT distance, and search integration.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#7.2
package hnsw

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
)

// TestPQ_TrainAndEncode verifies PQ training produces valid codes.
func TestPQ_TrainAndEncode(t *testing.T) {
	n, dims := 200, 64
	rng := rand.New(rand.NewPCG(42, 1))
	vecs := make([]float32, n*dims)
	for i := range vecs {
		vecs[i] = rng.Float32()*2 - 1
	}
	cfg := Config{M: 8, EFConstruction: 50, Metric: MetricDot, Seed: 42}
	g, err := BuildGraph(cfg, n, dims, vecs)
	require.NoError(t, err)

	pq, err := trainPQ(g, 8)
	require.NoError(t, err)
	assert.Equal(t, 8, pq.m)
	assert.Equal(t, 8, pq.dsub) // 64/8=8
	assert.Equal(t, n*8, len(pq.codes)) // n*m bytes
	// Each code must be in [0, 255].
	for _, c := range pq.codes {
		assert.LessOrEqual(t, c, byte(255))
	}
}

// TestPQ_MemBytes verifies memory reporting.
func TestPQ_MemBytes(t *testing.T) {
	n, dims := 100, 32
	rng := rand.New(rand.NewPCG(42, 1))
	vecs := make([]float32, n*dims)
	for i := range vecs {
		vecs[i] = rng.Float32()
	}
	cfg := Config{M: 4, Metric: MetricDot, Seed: 42}
	g, err := BuildGraph(cfg, n, dims, vecs)
	require.NoError(t, err)

	pq, err := trainPQ(g, 4)
	require.NoError(t, err)
	// Memory = codes (n*m) + centroids (m*256*dsub*4)
	expected := int64(n*4) + int64(4*256*8*4)
	assert.Equal(t, expected, pq.MemBytes())
}

// TestPQ_SearchIntegration verifies PQ search returns reasonable results.
func TestPQ_SearchIntegration(t *testing.T) {
	n, dims := 500, 32
	rng := rand.New(rand.NewPCG(42, 1))
	vecs := make([]float32, n*dims)
	for i := range vecs {
		vecs[i] = rng.Float32()*2 - 1
	}
	cfg := Config{M: 8, EFConstruction: 100, EFSearch: 50, Metric: MetricDot, Seed: 42}
	g, err := BuildGraph(cfg, n, dims, vecs)
	require.NoError(t, err)

	err = g.EnablePQ(4)
	require.NoError(t, err)
	assert.True(t, g.PQActive())

	// Search should return results.
	q := make([]float32, dims)
	copy(q, vecs[0:dims])
	hits, err := g.Search(context.Background(), q, 10, 50, nil, index.SearchBudget{})
	require.NoError(t, err)
	assert.NotEmpty(t, hits, "PQ search should return results")

	// Disable PQ.
	g.DisablePQ()
	assert.False(t, g.PQActive())
}

// TestPQ_L2MetricError verifies L2 metric returns error.
func TestPQ_L2MetricError(t *testing.T) {
	n, dims := 50, 32
	vecs := make([]float32, n*dims)
	cfg := Config{M: 4, Metric: MetricL2, Seed: 42}
	g, err := BuildGraph(cfg, n, dims, vecs)
	require.NoError(t, err)
	err = g.EnablePQ(4)
	require.Error(t, err, "PQ should reject L2 metric")
}

// TestPQ_DivisibleError verifies dims not divisible by m returns error.
func TestPQ_DivisibleError(t *testing.T) {
	n, dims := 50, 30 // 30 not divisible by 4
	vecs := make([]float32, n*dims)
	cfg := Config{M: 4, Metric: MetricDot, Seed: 42}
	g, err := BuildGraph(cfg, n, dims, vecs)
	require.NoError(t, err)
	err = g.EnablePQ(4)
	require.Error(t, err, "PQ should reject non-divisible dims")
}
