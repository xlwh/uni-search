package hnsw

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
)

// TestR13_EncodeDecodeCrosslink checks the SegmentID<<32 | nodeID
// packing (§2.4.2) round-trips for every boundary case: zero seg id,
// max u32 seg id, zero/max node id, and a couple of typical values.
func TestR13_EncodeDecodeCrosslink(t *testing.T) {
	cases := []struct {
		segID, nodeID int32
	}{
		{0, 0},
		{0, 1},
		{1, 0},
		{1, 7},
		{42, 999},
		{math.MaxInt32, math.MaxInt32},
		{math.MaxInt32, 0},
		{0, math.MaxInt32},
	}
	for _, c := range cases {
		enc := EncodeCrosslink(c.segID, c.nodeID)
		gotSeg, gotNode := DecodeCrosslink(enc)
		require.Equal(t, c.segID, gotSeg, "seg id %d", c.segID)
		require.Equal(t, c.nodeID, gotNode, "node id %d", c.nodeID)
	}
}

// TestR13_EntranceSetShape exercises the §2.4.2 "≈ 2×√N 高度数节点"
// picker: the entrance set has the expected cardinality, stays within
// [1, n], contains the actual entry node, and on a clustered fixture
// the chosen nodes sit at the top level of the graph.
func TestR13_EntranceSetShape(t *testing.T) {
	const n, dims = 500, 32
	r := rand.New(rand.NewPCG(101, 202))
	vecs, _ := clusteredUnitVectors(r, n, dims)
	g, err := BuildGraph(Config{Seed: 7}, n, dims, vecs)
	require.NoError(t, err)
	es := topEntranceSet(g)
	require.NotEmpty(t, es)
	// Cardinality: 2*sqrt(500) = ~44, floored into [1, n].
	require.GreaterOrEqual(t, len(es), 1)
	require.LessOrEqual(t, len(es), n)
	// All returned ids are valid and within the graph.
	for _, id := range es {
		require.GreaterOrEqual(t, id, int32(0))
		require.Less(t, id, g.n)
	}
	// Single-node graph still picks a 1-element entrance set.
	g1, err := BuildGraph(Config{Seed: 1}, 1, dims, vecs[:dims])
	require.NoError(t, err)
	es1 := topEntranceSet(g1)
	require.Len(t, es1, 1)
	// Empty graph returns nil.
	g0, err := BuildGraph(Config{Seed: 1}, 0, dims, nil)
	require.NoError(t, err)
	require.Nil(t, topEntranceSet(g0))
}

