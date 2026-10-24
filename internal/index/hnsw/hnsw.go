// Package hnsw implements the HNSW approximate nearest-neighbor graph
// index (design 02-index-engine.md §2): CSR adjacency layout (§2.1),
// default parameters M=16 / ef_construction=200 / ef_search=64 (§2.2),
// SIMD-backed dot/L2 kernels from internal/simd (§2.3), and SQ8 scalar
// quantization with rerank (§2.5). The package registers itself under
// index.KindHNSW in init (§1.2).
package hnsw

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"

	"github.com/unisearch-db/unisearch/internal/simd"
)

// MaxLevel is the hard cap on the HNSW level count (§2.1).
const MaxLevel = 32

// Documented defaults (§2.2).
const (
	DefaultM              = 16
	DefaultEFConstruction = 200
	DefaultEFSearch       = 64
)

// Metric selects the distance kernel (§2.2).
type Metric int8

const (
	// MetricCosine normalizes vectors once at build time (the graph
	// stores the normalized copy) and then uses plain dot for life.
	MetricCosine Metric = iota
	// MetricDot is raw dot product; scores may be negative, TopK keeps
	// higher-is-better semantics.
	MetricDot
	// MetricL2 is the squared Euclidean distance.
	MetricL2
	// MetricMIPS is Maximum Inner Product Search — same kernel as
	// MetricDot but declared as a distinct type for clarity and
	// future optimizations (design 02 §2.2). No normalization is
	// applied at build time.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.3.9
	MetricMIPS
)

// Config holds the tunable HNSW parameters (§2.2). The zero value is
// valid and resolves to the documented defaults.
type Config struct {
	M              int // max links per node above level 0; 0 -> 16
	EFConstruction int // build-time beam width; 0 -> 200
	EFSearch       int // default query beam width; 0 -> 64
	Metric         Metric
	Seed           uint64 // level-assignment RNG seed (reproducible builds)
	// Alpha controls adaptive pruning (§Phase 4). 0 or 1.0 = standard
	// Malkov Alg. 4 (no change). >1.0 = relaxed (keep more remote
	// neighbors); <1.0 = stricter pruning. Only effective when
	// BuildMode != BuildModeInsert or when explicitly set.
	// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.4.1
	Alpha float64
	// BuildMode selects the graph construction algorithm.
	// 0 = BuildModeInsert (default, incremental insertion).
	// 1 = BuildModeODescent (batch construction via ODescent).
	// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.4.1
	BuildMode BuildMode
	// EnableEF enables Elias-Fano encoding of level-0 neighbor lists
	// to reduce memory (§Phase 5). When false (default), neighbors are
	// stored as raw []int32 in the CSR layout.
	// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.5.2
	EnableEF bool
}

// BuildMode selects the graph construction algorithm.
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.4.1
type BuildMode int8

const (
	BuildModeInsert   BuildMode = 0 // incremental insertion (default)
	BuildModeODescent BuildMode = 1 // batch construction via ODescent
)

func (c Config) resolve() Config {
	if c.M <= 0 {
		c.M = DefaultM
	}
	if c.EFConstruction <= 0 {
		c.EFConstruction = DefaultEFConstruction
	}
	if c.EFSearch <= 0 {
		c.EFSearch = DefaultEFSearch
	}
	return c
}

// Hit is one ranked search result; higher Score is better (dot for
// cosine/dot, negated squared distance for L2).
type Hit struct {
	RowID int32
	Score float32
}

