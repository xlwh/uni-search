package inverted

import (
	"container/heap"
	"context"
	"sort"

	"github.com/RoaringBitmap/roaring"

	"github.com/unisearch-db/unisearch/internal/index"
)

// WANDScorer implements the WAND (Weak AND) / Block-Max WAND algorithm
// for top-K BM25 retrieval (design 02-index-engine §3.4). Instead of
// scoring every document matching any query term, WAND uses per-term
// upper bounds to prune documents that cannot make the top-K.
//
// The algorithm:
//  1. Compute each term's max BM25 contribution (upper bound).
//  2. Maintain sorted iterators over all terms' posting lists.
//  3. Find a pivot document where the cumulative upper bound exceeds
//     the current K-th best score.
//  4. Only fully evaluate documents at or above the pivot.
//  5. Skip documents below the pivot by advancing iterators.
//
// For single-term queries, WAND degrades to a linear scan (no pruning
// benefit), so the caller should use the existing ScoreDocs path.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.2
type WANDScorer struct {
	terms     []wandTerm
	stats     SegmentStats
	bm25      BM25Params
	k         int
	threshold float64 // current K-th best score (0 = no threshold yet)
}

// wandTerm is one query term's WAND state.
type wandTerm struct {
	termID     TermID
	pl         *PostingList
	upperBound float64 // max BM25 contribution of this term
	iter       roaring.IntPeekable
	currentDoc uint32 // current doc ID
	idx        int    // current index into tfs/fldLens
	done       bool
}

// wandHeap is a min-heap of (score, docID) for top-K selection.
type wandHeap []wandCand

type wandCand struct {
	score float64
	docID uint32
}

