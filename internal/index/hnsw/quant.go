package hnsw

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/unisearch-db/unisearch/internal/simd"
)

// sqState is the quantization-tier holder on Graph: an atomic pointer
// so Quantize can swap tiers while concurrent searches read the
// current one safely. The zero value (no quantization) is valid.
type sqState = atomic.Pointer[SQ8]

// rbState is the RaBitQ quantization-tier holder (§2.5 warm+ tier):
// 1-bit quantization via random projection + binary sign, achieving
// ~32× memory reduction with ≥95% recall + float32 rerank.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.3.4
type rbState = atomic.Pointer[RaBitQState]

// pqState is the PQ quantization-tier holder (§Phase 3):
// Product Quantization with K-means++ training, achieving ~4× memory
// reduction with LUT-based coarse ranking + float32 rerank.
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.3
type pqState = atomic.Pointer[PQState]

// RaBitQState holds the RaBitQ quantized representation of all graph
// vectors: contiguous binary bitmaps, plus the quantizer (projection
// matrix). Search uses Hamming distance for coarse ranking and float32
// original vectors for rerank (same pattern as SQ8).
//
// Phase 2 optimization: codes flattened from [][]byte to []byte +
// codeOffsets to reduce GC overhead and enable batch Hamming distance.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.2.3
type RaBitQState struct {
	quantizer   *RaBitQuantizer
	codesFlat   []byte  // contiguous n*nBytes binary bitmaps
	codeOffsets []int32 // per-node offset; codeOffsets[i] = i*nBytes
	nBits       int
	nBytes      int
}

// SQ8 is per-dimension int8 scalar quantization state (§2.5): the
// per-dim min/max of the column plus one int8 code per vector
// component. Codes are unsigned levels in [0, 255] stored in the int8
// byte pattern; DotI8F32 decodes via uint8. Training reuses the
// per-dim min/max scan (column_stats in the segment footer), and the
// stored codes approximate x[d] = mins[d] + scales[d]*uint8(codes[d]).
type SQ8 struct {
	n      int
	dims   int
	codes  []int8
	mins   []float32
	scales []float32 // (max-min)/255 per dim; 0 when the dim is constant
}

// trainSQ8 trains SQ8 over the graph's stored vectors and encodes them
// all. n*d int8 codes + 2*d float32 parameters ≈ 1B/dim versus the
// 4B/dim float32 hot tier.
func trainSQ8(g *Graph) *SQ8 {
	dims := g.dims
	sq := &SQ8{
		n:      int(g.n),
		dims:   dims,
		codes:  make([]int8, int(g.n)*dims),
		mins:   make([]float32, dims),
		scales: make([]float32, dims),
	}
	if sq.n == 0 {
		return sq
	}
	maxs := make([]float32, dims)
	for d := range sq.mins {
		sq.mins[d] = math.MaxFloat32
		maxs[d] = -math.MaxFloat32
	}
	for i := 0; i < sq.n; i++ {
		v := g.Vector(int32(i))
		for d := 0; d < dims; d++ {
			if v[d] < sq.mins[d] {
				sq.mins[d] = v[d]
			}
			if v[d] > maxs[d] {
				maxs[d] = v[d]
			}
		}
	}
	for d := 0; d < dims; d++ {
		sq.scales[d] = (maxs[d] - sq.mins[d]) / 255
	}
	for i := 0; i < sq.n; i++ {
		v := g.Vector(int32(i))
		row := sq.codes[i*dims : (i+1)*dims]
		for d := 0; d < dims; d++ {
			var u uint8
			if sq.scales[d] > 0 {
				x := math.Round((float64(v[d]) - float64(sq.mins[d])) / float64(sq.scales[d]))
				if x < 0 {
					x = 0
				} else if x > 255 {
					x = 255
				}
				u = uint8(x)
			}
			row[d] = int8(u)
		}
	}
	return sq
}

// MemBytes reports the quantization-state memory: codes + per-dim
// min/scale parameters.
func (sq *SQ8) MemBytes() int64 {
	return int64(len(sq.codes)) + int64(len(sq.mins)+len(sq.scales))*4
}

// DotI8F32 is the int8 x float32 mixed kernel (§2.3): the dot product
// of a float32 query (pre-scaled per dim by the caller) against an
// int8 code row decoded as unsigned levels. Panics when the lengths
// differ, mirroring the simd kernel contract.
func DotI8F32(q []float32, code []int8) float32 {
	if len(q) != len(code) {
		panic(fmt.Sprintf("hnsw: DotI8F32 length mismatch: %d vs %d", len(q), len(code)))
	}
	var s float32
	for i, c := range code {
		s += q[i] * float32(uint8(c))
	}
	return s
}

