// Package hnsw phase4_test.go tests adaptive pruning (alpha parameter)
// and ODescent batch graph construction.
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

// TestAdaptiveHeuristic_AlphaEqualsOne verifies that alpha=1.0 produces
// identical results to the standard heuristic.
func TestAdaptiveHeuristic_AlphaEqualsOne(t *testing.T) {
	n, dims := 200, 64
	rng := rand.New(rand.NewPCG(42, 1))
	vecs := make([]float32, n*dims)
	for i := range vecs {
		vecs[i] = rng.Float32()*2 - 1
	}
	cfg := Config{M: 8, EFConstruction: 50, Metric: MetricDot}
	cfg = cfg.resolve()
	b := &builder{
		cfg:      cfg,
		n:        n,
		dims:     dims,
		vecs:     vecs,
		searcher: newLayerSearcher(n, 2*cfg.M+1),
		batch:    make([]float32, 16*dims),
		rng:      rng,
		mult:     2.0,
	}
	// Build candidates.
	cands := make([]cand, 20)
	for i := range cands {
		cands[i] = cand{int32(i), b.distOne(b.vecRow(0), int32(i+1))}
	}
	sortCands(cands)
	// alpha=1.0 should match standard heuristic.
	stdResult := b.heuristic(cands, 8)
	adaptResult := b.adaptiveHeuristic(cands, 8, 1.0)
	require.Equal(t, len(stdResult), len(adaptResult), "alpha=1.0 should match standard")
	for i := range stdResult {
		assert.Equal(t, stdResult[i].id, adaptResult[i].id, "alpha=1.0 same selection at %d", i)
	}
}

// TestAdaptiveHeuristic_AlphaGreaterRelaxed verifies that alpha>1.0
// keeps at least as many neighbors as alpha=1.0.
func TestAdaptiveHeuristic_AlphaGreaterRelaxed(t *testing.T) {
	n, dims := 100, 32
	rng := rand.New(rand.NewPCG(99, 1))
	vecs := make([]float32, n*dims)
	for i := range vecs {
		vecs[i] = rng.Float32()*2 - 1
	}
	cfg := Config{M: 4, Metric: MetricDot}.resolve()
	b := &builder{
		cfg:      cfg,
		n:        n,
		dims:     dims,
		vecs:     vecs,
		searcher: newLayerSearcher(n, 2*cfg.M+1),
		batch:    make([]float32, 16*dims),
		rng:      rng,
		mult:     2.0,
	}
	cands := make([]cand, 30)
	for i := range cands {
		cands[i] = cand{int32(i), b.distOne(b.vecRow(0), int32(i+1))}
	}
	sortCands(cands)
	// alpha=2.0 (relaxed) should keep >= alpha=1.0 neighbors.
	stdResult := b.heuristic(cands, 4)
	relaxResult := b.adaptiveHeuristic(cands, 4, 2.0)
	assert.GreaterOrEqual(t, len(relaxResult), len(stdResult),
		"relaxed alpha should keep >= neighbors")
}

// TestDistanceCache_MatrixCache verifies matrix cache get/put symmetry.
func TestDistanceCache_MatrixCache(t *testing.T) {
	c := newMatrixCache(10)
	c.put(3, 7, 1.5)
	v, ok := c.get(3, 7)
	require.True(t, ok)
	assert.Equal(t, float32(1.5), v)
	// Symmetric: get(7,3) should also return cached value.
	v, ok = c.get(7, 3)
	require.True(t, ok)
	assert.Equal(t, float32(1.5), v)
	// Uncached returns false.
	_, ok = c.get(1, 2)
	require.False(t, ok)
}

// TestDistanceCache_MapCache verifies map cache get/put symmetry.
func TestDistanceCache_MapCache(t *testing.T) {
	c := newMapCache()
	c.put(100, 200, 3.14)
	v, ok := c.get(100, 200)
	require.True(t, ok)
	assert.Equal(t, float32(3.14), v)
	v, ok = c.get(200, 100)
	require.True(t, ok)
	assert.Equal(t, float32(3.14), v)
}

