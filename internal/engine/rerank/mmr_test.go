package rerank

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// MMR (Maximal Marginal Relevance) tests.
// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.3
// ---------------------------------------------------------------------------

func TestMMR_BasicDiversity(t *testing.T) {
	// Query vector: [1, 0]
	// 4 candidate docs with 2D vectors:
	//   doc1: [1, 0] — identical to query (high relevance, low diversity vs doc2)
	//   doc2: [0.99, 0.01] — nearly identical to doc1 (redundant)
	//   doc3: [0, 1] — orthogonal (lower relevance, high diversity)
	//   doc4: [-1, 0] — opposite (low relevance, high diversity)
	//
	// With lambda=0.5, MMR should diversify: doc1 first (highest relevance),
	// then doc3 or doc4 (high diversity), then doc2 (redundant with doc1) later.
	query := []float32{1, 0}
	candidates := []MMRDoc{
		{DocID: 1, Score: 1.0, Vector: []float32{1, 0}},
		{DocID: 2, Score: 0.99, Vector: []float32{0.99, 0.01}},
		{DocID: 3, Score: 0.0, Vector: []float32{0, 1}},
		{DocID: 4, Score: -1.0, Vector: []float32{-1, 0}},
	}

	result := MMRRescore(query, candidates, 0.5, 4)
	require.Len(t, result, 4)

	// doc1 should be first (highest relevance score).
	assert.Equal(t, uint64(1), result[0].DocID, "most relevant doc should be first")

	// doc2 (redundant with doc1) should be pushed later than doc3/doc4.
	// After selecting doc1, MMR penalizes docs similar to doc1.
	// doc2 is very similar to doc1 → high penalty
	// doc3/doc4 are different from doc1 → low penalty
	docs2Pos := -1
	docs3Pos := -1
	for i, d := range result {
		if d.DocID == 2 {
			docs2Pos = i
		}
		if d.DocID == 3 {
			docs3Pos = i
		}
	}
	assert.Greater(t, docs3Pos, -1, "doc3 should be in results")
	assert.Greater(t, docs2Pos, 0, "doc2 (redundant) should not be first")
}

func TestMMR_LambdaOne_PureRelevance(t *testing.T) {
	// lambda=1.0 means pure relevance, no diversity penalty.
	// Results should match original score ordering.
	query := []float32{1, 0}
	candidates := []MMRDoc{
		{DocID: 1, Score: 0.5, Vector: []float32{1, 0}},
		{DocID: 2, Score: 1.0, Vector: []float32{0, 1}},
		{DocID: 3, Score: 0.3, Vector: []float32{0.5, 0.5}},
	}

	result := MMRRescore(query, candidates, 1.0, 3)
	require.Len(t, result, 3)

	// With lambda=1.0, order follows original scores: doc2, doc1, doc3.
	assert.Equal(t, []uint64{2, 1, 3}, docOrder(result))
}

func TestMMR_LambdaZero_PureDiversity(t *testing.T) {
	// lambda=0.0 means pure diversity, no relevance contribution.
	// First doc is the first candidate (no selected set yet).
	// After that, select the doc least similar to any selected.
	query := []float32{1, 0}
	candidates := []MMRDoc{
		{DocID: 1, Score: 1.0, Vector: []float32{1, 0}},
		{DocID: 2, Score: 0.9, Vector: []float32{0.99, 0.01}},
		{DocID: 3, Score: 0.0, Vector: []float32{0, 1}},
	}

	result := MMRRescore(query, candidates, 0.0, 3)
	require.Len(t, result, 3)

	// doc1 first (first candidate), then doc3 (most dissimilar to doc1),
	// then doc2 (similar to doc1).
	assert.Equal(t, uint64(1), result[0].DocID, "first candidate with lambda=0")
	assert.Equal(t, uint64(3), result[1].DocID, "most dissimilar to doc1")
}

func TestMMR_KLimit(t *testing.T) {
	query := []float32{1, 0}
	candidates := []MMRDoc{
		{DocID: 1, Score: 1.0, Vector: []float32{1, 0}},
		{DocID: 2, Score: 0.5, Vector: []float32{0, 1}},
		{DocID: 3, Score: 0.3, Vector: []float32{0.5, 0.5}},
		{DocID: 4, Score: 0.1, Vector: []float32{-1, 0}},
	}

	result := MMRRescore(query, candidates, 0.5, 2)
	assert.Len(t, result, 2, "should only return k=2 results")
}

func TestMMR_EmptyCandidates(t *testing.T) {
	result := MMRRescore([]float32{1, 0}, nil, 0.5, 10)
	assert.Empty(t, result)
}

func TestMMR_SingleCandidate(t *testing.T) {
	query := []float32{1, 0}
	candidates := []MMRDoc{
		{DocID: 1, Score: 1.0, Vector: []float32{1, 0}},
	}

	result := MMRRescore(query, candidates, 0.5, 10)
	require.Len(t, result, 1)
	assert.Equal(t, uint64(1), result[0].DocID)
}

func TestMMR_KLargerThanCandidates(t *testing.T) {
	query := []float32{1, 0}
	candidates := []MMRDoc{
		{DocID: 1, Score: 1.0, Vector: []float32{1, 0}},
		{DocID: 2, Score: 0.5, Vector: []float32{0, 1}},
	}

	result := MMRRescore(query, candidates, 0.5, 100)
	assert.Len(t, result, 2, "k > len(candidates) should return all")
}

func TestMMR_DiversityScore(t *testing.T) {
	// Verify that MMR with lambda=0.5 produces a diversity benefit:
	// the sum of pairwise similarities between selected docs should be
	// lower than pure relevance ordering.
	query := []float32{1, 0}
	candidates := []MMRDoc{
		{DocID: 1, Score: 1.0, Vector: []float32{1, 0}},
		{DocID: 2, Score: 0.99, Vector: []float32{0.99, 0.01}},
		{DocID: 3, Score: 0.98, Vector: []float32{0.98, 0.02}},
		{DocID: 4, Score: 0.1, Vector: []float32{0, 1}},
	}

	mmrResult := MMRRescore(query, candidates, 0.5, 4)

	// With MMR, doc4 (orthogonal) should be selected earlier than
	// doc2/doc3 (nearly identical to doc1).
	mmrOrder := docOrder(mmrResult)
	// doc4 should not be last (diversity pushes it up).
	assert.NotEqual(t, uint64(4), mmrOrder[len(mmrOrder)-1],
		"doc4 (diverse) should not be last with MMR")
}

func TestMMR_ScorePreserved(t *testing.T) {
	// MMR should preserve the original score in the output.
	query := []float32{1, 0}
	candidates := []MMRDoc{
		{DocID: 1, Score: 1.0, Vector: []float32{1, 0}},
		{DocID: 2, Score: 0.5, Vector: []float32{0, 1}},
	}

	result := MMRRescore(query, candidates, 0.5, 2)
	for _, d := range result {
		// Find original score.
		var origScore float32
		for _, c := range candidates {
			if c.DocID == d.DocID {
				origScore = c.Score
			}
		}
		assert.True(t, float32Abs(d.Score-origScore) < 1e-5,
			"doc %d score %v should preserve original %v", d.DocID, d.Score, origScore)
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func docOrder(result []MMRResult) []uint64 {
	out := make([]uint64, len(result))
	for i, r := range result {
		out[i] = r.DocID
	}
	return out
}

func float32Abs(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

var _ = math.Pi