// Quantized reports whether a quantization tier is active.
func (g *Graph) Quantized() bool { return g.sq.Load() != nil }

// EnableSQ8 trains SQ8 over the stored vectors and installs it as the
// active tier (§2.5: warm tier, default for fresh segments). Only
// dot-based metrics support SQ8 in M0.
func (g *Graph) EnableSQ8() error {
	if g.cfg.Metric == MetricL2 {
		return fmt.Errorf("hnsw: SQ8 quantization requires a dot-based metric, got l2")
	}
	g.sq.Store(trainSQ8(g))
	return nil
}

// DisableSQ8 drops back to the float32 hot tier.
func (g *Graph) DisableSQ8() {
	g.sq.Store(nil)
}

// EnableRaBitQ trains RaBitQ 1-bit quantization over the stored vectors
// and installs it as the active tier (§2.5: warm+ tier, ~32× memory
// reduction, ≥95% recall with rerank). Only dot-based metrics support
// RaBitQ in M0.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.3.4
func (g *Graph) EnableRaBitQ() error {
	if g.cfg.Metric == MetricL2 {
		return fmt.Errorf("hnsw: RaBitQ quantization requires a dot-based metric, got l2")
	}
	rq := NewRaBitQuantizer(g.dims)
	nBytes := (g.dims + 7) / 8
	rb := &RaBitQState{
		quantizer:   rq,
		nBits:       g.dims,
		nBytes:      nBytes,
		codesFlat:   make([]byte, int(g.n)*nBytes),
		codeOffsets: make([]int32, int(g.n)),
	}
	for i := 0; i < int(g.n); i++ {
		rb.codeOffsets[i] = int32(i * nBytes)
		copy(rb.codesFlat[i*nBytes:(i+1)*nBytes], rq.Quantize(g.Vector(int32(i))))
	}
	g.rb.Store(rb)
	return nil
}

// DisableRaBitQ drops back to the float32 or SQ8 tier.
func (g *Graph) DisableRaBitQ() {
	g.rb.Store(nil)
}

// RaBitQActive reports whether RaBitQ quantization is active.
func (g *Graph) RaBitQActive() bool { return g.rb.Load() != nil }

// searchRaBitQ runs the RaBitQ search path (§2.5): quantize the query
// to a binary bitmap, use Hamming distance for coarse ranking, widen
// the beam by 1.5× to compensate quantization recall loss, and rerank
// every candidate against its float32 original. Same pattern as SQ8.
//
// Phase 2 optimization: uses HammingDistanceBatch for batch distance
// computation against flat contiguous codes.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.2.3
func (g *Graph) searchRaBitQ(rb *RaBitQState, st *searchState, q []float32, k, ef int, filter func(int32) bool, budget searchBudget) ([]Hit, error) {
	efQ := int(math.Ceil(float64(ef) * 1.5))
	if efQ < ef {
		efQ = ef
	}
	// Quantize query to binary bitmap.
	qBits := rb.quantizer.Quantize(q)
	nBits := rb.nBits
	dists := func(ids []int32, out []float32) {
		hamOut := make([]int, len(ids))
		simd.HammingDistanceBatch(qBits, rb.codesFlat, rb.codeOffsets, ids, hamOut)
		for i := range ids {
			if hamOut[i] < 0 {
				out[i] = math.MaxFloat32
				continue
			}
			out[i] = float32(hamOut[i])
		}
	}
	dists = countDists(dists, budget)
	// Rerank against float32 originals (same as SQ8).
	rerank := func(id int32, coarseD float32) float32 {
		return -simd.DotF32(q, g.Vector(id))
	}
	entryD := make([]float32, 1)
	dists([]int32{g.entry}, entryD)
	ep, epD := g.entry, entryD[0]
	for l := int(g.maxLevel); l >= 1; l-- {
		w := st.s.search(g, ep, epD, 1, l, dists, filter, budget.MaxVisited, rerank)
		if len(w) == 0 {
			break
		}
		ep, epD = w[0].id, w[0].d
	}
	w := st.s.search(g, ep, epD, efQ, 0, dists, filter, budget.MaxVisited, rerank)
	if len(w) > k {
		w = w[:k]
	}
	hits := make([]Hit, 0, len(w))
	for _, c := range w {
		hits = append(hits, Hit{RowID: c.id, Score: -c.d})
	}
	_ = nBits
	return hits, nil
}

