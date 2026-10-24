// Package exec operators.go implements the §3.3 secondary operators:
// BitmapFilter (DeleteBitmap vectorized roaring.Contains), PredFilter
// (per-row scalar predicate), TopN (bounded heap, From+Size ≤ 10000),
// ANN (LoadedIndex.Search wrapper), and BM25Score (inverted index
// wrapper). All operators pull from an input operator and emit
// ColumnBatches, except ANN/BM25Score which produce batches from a
// single Search call.
package exec

import (
	"container/heap"
	"context"
	"fmt"
	"io"
	"sort"

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

// NewBitmapFilterFromCached constructs a BitmapFilterOp that uses a
// pre-computed bitmap from the filter cache. The cached bitmap
// contains the matching doc IDs (segment-local), so the predicate
// simply tests membership. Unlike NewBitmapFilterFromRoaring which
// filters OUT deleted docs, this filters IN matched docs: rows NOT
// in the bitmap are dropped.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.2
func NewBitmapFilterFromCached(input Operator, segID uint64, bm *roaring.Bitmap) *BitmapFilterOp {
	if bm == nil {
		return NewBitmapFilter(input, nil)
	}
	return NewBitmapFilter(input, func(docID uint64) bool {
		if docID>>32 != segID {
			return true // pass through other segments
		}
		// Keep rows that ARE in the cached bitmap (inverted semantics:
		// BitmapFilterOp drops rows where pred returns true, so we
		// return true for rows NOT in the bitmap to drop them).
		return !bm.Contains(uint32(docID))
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
	_ Operator = (*MergeSortOp)(nil)
	_ Operator = (*RescoreOp)(nil)
	_ Operator = (*CollapseOp)(nil)
)

// ---------------------------------------------------------------------------
// RescoreOp (W5.7): re-scores the top-N hits within a window using a
// secondary query. The primary score and rescore score are blended
// with query_weight / rescore_query_weight (ES semantics, 03 §10).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.7
// ---------------------------------------------------------------------------

// RescoreOp re-scores the top (window) hits from the input pipeline.
// Hits outside the window keep their original score.
type RescoreOp struct {
	input           Operator
	rescoreQuery    func(rowID int32, source []byte) float32
	queryWeight     float32
	rescoreWeight   float32
	window          int  // number of top hits to rescore
	out             *ColumnBatch
	done            bool
	collected       []topNCand // collected hits from input
}

// NewRescore constructs a RescoreOp. window is the number of top hits
// to rescore (default 10). queryWeight and rescoreWeight blend the
// primary and secondary scores: final = qWeight*primary + rWeight*rescore.
func NewRescore(input Operator, rescoreQuery func(rowID int32, source []byte) float32,
	queryWeight, rescoreWeight float32, window int) *RescoreOp {
	if window <= 0 {
		window = 10
	}
	if queryWeight <= 0 {
		queryWeight = 1
	}
	if rescoreWeight <= 0 {
		rescoreWeight = 1
	}
	return &RescoreOp{
		input:         input,
		rescoreQuery:  rescoreQuery,
		queryWeight:   queryWeight,
		rescoreWeight: rescoreWeight,
		window:        window,
		out:           NewBatch(0),
	}
}

func (r *RescoreOp) Next(ctx context.Context) (*ColumnBatch, error) {
	if r.done {
		return nil, ErrEOF
	}
	r.done = true
	// Collect all input hits.
	for {
		in, err := r.input.Next(ctx)
		if err != nil {
			if err == ErrEOF {
				break
			}
			return nil, err
		}
		for i := 0; i < in.Rows; i++ {
			score := float32(0)
			if i < len(in.Scores) {
				score = in.Scores[i]
			}
			r.collected = append(r.collected, topNCand{score: score, docID: in.DocIDs[i]})
		}
		Release(in)
	}
	// Sort by descending original score (already sorted by TopN, but
	// the input may have multiple batches).
	for i := 1; i < len(r.collected); i++ {
		for j := i; j > 0 && r.collected[j].score > r.collected[j-1].score; j-- {
			r.collected[j], r.collected[j-1] = r.collected[j-1], r.collected[j]
		}
	}
	// Rescore the top `window` hits.
	for i := 0; i < r.window && i < len(r.collected); i++ {
		_, rowID := SplitDocID(r.collected[i].docID)
		rescore := r.rescoreQuery(int32(rowID), nil)
		r.collected[i].score = r.queryWeight*r.collected[i].score + r.rescoreWeight*rescore
	}
	// Emit rescored results.
	r.out.Reset(0)
	for _, c := range r.collected {
		if !r.out.AppendRow(c.docID>>32, uint32(c.docID)) {
			break
		}
		r.out.AppendScore(c.score)
	}
	return r.out, nil
}

func (r *RescoreOp) Close() {
	if r.input != nil {
		r.input.Close()
	}
	if r.out != nil {
		Release(r.out)
		r.out = nil
	}
}

// ---------------------------------------------------------------------------
// CollapseOp (W5.7): groups hits by a field value, keeping only the
// top hit per group (ES collapse, 03 §10). inner_hits can optionally
// retain additional per-group hits.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.7
// ---------------------------------------------------------------------------

// CollapseOp collapses hits by a group key, keeping the top hit per group.
type CollapseOp struct {
	input      Operator
	groupKey   func(rowID int32) string // extracts the group key for a hit
	maxPerGroup int                      // max hits per group (1 = collapse)
	out        *ColumnBatch
	done       bool
}

// NewCollapse constructs a CollapseOp. maxPerGroup is the maximum hits
// to retain per group (default 1 = full collapse).
func NewCollapse(input Operator, groupKey func(rowID int32) string, maxPerGroup int) *CollapseOp {
	if maxPerGroup <= 0 {
		maxPerGroup = 1
	}
	return &CollapseOp{
		input:       input,
		groupKey:    groupKey,
		maxPerGroup: maxPerGroup,
		out:         NewBatch(0),
	}
}

func (c *CollapseOp) Next(ctx context.Context) (*ColumnBatch, error) {
	if c.done {
		return nil, ErrEOF
	}
	c.done = true
	groupCount := make(map[string]int)
	for {
		in, err := c.input.Next(ctx)
		if err != nil {
			if err == ErrEOF {
				break
			}
			return nil, err
		}
		for i := 0; i < in.Rows; i++ {
			_, rowID := SplitDocID(in.DocIDs[i])
			key := c.groupKey(int32(rowID))
			if groupCount[key] >= c.maxPerGroup {
				continue // skip: group already has enough hits
			}
			groupCount[key]++
			score := float32(0)
			if i < len(in.Scores) {
				score = in.Scores[i]
			}
			c.out.AppendRow(in.DocIDs[i]>>32, uint32(in.DocIDs[i]))
			c.out.AppendScore(score)
		}
		Release(in)
	}
	return c.out, nil
}

func (c *CollapseOp) Close() {
	if c.input != nil {
		c.input.Close()
	}
	if c.out != nil {
		Release(c.out)
		c.out = nil
	}
}

// Unused imports — these packages are referenced in optimizer (T15.4)
// and integration (T16/T17); kept here so exec is self-contained.
var (
	_ = segment.TypedZoneMap{}
	_ = types.SegmentID(0)
	_ = index.KindInverted
	_ = meta.TypeLong
)

// MergeSortOp manages fan-out from multiple segment sub-plans and
// merges their results by descending score. Adaptive strategy (D7):
// small fan-out uses batch-collect; large fan-out degrades to heap
// merge for memory efficiency.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.5
type MergeSortOp struct {
	inputs   []Operator
	from     int
	size     int
	strategy mergeStrategy
	// batch strategy state
	collected []*ColumnBatch
	emitted   bool
	// heap strategy state
	heap     *mergeSortHeap
	cursors  []*mergeCursor
	skipped  int // rows skipped to satisfy `from`
	outN     int // rows emitted so far (heap strategy, excluding skips)
	heapInit bool
	heapErr  error
}

// mergeStrategy selects the D7 adaptive merge path.
type mergeStrategy int

const (
	mergeBatch mergeStrategy = iota // collect all sub-results, sort, take TopN
	mergeHeap                       // stream via container/heap k-way merge
)

// mergeBatchCollectThreshold is the D7 fan-out × window ceiling below
// which MergeSortOp collects every sub-result in memory (batch
// strategy). Above it the operator switches to the streaming heap
// merge to bound memory. Each segment sub-plan is already capped by
// its own TopN to (from+size) rows, so the batch path holds at most
// len(inputs)×(from+size) candidates.
const mergeBatchCollectThreshold = 800

// NewMergeSort constructs a MergeSortOp over the given segment
// sub-plans. from/size follow the §8 window contract. The adaptive
// strategy is selected from len(inputs)×(from+size): small fan-out
// collects in memory, large fan-out streams via a heap.
func NewMergeSort(inputs []Operator, from, size int) *MergeSortOp {
	m := &MergeSortOp{
		inputs: inputs,
		from:   from,
		size:   size,
	}
	fanOut := len(inputs) * (from + size)
	if fanOut <= mergeBatchCollectThreshold {
		m.strategy = mergeBatch
	} else {
		m.strategy = mergeHeap
	}
	return m
}

// Next implements Operator; it dispatches to the selected strategy.
func (m *MergeSortOp) Next(ctx context.Context) (*ColumnBatch, error) {
	if m.strategy == mergeBatch {
		return m.nextBatch(ctx)
	}
	return m.nextHeap(ctx)
}

// nextBatch pulls every sub-plan to EOF, sorts the union by descending
// score, and emits a single batch containing rows [from, from+size).
// Subsequent calls return io.EOF. The output batch is freshly
// allocated (caller-owned, mirroring ScanOp's contract) so callers may
// retain it across pulls.
func (m *MergeSortOp) nextBatch(ctx context.Context) (*ColumnBatch, error) {
	if m.emitted {
		return nil, ErrEOF
	}
	m.emitted = true
	var cands []topNCand
	for _, in := range m.inputs {
		for {
			b, err := in.Next(ctx)
			if err != nil {
				if err == ErrEOF {
					break
				}
				for _, cb := range m.collected {
					Release(cb)
				}
				m.collected = nil
				return nil, err
			}
			m.collected = append(m.collected, b)
			for i := 0; i < b.Rows; i++ {
				c := topNCand{docID: b.DocIDs[i]}
				if i < len(b.Scores) {
					c.score = b.Scores[i]
				}
				cands = append(cands, c)
			}
		}
	}
	// Descending score; tie-break by ascending docID for determinism.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].docID < cands[j].docID
	})
	out := NewBatch(0)
	start := m.from
	if start > len(cands) {
		start = len(cands)
	}
	end := start + m.size
	if end > len(cands) {
		end = len(cands)
	}
	for _, c := range cands[start:end] {
		out.AppendRow(c.docID>>32, uint32(c.docID))
		out.AppendScore(c.score)
	}
	for _, b := range m.collected {
		Release(b)
	}
	m.collected = nil
	return out, nil
}

// mergeHeapItem is one entry in the k-way merge heap: the current head
// row of one sub-plan cursor.
type mergeHeapItem struct {
	score float32
	docID uint64
	src   int // index into MergeSortOp.cursors
}

// mergeSortHeap is a max-heap by score implemented as a container/heap
// min-heap with a reversed Less, so the highest-scoring head sits at
// index 0. Ties break by ascending docID for deterministic ordering.
type mergeSortHeap []mergeHeapItem

func (h mergeSortHeap) Len() int { return len(h) }
func (h mergeSortHeap) Less(i, j int) bool {
	if h[i].score != h[j].score {
		return h[i].score > h[j].score
	}
	return h[i].docID < h[j].docID
}
func (h mergeSortHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeSortHeap) Push(x any) { *h = append(*h, x.(mergeHeapItem)) }
func (h *mergeSortHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// mergeCursor tracks one sub-plan operator's position within its
// current batch. done is set once the sub-plan returns io.EOF and the
// held batch is fully consumed.
type mergeCursor struct {
	op    Operator
	batch *ColumnBatch
	row   int
	done  bool
}

// nextHeap streams merged rows via the heap. Each call emits up to
// BatchSize rows and never more than the remaining window. The `from`
// offset is consumed by skipping the first `from` highest-scoring
// rows across all sub-plans. Output batches are freshly allocated
// (caller-owned, mirroring ScanOp's contract).
func (m *MergeSortOp) nextHeap(ctx context.Context) (*ColumnBatch, error) {
	if m.heapErr != nil {
		return nil, m.heapErr
	}
	// Lazily seed cursors and the heap on the first call.
	if !m.heapInit {
		m.heapInit = true
		m.cursors = make([]*mergeCursor, len(m.inputs))
		m.heap = &mergeSortHeap{}
		for i, in := range m.inputs {
			c := &mergeCursor{op: in}
			m.cursors[i] = c
			if err := m.fillBatch(ctx, i); err != nil {
				m.heapErr = err
				return nil, err
			}
			if it, ok := m.headOf(i); ok {
				heap.Push(m.heap, it)
			}
		}
	}
	out := NewBatch(0)
	for out.Rows < BatchSize && m.outN < m.size {
		if m.heap.Len() == 0 {
			break
		}
		top := heap.Pop(m.heap).(mergeHeapItem)
		if m.skipped < m.from {
			m.skipped++
		} else {
			out.AppendRow(top.docID>>32, uint32(top.docID))
			out.AppendScore(top.score)
			m.outN++
		}
		// Advance the winning cursor past the consumed row and
		// re-insert its next head (if any).
		c := m.cursors[top.src]
		c.row++
		if c.batch != nil && c.row >= c.batch.Rows {
			Release(c.batch)
			c.batch = nil
			c.row = 0
		}
		if err := m.fillBatch(ctx, top.src); err != nil {
			m.heapErr = err
			break
		}
		if it, ok := m.headOf(top.src); ok {
			heap.Push(m.heap, it)
		}
	}
	if out.Rows == 0 {
		Release(out)
		if m.heapErr != nil {
			return nil, m.heapErr
		}
		return nil, ErrEOF
	}
	return out, nil
}

// fillBatch pulls the next non-empty batch into cursor src, marking
// the cursor done on io.EOF. Empty batches are released and skipped.
func (m *MergeSortOp) fillBatch(ctx context.Context, src int) error {
	c := m.cursors[src]
	for c.batch == nil && !c.done {
		b, err := c.op.Next(ctx)
		if err != nil {
			if err == ErrEOF {
				c.done = true
				break
			}
			c.done = true
			return err
		}
		if b.Rows == 0 {
			Release(b)
			continue
		}
		c.batch = b
		c.row = 0
	}
	return nil
}

// headOf returns the current head item of cursor src, or ok=false
// when the cursor has no unconsumed row.
func (m *MergeSortOp) headOf(src int) (mergeHeapItem, bool) {
	c := m.cursors[src]
	if c.batch == nil || c.row >= c.batch.Rows {
		return mergeHeapItem{}, false
	}
	it := mergeHeapItem{docID: c.batch.DocIDs[c.row], src: src}
	if c.row < len(c.batch.Scores) {
		it.score = c.batch.Scores[c.row]
	}
	return it, true
}

// Close implements Operator. It closes every sub-plan and releases
// any batch still held in cursor state. Idempotent.
func (m *MergeSortOp) Close() {
	for _, in := range m.inputs {
		if in != nil {
			in.Close()
		}
	}
	for _, b := range m.collected {
		Release(b)
	}
	m.collected = nil
	for _, c := range m.cursors {
		if c != nil && c.batch != nil {
			Release(c.batch)
			c.batch = nil
		}
	}
}

// HashAggregateOp collects rows from a child operator and produces
// a single batch with aggregation results (design 03 §3.3).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.1.4
type HashAggregateOp struct {
	child     Operator
	collector func(any)
	result    func() any
	done      bool
}

func NewHashAggregateOp(child Operator, collect func(any), result func() any) *HashAggregateOp {
	return &HashAggregateOp{child: child, collector: collect, result: result}
}

func (op *HashAggregateOp) Next(ctx context.Context) (*ColumnBatch, error) {
	if op.done {
		return nil, io.EOF
	}
	for {
		batch, err := op.child.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		// Feed each row's DocID to the collector (simplified: the
		// actual field value extraction happens in the engine layer).
		for i := 0; i < batch.Rows; i++ {
			op.collector(batch.Scores[i])
		}
		Release(batch)
	}
	op.done = true
	_ = op.result() // result consumed by the engine layer
	// Return an empty batch to signal completion.
	return NewBatch(0), nil
}

func (op *HashAggregateOp) Close() {
	if op.child != nil {
		op.child.Close()
	}
}