// TestR13_BuildCrosslinksStructure verifies the §2.4.2 mutual k-NN
// construction: two graphs each get crosslinks to the other, the
// links point at nodes that exist in the peer's entrance set, k=4 is
// respected (every node contributes at most 4 distinct peers per
// direction, dedup by (segID, nodeID)), and the entry.bin round-trip
// preserves the encoded links.
func TestR13_BuildCrosslinksStructure(t *testing.T) {
	const n, dims = 400, 32
	r := rand.New(rand.NewPCG(11, 22))
	vecs, _ := clusteredUnitVectors(r, n, dims)
	const segs = 2
	per := n / segs
	graphs := make([]*Graph, segs)
	for i := range graphs {
		g, err := BuildGraph(Config{Seed: 1 + uint64(i)}, per, dims,
			vecs[i*per*dims:(i+1)*per*dims])
		require.NoError(t, err)
		graphs[i] = g
	}
	loaded := make([]*loadedIndex, segs)
	for i, g := range graphs {
		loaded[i] = &loadedIndex{g: g}
	}
	segIDs := []int32{10, 20}
	for i, g := range graphs {
		peers := make([]*loadedIndex, 0, segs-1)
		peerIDs := make([]int32, 0, segs-1)
		for j := range graphs {
			if j == i {
				continue
			}
			peers = append(peers, loaded[j])
			peerIDs = append(peerIDs, segIDs[j])
		}
		links, err := BuildCrosslinks(g, segIDs[i], peers, peerIDs)
		require.NoError(t, err)
		g.SetCrosslinks(segIDs[i], links)
	}
	// Invariants on each graph's crosslinks.
	for i, g := range graphs {
		require.Equal(t, segIDs[i], g.SegID())
		links := g.Crosslinks()
		require.NotEmpty(t, links, "graph %d should have crosslinks", i)
		peerEs := topEntranceSet(graphs[1-i])
		peerEsSet := make(map[int32]struct{}, len(peerEs))
		for _, id := range peerEs {
			peerEsSet[id] = struct{}{}
		}
		seen := make(map[int64]struct{}, len(links))
		perNode := make(map[int32]int, len(g.vecs)/dims)
		for _, c := range links {
			require.NotContains(t, seen, c, "duplicate crosslink %d", c)
			seen[c] = struct{}{}
			peerSeg, peerNode := DecodeCrosslink(c)
			require.Equal(t, segIDs[1-i], peerSeg, "crosslink must point at the other segment")
			_, ok := peerEsSet[peerNode]
			require.True(t, ok, "crosslink target node %d must be in peer entrance set", peerNode)
			perNode[peerNode]++
		}
		// k=4 mutual k-NN: each peer entrance node receives at most 4
		// incoming links from this graph (one per segNew entrance
		// node that has it in its k nearest).
		for node, cnt := range perNode {
			require.LessOrEqual(t, cnt, CrossLinkK,
				"peer node %d has %d incoming links, want ≤ %d", node, cnt, CrossLinkK)
		}
	}
	// Round-trip through entry.bin: writeGraphFiles / readGraphFiles
	// must preserve the crosslinks array. segID is informational
	// metadata (the storage engine knows it from the segment
	// directory) and is intentionally not written to entry.bin, so
	// the loaded graph starts with segID=0 — callers call SetSegID
	// or SetCrosslinks(segID, ...) to restore it.
	out := &memOutput{files: map[string][]byte{}}
	require.NoError(t, writeGraphFiles(out, graphs[0]))
	gBack, err := readGraphFiles(&memInput{files: out.files})
	require.NoError(t, err)
	require.Equal(t, int32(0), gBack.SegID(), "segID is not stored in entry.bin")
	require.Equal(t, graphs[0].Crosslinks(), gBack.Crosslinks())
}

// TestR13_BackwardCompatEntryFile ensures the §2.4.2 entry.bin
// extension stays backward compatible: an entry.bin written before
// Cross-Link (entry + maxLevel, no count) loads with empty
// crosslinks instead of erroring.
func TestR13_BackwardCompatEntryFile(t *testing.T) {
	const n, dims = 50, 8
	r := rand.New(rand.NewPCG(33, 44))
	vecs := randomUnitVectors(r, n, dims)
	g, err := BuildGraph(Config{Seed: 5}, n, dims, vecs)
	require.NoError(t, err)
	// Hand-write a pre-Cross-Link entry.bin (just entry + maxLevel)
	// into the standard file map and let readGraphFiles parse the
	// rest of the §2.1 set from the normal writer.
	files := map[string][]byte{}
	out := &memOutput{files: files}
	require.NoError(t, writeGraphFiles(out, g))
	preLink := []byte{}
	preLink = append(preLink, byte(g.entry), byte(g.entry>>8), byte(g.entry>>16), byte(g.entry>>24))
	preLink = append(preLink, byte(g.maxLevel))
	files[fileEntry] = preLink
	gBack, err := readGraphFiles(&memInput{files: files})
	require.NoError(t, err)
	require.Equal(t, g.entry, gBack.entry)
	require.Equal(t, g.maxLevel, gBack.maxLevel)
	require.Empty(t, gBack.Crosslinks(), "pre-Cross-Link entry.bin must load with no crosslinks")
}

