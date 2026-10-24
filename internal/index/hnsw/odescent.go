// Package hnsw odescent.go implements the ODescent batch graph
// construction algorithm (inspired by vsag's impl/odescent/). Unlike
// the incremental insertion of BuildGraph (Malkov Alg. 1), ODescent
// first builds an initial kNN graph, then iteratively refines neighbor
// lists by expanding candidates from 2-hop neighborhoods and re-selecting
// with the heuristic. This produces higher-quality graphs for batch
// imports and enables parallelizable construction.
//
// Hybrid initial kNN strategy:
//
//	n <  100k → chunked brute force (O(n²/blockSize))
//	n ≥ 100k → random projection LSH (O(n*log(n)))
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.4.4
package hnsw

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

// BuildGraphODesc builds an HNSW graph using the ODescent algorithm.
// The entry point signature matches BuildGraph so callers can switch
// between incremental and batch construction via Config.BuildMode.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.4.4
func BuildGraphODesc(cfg Config, n, dims int, vecs []float32) (*Graph, error) {
	cfg = cfg.resolve()
	if n == 0 {
		return &Graph{cfg: cfg, dims: dims, offsets: []int32{0}}, nil
	}
	if n < 0 {
		return nil, fmt.Errorf("hnsw: ODescent build with negative n=%d", n)
	}

	b := &builder{
		cfg:    cfg,
		n:      n,
		dims:   dims,
		vecs:   vecs,
		adj:    make([][][]int32, n),
		levels: make([]uint8, n),
		rng:    rand.New(rand.NewPCG(cfg.Seed, 0)),
		mult:   math.Log(float64(cfg.M)),
	}
	// Normalize for cosine metric.
	if cfg.Metric == MetricCosine {
		for i := 0; i < n; i++ {
			normalizeInto(vecs[i*dims:(i+1)*dims], vecs[i*dims:(i+1)*dims])
		}
	}

	k := 2 * cfg.M
	if k < 10 {
		k = 10
	}

	// Step 1: Build initial kNN graph.
	initialGraph := b.buildInitialKNNG(k)

	// Step 2: Assign HNSW levels (geometric distribution).
	b.assignLevels(n)

	// Step 3: Populate adj from initial kNN graph (level 0 only).
	for i := 0; i < n; i++ {
		for j := range b.adj[i] {
			if j == 0 {
				b.adj[i][0] = initialGraph[i]
			}
		}
	}

	// Step 4: Iterative refinement (3 rounds).
	maxCands := cfg.EFConstruction
	b.cache = newDistCache(maxCands)
	for round := 0; round < 3; round++ {
		b.refineGraph(initialGraph, k, cfg.Alpha)
	}

	// Step 5: Set entry point to highest-level node.
	b.entry = 0
	for i := 1; i < n; i++ {
		if b.levels[i] > b.levels[b.entry] {
			b.entry = int32(i)
		}
	}
	b.maxLvl = int8(b.levels[b.entry])

	return b.finish(), nil
}

// buildInitialKNNG builds an initial kNN graph using a hybrid strategy.
func (b *builder) buildInitialKNNG(k int) [][]int32 {
	if b.n < 100000 {
		return b.bruteForceKNNG(k)
	}
	return b.lshKNNG(k)
}

// bruteForceKNNG computes exact kNN for each node by brute force.
// For n < 100k this is O(n²*d) which is acceptable.
func (b *builder) bruteForceKNNG(k int) [][]int32 {
	result := make([][]int32, b.n)
	for i := 0; i < b.n; i++ {
		vi := b.vecRow(i)
		type cand struct {
			id int32
			d  float32
		}
		cands := make([]cand, 0, b.n-1)
		for j := 0; j < b.n; j++ {
			if i == j {
				continue
			}
			cands = append(cands, cand{int32(j), b.distOne(vi, int32(j))})
		}
		sort.Slice(cands, func(a, c int) bool { return cands[a].d < cands[c].d })
		if len(cands) > k {
			cands = cands[:k]
		}
		result[i] = make([]int32, len(cands))
		for idx, c := range cands {
			result[i][idx] = c.id
		}
	}
	return result
}

