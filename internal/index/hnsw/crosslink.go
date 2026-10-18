// Package hnsw crosslink.go implements the §2.4.2 inter-segment
// Cross-Link: after a segment flush, a background task builds the
// top-layer adjacency between the new segment's entrance set and each
// neighbor's entrance set (mutual k-NN, k=4). The encoded crosslinks
// are stored in entry.bin (§2.1) and consumed by the multi-segment
// search protocol (multi_search.go).
//
// M0 scope: the build is run synchronously from tests / the storage
// engine's post-flush hook; M2 will schedule it as a true background
// task. The mutual k-NN algorithm is the textbook one (a→b is kept
// only when a is in b's k-nearest and b is in a's k-nearest), so the
// resulting inter-segment graph is symmetric when both ends run the
// build.
package hnsw

import (
	"fmt"
	"math"
	"sort"

	"github.com/unisearch-db/unisearch/internal/simd"
)

// CrossLinkK is the mutual k-NN parameter (§2.4.2: "互近邻连接 k=4").
const CrossLinkK = 4

// EncodeCrosslink packs a peer (SegmentID, NodeID) pair into the
// int64 layout stored in entry.bin (§2.4.2: "对端 SegmentID<<32 |
// node_id"). NodeID is treated as unsigned so all in-segment node
// ids (>= 0) round-trip losslessly.
func EncodeCrosslink(segID, nodeID int32) int64 {
	return int64(uint32(segID))<<32 | int64(uint32(nodeID))
}

// DecodeCrosslink is the inverse of EncodeCrosslink.
func DecodeCrosslink(c int64) (segID, nodeID int32) {
	return int32(uint64(c) >> 32), int32(uint64(c) & 0xFFFFFFFF)
}

// entranceSetSize returns the §2.4.2 entrance-set cardinality
// "≈ 2×√N" for a graph of n nodes, floored at 1 and capped at n.
// The cap matters for tiny M0 fixtures where 2×√N can exceed n.
func entranceSetSize(n int) int {
	if n <= 0 {
		return 0
	}
	s := 2 * int(math.Sqrt(float64(n)))
	if s < 1 {
		s = 1
	}
	if s > n {
		s = n
	}
	return s
}

// topEntranceSet returns the §2.4.2 entrance set for a graph: the
// highest-degree top-layer nodes, sized to ≈ 2×√N. "Top-layer" here
// means the highest level present in the graph; nodes with the most
// neighbors at that level are the densest cut points, which is what
// the design calls "高度数节点". When the graph has only level 0
// (n < 2 or every node was assigned level 0 by the level RNG), the
// entrance set falls back to the level-0 highest-degree nodes. The
// returned slice is ordered by descending degree; ties break by
// ascending node id for determinism.
func topEntranceSet(g *Graph) []int32 {
	if g == nil || g.n == 0 {
		return nil
	}
	want := entranceSetSize(int(g.n))
	if want == 0 {
		return nil
	}
	// Walk the highest non-empty level. Levels above maxLevel have
	// zero-degree ranges by CSR construction (offsets are flat), so
	// we just sit at maxLevel and read degrees.
	level := int(g.maxLevel)
	type nd struct {
		id    int32
		deg   int
	}
	cands := make([]nd, 0, int(g.n))
	for id := int32(0); id < g.n; id++ {
		// A node only has neighbors at level l when l <= levels[id].
		// Skip nodes whose top level is below the chosen cut level —
		// they have no top-layer presence.
		if int(g.levels[id]) < level {
			continue
		}
		deg := len(g.layerNeighbors(id, level))
		cands = append(cands, nd{id: id, deg: deg})
	}
	// Fall back to level 0 when the chosen level has no candidates
	// (small graphs where every node got level 0).
	if len(cands) == 0 {
		level = 0
		for id := int32(0); id < g.n; id++ {
			deg := len(g.layerNeighbors(id, 0))
			cands = append(cands, nd{id: id, deg: deg})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].deg != cands[j].deg {
			return cands[i].deg > cands[j].deg
		}
		return cands[i].id < cands[j].id
	})
	if want > len(cands) {
		want = len(cands)
	}
	out := make([]int32, want)
	for i, c := range cands[:want] {
		out[i] = c.id
	}
	return out
}

// peerEntrance is one node of a neighbor's entrance set, carrying
// the back-pointer to the owning segment so BuildCrosslinks can
// encode the crosslink target.
type peerEntrance struct {
	segID  int32
	nodeID int32
	vec    []float32
}