// TestDistanceCache_AdaptiveSelection verifies that newDistCache
// returns matrix for small sizes and map for large.
func TestDistanceCache_AdaptiveSelection(t *testing.T) {
	mc := newDistCache(256)
	_, isMatrix := mc.(*matrixCache)
	assert.True(t, isMatrix, "256 should use matrixCache")
	mc2 := newDistCache(257)
	_, isMap := mc2.(*mapCache)
	assert.True(t, isMap, "257 should use mapCache")
}

// TestBuildGraphODesc_SmallGraph verifies ODescent produces a valid
// graph on a small dataset and search results are reasonable.
func TestBuildGraphODesc_SmallGraph(t *testing.T) {
	n, dims := 200, 64
	rng := rand.New(rand.NewPCG(42, 1))
	vecs := make([]float32, n*dims)
	for i := range vecs {
		vecs[i] = rng.Float32()*2 - 1
	}
	cfg := Config{M: 8, EFConstruction: 50, EFSearch: 20, Metric: MetricDot, Seed: 42}
	g, err := BuildGraphODesc(cfg, n, dims, vecs)
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, n, g.N())
	assert.Equal(t, dims, g.Dims())

	// Verify search returns results.
	q := make([]float32, dims)
	copy(q, vecs[0:dims])
	hits, err := g.Search(context.Background(), q, 10, 20, nil, index.SearchBudget{})
	require.NoError(t, err)
	assert.NotEmpty(t, hits, "ODescent graph should return search results")
	// The query is vector 0, so it should be in the results.
	found := false
	for _, h := range hits {
		if h.RowID == 0 {
			found = true
		}
	}
	assert.True(t, found, "query vector itself should be in top-10 results")
}

// TestBuildGraphODesc_RecallComparison compares ODescent recall with
// incremental insertion. ODescent should achieve similar recall.
func TestBuildGraphODesc_RecallComparison(t *testing.T) {
	n, dims := 500, 32
	rng := rand.New(rand.NewPCG(42, 1))
	vecs := make([]float32, n*dims)
	for i := range vecs {
		vecs[i] = rng.Float32()*2 - 1
	}
	cfg := Config{M: 8, EFConstruction: 100, EFSearch: 50, Metric: MetricDot, Seed: 42}

	// Build with incremental insertion.
	gInsert, err := BuildGraph(cfg, n, dims, vecs)
	require.NoError(t, err)

	// Build with ODescent.
	gODesc, err := BuildGraphODesc(cfg, n, dims, vecs)
	require.NoError(t, err)

	// Compare recall@10 for 20 queries.
	numQueries := 20
	insertRecall := 0
	odescRecall := 0
	for qi := 0; qi < numQueries; qi++ {
		q := vecs[qi*dims : (qi+1)*dims]
		// Ground truth: brute force top-10.
		type scored struct {
			id  int32
			dot float32
		}
		var truth []scored
		for i := 0; i < n; i++ {
			var dot float32
			for j := 0; j < dims; j++ {
				dot += q[j] * vecs[i*dims+j]
			}
			truth = append(truth, scored{int32(i), dot})
		}
		// Sort descending.
		for i := 1; i < len(truth); i++ {
			for j := i; j > 0 && truth[j].dot > truth[j-1].dot; j-- {
				truth[j], truth[j-1] = truth[j-1], truth[j]
			}
		}
		truthTopK := make(map[int32]bool)
		for i := 0; i < 10 && i < len(truth); i++ {
			truthTopK[truth[i].id] = true
		}
		// Insert graph search.
		hitsInsert, _ := gInsert.Search(context.Background(), q, 10, 50, nil, index.SearchBudget{})
		for _, h := range hitsInsert {
			if truthTopK[h.RowID] {
				insertRecall++
			}
		}
		// ODescent graph search.
		hitsODesc, _ := gODesc.Search(context.Background(), q, 10, 50, nil, index.SearchBudget{})
		for _, h := range hitsODesc {
			if truthTopK[h.RowID] {
				odescRecall++
			}
		}
	}
	insertRate := float64(insertRecall) / float64(numQueries*10)
	odescRate := float64(odescRecall) / float64(numQueries*10)
	t.Logf("Insert recall@10: %.1f%%, ODescent recall@10: %.1f%%",
		insertRate*100, odescRate*100)
	// ODescent should achieve at least 70% of insert recall.
	require.GreaterOrEqual(t, odescRate, insertRate*0.7,
		"ODescent recall should be >= 70%% of insert recall")
}