func (h wandHeap) Len() int { return len(h) }
func (h wandHeap) Less(i, j int) bool {
	if h[i].score != h[j].score {
		return h[i].score < h[j].score // min-heap: worst at top
	}
	return h[i].docID > h[j].docID // tie-break: larger docID = worse
}
func (h wandHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *wandHeap) Push(x any)   { *h = append(*h, x.(wandCand)) }
func (h *wandHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// NewWANDScorer creates a WAND scorer for the given terms. Terms with no
// posting list (not in dictionary) are silently dropped. If all terms
// are dropped, Search returns an empty result.
func NewWANDScorer(dict *TermDict, postings map[TermID]*PostingList, stats SegmentStats, bm25 BM25Params, terms [][]byte, k int) *WANDScorer {
	if k <= 0 {
		k = 10
	}
	w := &WANDScorer{stats: stats, bm25: bm25, k: k}
	for _, term := range terms {
		id, ok := dict.Get(term)
		if !ok {
			continue
		}
		pl := postings[id]
		if pl == nil || pl.docIDs == nil || pl.DocFreq() == 0 {
			continue
		}
		wt := wandTerm{
			termID: id,
			pl:     pl,
			iter:   pl.docIDs.Iterator(),
		}
		wt.upperBound = computeUpperBound(pl, stats, bm25)
		if wt.iter.HasNext() {
			wt.currentDoc = wt.iter.Next()
			wt.idx = 0
		} else {
			wt.done = true
		}
		w.terms = append(w.terms, wt)
	}
	return w
}

// computeUpperBound computes the maximum BM25 score contribution of a
// term across all documents in its posting list. The max occurs when
// TF is highest and field length is shortest.
func computeUpperBound(pl *PostingList, stats SegmentStats, bm25 BM25Params) float64 {
	df := pl.DocFreq()
	if df == 0 {
		return 0
	}
	// Find max TF and min field length in the posting list.
	maxTF := 0
	minFL := int(^uint(0) >> 1) // max int
	for i, tf := range pl.tfs {
		if int(tf) > maxTF {
			maxTF = int(tf)
		}
		if i < len(pl.fldLens) {
			fl := int(pl.fldLens[i])
			if fl > 0 && fl < minFL {
				minFL = fl
			}
		}
	}
	if maxTF == 0 {
		maxTF = 1
	}
	if minFL == int(^uint(0)>>1) {
		minFL = int(stats.AvgFieldLen)
		if minFL == 0 {
			minFL = 1
		}
	}
	return stats.ScoreTerm(bm25, maxTF, minFL, df)
}

// Search runs the WAND algorithm and returns the top-K results.
func (w *WANDScorer) Search(ctx context.Context) (*index.SearchResult, error) {
	if len(w.terms) == 0 {
		return &index.SearchResult{}, nil
	}
	h := &wandHeap{}
	for len(w.terms) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Sort terms by current doc ID (ascending). Done terms go to end.
		sort.Slice(w.terms, func(i, j int) bool {
			if w.terms[i].done {
				return false
			}
			if w.terms[j].done {
				return true
			}
			return w.terms[i].currentDoc < w.terms[j].currentDoc
		})
		// Remove done terms.
		for len(w.terms) > 0 && w.terms[len(w.terms)-1].done {
			w.terms = w.terms[:len(w.terms)-1]
		}
		if len(w.terms) == 0 {
			break
		}
		// Find pivot: the term where cumulative upper bound >= threshold.
		pivotDoc := uint32(0)
		cumBound := 0.0
		pivotIdx := -1
		for i, wt := range w.terms {
			cumBound += wt.upperBound
			if cumBound > w.threshold {
				pivotDoc = wt.currentDoc
				pivotIdx = i
				break
			}
		}
		if pivotIdx < 0 {
			// No term can beat the threshold; advance all to end.
			for i := range w.terms {
				w.terms[i].done = true
			}
			continue
		}
		// Check if all terms at pivotDoc are at the same doc ID.
		allAtPivot := true
		for i := 0; i <= pivotIdx; i++ {
			if w.terms[i].currentDoc != pivotDoc {
				allAtPivot = false
				break
			}
		}
		if allAtPivot {
			// Fully evaluate the pivot document.
			score := w.scoreDoc(pivotDoc, pivotIdx)
			if h.Len() < w.k {
				heap.Push(h, wandCand{score: score, docID: pivotDoc})
				if h.Len() == w.k {
					w.threshold = h.Top().(wandCand).score
				}
			} else if score > h.Top().(wandCand).score {
				heap.Pop(h)
				heap.Push(h, wandCand{score: score, docID: pivotDoc})
				w.threshold = h.Top().(wandCand).score
			}
			// Advance ALL terms at pivotDoc (not just 0..pivotIdx,
			// because terms after pivot may also be at the same doc).
			for i := 0; i < len(w.terms); i++ {
				if w.terms[i].currentDoc == pivotDoc {
					w.advanceTerm(i)
				}
			}
		} else {
			// Advance terms before pivot to pivotDoc.
			for i := 0; i < pivotIdx; i++ {
				if w.terms[i].currentDoc < pivotDoc {
					w.advanceTermTo(i, pivotDoc)
				}
			}
		}
	}
	// Extract results from heap (sorted by descending score).
	out := make([]wandCand, 0, h.Len())
	for h.Len() > 0 {
		out = append(out, heap.Pop(h).(wandCand))
	}
	// Reverse to get descending score order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	hits := make([]index.SearchHit, len(out))
	for i, c := range out {
		hits[i] = index.SearchHit{RowID: int32(c.docID), Score: float32(c.score)}
	}
	return &index.SearchResult{Hits: hits}, nil
}

// scoreDoc computes the full BM25 score for a document by summing
// contributions from ALL terms that contain it (not just 0..pivotIdx,
// because terms after the pivot may also contain the same docID).
func (w *WANDScorer) scoreDoc(docID uint32, pivotIdx int) float64 {
	var total float64
	for i := 0; i < len(w.terms); i++ {
		if w.terms[i].currentDoc != docID || w.terms[i].done {
			continue
		}
		pl := w.terms[i].pl
		idx := w.terms[i].idx
		if idx < 0 || idx >= len(pl.tfs) {
			continue
		}
		tf := int(pl.tfs[idx])
		fldLen := int(pl.fldLens[idx])
		df := pl.DocFreq()
		total += w.stats.ScoreTerm(w.bm25, tf, fldLen, df)
	}
	return total
}

// advanceTerm advances term i to its next doc ID.
func (w *WANDScorer) advanceTerm(i int) {
	if w.terms[i].iter.HasNext() {
		w.terms[i].currentDoc = w.terms[i].iter.Next()
		w.terms[i].idx++
	} else {
		w.terms[i].done = true
	}
}

// advanceTermTo advances term i to the first doc ID >= target.
// Uses manual advance (not AdvanceIfNeeded) to correctly track idx.
func (w *WANDScorer) advanceTermTo(i int, target uint32) {
	for w.terms[i].currentDoc < target && !w.terms[i].done {
		w.advanceTerm(i)
	}
}

// Top method for heap to access the minimum element.
func (h wandHeap) Top() any { return h[0] }