// BuildCrosslinks computes the §2.4.2 inter-segment Cross-Link
// adjacency for segNew: the new segment's entrance-set nodes are
// linked to the k=4 nearest entrance-set nodes of each neighbor
// segment (mutual k-NN: the link is kept only when the peer also
// ranks the segNew node among its k=4 nearest). The returned slice
// is the encoded crosslinks (peer SegmentID<<32 | nodeID) for segNew,
// suitable for Graph.SetCrosslinks.
//
// peers may be empty (single-segment shard): the result is an empty
// slice. All graphs must share the same dims and metric; mismatch is
// an error so a routing bug does not silently produce garbage links.
// segNewID is the segment id of segNew and is informational — it
// is not embedded in the returned crosslinks (peers already carry
// their own segment ids).
func BuildCrosslinks(segNew *Graph, segNewID int32, peers []*loadedIndex, peerIDs []int32) ([]int64, error) {
	if segNew == nil {
		return nil, fmt.Errorf("hnsw: BuildCrosslinks: segNew is nil")
	}
	if len(peers) != len(peerIDs) {
		return nil, fmt.Errorf("hnsw: BuildCrosslinks: peers/peerIDs length mismatch %d vs %d",
			len(peers), len(peerIDs))
	}
	// Stash segNew's id so SetCrosslinks below is consistent; the
	// caller can override afterwards if it prefers.
	segNew.segID = segNewID
	if segNew.n == 0 || len(peers) == 0 {
		return nil, nil
	}
	// Validate peer graphs share segNew's metric/dims and gather
	// each peer's entrance set as a flat peerEntrance slice.
	segNewSet := topEntranceSet(segNew)
	if len(segNewSet) == 0 {
		return nil, nil
	}
	var flatPeers []peerEntrance
	peerSlices := make([][]peerEntrance, len(peers))
	for i, li := range peers {
		if li == nil || li.g == nil {
			return nil, fmt.Errorf("hnsw: BuildCrosslinks: peer %d is nil", i)
		}
		pg := li.g
		if pg.dims != segNew.dims {
			return nil, fmt.Errorf("hnsw: crosslink dims mismatch: %d vs %d", pg.dims, segNew.dims)
		}
		if pg.cfg.Metric != segNew.cfg.Metric {
			return nil, fmt.Errorf("hnsw: crosslink metric mismatch")
		}
		es := topEntranceSet(pg)
		peerSlices[i] = make([]peerEntrance, 0, len(es))
		for _, nodeID := range es {
			peerSlices[i] = append(peerSlices[i], peerEntrance{
				segID:  peerIDs[i],
				nodeID: nodeID,
				vec:    pg.Vector(nodeID),
			})
		}
		flatPeers = append(flatPeers, peerSlices[i]...)
	}
	// Pre-compute the segNew entrance vectors in a flat block so we
	// can run mutual k-NN over flatPeers × segNewSet cheaply.
	segNewVecs := make([]float32, len(segNewSet)*segNew.dims)
	for i, id := range segNewSet {
		copy(segNewVecs[i*segNew.dims:(i+1)*segNew.dims], segNew.Vector(id))
	}
	// distSegToPeer: distance between segNew node i and peer p, under
	// the build metric. Cosine: segNew/peer vectors are already
	// normalized (BuildGraph §2.2 stores the normalized copy), so dot
	// is the cosine and -dot is the ascending-distance form.
	distSegToPeer := func(segIdx int, p peerEntrance) float32 {
		q := segNewVecs[segIdx*segNew.dims : (segIdx+1)*segNew.dims]
		if segNew.cfg.Metric == MetricL2 {
			return simd.L2F32(q, p.vec)
		}
		return -simd.DotF32(q, p.vec)
	}
	// knnFromSeg: for a segNew node, the k nearest peers (across all
	// neighbor segments combined). Returns indices into flatPeers.
	knnFromSeg := func(segIdx int, k int) []int {
		if k > len(flatPeers) {
			k = len(flatPeers)
		}
		type pd struct {
			idx int
			d   float32
		}
		arr := make([]pd, len(flatPeers))
		for i, p := range flatPeers {
			arr[i] = pd{idx: i, d: distSegToPeer(segIdx, p)}
		}
		sort.Slice(arr, func(a, b int) bool {
			if arr[a].d != arr[b].d {
				return arr[a].d < arr[b].d
			}
			return arr[a].idx < arr[b].idx
		})
		out := make([]int, 0, k)
		for i := 0; i < k; i++ {
			out = append(out, arr[i].idx)
		}
		return out
	}
	// knnFromPeer: for a peer p, the k nearest segNew entrance nodes.
	// Used for the "mutual" half of mutual k-NN.
	knnFromPeer := func(p peerEntrance, k int) []int {
		if k > len(segNewSet) {
			k = len(segNewSet)
		}
		type pd struct {
			idx int
			d   float32
		}
		arr := make([]pd, len(segNewSet))
		for i := range segNewSet {
			q := segNewVecs[i*segNew.dims : (i+1)*segNew.dims]
			var d float32
			if segNew.cfg.Metric == MetricL2 {
				d = simd.L2F32(q, p.vec)
			} else {
				d = -simd.DotF32(q, p.vec)
			}
			arr[i] = pd{idx: i, d: d}
		}
		sort.Slice(arr, func(a, b int) bool {
			if arr[a].d != arr[b].d {
				return arr[a].d < arr[b].d
			}
			return arr[a].idx < arr[b].idx
		})
		out := make([]int, 0, k)
		for i := 0; i < k; i++ {
			out = append(out, arr[i].idx)
		}
		return out
	}
	// For each segNew entrance node, find its k nearest peers. For
	// each such peer, keep the link only when the peer also ranks the
	// segNew node among its k nearest (mutual k-NN). Dedup by peer
	// (segID, nodeID) so a segNew node with multiple identical peer
	// hits gets one crosslink per peer.
	k := CrossLinkK
	var links []int64
	seen := make(map[int64]struct{}, len(segNewSet)*k)
	for i := range segNewSet {
		peerNN := knnFromSeg(i, k)
		for _, pi := range peerNN {
			p := flatPeers[pi]
			segNN := knnFromPeer(p, k)
			mutual := false
			for _, si := range segNN {
				if si == i {
					mutual = true
					break
				}
			}
			if !mutual {
				continue
			}
			enc := EncodeCrosslink(p.segID, p.nodeID)
			if _, dup := seen[enc]; dup {
				continue
			}
			seen[enc] = struct{}{}
			links = append(links, enc)
		}
	}
	return links, nil
}
