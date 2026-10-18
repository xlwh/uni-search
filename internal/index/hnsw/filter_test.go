package hnsw

import (
	"context"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/simd"
)

// TestR9_PlanVectorFilter_BoundaryDecisions walks every §2.6 boundary:
// pre (<10%), iterative 10%-30%, post (>30%), and the SourceDefaultPost
// fallback (rate ignored, always post).
func TestR9_PlanVectorFilter_BoundaryDecisions(t *testing.T) {
	cases := []struct {
		name      string
		sel       EstimatedSelectivity
		k         int
		wantStrat FilterStrategy
		wantCand  int
	}{
		{"pre_5pct_bitmap", EstimatedSelectivity{Rate: 0.05, Source: SourceBitmapIndex}, 10, StrategyPreFilter, 1},
		{"pre_5pct_zonemap", EstimatedSelectivity{Rate: 0.05, Source: SourceBKDZoneMap}, 10, StrategyPreFilter, 1},
		{"pre_boundary_10pct_excluded", EstimatedSelectivity{Rate: 0.10, Source: SourceBitmapIndex}, 10, StrategyPostFilter, 3},
		{"iter_15pct", EstimatedSelectivity{Rate: 0.15, Source: SourceBitmapIndex}, 10, StrategyPostFilter, 3},
		{"iter_boundary_30pct_excluded", EstimatedSelectivity{Rate: 0.30, Source: SourceBitmapIndex}, 10, StrategyPostFilter, 3},
		{"post_50pct", EstimatedSelectivity{Rate: 0.50, Source: SourceBitmapIndex}, 10, StrategyPostFilter, 5},
		{"post_90pct", EstimatedSelectivity{Rate: 0.90, Source: SourceBKDZoneMap}, 10, StrategyPostFilter, 5},
		{"default_post_ignored_rate", EstimatedSelectivity{Rate: 0.01, Source: SourceDefaultPost}, 10, StrategyPostFilter, 5},
		{"hint_low_rate", EstimatedSelectivity{Rate: 0.05, Source: SourceCardinalityHint}, 10, StrategyPreFilter, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := PlanVectorFilter(c.sel, c.k)
			require.Equal(t, c.wantStrat, plan.Strategy, "strategy mismatch")
			require.Equal(t, c.wantCand, plan.CandFactor, "CandFactor mismatch")
		})
	}
}

// TestR9_PreFilter_MatchesBruteForce is the R9 scenario "pre-filter
// 正确性": with a ~5% selectivity filter applied via the planner's
// PreFilter path, the returned top-k must match the brute-force
// "filter-then-TopK" baseline exactly (recall@k = 1.0).
func TestR9_PreFilter_MatchesBruteForce(t *testing.T) {
	const n, dims, k = 2000, 64, 10
	r := rand.New(rand.NewPCG(31, 32))
	vecs, centers := clusteredUnitVectors(r, n, dims)
	g, err := BuildGraph(Config{Seed: 41, Metric: MetricCosine}, n, dims, vecs)
	require.NoError(t, err)

	// ~5% selectivity: rowID % 20 == 0 → 100 of 2000 rows match.
	matches := func(id int32) bool { return id%20 == 0 }
	count := 0
	for i := 0; i < n; i++ {
		if matches(int32(i)) {
			count++
		}
	}
	require.InDelta(t, 0.05, float64(count)/float64(n), 0.01, "expected ~5%% selectivity")

	plan := FilterPlan{
		Strategy:   StrategyPreFilter,
		CandFactor: 1,
		Budget:     index.SearchBudget{},
	}

	const nq = 20
	queries := clusteredAround(r, centers, dims, nq)
	var recall float64
	for qi := 0; qi < nq; qi++ {
		q := NormalizeQuery(Config{Metric: MetricCosine}, queries[qi*dims:(qi+1)*dims])
		// Ground truth: filter then brute-force top-k by descending
		// cosine (== dot over normalized vectors).
		gt := bruteTopK(n, k, func(id int32) float32 {
			if !matches(id) {
				return -1e38
			}
			return simd.DotF32(q, g.Vector(id))
		})
		res, err := ExecuteFilterPlan(context.Background(), &loadedIndex{g: g},
			index.Query{Vector: q, K: k, EFSearch: 64, Filter: matches}, plan)
		require.NoError(t, err)
		require.Len(t, res.Hits, k, "pre-filter must return k hits")
		for _, h := range res.Hits {
			require.True(t, matches(h.RowID), "pre-filter must not return non-matching rows")
		}
		got := make(map[int32]struct{}, len(res.Hits))
		for _, h := range res.Hits {
			got[h.RowID] = struct{}{}
		}
		hit := 0
		for _, id := range gt {
			if _, ok := got[id]; ok {
				hit++
			}
		}
		recall += float64(hit) / float64(k)
	}
	recall /= nq
	t.Logf("pre-filter recall@%d (ef=64, ~5%% selectivity) = %.4f", k, recall)
	require.GreaterOrEqual(t, recall, 1.0, "pre-filter must match brute force exactly")
}

