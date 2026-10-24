// Package inverted sparse_index.go implements the sparse vector inverted
// index for SPLADE/BGE-M3 style retrieval. Unlike the text BM25 inverted
// index which stores integer TF, the sparse vector index stores float
// weights per dimension, enabling dot-product scoring with WAND pruning.
//
// @spec: ./archive/2026-10-23-qdrant-features-import/02_spec.md#2.1
package inverted

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/unisearch-db/unisearch/internal/index"
)

// WeightPrecision controls the storage precision of sparse vector weights.
type WeightPrecision uint8

const (
	WeightF32 WeightPrecision = 0 // float32 exact (4 bytes/weight)
	WeightF16 WeightPrecision = 1 // float16 compressed (2 bytes/weight)
	WeightU8  WeightPrecision = 2 // uint8 quantized (1 byte/weight)
)

// WeightedPostingList stores a sparse vector dimension's posting list with
// float weights. Unlike the text PostingList which uses uint8 TF, this
// stores float32 weights for dot-product scoring.
type WeightedPostingList struct {
	termID    uint64    // dimension ID (from FST dict)
	docIDs    []int32   // ordered row IDs
	weights   []float32 // per-doc weights (parallel to docIDs)
	maxWeight float32   // maximum weight in this dimension (for WAND upper bound)
	precision WeightPrecision
}

// SparseInvertedIndex is the segment-level sparse vector inverted index.
// It maps dimension IDs to WeightedPostingLists, enabling WAND-pruned
// dot-product search for SPLADE/BGE-M3 sparse vectors.
type SparseInvertedIndex struct {
	mu        sync.RWMutex
	dict      *TermDict // reused FST term dictionary
	postings  map[TermID]*WeightedPostingList
	numDocs   int32
	precision WeightPrecision
	field     string
}

// NewSparseInvertedIndex creates a new sparse vector inverted index.
func NewSparseInvertedIndex(field string, precision WeightPrecision) *SparseInvertedIndex {
	return &SparseInvertedIndex{
		postings:  make(map[TermID]*WeightedPostingList),
		precision: precision,
		field:     field,
	}
}