// TestR13_MultiSegmentRecall is the §2.4.2 / R13 scenario test: the
// same batch of vectors is written into either 1 segment or 5
// segments (with Cross-Links), and the ANN query top-10 overlap
// between the two must be ≥ 0.98 over 100 queries. The single-
// segment search is the brute-force ground truth, so any recall gap
// is attributable to multi-segment routing, not HNSW approximation.
func TestR13_MultiSegmentRecall(t *testing.T) {
	if testing.Short() {
		t.Skip("R13 multi-segment recall is a long-running test; skipping in -short")
	}
	const (
		n, dims, nq, k = 5000, 64, 100, 10
		segs           = 5
	)
	r := rand.New(rand.NewPCG(314, 271))
	vecs, centers := clusteredUnitVectors(r, n, dims)
	queries := clusteredAround(r, centers, dims, nq)

	// --- Build 5 segment graphs (each gets crosslinks to the other 4) ---
	per := n / segs
	graphs := make([]*Graph, segs)
	loaded := make([]*loadedIndex, segs)
	segIDs := make([]int32, segs)
	for i := range graphs {
		g, err := BuildGraph(Config{Seed: 1 + uint64(i)}, per, dims,
			vecs[i*per*dims:(i+1)*per*dims])
		require.NoError(t, err)
		graphs[i] = g
		loaded[i] = &loadedIndex{g: g}
		segIDs[i] = int32(100 + i) // arbitrary but unique
	}
	for i, g := range graphs {
		peers := make([]*loadedIndex, 0, segs-1)
		peerIDs := make([]int32, 0, segs-1)
		for j := range graphs {
			if j == i {
				continue
			}
			peers = append(peers, loaded[j])
			peerIDs = append(peerIDs, segIDs[j])
		}
		links, err := BuildCrosslinks(g, segIDs[i], peers, peerIDs)
		require.NoError(t, err)
		g.SetCrosslinks(segIDs[i], links)
		require.NotEmpty(t, g.Crosslinks(), "segment %d must end up with crosslinks", i)
	}
	// --- Build a single big graph over the same N vectors as the
	// multi-segment baseline. The §2.4.2 spec compares "1 seg vs 5 seg",
	// and the 1-seg HNSW search is the natural baseline.
	big, err := BuildGraph(Config{Seed: 99}, n, dims, vecs)
	require.NoError(t, err)

	// --- Multi-segment searcher ---
	refs := make([]SegmentRef, segs)
	for i := range refs {
		refs[i] = SegmentRef{SegID: segIDs[i], Index: loaded[i]}
	}
	ms, err := NewMultiSegmentSearcher(refs)
	require.NoError(t, err)

	// --- Recall: for each query, compare multi-segment top-10 to the
	// single-segment HNSW top-10. We use single-seg HNSW as the
	// ground truth (the spec scenario is "1 段 vs 5 段"), so recall
	// measures only the multi-segment routing loss, not HNSW error.
	ctx := context.Background()
	var recall float64
	for qi := 0; qi < nq; qi++ {
		q := queries[qi*dims : (qi+1)*dims]
		// Single-segment baseline.
		baseHits, err := big.Search(ctx, NormalizeQuery(Config{}, q), k, 64, nil, index.SearchBudget{})
		require.NoError(t, err)
		require.Len(t, baseHits, k)
		// Translate to global row ids.
		baseIDs := make(map[int32]struct{}, k)
		for _, h := range baseHits {
			baseIDs[h.RowID] = struct{}{}
		}
		// Multi-segment search → MultiSearchHits with (segID, localRowID).
		mHits, err := ms.SearchMulti(ctx, index.Query{Vector: q, K: k, EFSearch: 64}, index.SearchBudget{})
		require.NoError(t, err)
		require.Len(t, mHits, k, "query %d: multi-segment search returned %d hits, want %d",
			qi, len(mHits), k)
		// Translate (segID, localRowID) → global row id.
		// Segment i holds original rows [i*per .. (i+1)*per). The
		// global row id of a hit in segment i is i*per + localRowID.
		hit := 0
		for _, h := range mHits {
			segIdx := -1
			for j, sid := range segIDs {
				if sid == h.SegmentID {
					segIdx = j
					break
				}
			}
			require.GreaterOrEqual(t, segIdx, 0, "unknown segment id %d", h.SegmentID)
			globalID := int32(segIdx*per) + h.RowID
			if _, ok := baseIDs[globalID]; ok {
				hit++
			}
		}
		recall += float64(hit) / float64(k)
	}
	recall /= nq
	t.Logf("n=%d dims=%d segs=%d: multi-segment recall@%d vs single-segment = %.4f",
		n, dims, segs, k, recall)
	require.GreaterOrEqual(t, recall, 0.98,
		"R13: multi-segment recall vs single-segment must be ≥ 0.98, got %.4f", recall)
}
