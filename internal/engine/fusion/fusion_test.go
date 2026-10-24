package fusion

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	"github.com/unisearch-db/unisearch/internal/engine/exec"
)

// ---------------------------------------------------------------------------
// Spec R16 scenario 1: RRF correctness. Two paths each return ordered
// documents; the fused score MUST equal the hand-computed
// Σ_i 1/(k + rank_i(d)) and the output MUST be sorted by descending
// score with the smaller-DocID tie-break.
// ---------------------------------------------------------------------------

func TestR16_RRFCorrectness(t *testing.T) {
	// Path A: [doc1, doc2, doc3] (ranks 0,1,2)
	// Path B: [doc2, doc1, doc4] (ranks 0,1,2)
	// k = 60 (default)
	// Expected scores:
	//   doc1: 1/60 + 1/61  = 0.016667 + 0.016393 = 0.033060
	//   doc2: 1/61 + 1/60  = 0.016393 + 0.016667 = 0.033060 (tie with doc1 → smaller DocID wins)
	//   doc3: 1/62         = 0.016129
	//   doc4: 1/62         = 0.016129
	pathA := []ScoredDoc{{DocID: 1}, {DocID: 2}, {DocID: 3}}
	pathB := []ScoredDoc{{DocID: 2}, {DocID: 1}, {DocID: 4}}

	f, err := NewFusion(dsl.RankSpec{Method: dsl.RankMethodRRF})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA, pathB})
	require.NoError(t, err)
	require.Len(t, merged, 4)

	// Hand-computed scores.
	const k = 60.0
	want := map[uint64]float32{
		1: float32(1.0/(k+0) + 1.0/(k+1)),
		2: float32(1.0/(k+1) + 1.0/(k+0)),
		3: float32(1.0 / (k + 2)),
		4: float32(1.0 / (k + 2)),
	}
	for _, d := range merged {
		assert.True(t, float32Abs(d.Score-want[d.DocID]) < 1e-5,
			"doc %d score %v want %v", d.DocID, d.Score, want[d.DocID])
	}

	// Sorted descending by score; ties (doc1 vs doc2, doc3 vs doc4)
	// broken by smaller DocID.
	assert.Equal(t, []uint64{1, 2, 3, 4}, docIDOrder(merged),
		"ties broken by smaller DocID")
}

// ---------------------------------------------------------------------------
// Spec R16 scenario 2: Pagination cap. from+size ≤ 10000 enforced;
// from=9991, size=20 (sum=10011) returns ErrFromSizeTooLarge;
// from+size=10000 is the legal boundary.
// ---------------------------------------------------------------------------

func TestR16_PaginationCap(t *testing.T) {
	// Over-cap: 9991 + 20 = 10011 > 10000 → reject.
	err := ValidateFromSize(9991, 20)
	assert.ErrorIs(t, err, exec.ErrFromSizeTooLarge)

	// Exactly at cap: 5000 + 5000 = 10000 → OK.
	err = ValidateFromSize(5000, 5000)
	assert.NoError(t, err)

	// Same cap enforced by TopNOp at construction time.
	_, err = exec.NewTopN(nil, 9991, 20)
	assert.ErrorIs(t, err, exec.ErrFromSizeTooLarge)
}

// ---------------------------------------------------------------------------
// Extra: weighted fusion math. Per-path min-max normalization then
// Σ w_i · ŝ_i(d); equal-weight paths share 1/N weight each.
// ---------------------------------------------------------------------------

func TestR16_WeightedCorrectness(t *testing.T) {
	// Path A raw scores: doc1=0.0 (min), doc2=0.5, doc3=1.0 (max)
	// Normalized A:      doc1=0.0, doc2=0.5, doc3=1.0
	// Path B raw scores: doc2=2.0 (min), doc3=4.0, doc4=6.0 (max)
	// Normalized B:      doc2=0.0, doc3=0.5, doc4=1.0
	// Equal weights (wA=wB=0.5):
	//   doc1: 0.5 * 0.0         = 0.0
	//   doc2: 0.5 * 0.5 + 0.5 * 0.0 = 0.25
	//   doc3: 0.5 * 1.0 + 0.5 * 0.5 = 0.75
	//   doc4: 0.5 * 0.0 + 0.5 * 1.0 = 0.5
	pathA := []ScoredDoc{{DocID: 1, Score: 0.0}, {DocID: 2, Score: 0.5}, {DocID: 3, Score: 1.0}}
	pathB := []ScoredDoc{{DocID: 2, Score: 2.0}, {DocID: 3, Score: 4.0}, {DocID: 4, Score: 6.0}}

	f, err := NewFusion(dsl.RankSpec{Method: dsl.RankMethodWeighted})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA, pathB})
	require.NoError(t, err)
	require.Len(t, merged, 4)

	want := map[uint64]float32{
		1: 0.0,
		2: 0.25,
		3: 0.75,
		4: 0.5,
	}
	for _, d := range merged {
		assert.True(t, float32Abs(d.Score-want[d.DocID]) < 1e-5,
			"doc %d score %v want %v", d.DocID, d.Score, want[d.DocID])
	}
	assert.Equal(t, []uint64{3, 4, 2, 1}, docIDOrder(merged),
		"desc score → doc3, doc4, doc2, doc1")
}

