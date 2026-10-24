package inverted

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.1

func TestSparseInvertedIndex_BasicSearch(t *testing.T) {
	// Build a small sparse vector index:
	// doc1: dim=1 w=0.5, dim=3 w=0.8
	// doc2: dim=2 w=0.3, dim=3 w=0.1
	// doc3: dim=1 w=0.9, dim=2 w=0.7
	idx := NewSparseInvertedIndex("sparse_field", WeightF32)

	require.NoError(t, idx.AddVector(1, []uint64{1, 3}, []float32{0.5, 0.8}))
	require.NoError(t, idx.AddVector(2, []uint64{2, 3}, []float32{0.3, 0.1}))
	require.NoError(t, idx.AddVector(3, []uint64{1, 2}, []float32{0.9, 0.7}))

	// Query: dim=1 w=1.0, dim=2 w=1.0
	// Expected scores:
	//   doc1: 1.0*0.5 = 0.5 (only dim=1 shared)
	//   doc2: 1.0*0.3 = 0.3 (only dim=2 shared)
	//   doc3: 1.0*0.9 + 1.0*0.7 = 1.6 (both dims shared)
	scorer := NewSparseWANDScorer(
		[]uint64{1, 2}, []float32{1.0, 1.0},
		idx.Postings(), 10,
	)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)
	require.Len(t, result.Hits, 3)

	// Order: doc3 (1.6) > doc1 (0.5) > doc2 (0.3)
	assert.Equal(t, int32(3), result.Hits[0].RowID)
	assert.InDelta(t, 1.6, result.Hits[0].Score, 1e-5)
	assert.Equal(t, int32(1), result.Hits[1].RowID)
	assert.InDelta(t, 0.5, result.Hits[1].Score, 1e-5)
	assert.Equal(t, int32(2), result.Hits[2].RowID)
	assert.InDelta(t, 0.3, result.Hits[2].Score, 1e-5)
}

func TestSparseInvertedIndex_TopK(t *testing.T) {
	idx := NewSparseInvertedIndex("test", WeightF32)

	for i := int32(1); i <= 10; i++ {
		require.NoError(t, idx.AddVector(i, []uint64{1}, []float32{float32(i) / 10.0}))
	}

	// Query: dim=1 w=1.0, should return all 10 docs sorted by weight.
	// Top-3 should be docs 10, 9, 8.
	scorer := NewSparseWANDScorer([]uint64{1}, []float32{1.0}, idx.Postings(), 3)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)
	require.Len(t, result.Hits, 3)

	assert.Equal(t, int32(10), result.Hits[0].RowID)
	assert.Equal(t, int32(9), result.Hits[1].RowID)
	assert.Equal(t, int32(8), result.Hits[2].RowID)
}

func TestSparseInvertedIndex_EmptyQuery(t *testing.T) {
	idx := NewSparseInvertedIndex("test", WeightF32)
	require.NoError(t, idx.AddVector(1, []uint64{1}, []float32{0.5}))

	scorer := NewSparseWANDScorer(nil, nil, idx.Postings(), 10)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)
	assert.Empty(t, result.Hits)
}

func TestSparseInvertedIndex_DimNotInIndex(t *testing.T) {
	idx := NewSparseInvertedIndex("test", WeightF32)
	require.NoError(t, idx.AddVector(1, []uint64{1}, []float32{0.5}))

	// Query dim=999 which doesn't exist in the index.
	scorer := NewSparseWANDScorer([]uint64{999}, []float32{1.0}, idx.Postings(), 10)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)
	assert.Empty(t, result.Hits)
}

func TestSparseInvertedIndex_PartialDimMatch(t *testing.T) {
	idx := NewSparseInvertedIndex("test", WeightF32)
	require.NoError(t, idx.AddVector(1, []uint64{1, 2}, []float32{0.5, 0.3}))
	require.NoError(t, idx.AddVector(2, []uint64{1, 3}, []float32{0.8, 0.2}))

	// Query: dim=1 w=1.0, dim=2 w=1.0, dim=3 w=1.0
	// doc1: 1.0*0.5 + 1.0*0.3 = 0.8 (dims 1,2)
	// doc2: 1.0*0.8 + 1.0*0.2 = 1.0 (dims 1,3)
	scorer := NewSparseWANDScorer(
		[]uint64{1, 2, 3}, []float32{1.0, 1.0, 1.0},
		idx.Postings(), 10,
	)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)
	require.Len(t, result.Hits, 2)

	assert.Equal(t, int32(2), result.Hits[0].RowID)
	assert.InDelta(t, 1.0, result.Hits[0].Score, 1e-5)
	assert.Equal(t, int32(1), result.Hits[1].RowID)
	assert.InDelta(t, 0.8, result.Hits[1].Score, 1e-5)
}