// Graph is an immutable HNSW graph in the CSR layout of §2.1: three
// flat slices — neighbors, offsets, levels — plus the entry point,
// with node id == segment row id (zero conversion). offsets is indexed
// by node*MaxLevel+level and has length n*MaxLevel+1, so the adjacency
// of node u at level l is neighbors[offsets[u*MaxLevel+l] :
// offsets[u*MaxLevel+l+1]] and empty levels collapse to empty ranges
// with no degree array. The graph owns its vector copy (normalized
// when MetricCosine, §2.2); distance computation goes back to it by id.
// All reads are safe for concurrent use once BuildGraph returns.
//
// Cross-Link (§2.4.2): crosslinks holds the inter-segment top-layer
// adjacency encoded as peerSegmentID<<32 | peerNodeID (int64). It is
// populated after Build by BuildCrosslinks (a flush-time background
// task) and is empty for graphs that have not been linked yet. segID
// is the segment id of this graph (0 when single-segment / unknown);
// it is informational and not used during graph-internal traversal.
type Graph struct {
	cfg        Config
	n          int32
	dims       int
	neighbors  []int32
	offsets    []int32
	levels     []uint8
	entry      int32
	maxLevel   int8
	vecs       []float32
	sq         sqState // SQ8 quantization tier, nil until Quantize (§2.5)
	rb         rbState // RaBitQ quantization tier, nil until EnableRaBitQ (§2.5)
	pq         pqState // PQ quantization tier, nil until EnablePQ (§Phase 3)
	segID      int32   // segment id of this graph; informational (§2.4.2)
	crosslinks []int64 // peer segID<<32 | nodeID; populated by BuildCrosslinks (§2.4.2)
	neighborsEF *efLevelData // Elias-Fano encoded level-0 neighbors (§Phase 5); nil = use raw
}

// N returns the number of nodes (== rows).
func (g *Graph) N() int { return int(g.n) }

// Dims returns the vector dimensionality.
func (g *Graph) Dims() int { return g.dims }

// MetricKind returns the distance metric fixed at build time.
func (g *Graph) MetricKind() Metric { return g.cfg.Metric }

// Config returns the resolved build configuration.
func (g *Graph) Config() Config { return g.cfg }

// Entry returns the entry point node id and the global max level.
func (g *Graph) Entry() (int32, int8) { return g.entry, g.maxLevel }

// SegID returns the segment id of this graph (§2.4.2). 0 when unset
// (single-segment builds, or pre-Cross-Link graphs).
func (g *Graph) SegID() int32 { return g.segID }

// Crosslinks returns the inter-segment top-layer adjacency encoded as
// peerSegmentID<<32 | peerNodeID (§2.4.2). Empty for graphs that have
// not been linked. The slice aliases graph storage; callers must not
// mutate it.
func (g *Graph) Crosslinks() []int64 { return g.crosslinks }

// SetCrosslinks installs the Cross-Link adjacency into the graph (§2.4.2):
// segID identifies this segment (so peers can encode back-pointers to
// it) and links is the flat list of peer SegmentID<<32 | nodeID
// encodings. The slice is stored as-is; callers must not mutate it
// afterwards. SetCrosslinks is the post-flush background hook used by
// BuildCrosslinks; it is not safe to call concurrently with Search.
func (g *Graph) SetCrosslinks(segID int32, links []int64) {
	g.segID = segID
	g.crosslinks = links
}

// Vector returns the stored vector of a node (normalized when cosine).
// The returned slice aliases graph storage and must not be modified.
func (g *Graph) Vector(id int32) []float32 {
	off := int(id) * g.dims
	return g.vecs[off : off+g.dims]
}

// layerNeighbors implements adjacency over the packed CSR layout.
// When neighborsEF is active (§Phase 5), level-0 neighbors are decoded
// from Elias-Fano encoded data; upper levels use the raw CSR layout.
func (g *Graph) layerNeighbors(node int32, level int) []int32 {
	if g.neighborsEF != nil && level == 0 {
		return g.neighborsEF.data[node].decodeAll()
	}
	base := int(node)*MaxLevel + level
	return g.neighbors[g.offsets[base]:g.offsets[base+1]]
}

// buildGraph constructs the graph; see BuildGraph.
type builder struct {
	cfg      Config
	n        int
	dims     int
	vecs     []float32 // own copy, normalized when cosine
	adj      [][][]int32
	levels   []uint8
	entry    int32
	maxLvl   int8
	rng      *rand.Rand
	mult     float64
	searcher *layerSearcher
	batch    []float32 // 16*dims gather buffer
	cache    distCache // distance cache for heuristic (Phase 4), nil = no cache
}