// searchQuantized runs the SQ8 search path (§2.5): quantize the query
// per dim (pre-multiplying the per-dim scales into it), widen the beam
// by 1.5x to compensate quantization recall loss, and run the graph
// traversal on the int8 kernel. Every candidate admitted to the result
// heap is immediately reranked against its float32 original (table
// lookup by row id), so quantization noise can neither occupy result
// slots nor stall the beam boundary — the final top k is already
// exactly ordered.
func (g *Graph) searchQuantized(sq *SQ8, st *searchState, q []float32, k, ef int, filter func(int32) bool, budget searchBudget) ([]Hit, error) {
	efQ := int(math.Ceil(float64(ef) * 1.5))
	if efQ < ef {
		efQ = ef
	}
	qs := make([]float32, g.dims)
	var off float32
	for d := 0; d < g.dims; d++ {
		qs[d] = q[d] * sq.scales[d]
		off += q[d] * sq.mins[d]
	}
	dims := g.dims
	dists := func(ids []int32, out []float32) {
		for i, id := range ids {
			c := int(id) * dims
			out[i] = -(off + DotI8F32(qs, sq.codes[c:c+dims]))
		}
	}
	dists = countDists(dists, budget)
	rerank := func(id int32, coarseD float32) float32 {
		return -simd.DotF32(q, g.Vector(id))
	}
	entryD := make([]float32, 1)
	dists([]int32{g.entry}, entryD)
	ep, epD := g.entry, entryD[0]
	for l := int(g.maxLevel); l >= 1; l-- {
		w := st.s.search(g, ep, epD, 1, l, dists, filter, budget.MaxVisited, rerank)
		if len(w) == 0 {
			break
		}
		ep, epD = w[0].id, w[0].d
	}
	w := st.s.search(g, ep, epD, efQ, 0, dists, filter, budget.MaxVisited, rerank)
	if len(w) > k {
		w = w[:k]
	}
	hits := make([]Hit, 0, len(w))
	for _, c := range w {
		hits = append(hits, Hit{RowID: c.id, Score: -c.d})
	}
	return hits, nil
}

// EnablePQ trains PQ quantization over the stored vectors and installs
// it as the active tier. m = subspace count; 0 = auto-select.
// Only dot-based metrics support PQ in the current implementation.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.3.4
func (g *Graph) EnablePQ(m int) error {
	if g.cfg.Metric == MetricL2 {
		return fmt.Errorf("hnsw: PQ quantization requires a dot-based metric, got l2")
	}
	pq, err := trainPQ(g, m)
	if err != nil {
		return err
	}
	g.pq.Store(pq)
	return nil
}

// DisablePQ drops back to the float32 or other tier.
func (g *Graph) DisablePQ() { g.pq.Store(nil) }

// PQActive reports whether PQ quantization is active.
func (g *Graph) PQActive() bool { return g.pq.Load() != nil }

// searchPQ runs the PQ search path: build LUT from query, use LUT
// lookup for coarse ranking, widen beam 1.5×, rerank with float32.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.3.4
func (g *Graph) searchPQ(pq *PQState, st *searchState, q []float32, k, ef int, filter func(int32) bool, budget searchBudget) ([]Hit, error) {
	efQ := int(math.Ceil(float64(ef) * 1.5))
	if efQ < ef {
		efQ = ef
	}
	lut := pq.buildLUT(q)
	m := pq.m
	dists := func(ids []int32, out []float32) {
		for i, id := range ids {
			if id < 0 || int(id) >= pq.n {
				out[i] = math.MaxFloat32
				continue
			}
			code := pq.codes[int(id)*m : int(id+1)*m]
			out[i] = -pqDistance(lut, code, m) // negate for ascending sort
		}
	}
	dists = countDists(dists, budget)
	rerank := func(id int32, coarseD float32) float32 {
		return -simd.DotF32(q, g.Vector(id))
	}
	entryD := make([]float32, 1)
	dists([]int32{g.entry}, entryD)
	ep, epD := g.entry, entryD[0]
	for l := int(g.maxLevel); l >= 1; l-- {
		w := st.s.search(g, ep, epD, 1, l, dists, filter, budget.MaxVisited, rerank)
		if len(w) == 0 {
			break
		}
		ep, epD = w[0].id, w[0].d
	}
	w := st.s.search(g, ep, epD, efQ, 0, dists, filter, budget.MaxVisited, rerank)
	if len(w) > k {
		w = w[:k]
	}
	hits := make([]Hit, 0, len(w))
	for _, c := range w {
		hits = append(hits, Hit{RowID: c.id, Score: -c.d})
	}
	return hits, nil
}