// ---------------------------------------------------------------------------
// Extra: weighted fusion with explicit weights — wA=0.7, wB=0.3.
// ---------------------------------------------------------------------------

func TestR16_WeightedExplicitWeights(t *testing.T) {
	pathA := []ScoredDoc{{DocID: 1, Score: 0.0}, {DocID: 2, Score: 0.5}, {DocID: 3, Score: 1.0}}
	pathB := []ScoredDoc{{DocID: 2, Score: 2.0}, {DocID: 3, Score: 4.0}, {DocID: 4, Score: 6.0}}

	f, err := NewFusion(dsl.RankSpec{
		Method:  dsl.RankMethodWeighted,
		Weights: []float64{0.7, 0.3},
	})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA, pathB})
	require.NoError(t, err)

	want := map[uint64]float32{
		1: 0.0,
		2: float32(0.7*0.5 + 0.3*0.0),
		3: float32(0.7*1.0 + 0.3*0.5),
		4: float32(0.7*0.0 + 0.3*1.0),
	}
	for _, d := range merged {
		assert.True(t, float32Abs(d.Score-want[d.DocID]) < 1e-5,
			"doc %d score %v want %v", d.DocID, d.Score, want[d.DocID])
	}
}

// ---------------------------------------------------------------------------
// Extra: WeightedFusion rejects weight/path count mismatch.
// ---------------------------------------------------------------------------

func TestR16_WeightedMismatch(t *testing.T) {
	f, err := NewFusion(dsl.RankSpec{
		Method:  dsl.RankMethodWeighted,
		Weights: []float64{0.5, 0.3, 0.2}, // 3 weights
	})
	require.NoError(t, err)

	_, err = f.Merge([][]ScoredDoc{{{DocID: 1}}, {{DocID: 1}}}) // 2 paths
	assert.ErrorIs(t, err, ErrWeightsMismatch)
}

// ---------------------------------------------------------------------------
// Extra: WindowSize truncation. Each path is capped to Window entries
// before fusion so deep ranks do not contribute.
// ---------------------------------------------------------------------------

func TestR16_WindowSizeTruncation(t *testing.T) {
	// Path A has 100 docs; window=3 → only ranks 0,1,2 contribute.
	pathA := make([]ScoredDoc, 100)
	for i := range pathA {
		pathA[i] = ScoredDoc{DocID: uint64(i + 1)}
	}
	pathB := []ScoredDoc{{DocID: 50}, {DocID: 1}, {DocID: 200}}

	f, err := NewFusion(dsl.RankSpec{
		Method:     dsl.RankMethodRRF,
		WindowSize: 3,
	})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA, pathB})
	require.NoError(t, err)

	// pathA truncated to [doc1, doc2, doc3] (window=3).
	// pathB = [doc50, doc1, doc200] (ranks 0,1,2).
	//
	// doc1:  pathA rank 0 + pathB rank 1 → 1/60 + 1/61
	// doc2:  pathA rank 1 only           → 1/61
	// doc3:  pathA rank 2 only           → 1/62
	// doc50: pathA rank 49 (truncated) + pathB rank 0 → 1/60
	// doc200:pathB rank 2 only           → 1/62
	scores := map[uint64]float32{}
	for _, d := range merged {
		scores[d.DocID] = d.Score
	}
	const k = 60.0
	want := map[uint64]float32{
		1:   float32(1.0/(k+0) + 1.0/(k+1)),
		2:   float32(1.0 / (k + 1)),
		3:   float32(1.0 / (k + 2)),
		50:  float32(1.0 / (k + 0)),
		200: float32(1.0 / (k + 2)),
	}
	for doc, w := range want {
		assert.True(t, float32Abs(scores[doc]-w) < 1e-5,
			"doc %d score %v want %v", doc, scores[doc], w)
	}
	// Doc 4 must NOT appear: path A rank 3 truncated (window=3); not in path B.
	_, present := scores[4]
	assert.False(t, present, "doc4 should be truncated out by window=3")
}

