// Package hnsw filter.go implements the §2.6 scalar-filter × vector
// unified planner: given an estimated selectivity, decide whether the
// query runs as pre-filter (bitmap-restricted graph traversal), post-
// filter (ANN oversampling + filter + ef×2 retry), or the iterative
// middle档 (M0 simplification: also post-filter, since WAND-style
// traversal is M1+). The plan is produced at query planning time by
// the optimizer (03 §2 R4) and executed by ExecuteFilterPlan below.
package hnsw

import (
	"context"
	"fmt"
	"sort"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/simd"
)

// FilterStrategy enumerates the §2.6 plan strategies.
type FilterStrategy int8

const (
	// StrategyPreFilter restricts graph traversal to rows the scalar
	// filter accepts (§2.6: 标量索引求交得候选位图 → 图遍历仅入
	// 候选堆中位图命中节点). Selected when selectivity < 10%.
	StrategyPreFilter FilterStrategy = iota
	// StrategyPostFilter runs the ANN with an oversampled k and applies
	// the scalar filter to the result heap, retrying with ef×2 when
	// fewer than k rows survive (§2.6: 5×k 过采样 → 过滤 → 不足 k 自动
	// ef×2 重试 ≤2 次). Selected when selectivity > 30%.
	StrategyPostFilter
	// StrategyIterative interleaves WAND-style term pruning with graph
	// traversal (§2.6: WAND 剪枝与图遍历交替). M0 does not implement
	// WAND; the planner degrades Iterative to PostFilter and the
	// executor treats them identically.
	StrategyIterative
)

// SelectivitySource identifies how the EstimatedSelectivity was
// derived. The planner trusts higher-priority sources (§2.6: bitmap
// cardinality → BKD/zone map → mapping cardinality_hint → default
// post-filter) and falls back to conservative post-filter when the
// source is "unknown/default".
type SelectivitySource int8

const (
	// SourceBitmapIndex is the exact-cardinality path from the
	// low-cardinality bitmap index (§6). Highest priority.
	SourceBitmapIndex SelectivitySource = iota
	// SourceBKDZoneMap is the range-estimation path from BKD tree
	// statistics or column zone maps (01 §4.2).
	SourceBKDZoneMap
	// SourceCardinalityHint is the static mapping-declared
	// cardinality_hint (§2.6 "mapping 静态基数提示").
	SourceCardinalityHint
	// SourceDefaultPost is the conservative fallback when no
	// statistics are available: the planner assumes the worst
	// (post-filter with full oversampling).
	SourceDefaultPost
)

// EstimatedSelectivity is the input to PlanVectorFilter. Rate is the
// fraction of rows the scalar filter is expected to match (0..1);
// Source tells the planner whether to trust the estimate.
type EstimatedSelectivity struct {
	Rate   float32
	Source SelectivitySource
}

// FilterPlan is the §2.6 query-time plan. The optimizer (03 §2 R4)
// produces it via PlanVectorFilter and the executor consumes it via
// ExecuteFilterPlan.
type FilterPlan struct {
	Strategy   FilterStrategy
	CandFactor int // post-filter oversample multiplier (default 5×k)
	Budget     index.SearchBudget
}

// PlanVectorFilter decides the §2.6 strategy from an estimated
// selectivity and the requested k. Thresholds: <10% → PreFilter,
// >30% → PostFilter, 10%~30% → Iterative (M0: PostFilter).
//
// When Source is SourceDefaultPost the rate is ignored and the plan
// is conservative PostFilter — matching the design's "保守默认 post-
// filter" fallback (§2.6).
func PlanVectorFilter(sel EstimatedSelectivity, k int) FilterPlan {
	plan := FilterPlan{
		Strategy:   StrategyPostFilter,
		CandFactor:  5,
		Budget:     index.SearchBudget{},
	}
	if sel.Source == SourceDefaultPost {
		return plan
	}
	switch {
	case sel.Rate < 0.10:
		plan.Strategy = StrategyPreFilter
		// Pre-filter does not oversample: the filter is applied during
		// traversal, so the candidate heap already contains only
		// matching rows.
		plan.CandFactor = 1
	case sel.Rate > 0.30:
		plan.Strategy = StrategyPostFilter
		plan.CandFactor = 5
	default:
		// 10%~30%: design calls for Iterative; M0 lacks WAND so we
		// fall back to PostFilter with a smaller oversample.
		plan.Strategy = StrategyPostFilter
		plan.CandFactor = 3
	}
	return plan
}