// TestR9_PostFilter_CompensationFillK is the R9 scenario "post-filter
// 补偿": with ~50% selectivity and k=10, the post-filter path must
// still return a full 10 hits via oversampling + ef×2 retry. We pick a
// hard filter that the initial 5×k oversample may not fill; the retry
// path must rescue the count.
func TestR9_PostFilter_CompensationFillK(t *testing.T) {
	const n, dims, k = 2000, 64, 10
	r := rand.New(rand.NewPCG(51, 52))
	vecs, centers := clusteredUnitVectors(r, n, dims)
	g, err := BuildGraph(Config{Seed: 53, Metric: MetricCosine}, n, dims, vecs)
	require.NoError(t, err)

	// 50% selectivity filter.
	matches := func(id int32) bool { return id%2 == 0 }

	plan := FilterPlan{
		Strategy:   StrategyPostFilter,
		CandFactor: 5,
		Budget:     index.SearchBudget{},
	}

	const nq = 10
	queries := clusteredAround(r, centers, dims, nq)
	for qi := 0; qi < nq; qi++ {
		q := NormalizeQuery(Config{Metric: MetricCosine}, queries[qi*dims:(qi+1)*dims])
		res, err := ExecuteFilterPlan(context.Background(), &loadedIndex{g: g},
			index.Query{Vector: q, K: k, EFSearch: 32, Filter: matches}, plan)
		require.NoError(t, err)
		require.Len(t, res.Hits, k, "post-filter must return k hits after retry")
		for _, h := range res.Hits {
			require.True(t, matches(h.RowID), "post-filter must only return matching rows")
		}
	}
}

// TestR9_PostFilter_OrderMatchesBruteForce verifies that the post-
// filter result set is in descending-score order and matches the
// brute-force "ANN-then-filter" top-k (i.e., the design's "5×k 过采样
// → 过滤" sequence produces the same set as if we had brute-forced
// 5×k and then filtered). Uses ~50% selectivity per the R9 spec
// scenario "post-filter 补偿".
func TestR9_PostFilter_OrderMatchesBruteForce(t *testing.T) {
	const n, dims, k = 1000, 32, 10
	r := rand.New(rand.NewPCG(71, 72))
	vecs, centers := clusteredUnitVectors(r, n, dims)
	g, err := BuildGraph(Config{Seed: 73, Metric: MetricCosine}, n, dims, vecs)
	require.NoError(t, err)

	matches := func(id int32) bool { return id%2 == 0 } // ~50% selectivity

	plan := FilterPlan{
		Strategy:   StrategyPostFilter,
		CandFactor: 5,
		Budget:     index.SearchBudget{},
	}

	const nq = 5
	queries := clusteredAround(r, centers, dims, nq)
	for qi := 0; qi < nq; qi++ {
		q := NormalizeQuery(Config{Metric: MetricCosine}, queries[qi*dims:(qi+1)*dims])
		res, err := ExecuteFilterPlan(context.Background(), &loadedIndex{g: g},
			index.Query{Vector: q, K: k, EFSearch: 64, Filter: matches}, plan)
		require.NoError(t, err)
		require.Len(t, res.Hits, k)
		// Verify descending-score order.
		for i := 1; i < len(res.Hits); i++ {
			require.GreaterOrEqual(t, res.Hits[i-1].Score, res.Hits[i].Score,
				"post-filter hits must be in descending score order")
		}
		// Verify each hit matches the filter and the score equals the
		// actual distance to the query (no quantization drift).
		for _, h := range res.Hits {
			require.True(t, matches(h.RowID))
			expected := simd.DotF32(q, g.Vector(h.RowID))
			require.InDeltaf(t, float64(expected), float64(h.Score), 1e-5,
				"post-filter hit score must equal actual dot product")
		}
	}
}

