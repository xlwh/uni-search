// Package hnsw adaptive_pruning.go implements adaptive neighbor pruning
// with an alpha parameter (inspired by vsag's adaptive_pruning.h).
// When alpha == 0 or 1.0, behavior is identical to the standard Malkov
// Alg. 4 heuristic. When alpha > 1.0, more remote neighbors are kept
// (relaxed); when alpha < 1.0, pruning is stricter.
//
// Also provides a distance cache interface to eliminate redundant
// distance computations in the O(m²) heuristic comparison loop.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.4.2
package hnsw

// distCache caches pairwise distances between candidate nodes to avoid
// redundant computation in the heuristic selection loop. The cache is
// per-build-call and discarded when the builder finishes.
type distCache interface {
	get(a, b int32) (float32, bool)
	put(a, b int32, v float32)
}

// matrixCache is a pre-allocated [n][n]float32 cache for small candidate
// sets (n <= 256). Lookup is O(1) with no hash overhead.
type matrixCache struct {
	data []float32
	n    int
}

func newMatrixCache(n int) *matrixCache {
	return &matrixCache{data: make([]float32, n*n), n: n}
}

func (c *matrixCache) get(a, b int32) (float32, bool) {
	if a > b {
		a, b = b, a
	}
	v := c.data[int(a)*c.n+int(b)]
	return v, v != 0
}

func (c *matrixCache) put(a, b int32, v float32) {
	if a > b {
		a, b = b, a
	}
	c.data[int(a)*c.n+int(b)] = v
}

// mapCache uses a map for larger candidate sets where a full matrix
// would waste memory.
type mapCache struct {
	m map[[2]int32]float32
}

func newMapCache() *mapCache {
	return &mapCache{m: make(map[[2]int32]float32, 64)}
}

func (c *mapCache) get(a, b int32) (float32, bool) {
	if a > b {
		a, b = b, a
	}
	v, ok := c.m[[2]int32{a, b}]
	return v, ok
}

func (c *mapCache) put(a, b int32, v float32) {
	if a > b {
		a, b = b, a
	}
	c.m[[2]int32{a, b}] = v
}

// newDistCache creates an adaptive distance cache: matrix for small
// candidate sets (<=256), map for larger ones.
func newDistCache(maxCands int) distCache {
	if maxCands <= 256 {
		return newMatrixCache(maxCands)
	}
	return newMapCache()
}

// cachedDist wraps distOne with cache lookup for the heuristic loop.
func (b *builder) cachedDist(a, other int32) float32 {
	if b.cache != nil {
		if v, ok := b.cache.get(a, other); ok {
			return v
		}
	}
	v := b.distOne(b.vecRow(int(a)), other)
	if b.cache != nil {
		b.cache.put(a, other, v)
	}
	return v
}

// adaptiveHeuristic implements heuristic neighbor selection with an
// adaptive alpha parameter. When alpha == 0 or 1.0, delegates to the
// standard heuristic (Malkov Alg. 4). When alpha != 1.0, the rejection
// condition becomes dist(e, s) < alpha * dist(e, q) instead of
// dist(e, s) < dist(e, q).
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.4.2
func (b *builder) adaptiveHeuristic(cands []cand, m int, alpha float64) []cand {
	if alpha <= 0 || alpha == 1.0 {
		return b.heuristic(cands, m)
	}
	if len(cands) <= m {
		out := make([]cand, len(cands))
		copy(out, cands)
		return out
	}
	sel := make([]cand, 0, m)
	for _, c := range cands {
		if len(sel) >= m {
			break
		}
		ok := true
		for _, s := range sel {
			if b.cachedDist(c.id, s.id) < float32(alpha)*c.d {
				ok = false
				break
			}
		}
		if ok {
			sel = append(sel, c)
		}
	}
	return sel
}

// adaptiveAlpha runs a two-phase RELAX/TIGHTEN alpha tuning.
// Phase 1 (RELAX): build with alpha=1.2, track acceptance rate.
// Phase 2 (TIGHTEN): if acceptance > 0.7, reduce alpha to 1.0;
// if acceptance < 0.3, increase alpha to 1.5.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.4.2
func (b *builder) adaptiveAlpha(cands []cand, m int) []cand {
	const relaxAlpha = 1.2
	result := b.adaptiveHeuristic(cands, m, relaxAlpha)
	acceptRate := float64(len(result)) / float64(len(cands))
	var tightenAlpha float64
	switch {
	case acceptRate > 0.7:
		tightenAlpha = 1.0
	case acceptRate < 0.3:
		tightenAlpha = 1.5
	default:
		return result // RELAX result is acceptable
	}
	return b.adaptiveHeuristic(cands, m, tightenAlpha)
}