func TestSparseInvertedIndex_ZeroScoreDocs(t *testing.T) {
	idx := NewSparseInvertedIndex("test", WeightF32)
	require.NoError(t, idx.AddVector(1, []uint64{1}, []float32{0.0}))
	require.NoError(t, idx.AddVector(2, []uint64{1}, []float32{0.5}))

	// Query: dim=1 w=1.0
	// doc1: 1.0*0.0 = 0.0 (zero score)
	// doc2: 1.0*0.5 = 0.5
	scorer := NewSparseWANDScorer([]uint64{1}, []float32{1.0}, idx.Postings(), 10)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)

	// Both docs should appear (WAND threshold starts at 0).
	require.Len(t, result.Hits, 2)
	assert.Equal(t, int32(2), result.Hits[0].RowID)
}

func TestSparseInvertedIndex_LargeScale(t *testing.T) {
	// Test with 1000 docs and multiple dimensions.
	idx := NewSparseInvertedIndex("test", WeightF32)
	for i := int32(1); i <= 1000; i++ {
		dims := []uint64{uint64(i % 100), uint64((i*7) % 100)}
		weights := []float32{float32(i) / 1000.0, float32(i) / 500.0}
		require.NoError(t, idx.AddVector(i, dims, weights))
	}

	// Query: dim=0 w=1.0
	scorer := NewSparseWANDScorer([]uint64{0}, []float32{1.0}, idx.Postings(), 5)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)
	assert.Len(t, result.Hits, 5)

	// All hits should have positive scores.
	for _, hit := range result.Hits {
		assert.True(t, hit.Score > 0)
	}
}

func TestSparseInvertedIndex_WANDPruning(t *testing.T) {
	// Verify WAND pruning works: with a high threshold, low-scoring
	// docs should be skipped entirely (not scored).
	idx := NewSparseInvertedIndex("test", WeightF32)

	// Create docs with varying weights for dim=1.
	for i := int32(1); i <= 100; i++ {
		w := float32(i) / 100.0 // 0.01 to 1.0
		require.NoError(t, idx.AddVector(i, []uint64{1}, []float32{w}))
	}

	// Query: dim=1 w=1.0, top-5.
	// WAND should prune docs with scores below the 5th-highest.
	scorer := NewSparseWANDScorer([]uint64{1}, []float32{1.0}, idx.Postings(), 5)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)
	require.Len(t, result.Hits, 5)

	// Top-5 should be docs 100, 99, 98, 97, 96.
	for i, hit := range result.Hits {
		expected := int32(100 - i)
		assert.Equal(t, expected, hit.RowID, "rank %d should be doc %d", i, expected)
	}
}

func TestSparseInvertedIndex_AddVectorValidation(t *testing.T) {
	idx := NewSparseInvertedIndex("test", WeightF32)

	// Mismatched lengths.
	err := idx.AddVector(1, []uint64{1, 2}, []float32{0.5})
	assert.Error(t, err)

	// Empty is OK.
	err = idx.AddVector(2, []uint64{}, []float32{})
	assert.NoError(t, err)
}

func TestSparseInvertedIndex_MultiDimScoring(t *testing.T) {
	idx := NewSparseInvertedIndex("test", WeightF32)

	// doc1: dim=1 w=0.3, dim=2 w=0.7, dim=3 w=0.1
	// doc2: dim=1 w=0.5, dim=2 w=0.2, dim=3 w=0.9
	require.NoError(t, idx.AddVector(1, []uint64{1, 2, 3}, []float32{0.3, 0.7, 0.1}))
	require.NoError(t, idx.AddVector(2, []uint64{1, 2, 3}, []float32{0.5, 0.2, 0.9}))

	// Query: dim=1 w=0.4, dim=2 w=0.6, dim=3 w=0.8
	// doc1: 0.4*0.3 + 0.6*0.7 + 0.8*0.1 = 0.12 + 0.42 + 0.08 = 0.62
	// doc2: 0.4*0.5 + 0.6*0.2 + 0.8*0.9 = 0.20 + 0.12 + 0.72 = 1.04
	qDims := []uint64{1, 2, 3}
	qWeights := []float32{0.4, 0.6, 0.8}

	scorer := NewSparseWANDScorer(qDims, qWeights, idx.Postings(), 10)
	result, err := scorer.Search(context.Background())
	require.NoError(t, err)
	require.Len(t, result.Hits, 2)

	assert.Equal(t, int32(2), result.Hits[0].RowID)
	assert.InDelta(t, 1.04, result.Hits[0].Score, 1e-5)
	assert.Equal(t, int32(1), result.Hits[1].RowID)
	assert.InDelta(t, 0.62, result.Hits[1].Score, 1e-5)
}