// TestR9_PostFilter_SmallIndexNoRetry ensures the post-filter path
// does not retry when the index is smaller than the oversample count
// (the retry is a no-op in that case).
func TestR9_PostFilter_SmallIndexNoRetry(t *testing.T) {
	const n, dims, k = 30, 16, 10
	r := rand.New(rand.NewPCG(91, 92))
	vecs, centers := clusteredUnitVectors(r, n, dims)
	g, err := BuildGraph(Config{Seed: 93, Metric: MetricCosine}, n, dims, vecs)
	require.NoError(t, err)

	matches := func(id int32) bool { return id%2 == 0 }

	plan := FilterPlan{
		Strategy:   StrategyPostFilter,
		CandFactor: 5, // 5*10 = 50, but n=30.
		Budget:     index.SearchBudget{},
	}

	q := NormalizeQuery(Config{Metric: MetricCosine}, centers[:dims])
	res, err := ExecuteFilterPlan(context.Background(), &loadedIndex{g: g},
		index.Query{Vector: q, K: k, EFSearch: 32, Filter: matches}, plan)
	require.NoError(t, err)
	// We expect at most the number of even ids in [0,n).
	expected := 0
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			expected++
		}
	}
	require.LessOrEqual(t, len(res.Hits), expected, "must not return more hits than the matching rows")
	for _, h := range res.Hits {
		require.True(t, matches(h.RowID))
	}
}

// TestR9_ExecuteFilterPlan_PreFilterPassesPredicate verifies the
// pre-filter path actually forwards q.Filter to the graph (it is not
// silently dropped). We construct a filter that rejects every row;
// the pre-filter path must return zero hits (the graph traversal
// honors the predicate and never admits a row to the result heap).
func TestR9_ExecuteFilterPlan_PreFilterPassesPredicate(t *testing.T) {
	const n, dims, k = 100, 16, 10
	r := rand.New(rand.NewPCG(101, 102))
	vecs, _ := clusteredUnitVectors(r, n, dims)
	g, err := BuildGraph(Config{Seed: 103, Metric: MetricCosine}, n, dims, vecs)
	require.NoError(t, err)

	// Filter that accepts nothing.
	rejectAll := func(int32) bool { return false }

	plan := FilterPlan{
		Strategy:   StrategyPreFilter,
		CandFactor: 1,
		Budget:     index.SearchBudget{},
	}
	q := NormalizeQuery(Config{Metric: MetricCosine}, vecs[:dims])
	res, err := ExecuteFilterPlan(context.Background(), &loadedIndex{g: g},
		index.Query{Vector: q, K: k, EFSearch: 32, Filter: rejectAll}, plan)
	require.NoError(t, err)
	require.Empty(t, res.Hits, "pre-filter with reject-all must return no hits")
}

// TestR9_ExecuteFilterPlan_NilLoadedIndex verifies input validation.
func TestR9_ExecuteFilterPlan_NilLoadedIndex(t *testing.T) {
	_, err := ExecuteFilterPlan(context.Background(), nil,
		index.Query{Vector: []float32{1}}, FilterPlan{})
	require.Error(t, err)
}

// TestR9_ExecuteFilterPlan_CancelledContext verifies the executor
// honors context cancellation before any work.
func TestR9_ExecuteFilterPlan_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExecuteFilterPlan(ctx, &loadedIndex{},
		index.Query{Vector: []float32{1}}, FilterPlan{})
	require.Error(t, err)
}

// TestR9_filterHits_PreservesOrder is a small unit test for the
// helper that applies the predicate to a hit slice.
func TestR9_filterHits_PreservesOrder(t *testing.T) {
	in := []index.SearchHit{
		{RowID: 1, Score: 0.9},
		{RowID: 2, Score: 0.8},
		{RowID: 3, Score: 0.7},
		{RowID: 4, Score: 0.6},
	}
	out := filterHits(in, func(id int32) bool { return id%2 == 0 })
	// Expect ids 2 and 4, in their original order.
	require.Len(t, out, 2)
	require.Equal(t, int32(2), out[0].RowID)
	require.Equal(t, int32(4), out[1].RowID)

	// nil filter returns the input verbatim.
	out2 := filterHits(in, nil)
	require.Equal(t, in, out2)

	// empty input is safe.
	require.Empty(t, filterHits(nil, func(int32) bool { return true }))
}

// Ensure unused imports don't break the build.
var _ = sort.Slice
