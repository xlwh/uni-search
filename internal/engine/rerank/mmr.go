// Package rerank mmr.go implements Maximal Marginal Relevance (MMR)
// diversity re-ranking. MMR balances query relevance against result
// diversity by iteratively selecting the document that maximizes:
//
//	MMR(d) = λ · sim(q, d) − (1−λ) · max_{s ∈ S} sim(d, s)
//
// where λ (lambda) controls the relevance/diversity trade-off:
//   λ=1.0 → pure relevance (original score ordering)
//   λ=0.0 → pure diversity (maximize dissimilarity to selected set)
//
// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.3
package rerank

import (
	"sort"

	"github.com/unisearch-db/unisearch/internal/simd"
)

// MMRDoc is one candidate document for MMR re-ranking.
type MMRDoc struct {
	DocID  uint64    // GlobalDocID (SegmentID<<32 | RowID)
	Score  float32   // Original retrieval score (BM25 / cosine / etc.)
	Vector []float32 // Document vector for similarity computation
}

// MMRResult is one re-ranked document.
type MMRResult struct {
	DocID uint64
	Score float32 // Preserved original score (not the MMR score)
}

// MMRRescore applies Maximal Marginal Relevance re-ranking to the
// candidate documents. It iteratively selects k documents, each time
// choosing the document with the highest MMR score that balances
// query relevance against similarity to already-selected documents.
//
// Parameters:
//   - queryVec: the query vector (for relevance computation)
//   - candidates: candidate documents with vectors and original scores
//   - lambda: trade-off parameter (0=diversity, 1=relevance, typical 0.5)
//   - k: number of documents to return
//
// When candidates have no vector (len(Vector)==0), MMR degrades to
// pure relevance ordering (lambda is effectively 1.0).
func MMRRescore(queryVec []float32, candidates []MMRDoc, lambda float64, k int) []MMRResult {
	if len(candidates) == 0 {
		return nil
	}
	if k <= 0 || k > len(candidates) {
		k = len(candidates)
	}

	n := len(candidates)
	selected := make([]bool, n)
	result := make([]MMRResult, 0, k)

	// Relevance term uses the original retrieval score (BM25 / cosine / etc.).
	// Document vectors are used only for the diversity term (doc-doc similarity).
	hasVectors := len(queryVec) > 0
	for _, c := range candidates {
		if len(c.Vector) > 0 {
			hasVectors = true
			break
		}
	}

	// Precompute pairwise document similarities for diversity term.
	// Only needed when lambda < 1.0 and vectors are available.
	var docSims [][]float32
	if lambda < 1.0 && hasVectors {
		docSims = make([][]float32, n)
		for i := range docSims {
			docSims[i] = make([]float32, n)
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if len(candidates[i].Vector) > 0 && len(candidates[i].Vector) == len(candidates[j].Vector) {
					s := simd.DotF32(candidates[i].Vector, candidates[j].Vector)
					docSims[i][j] = s
					docSims[j][i] = s
				}
			}
		}
	}

	// Iterative selection.
	for len(result) < k {
		bestIdx := -1
		bestMMR := float32(-1e30)

		for i := 0; i < n; i++ {
			if selected[i] {
				continue
			}

			// Relevance term: λ · original_score(d)
			relevance := float32(lambda) * candidates[i].Score

			// Diversity term: (1−λ) · max_{s ∈ S} sim(d, s)
			var maxSim float32
			if docSims != nil {
				for j := 0; j < n; j++ {
					if selected[j] && docSims[i][j] > maxSim {
						maxSim = docSims[i][j]
					}
				}
			}
			diversity := float32(1.0-lambda) * maxSim

			mmrScore := relevance - diversity
			if mmrScore > bestMMR {
				bestMMR = mmrScore
				bestIdx = i
			}
		}

		if bestIdx < 0 {
			break
		}

		selected[bestIdx] = true
		result = append(result, MMRResult{
			DocID: candidates[bestIdx].DocID,
			Score: candidates[bestIdx].Score,
		})
	}

	return result
}

// MMRConfig configures MMR re-ranking from the DSL.
type MMRConfig struct {
	Enabled bool
	Lambda  float64 // 0=diversity, 1=relevance, default 0.5
}

// DefaultMMRLambda is the default MMR lambda trade-off.
const DefaultMMRLambda = 0.5

// SortMMRByScore sorts MMR results by descending original score.
// This is used when MMR is disabled (lambda=1.0) to preserve the
// original ranking.
func SortMMRByScore(results []MMRResult) {
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].DocID < results[j].DocID
	})
}