// ---------------------------------------------------------------------------
// Extra: relative_score / rerank methods return ErrUnsupportedMethod.
// ---------------------------------------------------------------------------

func TestR16_UnsupportedMethod(t *testing.T) {
	// relative_score is now supported (deep-gap-remediation).
	_, err := NewFusion(dsl.RankSpec{Method: "relative_score"})
	assert.NoError(t, err)

	// rerank (external HTTP) is still unsupported.
	_, err = NewFusion(dsl.RankSpec{Method: "rerank"})
	assert.ErrorIs(t, err, ErrUnsupportedMethod)
}

// ---------------------------------------------------------------------------
// DBSF (Distribution-Based Score Fusion) tests.
// @spec: ./archive/2026-10-23-qdrant-features-import/02_spec.md#2.2
// ---------------------------------------------------------------------------

func TestDBSF_Correctness(t *testing.T) {
	// Path A: doc1=0.0, doc2=0.5, doc3=1.0 → mean=0.5, var=1/6, stddev≈0.4082
	// Path B: doc2=2.0, doc3=4.0, doc4=6.0 → mean=4.0, var=8/3, stddev≈1.6330
	// k=3.0, equal weights (0.5, 0.5)
	//
	// z-A: doc1=(0.0-0.5)/(3*0.4082)≈-0.4082, doc2=0.0, doc3≈0.4082
	// z-B: doc2=(2.0-4.0)/(3*1.6330)≈-0.4082, doc3=0.0, doc4≈0.4082
	//
	// final (equal weights 0.5):
	//   doc1: 0.5*(-0.4082)+0           = -0.2041
	//   doc2: 0.5*0.0+0.5*(-0.4082)     = -0.2041
	//   doc3: 0.5*0.4082+0.5*0.0        =  0.2041
	//   doc4: 0+0.5*0.4082              =  0.2041
	//
	// Sorted desc (ties by smaller DocID): doc3, doc4, doc1, doc2
	pathA := []ScoredDoc{{DocID: 1, Score: 0.0}, {DocID: 2, Score: 0.5}, {DocID: 3, Score: 1.0}}
	pathB := []ScoredDoc{{DocID: 2, Score: 2.0}, {DocID: 3, Score: 4.0}, {DocID: 4, Score: 6.0}}

	f, err := NewFusion(dsl.RankSpec{Method: dsl.RankMethodDBSF})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA, pathB})
	require.NoError(t, err)
	require.Len(t, merged, 4)

	// doc3 and doc4 should have the highest (positive) scores.
	assert.True(t, merged[0].Score > 0, "top result should have positive score")
	assert.True(t, merged[1].Score > 0, "second result should have positive score")
	// doc1 and doc2 should have the lowest (negative) scores.
	assert.True(t, merged[2].Score < 0, "third result should have negative score")
	assert.True(t, merged[3].Score < 0, "fourth result should have negative score")
	// Ties broken by smaller DocID.
	assert.Equal(t, []uint64{3, 4, 1, 2}, docIDOrder(merged))
}

func TestDBSF_ExplicitWeights(t *testing.T) {
	pathA := []ScoredDoc{{DocID: 1, Score: 0.0}, {DocID: 2, Score: 0.5}, {DocID: 3, Score: 1.0}}
	pathB := []ScoredDoc{{DocID: 2, Score: 2.0}, {DocID: 3, Score: 4.0}, {DocID: 4, Score: 6.0}}

	f, err := NewFusion(dsl.RankSpec{
		Method:  dsl.RankMethodDBSF,
		Weights: []float64{0.7, 0.3},
	})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA, pathB})
	require.NoError(t, err)
	require.Len(t, merged, 4)

	// With weight 0.7 on A: doc3 (high in A) should rank first.
	assert.Equal(t, uint64(3), merged[0].DocID, "doc3 should rank first with 0.7 weight on path A")
}

func TestDBSF_WeightMismatch(t *testing.T) {
	f, err := NewFusion(dsl.RankSpec{
		Method:  dsl.RankMethodDBSF,
		Weights: []float64{0.5, 0.3, 0.2}, // 3 weights for 2 paths
	})
	require.NoError(t, err)

	_, err = f.Merge([][]ScoredDoc{
		{{DocID: 1, Score: 1.0}},
		{{DocID: 2, Score: 2.0}},
	})
	assert.ErrorIs(t, err, ErrWeightsMismatch)
}