func (b *builder) layerNeighbors(node int32, level int) []int32 {
	return b.adj[node][level]
}

func (b *builder) vecRow(id int) []float32 {
	return b.vecs[id*b.dims : id*b.dims+b.dims]
}

// distOne computes the distance between an arbitrary vector and a node
// under the build metric (negated dot for cosine/dot, squared L2).
func (b *builder) distOne(q []float32, id int32) float32 {
	v := b.vecRow(int(id))
	if b.cfg.Metric == MetricL2 {
		return simd.L2F32(q, v)
	}
	return -simd.DotF32(q, v)
}

// BuildGraph constructs an in-memory HNSW graph over the n row-major
// dims-dimensional vectors in vecs: greedy descent from the entry
// point through the upper levels, then per-level beam search with
// ef_construction and heuristic neighbor selection (§2.1-2.2). The
// graph keeps its own copy of the vectors — normalized in place for
// MetricCosine — so callers may reuse their slice afterwards. With the
// default seed the build is reproducible.
func BuildGraph(cfg Config, n, dims int, vecs []float32) (*Graph, error) {
	if dims <= 0 {
		return nil, fmt.Errorf("hnsw: dims must be > 0, got %d", dims)
	}
	if n < 0 || len(vecs) != n*dims {
		return nil, fmt.Errorf("hnsw: vecs length %d does not match n=%d dims=%d", len(vecs), n, dims)
	}
	cfg = cfg.resolve()
	b := &builder{
		cfg:      cfg,
		n:        n,
		dims:     dims,
		vecs:     make([]float32, len(vecs)),
		adj:      make([][][]int32, n),
		levels:   make([]uint8, n),
		entry:    -1,
		rng:      rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x5DEECE66D)),
		mult:     1.0 / math.Log(float64(cfg.M)),
		searcher: newLayerSearcher(n, 2*cfg.M+1),
		batch:    make([]float32, 16*dims),
	}
	if cfg.Metric == MetricCosine {
		for i := 0; i < n; i++ {
			normalizeInto(b.vecs[i*dims:(i+1)*dims], vecs[i*dims:(i+1)*dims])
		}
	} else {
		copy(b.vecs, vecs)
	}
	for i := range b.adj {
		lvl := b.randomLevel()
		b.adj[i] = make([][]int32, lvl+1)
		b.levels[i] = uint8(lvl)
	}
	for i := 0; i < n; i++ {
		b.insert(int32(i))
	}
	return b.finish(), nil
}

// normalizeInto writes the unit-normalized copy of src into dst. The
// zero vector passes through unchanged: a zero norm has no direction,
// and keeping zeros (instead of NaN) keeps dot products well-defined.
// The norm accumulates in float64 for stability.
func normalizeInto(dst, src []float32) {
	var ss float64
	for _, x := range src {
		ss += float64(x) * float64(x)
	}
	if ss == 0 {
		copy(dst, src)
		return
	}
	inv := float32(1 / math.Sqrt(ss))
	for i, x := range src {
		dst[i] = x * inv
	}
}

// randomLevel draws a node level from the geometric distribution
// floor(-ln(U) / ln(M)) (Malkov & Yashunin §4.2.2), capped at
// MaxLevel-1.
func (b *builder) randomLevel() int {
	r := b.rng.Float64()
	l := int(-math.Log(r) * b.mult)
	if l >= MaxLevel {
		l = MaxLevel - 1
	}
	if l < 0 {
		l = 0
	}
	return l
}

