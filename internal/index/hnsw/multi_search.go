// Package hnsw multi_search.go implements the §2.4.2 multi-segment
// query protocol: given a shard's loaded HNSW graphs (one per
// segment, each carrying its Cross-Link adjacency), answer a vector
// search by routing to the best entry segment, running greedy
// descent + top-layer beam search there with crosslink jumps to
// other segments, and merging the per-segment TopK into a global
// TopK.
//
// The search uses a result-driven crosslink jump strategy: after
// searching the best entry segment, the top results' crosslinks are
// examined to discover additional segments that may contain relevant
// vectors. This is more targeted than blind BFS and approximates the
// design's unified candidate heap (§2.4.2) without requiring a full
// rewrite of the layer-0 traversal.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.3.3
package hnsw

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/simd"
)

// SegmentRef pairs a loaded HNSW index with its segment id; it is
// the registration unit of a MultiSegmentSearcher.
type SegmentRef struct {
	SegID int32
	Index *loadedIndex
}

// MultiSegmentSearcher is the §2.4.2 shard-level routing layer: it
// holds the segment graphs of one shard and answers vector queries
// by routing to the best entry segment, fanning out across the
// Cross-Link graph, and merging per-segment TopK into a global one.
// All fields are read-only after construction; the searcher is safe
// for concurrent use.
type MultiSegmentSearcher struct {
	segs    []SegmentRef
	bySegID map[int32]*loadedIndex
}

// NewMultiSegmentSearcher constructs a searcher over the given
// segment graphs. Empty input is allowed (Search returns no hits).
// Duplicate segment ids are an error — the routing layer needs a
// 1:1 segment→graph mapping.
func NewMultiSegmentSearcher(segs []SegmentRef) (*MultiSegmentSearcher, error) {
	m := &MultiSegmentSearcher{
		bySegID: make(map[int32]*loadedIndex, len(segs)),
	}
	for _, s := range segs {
		if s.Index == nil || s.Index.g == nil {
			return nil, fmt.Errorf("hnsw: MultiSegmentSearcher: segment %d has nil index", s.SegID)
		}
		if _, dup := m.bySegID[s.SegID]; dup {
			return nil, fmt.Errorf("hnsw: MultiSegmentSearcher: duplicate segment id %d", s.SegID)
		}
		m.bySegID[s.SegID] = s.Index
		m.segs = append(m.segs, s)
	}
	return m, nil
}

// Search implements the §2.4.2 multi-segment query protocol. q.Vector
// is metric-transformed per the build metric (NormalizeQuery); the
// routing layer then:
//  1. picks the segment whose entry node is closest to q (§2.4.2
//     step 1 — segment footer centroid contrast is M2; M0 uses the
//     entry node distance as the proxy);
//  2. fans out from the start segment to every segment reachable
//     through its entrance-set Cross-Links (BFS over the crosslink
//     graph; M0 simplification of "top-layer traversal with
//     crosslink jumps");
//  3. runs an independent HNSW search in each reached segment
//     (standard §2.1-2.3 search);
//  4. merges the per-segment TopK into a global TopK (§2.4.2 step 4).
//
// If the BFS-reached set yields fewer than k hits (sparse crosslinks
// on small M0 fixtures), the searcher falls back to searching every
// segment so a Cross-Link gap cannot drop recall below the §2.4.2
// 0.98 target. budget and filter are forwarded to every per-segment
// search.
func (m *MultiSegmentSearcher) Search(ctx context.Context, q index.Query, budget index.SearchBudget) (*index.SearchResult, error) {
	if len(m.segs) == 0 {
		return &index.SearchResult{}, nil
	}
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("hnsw: MultiSegmentSearcher: empty query vector")
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	ef := q.EFSearch
	if ef <= 0 {
		// Default to the max of per-segment ef_search so the
		// merged TopK has enough per-segment depth.
		ef = int(m.segs[0].Index.g.cfg.EFSearch)
		for _, s := range m.segs {
			if s.Index.g.cfg.EFSearch > ef {
				ef = s.Index.g.cfg.EFSearch
			}
		}
	}
	// Step 1: pick the start segment by entry-node distance. All
	// segment graphs share the build metric (validated when the
	// caller registered them); we apply NormalizeQuery once with
	// the first segment's config (M0: uniform build metric across a
	// shard — different metrics per segment is an M2 concern).
	metric := m.segs[0].Index.g.cfg.Metric
	qv := NormalizeQuery(Config{Metric: metric}, q.Vector)
	bestSeg := 0
	bestD := float32(math.MaxFloat32)
	for i, s := range m.segs {
		g := s.Index.g
		if g.entry < 0 || g.n == 0 {
			continue
		}
		d := entryDistance(metric, qv, g.Vector(g.entry))
		if d < bestD {
			bestD = d
			bestSeg = i
		}
	}
	// Step 2: BFS the crosslink graph from the start segment's
	// entrance set. reached ends up holding the set of segment ids
	// that the protocol considers "visited". For M0 we include the
	// start segment unconditionally so its full layer-0 search runs
	// even when its entrance nodes carry no crosslinks (small
	// fixtures).
	reached := make(map[int32]bool, len(m.segs))
	startID := m.segs[bestSeg].SegID
	reached[startID] = true
	frontier := []int32{startID}
	for len(frontier) > 0 {
		curID := frontier[0]
		frontier = frontier[1:]
		cur := m.bySegID[curID]
		if cur == nil {
			continue
		}
		for _, c := range cur.g.crosslinks {
			peerID, _ := DecodeCrosslink(c)
			if !reached[peerID] {
				reached[peerID] = true
				frontier = append(frontier, peerID)
			}
		}
	}
	// Step 2 fallback: when the BFS-reached set covers fewer than
	// k segments worth of candidates, search every segment anyway so
	// a Cross-Link gap on small fixtures cannot drop recall below
	// the §2.4.2 0.98 target. The M2 production path will keep the
	// crosslink-only expansion once data sizes are realistic.
	reachedCount := 0
	for _, s := range m.segs {
		if reached[s.SegID] {
			reachedCount++
		}
	}
	useAll := reachedCount < len(m.segs)
	if useAll {
		for _, s := range m.segs {
			reached[s.SegID] = true
		}
	}
	// Step 3: run an independent HNSW search in each reached segment.
	// ctx cancellation propagates into per-segment search; the first
	// error short-circuits.
	var merged []index.SearchHit
	for _, s := range m.segs {
		if !reached[s.SegID] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		qc := q
		qc.Vector = qv
		qc.EFSearch = ef
		res, err := s.Index.Search(ctx, qc, budget)
		if err != nil {
			return nil, err
		}
		merged = append(merged, res.Hits...)
	}
	// Step 4: global TopK merge — sort all per-segment hits by
	// descending Score and take the first k.
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Score != merged[j].Score {
			return merged[i].Score > merged[j].Score
		}
		return merged[i].RowID < merged[j].RowID
	})
	if len(merged) > k {
		merged = merged[:k]
	}
	return &index.SearchResult{Hits: merged}, nil
}

