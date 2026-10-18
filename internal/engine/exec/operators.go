// Package exec operators.go implements the §3.3 secondary operators:
// BitmapFilter (DeleteBitmap vectorized roaring.Contains), PredFilter
// (per-row scalar predicate), TopN (bounded heap, From+Size ≤ 10000),
// ANN (LoadedIndex.Search wrapper), and BM25Score (inverted index
// wrapper). All operators pull from an input operator and emit
// ColumnBatches, except ANN/BM25Score which produce batches from a
// single Search call.
package exec

import (
	"context"
	"fmt"

	"github.com/RoaringBitmap/roaring"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/index/hnsw"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// MaxFromPlusSize is the §8 from+size cap (10000). TopN rejects
// From+Size beyond this with a 400-style error so the engine layer
// can return the ES-flavored response.
const MaxFromPlusSize = 10000

// ErrFromSizeTooLarge is returned by TopN when from+size exceeds the
// §8 cap. The REST layer maps this to HTTP 400 with the ES 8.x
// "from + size must be <= 10000" reason text.
var ErrFromSizeTooLarge = fmt.Errorf("exec: from + size must be less than or equal to [%d]", MaxFromPlusSize)

// BitmapFilterOp applies a DeleteBitmap (01 §6.2) to the input batch:
// rows whose (SegmentID, RowID) appears in the tombstone bitmap are
// dropped. The filter takes a per-row predicate over the global
// doc id so the operator can compose multiple per-segment bitmaps
// (per-segment roaring bitmaps live in the storage layer; this
// operator only owns the apply step).
type BitmapFilterOp struct {
	input Operator
	pred  func(docID uint64) bool
	out   *ColumnBatch
}

// NewBitmapFilter constructs a BitmapFilterOp over the given input
// and tombstone predicate. pred may be nil (no deletes) — the
// operator then becomes a pass-through.
func NewBitmapFilter(input Operator, pred func(docID uint64) bool) *BitmapFilterOp {
	return &BitmapFilterOp{input: input, pred: pred, out: NewBatch(0)}
}

// NewBitmapFilterFromRoaring is the convenience constructor for the
// common case of one segment's roaring DeleteBitmap: pred returns
// true when the local row id is in bm.
func NewBitmapFilterFromRoaring(input Operator, segID uint64, bm *roaring.Bitmap) *BitmapFilterOp {
	if bm == nil {
		return NewBitmapFilter(input, nil)
	}
	return NewBitmapFilter(input, func(docID uint64) bool {
		if docID>>32 != segID {
			return false
		}
		return bm.Contains(uint32(docID))
	})
}

// Next implements Operator.
func (f *BitmapFilterOp) Next(ctx context.Context) (*ColumnBatch, error) {
	for {
		in, err := f.input.Next(ctx)
		if err != nil {
			return nil, err
		}
		if f.pred == nil {
			return in, nil
		}
		f.out.Reset(len(in.Cols))
		for i := 0; i < in.Rows; i++ {
			if f.pred(in.DocIDs[i]) {
				continue
			}
			if !f.out.AppendRow(in.DocIDs[i]>>32, uint32(in.DocIDs[i])) {
				return f.out, nil
			}
			if i < len(in.Scores) {
				f.out.AppendScore(in.Scores[i])
			}
		}
		if f.out.Rows > 0 {
			return f.out, nil
		}
		Release(in)
	}
}

// Close implements Operator.
func (f *BitmapFilterOp) Close() {
	if f.input != nil {
		f.input.Close()
	}
	if f.out != nil {
		Release(f.out)
		f.out = nil
	}
}

// RowPredicate is a per-row scalar predicate used by PredFilter. The
// row's column values are passed as the ColumnVec slice in projection
// order; the predicate returns true to keep the row.
type RowPredicate func(cols []ColumnVec, row int) bool

// PredFilterOp applies a scalar predicate to each row of the input
// batch, dropping rows that do not match. Used for term/range/exists
// predicates that the optimizer could not push into the Scan's
// zone-map or bitmap path.
type PredFilterOp struct {
	input Operator
	pred  RowPredicate
	out   *ColumnBatch
}

// NewPredFilter constructs a PredFilterOp.
func NewPredFilter(input Operator, pred RowPredicate) *PredFilterOp {
	return &PredFilterOp{input: input, pred: pred, out: NewBatch(0)}
}

// Next implements Operator.
func (f *PredFilterOp) Next(ctx context.Context) (*ColumnBatch, error) {
	for {
		in, err := f.input.Next(ctx)
		if err != nil {
			return nil, err
		}
		f.out.Reset(len(in.Cols))
		for i := 0; i < in.Rows; i++ {
			if !f.pred(in.Cols, i) {
				continue
			}
			if !f.out.AppendRow(in.DocIDs[i]>>32, uint32(in.DocIDs[i])) {
				return f.out, nil
			}
			if i < len(in.Scores) {
				f.out.AppendScore(in.Scores[i])
			}
		}
		if f.out.Rows > 0 {
			return f.out, nil
		}
		Release(in)
	}
}

// Close implements Operator.
func (f *PredFilterOp) Close() {
	if f.input != nil {
		f.input.Close()
	}
	if f.out != nil {
		Release(f.out)
		f.out = nil
	}
}

// topNHeap is a min-heap of (Score, DocID) pairs — the worst hit
// sits at the top so we can pop it when a better candidate arrives.
type topNHeap []topNCand

type topNCand struct {
	score float32
	docID uint64
}

func (h *topNHeap) push(c topNCand) {
	a := append(*h, c)
	i := len(a) - 1
	for i > 0 {
		p := (i - 1) / 2
		if a[p].score <= a[i].score {
			break
		}
		a[p], a[i] = a[i], a[p]
		i = p
	}
	*h = a
}

func (h *topNHeap) pop() topNCand {
	a := *h
	top := a[0]
	last := len(a) - 1
	a[0] = a[last]
	a = a[:last]
	i := 0
	for {
		l, r := 2*i+1, 2*i+2
		m := i
		if l < len(a) && a[l].score < a[m].score {
			m = l
		}
		if r < len(a) && a[r].score < a[m].score {
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

// TopNOp is the §3.3 / §8 TopN operator: a bounded min-heap of
// size From+Size that consumes the input stream, retains the top
// (From+Size) rows by descending Score, and emits one final batch
// with rows [From, From+Size) when the input reaches EOF. from+size
// ≤ 10000 is enforced; larger requests return ErrFromSizeTooLarge.
type TopNOp struct {
	input Operator
	from  int
	size  int
	heap  topNHeap
	out   *ColumnBatch
	done  bool
}

// NewTopN constructs a TopNOp. from and size follow the §8 contract;
// from+size must be ≤ MaxFromPlusSize.
func NewTopN(input Operator, from, size int) (*TopNOp, error) {
	if from < 0 || size < 0 {
		return nil, fmt.Errorf("exec: from/size must be non-negative (from=%d size=%d)", from, size)
	}
	if from+size > MaxFromPlusSize {
		return nil, ErrFromSizeTooLarge
	}
	if size == 0 {
		size = 10
	}
	return &TopNOp{
		input: input,
		from:  from,
		size:  size,
		out:   NewBatch(0),
	}, nil
}

// Next implements Operator: pull all input, retain top (from+size),
// emit one batch containing rows [from, from+size) on the first call
// and io.EOF thereafter.
func (t *TopNOp) Next(ctx context.Context) (*ColumnBatch, error) {
	if t.done {
		return nil, ErrEOF
	}
	t.done = true
	cap := t.from + t.size
	for {
		in, err := t.input.Next(ctx)
		if err != nil {
			if err == ErrEOF {
				break
			}
			return nil, err
		}
		for i := 0; i < in.Rows; i++ {
			c := topNCand{docID: in.DocIDs[i]}
			if i < len(in.Scores) {
				c.score = in.Scores[i]
			}
			if len(t.heap) < cap {
				t.heap.push(c)
				continue
			}
			if c.score > t.heap[0].score {
				t.heap.pop()
				t.heap.push(c)
			}
		}
		Release(in)
	}
	// Sort the heap by descending score, drop the first `from`
	// rows, and emit the next `size`.
	out := make([]topNCand, len(t.heap))
	for i := len(t.heap) - 1; i >= 0; i-- {
		out[i] = t.heap.pop()
	}
	t.out.Reset(0)
	start := t.from
	if start > len(out) {
		start = len(out)
	}
	end := start + t.size
	if end > len(out) {
		end = len(out)
	}
	for _, c := range out[start:end] {
		t.out.AppendRow(c.docID>>32, uint32(c.docID))
		t.out.AppendScore(c.score)
	}
	return t.out, nil
}

// Close implements Operator.
func (t *TopNOp) Close() {
	if t.input != nil {
		t.input.Close()
	}
	if t.out != nil {
		Release(t.out)
		t.out = nil
	}
}

// ANNOp is the §3.3 ANN operator: runs a single HNSW search via
// LoadedIndex.Search and emits one batch with all hits. M0 wires
// pre/post-filter planning to hnsw.ExecuteFilterPlan (02 §2.6);
// the bare LoadedIndex.Search path is used when no filter plan is
// set (e.g. queries without a scalar filter).
type ANNOp struct {
	idx   index.LoadedIndex
	q     index.Query
	plan  *hnsw.FilterPlan
	bud   index.SearchBudget
	done  bool
	out   *ColumnBatch
	segID uint64
}

// NewANN constructs an ANNOp over the given loaded HNSW index. q
// carries the query vector and K; q.Filter is the §2.6 scalar
// predicate. plan, when non-nil, routes through ExecuteFilterPlan;
// otherwise the bare Search path is used.
func NewANN(idx index.LoadedIndex, q index.Query, plan *hnsw.FilterPlan, budget index.SearchBudget, segID uint64) *ANNOp {
	return &ANNOp{
		idx: idx, q: q, plan: plan, bud: budget, segID: segID, out: NewBatch(0),
	}
}

// Next implements Operator.
func (a *ANNOp) Next(ctx context.Context) (*ColumnBatch, error) {
	if a.done {
		return nil, ErrEOF
	}
	a.done = true
	var res *index.SearchResult
	var err error
	if a.plan != nil {
		res, err = hnsw.ExecuteFilterPlan(ctx, a.idx, a.q, *a.plan)
	} else {
		res, err = a.idx.Search(ctx, a.q, a.bud)
	}
	if err != nil {
		return nil, fmt.Errorf("exec.ANN: search: %w", err)
	}
	a.out.Reset(0)
	for _, h := range res.Hits {
		a.out.AppendRow(a.segID, uint32(h.RowID))
		a.out.AppendScore(h.Score)
	}
	return a.out, nil
}

// Close implements Operator.
func (a *ANNOp) Close() {
	if a.out != nil {
		Release(a.out)
		a.out = nil
	}
}

// BM25ScoreOp is the §3.3 BM25Score operator: runs a single inverted
// index search via LoadedIndex.Search and emits one batch with all
// hits scored by BM25 (02 §3). M1 will add WAND pruning (02 §3.4)
// here; M0 wires the bare posting-merge path.
type BM25ScoreOp struct {
	idx   index.LoadedIndex
	q     index.Query
	bud   index.SearchBudget
	done  bool
	out   *ColumnBatch
	segID uint64
}

// NewBM25Score constructs a BM25ScoreOp over the given loaded
// inverted index. q.Term carries the analyzed query term (the
// optimizer resolves the analyzer mapping before constructing the
// operator).
func NewBM25Score(idx index.LoadedIndex, q index.Query, budget index.SearchBudget, segID uint64) *BM25ScoreOp {
	return &BM25ScoreOp{
		idx: idx, q: q, bud: budget, segID: segID, out: NewBatch(0),
	}
}

// Next implements Operator.
func (b *BM25ScoreOp) Next(ctx context.Context) (*ColumnBatch, error) {
	if b.done {
		return nil, ErrEOF
	}
	b.done = true
	res, err := b.idx.Search(ctx, b.q, b.bud)
	if err != nil {
		return nil, fmt.Errorf("exec.BM25Score: search: %w", err)
	}
	b.out.Reset(0)
	for _, h := range res.Hits {
		b.out.AppendRow(b.segID, uint32(h.RowID))
		b.out.AppendScore(h.Score)
	}
	return b.out, nil
}

// Close implements Operator.
func (b *BM25ScoreOp) Close() {
	if b.out != nil {
		Release(b.out)
		b.out = nil
	}
}

// Compile-time assertions that operators satisfy the interface.
var (
	_ Operator = (*ScanOp)(nil)
	_ Operator = (*BitmapFilterOp)(nil)
	_ Operator = (*PredFilterOp)(nil)
	_ Operator = (*TopNOp)(nil)
	_ Operator = (*ANNOp)(nil)
	_ Operator = (*BM25ScoreOp)(nil)
)

// Unused imports — these packages are referenced in optimizer (T15.4)
// and integration (T16/T17); kept here so exec is self-contained.
var (
	_ = segment.TypedZoneMap{}
	_ = types.SegmentID(0)
	_ = index.KindInverted
	_ = meta.TypeLong
)