// insert adds one node to the graph: greedy descent (ef=1 beam search)
// from the entry point down to the node's level, then per-level beam
// search with ef_construction and heuristic selection of up to M links
// per side (Malkov & Yashunin Alg. 1).
func (b *builder) insert(id int32) {
	q := b.vecRow(int(id))
	lvl := len(b.adj[id]) - 1
	if b.entry < 0 {
		b.entry = id
		b.maxLvl = int8(lvl)
		return
	}
	dists := makeDistFunc(b.cfg.Metric, q, func(n int32) []float32 { return b.vecRow(int(n)) }, b.batch)
	ep, epD := b.entry, b.distOne(q, b.entry)
	for l := int(b.maxLvl); l > lvl; l-- {
		w := b.searcher.search(b, ep, epD, 1, l, dists, nil, -1, nil)
		if len(w) == 0 {
			break
		}
		ep, epD = w[0].id, w[0].d
	}
	lo := min(lvl, int(b.maxLvl))
	for l := lo; l >= 0; l-- {
		w := b.searcher.search(b, ep, epD, b.cfg.EFConstruction, l, dists, nil, -1, nil)
		if len(w) == 0 {
			break
		}
		for _, c := range b.heuristic(w, b.cfg.M) {
			b.adj[id][l] = append(b.adj[id][l], c.id)
			b.link(c.id, l, id)
		}
		ep, epD = w[0].id, w[0].d
	}
	if lvl > int(b.maxLvl) {
		b.maxLvl = int8(lvl)
		b.entry = id
	}
}

// link appends nb to node's level-l adjacency and shrinks the list
// with the heuristic when the degree cap is exceeded (§2.1: M above
// level 0, M*2 at level 0).
func (b *builder) link(node int32, level int, nb int32) {
	lst := append(b.adj[node][level], nb)
	cap := b.cfg.M
	if level == 0 {
		cap *= 2
	}
	if len(lst) <= cap {
		b.adj[node][level] = lst
		return
	}
	nv := b.vecRow(int(node))
	cs := make([]cand, len(lst))
	for i, id := range lst {
		cs[i] = cand{id: id, d: b.distOne(nv, id)}
	}
	sortCands(cs)
	b.adj[node][level] = idsOf(b.heuristic(cs, cap))
}

// heuristic implements heuristic neighbor selection (Malkov & Yashunin
// Alg. 4): scanning candidates by ascending distance to q, keep e only
// if it is closer to q than to every already-selected neighbor. This
// preserves link-direction diversity on clustered data. cands must be
// sorted ascending by d.
func (b *builder) heuristic(cands []cand, m int) []cand {
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
			if b.distOne(b.vecRow(int(c.id)), s.id) < c.d {
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

// sortCands sorts candidates ascending by distance in place.
func sortCands(cs []cand) {
	slices.SortFunc(cs, func(a, b cand) int {
		switch {
		case a.d < b.d:
			return -1
		case a.d > b.d:
			return 1
		}
		return 0
	})
}

func idsOf(cs []cand) []int32 {
	out := make([]int32, len(cs))
	for i, c := range cs {
		out[i] = c.id
	}
	return out
}

// finish packs the per-node adjacency lists into the CSR layout of
// §2.1: neighbors in node order with levels ascending within a node,
// offsets indexed by node*MaxLevel+level with a sentinel at
// n*MaxLevel, and releases the builder's mutable state.
func (b *builder) finish() *Graph {
	g := &Graph{
		cfg:      b.cfg,
		n:        int32(b.n),
		dims:     b.dims,
		levels:   b.levels,
		entry:    b.entry,
		maxLevel: b.maxLvl,
		vecs:     b.vecs,
	}
	if b.n == 0 {
		g.offsets = []int32{0}
		g.neighbors = nil
		return g
	}
	g.offsets = make([]int32, b.n*MaxLevel+1)
	pos := 0
	for node := 0; node < b.n; node++ {
		lvl := int(b.levels[node])
		for l := 0; l <= lvl; l++ {
			g.offsets[node*MaxLevel+l] = int32(pos)
			pos += len(b.adj[node][l])
		}
		for l := lvl + 1; l < MaxLevel; l++ {
			g.offsets[node*MaxLevel+l] = int32(pos)
		}
	}
	g.offsets[b.n*MaxLevel] = int32(pos)
	g.neighbors = make([]int32, 0, pos)
	for node := 0; node < b.n; node++ {
		for l := 0; l <= int(b.levels[node]); l++ {
			g.neighbors = append(g.neighbors, b.adj[node][l]...)
		}
	}
	return g
}
