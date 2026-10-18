package hnsw

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/unisearch-db/unisearch/internal/index"
)

// searchBudget aliases the registry-level budget consumed by the
// search paths.
type searchBudget = index.SearchBudget

// NormalizeQuery applies the query-side transform matching the metric
// (§2.2): unit normalization for cosine (zero vectors pass through
// unchanged), identity for dot and l2. Always returns a fresh copy.
func NormalizeQuery(cfg Config, q []float32) []float32 {
	out := make([]float32, len(q))
	if cfg.resolve().Metric == MetricCosine {
		normalizeInto(out, q)
		return out
	}
	copy(out, q)
	return out
}

// searchState bundles per-search scratch: the visit-epoch searcher and
// the 16*dims gather buffer for DotF32Batch16. Recycled through a
// package-level pool; Graph is immutable so no extra sync is needed.
type searchState struct {
	s     *layerSearcher
	batch []float32
}

var statePool sync.Pool // of *searchState

func (g *Graph) getState() *searchState {
	if v, ok := statePool.Get().(*searchState); ok && v.s != nil &&
		len(v.s.gen) == int(g.n) && len(v.batch) == 16*g.dims {
		return v
	}
	return &searchState{
		s:     newLayerSearcher(int(g.n), 2*g.cfg.M+1),
		batch: make([]float32, 16*g.dims),
	}
}

func (g *Graph) putState(st *searchState) {
	st.s.ids = st.s.ids[:0]
	statePool.Put(st)
}

// Search returns the top-k rows nearest to q under the graph metric,
// ordered by descending Score (dot for cosine/dot metrics, negated
// squared distance for L2). q must already be metric-transformed
// (NormalizeQuery). ef <= 0 falls back to cfg.EFSearch. filter, when
// non-nil, restricts which rows may enter the result heaps during
// traversal (§2.4.4/§2.6). budget caps visited nodes and distance
// computations; ctx cancellation aborts between levels.
func (g *Graph) Search(ctx context.Context, q []float32, k int, ef int, filter func(int32) bool, budget index.SearchBudget) ([]Hit, error) {
	if len(q) != g.dims {
		return nil, fmt.Errorf("hnsw: query has %d dims, index has %d", len(q), g.dims)
	}
	if g.n == 0 {
		return nil, nil
	}
	if k <= 0 {
		k = 10
	}
	if k > int(g.n) {
		k = int(g.n)
	}
	if ef <= 0 {
		ef = g.cfg.EFSearch
	}
	if ef < k {
		ef = k
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st := g.getState()
	defer g.putState(st)
	if sq := g.sq.Load(); sq != nil {
		return g.searchQuantized(sq, st, q, k, ef, filter, budget)
	}
	return g.searchExact(st, q, k, ef, filter, budget)
}

// searchExact is the float32 hot-tier path (§2.5): greedy descent
// (ef=1 beam search) from the entry point through the upper levels,
// then an ef-wide beam search on level 0. Results come out of the beam
// search already sorted by ascending distance.
func (g *Graph) searchExact(st *searchState, q []float32, k, ef int, filter func(int32) bool, budget searchBudget) ([]Hit, error) {
	dists := makeDistFunc(g.cfg.Metric, q, g.Vector, st.batch)
	dists = countDists(dists, budget)
	entryD := make([]float32, 1)
	dists([]int32{g.entry}, entryD)
	ep, epD := g.entry, entryD[0]
	for l := int(g.maxLevel); l >= 1; l-- {
		w := st.s.search(g, ep, epD, 1, l, dists, filter, budget.MaxVisited, nil)
		if len(w) == 0 {
			break
		}
		ep, epD = w[0].id, w[0].d
	}
	w := st.s.search(g, ep, epD, ef, 0, dists, filter, budget.MaxVisited, nil)
	if len(w) > k {
		w = w[:k]
	}
	hits := make([]Hit, len(w))
	for i, c := range w {
		hits[i] = Hit{RowID: c.id, Score: -c.d}
	}
	return hits, nil
}

// countDists wraps a distFunc with the MaxDistComps budget: once the
// computation count is exhausted, remaining candidates score +MaxFloat32
// and stop propagating through the beam search (best-effort early
// exit). budget.MaxDistComps <= 0 means unlimited.
func countDists(f distFunc, budget searchBudget) distFunc {
	if budget.MaxDistComps <= 0 {
		return f
	}
	remain := budget.MaxDistComps
	return func(ids []int32, out []float32) {
		n := int64(len(ids))
		if remain <= 0 {
			for i := range ids {
				out[i] = math.MaxFloat32
			}
			return
		}
		if n <= remain {
			remain -= n
			f(ids, out)
			return
		}
		f(ids[:remain], out[:remain])
		for i := remain; i < n; i++ {
			out[i] = math.MaxFloat32
		}
		remain = 0
	}
}

// MemBytes reports total graph memory: the CSR neighbors/offsets, the
// levels array, the vector copy, and the active quantization state —
// the input for Index.MemEstimate (§1.1).
func (g *Graph) MemBytes() int64 {
	m := int64(cap(g.neighbors)+cap(g.offsets))*4 + int64(cap(g.levels)) + int64(cap(g.vecs))*4
	if sq := g.sq.Load(); sq != nil {
		m += sq.MemBytes()
	}
	return m
}