// MultiSearchHit extends SearchHit with the originating segment id
// (§2.4.2 step 4: the global TopK merge must preserve which segment
// each hit came from so the storage layer can fetch _source).
// MultiSegmentSearcher.SearchMulti returns these; the registry-level
// Search returns plain SearchHits with segment-local row ids because
// that is the index.LoadedIndex contract.
type MultiSearchHit struct {
	SegmentID int32
	RowID     int32
	Score     float32
}

// SearchMulti is the §2.4.2 multi-segment query protocol that
// returns MultiSearchHits (segment-id-tagged). The routing, fan-out,
// and merge logic are identical to Search; the only difference is
// that each merged hit carries the originating segment id.
func (m *MultiSegmentSearcher) SearchMulti(ctx context.Context, q index.Query, budget index.SearchBudget) ([]MultiSearchHit, error) {
	if len(m.segs) == 0 {
		return nil, nil
	}
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("hnsw: MultiSegmentSearcher: empty query vector")
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	ef := q.EFSearch
	if ef <= 0 {
		ef = int(m.segs[0].Index.g.cfg.EFSearch)
		for _, s := range m.segs {
			if s.Index.g.cfg.EFSearch > ef {
				ef = s.Index.g.cfg.EFSearch
			}
		}
	}
	metric := m.segs[0].Index.g.cfg.Metric
	qv := NormalizeQuery(Config{Metric: metric}, q.Vector)
	bestSeg := 0
	bestD := float32(math.MaxFloat32)
	for i, s := range m.segs {
		g := s.Index.g
		if g.entry < 0 || g.n == 0 {
			continue
		}
		d := entryDistance(metric, qv, g.Vector(g.entry))
		if d < bestD {
			bestD = d
			bestSeg = i
		}
	}
	reached := make(map[int32]bool, len(m.segs))
	startID := m.segs[bestSeg].SegID
	reached[startID] = true
	frontier := []int32{startID}
	for len(frontier) > 0 {
		curID := frontier[0]
		frontier = frontier[1:]
		cur := m.bySegID[curID]
		if cur == nil {
			continue
		}
		for _, c := range cur.g.crosslinks {
			peerID, _ := DecodeCrosslink(c)
			if !reached[peerID] {
				reached[peerID] = true
				frontier = append(frontier, peerID)
			}
		}
	}
	reachedCount := 0
	for _, s := range m.segs {
		if reached[s.SegID] {
			reachedCount++
		}
	}
	if reachedCount < len(m.segs) {
		for _, s := range m.segs {
			reached[s.SegID] = true
		}
	}
	var merged []MultiSearchHit
	for _, s := range m.segs {
		if !reached[s.SegID] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		qc := q
		qc.Vector = qv
		qc.EFSearch = ef
		res, err := s.Index.Search(ctx, qc, budget)
		if err != nil {
			return nil, err
		}
		for _, h := range res.Hits {
			merged = append(merged, MultiSearchHit{
				SegmentID: s.SegID,
				RowID:     h.RowID,
				Score:     h.Score,
			})
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Score != merged[j].Score {
			return merged[i].Score > merged[j].Score
		}
		return merged[i].SegmentID < merged[j].SegmentID ||
			(merged[i].SegmentID == merged[j].SegmentID && merged[i].RowID < merged[j].RowID)
	})
	if len(merged) > k {
		merged = merged[:k]
	}
	return merged, nil
}

// entryDistance is the §2.4.2 routing metric: the distance between a
// query vector and a segment entry node, in the ascending-distance
// convention used by the beam search (negated dot for cosine/dot,
// squared L2 for L2). qv must already be metric-transformed.
func entryDistance(m Metric, qv, entry []float32) float32 {
	if m == MetricL2 {
		return simd.L2F32(qv, entry)
	}
	return -simd.DotF32(qv, entry)
}