// ExecuteFilterPlan runs a vector search honoring the §2.6 plan. It is
// the executor side of the planner; the LoadedIndex.Search path stays
// the low-level primitive that both pre- and post-filter compose
// over.
//
// Pre-filter: the q.Filter predicate is forwarded to the graph
// traversal (§2.6 "受限遍历"). When the restricted traversal cannot
// recover k hits — sparse filters can isolate matching nodes behind
// non-matching cuts — the executor falls back to an exact brute-
// force scan over the matching set (O(n_matching · dims); the §2.6
// assumption is that <10% selectivity keeps this affordable). The
// fallback is what makes pre-filter's "结果与先过滤后暴力 TopK 完
// 全一致" guarantee hold in M0.
//
// Post-filter/Iterative (M0): the filter is removed from the graph
// search, the search runs with k×CandFactor oversampling and the
// base ef; the returned hits are then filtered. If fewer than k
// survive AND the search returned the full oversample count (i.e., the
// search was not exhausted), the search retries with ef×2 up to 2
// times — the design's "ef×2 重试 ≤2 次" (§2.6). The best (largest)
// filtered set across retries is returned, so a wide retry never
// discards an earlier better result.
//
// Iterative is treated identically to PostFilter in M0 (no WAND).
func ExecuteFilterPlan(ctx context.Context, li index.LoadedIndex, q index.Query, plan FilterPlan) (*index.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if li == nil {
		return nil, fmt.Errorf("hnsw: ExecuteFilterPlan requires a non-nil LoadedIndex")
	}

	// Pre-filter path: forward q.Filter to the graph. The graph's
	// beam search honors the predicate during traversal (§2.4.4/§2.6
	// "受限遍历"). No oversampling.
	if plan.Strategy == StrategyPreFilter {
		res, err := li.Search(ctx, q, plan.Budget)
		if err != nil {
			return nil, err
		}
		k := q.K
		if k <= 0 {
			k = 10
		}
		if len(res.Hits) >= k {
			return res, nil
		}
		// Restricted traversal could not recover k matching rows.
		// Fall back to an exact brute-force scan over the matching
		// set so pre-filter always matches the "filter-then-brute-
		// force-TopK" baseline (§2.6 spec test). Only the concrete
		// *loadedIndex carries the graph needed for the fallback;
		// foreign implementations return what the graph gave.
		if con, ok := li.(*loadedIndex); ok {
			return bruteForceFiltered(ctx, con, q, k)
		}
		return res, nil
	}

	// Post-filter path: oversample + filter + ef×2 retry (≤2).
	filter := q.Filter
	k := q.K
	if k <= 0 {
		k = 10
	}
	candK := k * plan.CandFactor
	if candK < k {
		candK = k
	}
	ef := q.EFSearch
	if ef <= 0 {
		ef = 64
	}
	const maxRetries = 2
	var best []index.SearchHit
	for retry := 0; retry <= maxRetries; retry++ {
		qc := q
		qc.Filter = nil // don't restrict traversal; we post-filter.
		qc.K = candK
		qc.EFSearch = ef
		res, err := li.Search(ctx, qc, plan.Budget)
		if err != nil {
			return nil, err
		}
		filtered := filterHits(res.Hits, filter)
		if len(filtered) > len(best) {
			best = filtered
		}
		if len(best) >= k {
			break
		}
		// Retry only when the search actually returned the full
		// oversample budget — a short result means the index is
		// smaller than candK, so widening ef cannot add more hits.
		if len(res.Hits) < candK {
			break
		}
		ef *= 2
	}
	if len(best) > k {
		best = best[:k]
	}
	return &index.SearchResult{Hits: best}, nil
}

// bruteForceFiltered is the pre-filter exact fallback: scan every row
// the predicate accepts, score it under the graph's metric, and
// return the top-k by descending score. Used when restricted graph
// traversal cannot recover k matching rows (sparse filters whose
// matching set is topologically scattered). q.Vector must already be
// metric-transformed by the caller (NormalizeQuery); this helper
// applies the same transform defensively in case it was not.
func bruteForceFiltered(ctx context.Context, con *loadedIndex, q index.Query, k int) (*index.SearchResult, error) {
	g := con.g
	if g == nil || g.n == 0 {
		return &index.SearchResult{Hits: nil}, nil
	}
	qv := NormalizeQuery(g.cfg, q.Vector)
	if len(qv) != g.dims {
		return nil, fmt.Errorf("hnsw: bruteForceFiltered query has %d dims, index has %d",
			len(qv), g.dims)
	}
	// Min-heap of size k keyed by ascending score (worst at top so we
	// can pop it when a better candidate arrives).
	type cand struct {
		id int32
		s  float32
	}
	h := make([]cand, 0, k+1)
	push := func(c cand) {
		if len(h) < k {
			h = append(h, c)
			return
		}
		// Replace the worst if c beats it.
		wi := 0
		for i := 1; i < len(h); i++ {
			if h[i].s < h[wi].s {
				wi = i
			}
		}
		if c.s > h[wi].s {
			h[wi] = c
		}
	}
	for i := int32(0); i < int32(g.n); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if q.Filter != nil && !q.Filter(i) {
			continue
		}
		v := g.Vector(i)
		var s float32
		switch g.cfg.Metric {
		case MetricL2:
			s = -simd.L2F32(qv, v)
		default:
			s = simd.DotF32(qv, v)
		}
		push(cand{id: i, s: s})
	}
	// Sort h by descending score.
	sort.Slice(h, func(a, b int) bool { return h[a].s > h[b].s })
	out := &index.SearchResult{Hits: make([]index.SearchHit, len(h))}
	for i, c := range h {
		out.Hits[i] = index.SearchHit{RowID: c.id, Score: c.s}
	}
	return out, nil
}

// filterHits applies the predicate to a hit slice, preserving the
// descending-Score order from the graph search. nil filter returns
// the input verbatim (defensive copy not needed since callers treat
// the slice as read-only).
func filterHits(hits []index.SearchHit, filter func(int32) bool) []index.SearchHit {
	if filter == nil {
		return hits
	}
	out := make([]index.SearchHit, 0, len(hits))
	for _, h := range hits {
		if filter(h.RowID) {
			out = append(out, h)
		}
	}
	return out
}