// AddVector adds a sparse vector to the index. dimIDs and weights must be
// the same length. The index must not have been finalized (Flush called).
func (s *SparseInvertedIndex) AddVector(rowID int32, dimIDs []uint64, weights []float32) error {
	if len(dimIDs) != len(weights) {
		return fmt.Errorf("sparse: dimIDs/weights length mismatch: %d vs %d", len(dimIDs), len(weights))
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, dimID := range dimIDs {
		termID := TermID(dimID)
		pl, ok := s.postings[termID]
		if !ok {
			pl = &WeightedPostingList{
				termID:    dimID,
				precision: s.precision,
			}
			s.postings[termID] = pl
		}
		w := weights[i]
		pl.docIDs = append(pl.docIDs, rowID)
		pl.weights = append(pl.weights, w)
		if w > pl.maxWeight {
			pl.maxWeight = w
		}
	}
	if rowID+1 > s.numDocs {
		s.numDocs = rowID + 1
	}
	return nil
}

// Postings returns all posting lists (for search).
func (s *SparseInvertedIndex) Postings() map[TermID]*WeightedPostingList {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.postings
}

// NumDocs returns the total number of documents.
func (s *SparseInvertedIndex) NumDocs() int32 { return s.numDocs }

// Field returns the field name.
func (s *SparseInvertedIndex) Field() string { return s.field }

// SparseWANDScorer implements WAND-pruned dot-product search for sparse
// vectors. Unlike BM25 WANDScorer which scores with TF saturation, this
// scores with: score(d) = Σ_i w_q[i] · w_d[i] over shared dimensions.
//
// The WAND upper bound for each dimension is: w_q[i] · maxWeight[i],
// enabling efficient pruning of documents that cannot make the top-K.
type SparseWANDScorer struct {
	terms     []sparseWANDTerm
	k         int
	threshold float32
}

type sparseWANDTerm struct {
	dimID       uint64
	queryWeight float32
	pl          *WeightedPostingList
	upperBound  float32
	idx         int // current position in pl.docIDs
	currentDoc  int32
	done        bool
}

// NewSparseWANDScorer creates a WAND scorer for sparse vector search.
// queryDimIDs and queryWeights are the query sparse vector. postings is
// the index's posting list map. Dimensions not in the index are skipped.
func NewSparseWANDScorer(queryDimIDs []uint64, queryWeights []float32, postings map[TermID]*WeightedPostingList, k int) *SparseWANDScorer {
	if k <= 0 {
		k = 10
	}
	w := &SparseWANDScorer{k: k}
	for i, dimID := range queryDimIDs {
		termID := TermID(dimID)
		pl, ok := postings[termID]
		if !ok || len(pl.docIDs) == 0 {
			continue
		}
		wt := sparseWANDTerm{
			dimID:       dimID,
			queryWeight: queryWeights[i],
			pl:          pl,
			upperBound:  queryWeights[i] * pl.maxWeight,
		}
		if len(pl.docIDs) > 0 {
			wt.currentDoc = pl.docIDs[0]
		}
		w.terms = append(w.terms, wt)
	}
	return w
}

// Search executes the WAND-pruned sparse vector search and returns top-K
// results sorted by descending dot-product score.
func (w *SparseWANDScorer) Search(ctx context.Context) (*index.SearchResult, error) {
	if len(w.terms) == 0 {
		return &index.SearchResult{}, nil
	}

	h := &sparseHeap{}
	heap.Init(h)

	for len(w.terms) > 0 {
		// Remove exhausted terms (filter out all done terms, not just from the end).
		alive := w.terms[:0]
		for _, t := range w.terms {
			if !t.done {
				alive = append(alive, t)
			}
		}
		w.terms = alive
		if len(w.terms) == 0 {
			break
		}

		// Sort terms by current docID ascending.
		sort.Slice(w.terms, func(i, j int) bool {
			return w.terms[i].currentDoc < w.terms[j].currentDoc
		})

		// Find pivot: first term where cumulative upper bound exceeds threshold.
		pivotDoc := int32(0)
		cumBound := float32(0)
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
			break // No term can exceed threshold.
		}

		// Check if all terms 0..pivotIdx point to pivotDoc.
		allAtPivot := true
		for i := 0; i <= pivotIdx; i++ {
			if w.terms[i].currentDoc != pivotDoc {
				allAtPivot = false
				break
			}
		}

		if allAtPivot {
			// Score the pivot document fully.
			score := w.scoreDoc(pivotDoc)
			if h.Len() < w.k {
				heap.Push(h, sparseCand{score: score, docID: pivotDoc})
				if h.Len() == w.k {
					w.threshold = h.Top().(sparseCand).score
				}
			} else if score > h.Top().(sparseCand).score {
				heap.Pop(h)
				heap.Push(h, sparseCand{score: score, docID: pivotDoc})
				w.threshold = h.Top().(sparseCand).score
			}
			// Advance all terms at pivotDoc.
			for i := range w.terms {
				if w.terms[i].currentDoc == pivotDoc {
					w.advanceTerm(i)
				}
			}
		} else {
			// Skip: advance terms before pivot to pivotDoc.
			advanced := false
			for i := 0; i < pivotIdx; i++ {
				if w.terms[i].currentDoc < pivotDoc {
					w.advanceTermTo(i, pivotDoc)
					advanced = true
				}
			}
			// Guarantee progress: if no term was advanced, advance the
			// first term (smallest docID) to avoid an infinite loop.
			if !advanced {
				w.advanceTerm(0)
			}
		}
	}

	// Extract results in descending score order.
	results := make([]index.SearchHit, 0, h.Len())
	for h.Len() > 0 {
		c := heap.Pop(h).(sparseCand)
		results = append(results, index.SearchHit{RowID: c.docID, Score: c.score})
	}
	// Reverse to descending order (heap is min-heap).
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return &index.SearchResult{Hits: results}, nil
}

// scoreDoc computes the full dot-product score for a document.
func (w *SparseWANDScorer) scoreDoc(docID int32) float32 {
	var score float32
	for i := range w.terms {
		wt := &w.terms[i]
		// Binary search for docID in this term's posting list.
		idx := sort.Search(len(wt.pl.docIDs), func(j int) bool {
			return wt.pl.docIDs[j] >= docID
		})
		if idx < len(wt.pl.docIDs) && wt.pl.docIDs[idx] == docID {
			score += wt.queryWeight * wt.pl.weights[idx]
		}
	}
	return score
}

// advanceTerm moves a term to its next document.
func (w *SparseWANDScorer) advanceTerm(i int) {
	wt := &w.terms[i]
	wt.idx++
	if wt.idx >= len(wt.pl.docIDs) {
		wt.done = true
	} else {
		wt.currentDoc = wt.pl.docIDs[wt.idx]
	}
}

// advanceTermTo advances a term to at least targetDoc using binary search.
func (w *SparseWANDScorer) advanceTermTo(i int, targetDoc int32) {
	wt := &w.terms[i]
	idx := sort.Search(len(wt.pl.docIDs), func(j int) bool {
		return wt.pl.docIDs[j] >= targetDoc
	})
	if idx >= len(wt.pl.docIDs) {
		wt.done = true
	} else {
		wt.idx = idx
		wt.currentDoc = wt.pl.docIDs[idx]
	}
}

// sparseHeap is a min-heap by score (for top-K selection).
type sparseHeap []sparseCand

type sparseCand struct {
	score float32
	docID int32
}

func (h sparseHeap) Len() int           { return len(h) }
func (h sparseHeap) Less(i, j int) bool { return h[i].score < h[j].score }
func (h sparseHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *sparseHeap) Push(x any)        { *h = append(*h, x.(sparseCand)) }
func (h *sparseHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
func (h sparseHeap) Top() any { return h[0] }
