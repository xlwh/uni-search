package hnsw

import (
	"github.com/unisearch-db/unisearch/internal/simd"
)

// cand pairs a node id with its distance to the query vector; smaller
// distance is better under every supported metric.
type cand struct {
	id int32
	d  float32
}

// minHeap is a binary min-heap of cand ordered by ascending d.
type minHeap []cand

func (h *minHeap) push(c cand) {
	a := append(*h, c)
	i := len(a) - 1
	for i > 0 {
		p := (i - 1) / 2
		if a[p].d <= a[i].d {
			break
		}
		a[p], a[i] = a[i], a[p]
		i = p
	}
	*h = a
}

func (h *minHeap) pop() cand {
	a := *h
	top := a[0]
	last := len(a) - 1
	a[0] = a[last]
	a = a[:last]
	i := 0
	for {
		l, r := 2*i+1, 2*i+2
		m := i
		if l < len(a) && a[l].d < a[m].d {
			m = l
		}
		if r < len(a) && a[r].d < a[m].d {
			m = r
		}
		if m == i {
			break
		}
		a[i], a[m] = a[m], a[i]
		i = m
	}
	*h = a
	return top
}

// maxHeap is a binary max-heap of cand ordered by descending d.
type maxHeap []cand

func (h *maxHeap) push(c cand) {
	a := append(*h, c)
	i := len(a) - 1
	for i > 0 {
		p := (i - 1) / 2
		if a[p].d >= a[i].d {
			break
		}
		a[p], a[i] = a[i], a[p]
		i = p
	}
	*h = a
}

func (h *maxHeap) pop() cand {
	a := *h
	top := a[0]
	last := len(a) - 1
	a[0] = a[last]
	a = a[:last]
	i := 0
	for {
		l, r := 2*i+1, 2*i+2
		m := i
		if l < len(a) && a[l].d > a[m].d {
			m = l
		}
		if r < len(a) && a[r].d > a[m].d {
			m = r
		}
		if m == i {
			break
		}
		a[i], a[m] = a[m], a[i]
		i = m
	}
	*h = a
	return top
}

// adjacency abstracts per-level neighbor access. Both the mutable
// builder (per-node adjacency lists) and the packed CSR Graph (§2.1)
// implement it, so construction and search share one beam-search core.
type adjacency interface {
	layerNeighbors(node int32, level int) []int32
}

// distFunc fills out[i] with the distance (smaller is better) between
// the query vector and node ids[i]. It must not modify ids.
type distFunc func(ids []int32, out []float32)

// vecSource returns the stored vector of a node id.
type vecSource func(id int32) []float32

// makeDistFunc binds a query vector to a distance function under the
// given metric. For dot/cosine the distance is the negated dot product
// (§2.2: cosine normalizes once at build time, then it is dot for
// life); for L2 it is the squared distance. Full blocks of 16
// candidates go through simd.DotF32Batch16 via a gather buffer (§2.3),
// the scalar tail through simd.DotF32 / simd.L2F32.
func makeDistFunc(m Metric, q []float32, src vecSource, batch []float32) distFunc {
	dims := len(q)
	if m == MetricL2 {
		return func(ids []int32, out []float32) {
			for i, id := range ids {
				out[i] = simd.L2F32(q, src(id))
			}
		}
	}
	return func(ids []int32, out []float32) {
		i := 0
		for ; i+16 <= len(ids); i += 16 {
			g := batch[:16*dims]
			for j := 0; j < 16; j++ {
				copy(g[j*dims:(j+1)*dims], src(ids[i+j]))
			}
			blk := out[i : i+16 : i+16]
			simd.DotF32Batch16(q, g, dims, blk)
			for j := range blk {
				blk[j] = -blk[j]
			}
		}
		for ; i < len(ids); i++ {
			out[i] = -simd.DotF32(q, src(ids[i]))
		}
	}
}

// layerSearcher holds the per-search scratch state: a visit-epoch array
// (avoiding an O(n) clear per layer search) and neighbor/dist buffers.
type layerSearcher struct {
	gen    []int32 // per-node visit epoch; != current epoch means unvisited
	epoch  int32
	ids    []int32   // unvisited neighbor batch
	scores []float32 // distances for the batch
}

func newLayerSearcher(n int, maxNeighbors int) *layerSearcher {
	return &layerSearcher{
		gen:    make([]int32, n),
		ids:    make([]int32, 0, maxNeighbors),
		scores: make([]float32, maxNeighbors),
	}
}

// rerankFunc upgrades a coarse (e.g. SQ8) distance to the exact one by
// going back to the float32 vector (§2.5 rerank). nil means the coarse
// distance is already exact.
type rerankFunc func(id int32, coarseD float32) float32

// search runs the HNSW beam search (Malkov & Yashunin Alg. 2) on one
// level: expand the closest candidates, keep the ef best results, stop
// when the nearest remaining candidate is farther than the worst
// result. Returns the result set sorted by ascending distance. filter,
// when non-nil, restricts which nodes may enter both heaps (§2.6
// pre-filter / §2.4.4 delete handling); traversal cannot pass through
// filtered nodes. maxVisited caps the number of expanded nodes
// (<= 0 = unlimited). rerank, when non-nil, replaces the coarse
// distance of every candidate admitted to the result heap with the
// exact float32 distance (table lookup by id), so quantization noise
// cannot occupy result slots or stall the beam boundary.
func (s *layerSearcher) search(view adjacency, ep int32, epD float32, ef, level int, dists distFunc, filter func(int32) bool, maxVisited int64, rerank rerankFunc) []cand {
	if ef < 1 {
		ef = 1
	}
	if maxVisited <= 0 {
		maxVisited = -1 // <= 0 means unlimited (index.SearchBudget contract)
	}
	s.epoch++
	epo := s.epoch
	s.gen[ep] = epo
	ch := minHeap{cand{ep, epD}}
	var rh maxHeap
	if filter == nil || filter(ep) {
		rh = maxHeap{cand{ep, epD}}
	}
	var visited int64
	for len(ch) > 0 {
		if maxVisited >= 0 && visited >= maxVisited {
			break
		}
		visited++
		c := ch.pop()
		if len(rh) > 0 && c.d > rh[0].d {
			break
		}
		for _, nb := range view.layerNeighbors(c.id, level) {
			if s.gen[nb] == epo {
				continue
			}
			s.gen[nb] = epo
			s.ids = append(s.ids, nb)
		}
		if len(s.ids) == 0 {
			continue
		}
		n := len(s.ids)
		dists(s.ids, s.scores[:n])
		for i, id := range s.ids {
			d := s.scores[i]
			if filter != nil && !filter(id) {
				continue
			}
			if len(rh) >= ef && d >= rh[0].d {
				continue
			}
			if rerank != nil {
				d = rerank(id, d)
				if len(rh) >= ef && d >= rh[0].d {
					continue
				}
			}
			ch.push(cand{id, d})
			rh.push(cand{id, d})
			if len(rh) > ef {
				rh.pop()
			}
		}
		s.ids = s.ids[:0]
	}
	out := make([]cand, len(rh))
	for i := len(rh) - 1; i >= 0; i-- {
		out[i] = rh.pop()
	}
	return out
}