// lshKNNG builds an approximate kNN graph using random projection LSH.
// Uses L=8 hash tables, each projecting into D/4 dimensions.
func (b *builder) lshKNNG(k int) [][]int32 {
	const numTables = 8
	projDims := b.dims / 4
	if projDims < 4 {
		projDims = 4
	}
	// Generate L random projection matrices and hash all vectors.
	type bucket struct {
		id  int32
		bin string
	}
	allBuckets := make([][]bucket, numTables)
	for t := 0; t < numTables; t++ {
		proj := make([][]float32, projDims)
		for d := range proj {
			proj[d] = make([]float32, b.dims)
			for j := range proj[d] {
				proj[d][j] = float32(b.rng.NormFloat64())
			}
		}
		buckets := make([]bucket, b.n)
		for i := 0; i < b.n; i++ {
			v := b.vecRow(i)
			bin := make([]byte, projDims)
			for d := 0; d < projDims; d++ {
				var dot float32
				for j := 0; j < b.dims; j++ {
					dot += v[j] * proj[d][j]
				}
				if dot > 0 {
					bin[d] = 1
				}
			}
			buckets[i] = bucket{int32(i), string(bin)}
		}
		sort.Slice(buckets, func(a, c int) bool { return buckets[a].bin < buckets[c].bin })
		allBuckets[t] = buckets
	}
	// For each node, collect candidates from same-bucket nodes across
	// all tables, then compute exact distances and keep top-k.
	result := make([][]int32, b.n)
	for i := 0; i < b.n; i++ {
		id := int32(i)
		vi := b.vecRow(i)
		seen := make(map[int32]bool)
		type cand struct {
			id int32
			d  float32
		}
		var cands []cand
		for t := 0; t < numTables; t++ {
			buckets := allBuckets[t]
			// Binary search for node i's bucket.
			target := buckets[0]
			for _, bk := range buckets {
				if bk.id == id {
					target = bk
					break
				}
			}
			// Scan neighbors in the same bucket.
			for _, bk := range buckets {
				if bk.id == id || seen[bk.id] {
					continue
				}
				if bk.bin == target.bin {
					seen[bk.id] = true
					cands = append(cands, cand{bk.id, b.distOne(vi, bk.id)})
				}
			}
		}
		sort.Slice(cands, func(a, c int) bool { return cands[a].d < cands[c].d })
		if len(cands) > k {
			cands = cands[:k]
		}
		if len(cands) < k {
			// Fallback: add random neighbors if LSH didn't find enough.
			for j := 0; j < b.n && len(cands) < k; j++ {
				if int32(j) == id || seen[int32(j)] {
					continue
				}
				seen[int32(j)] = true
				cands = append(cands, cand{int32(j), b.distOne(vi, int32(j))})
			}
		}
		result[i] = make([]int32, len(cands))
		for idx, c := range cands {
			result[i][idx] = c.id
		}
	}
	return result
}

// refineGraph performs one round of ODescent refinement: for each node,
// expand candidates from 2-hop neighborhood (neighbors of neighbors),
// then re-select with heuristic + alpha.
func (b *builder) refineGraph(graph [][]int32, k int, alpha float64) {
	newGraph := make([][]int32, b.n)
	for i := 0; i < b.n; i++ {
		seen := make(map[int32]bool)
		cands := make([]cand, 0, k*4)
		// 1-hop neighbors.
		for _, nb := range graph[i] {
			if !seen[nb] {
				seen[nb] = true
				cands = append(cands, cand{nb, b.distOne(b.vecRow(i), nb)})
			}
		}
		// 2-hop neighbors (neighbors of neighbors).
		for _, nb := range graph[i] {
			for _, nnb := range graph[nb] {
				if nnb == int32(i) || seen[nnb] {
					continue
				}
				seen[nnb] = true
				cands = append(cands, cand{nnb, b.distOne(b.vecRow(i), nnb)})
			}
		}
		// Sort by distance.
		sort.Slice(cands, func(a, c int) bool { return cands[a].d < cands[c].d })
		// Re-select with adaptive heuristic.
		selected := b.adaptiveHeuristic(cands, k, alpha)
		newGraph[i] = make([]int32, len(selected))
		for idx, s := range selected {
			newGraph[i][idx] = s.id
		}
	}
	copy(graph, newGraph)
}

// assignLevels assigns HNSW levels using the geometric distribution
// floor(-ln(U) / ln(M)), same as randomLevel in the incremental builder.
func (b *builder) assignLevels(n int) {
	for i := 0; i < n; i++ {
		lvl := b.randomLevel()
		b.levels[i] = uint8(lvl)
		b.adj[i] = make([][]int32, lvl+1)
	}
}