func TestDBSF_SinglePath(t *testing.T) {
	// Single path: DBSF should still produce z-score normalized results.
	pathA := []ScoredDoc{{DocID: 1, Score: 0.0}, {DocID: 2, Score: 1.0}, {DocID: 3, Score: 2.0}}

	f, err := NewFusion(dsl.RankSpec{Method: dsl.RankMethodDBSF})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA})
	require.NoError(t, err)
	require.Len(t, merged, 3)

	// mean=1.0, stddev≈0.8165, k=3.0
	// z: doc1=(0-1)/(3*0.8165)≈-0.4082, doc2=0, doc3≈0.4082
	// doc3 should be first (highest z-score).
	assert.Equal(t, uint64(3), merged[0].DocID)
	assert.Equal(t, uint64(2), merged[1].DocID)
	assert.Equal(t, uint64(1), merged[2].DocID)
}

func TestDBSF_AllSameScores(t *testing.T) {
	// All same scores → stddev=0 → z-score=0 for all → equal scores.
	pathA := []ScoredDoc{{DocID: 1, Score: 5.0}, {DocID: 2, Score: 5.0}, {DocID: 3, Score: 5.0}}

	f, err := NewFusion(dsl.RankSpec{Method: dsl.RankMethodDBSF})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA})
	require.NoError(t, err)
	require.Len(t, merged, 3)

	// All scores should be 0 (z-score of a constant is 0).
	for _, d := range merged {
		assert.True(t, float32Abs(d.Score) < 1e-5, "constant scores should normalize to 0, got %v", d.Score)
	}
}

func TestDBSF_EmptyPaths(t *testing.T) {
	f, err := NewFusion(dsl.RankSpec{Method: dsl.RankMethodDBSF})
	require.NoError(t, err)

	merged, err := f.Merge(nil)
	require.NoError(t, err)
	assert.Empty(t, merged)
}

func TestDBSF_WindowTruncation(t *testing.T) {
	// Window=2: only first 2 docs per path contribute to statistics and fusion.
	pathA := []ScoredDoc{{DocID: 1, Score: 0.0}, {DocID: 2, Score: 10.0}, {DocID: 3, Score: 100.0}}

	f, err := NewFusion(dsl.RankSpec{Method: dsl.RankMethodDBSF, WindowSize: 2})
	require.NoError(t, err)

	merged, err := f.Merge([][]ScoredDoc{pathA})
	require.NoError(t, err)
	require.Len(t, merged, 2, "only 2 docs should survive window truncation")

	// doc3 (score=100) should be truncated out.
	for _, d := range merged {
		assert.NotEqual(t, uint64(3), d.DocID, "doc3 should be truncated")
	}
}

func TestDBSF_RobustnessVsWeighted(t *testing.T) {
	// Key property of DBSF: a consistently good document (high in both
	// paths) should rank higher than a document that's only high in one
	// path, even if the latter has a higher raw score.
	// Path A: doc1=0.0, doc2=1.0, doc3=2.0, doc4=3.0 (doc4 highest in A)
	// Path B: doc1=5.0, doc2=6.0, doc3=7.0 (doc3 highest in B, doc4 absent)
	//
	// DBSF should rank doc3 first (consistently high across paths),
	// while doc4 (high only in A, absent in B) ranks second.
	pathA := []ScoredDoc{
		{DocID: 1, Score: 0.0},
		{DocID: 2, Score: 1.0},
		{DocID: 3, Score: 2.0},
		{DocID: 4, Score: 3.0},
	}
	pathB := []ScoredDoc{
		{DocID: 1, Score: 5.0},
		{DocID: 2, Score: 6.0},
		{DocID: 3, Score: 7.0},
	}

	dbsf, err := NewFusion(dsl.RankSpec{Method: dsl.RankMethodDBSF})
	require.NoError(t, err)
	dbsfMerged, err := dbsf.Merge([][]ScoredDoc{pathA, pathB})
	require.NoError(t, err)

	dbsfOrder := docIDOrder(dbsfMerged)

	// doc3 (consistently high in both paths) should rank first.
	assert.Equal(t, uint64(3), dbsfOrder[0], "consistently good doc3 should rank first in DBSF")
	// doc4 (high only in A, absent from B) should rank second, not first.
	assert.Equal(t, uint64(4), dbsfOrder[1], "doc4 should rank second (high in A only)")
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// docIDOrder returns the DocIDs of merged in their returned order.
func docIDOrder(merged []ScoredDoc) []uint64 {
	out := make([]uint64, len(merged))
	for i, d := range merged {
		out[i] = d.DocID
	}
	return out
}

// float32Abs returns |x|.
func float32Abs(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

// Silence the unused math import warning in case the helper above is
// refactored to drop the constant dependency.
var _ = math.Pi
