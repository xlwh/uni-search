package embedded

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/unisearch-db/unisearch/internal/engine/agg"
	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	"github.com/unisearch-db/unisearch/internal/engine/exec"
	"github.com/unisearch-db/unisearch/internal/engine/fusion"
	"github.com/unisearch-db/unisearch/internal/engine/plan"
	"github.com/unisearch-db/unisearch/internal/engine/script"
	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/index/analyzer"
	"github.com/unisearch-db/unisearch/internal/jsonx"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// scoredHit is an intermediate result carrying the segment entry, row id,
// score, and optional _source (filled for memtable hits). The segment entry
// is stored directly to avoid GlobalDocID truncation issues with 64-bit
// snowflake segment IDs.
type scoredHit struct {
	entry  *storage.SegmentEntry // nil for memtable hits
	rowID  uint32
	score  float32
	source []byte // only for memtable hits
	id     string // only for memtable hits
}

// buildAndExecute constructs and executes query pipelines for all
// segments, searches the memtable, merges results, and assembles
// SearchHits. This is the core query execution path (D2/D7).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.5
func buildAndExecute(
	ctx context.Context,
	plans []plan.LogicalPlan,
	idx *indexState,
	snap types.Snapshot,
	segSet *storage.SegmentSet,
	from, size int,
	searchAfter []any,
	q dsl.Query, // @spec: deep-gap-remediation 02_spec.md#memtable-search
) ([]scoredHit, int, error) {
	var allHits []scoredHit
	total := 0

	// 1. Execute per-segment plans.
	if segSet != nil {
		for i, entry := range segSet.Items() {
			if i >= len(plans) || entry.Reader == nil {
				continue
			}
			hits, cnt, err := executeSegmentPlan(ctx, plans[i], entry, idx, snap)
			if err != nil {
				return nil, 0, err
			}
			// Filter out deleted rows (MVCC tombstones from DeleteTracker).
			if idx.shard != nil {
				filtered := hits[:0]
				for _, h := range hits {
					if !idx.shard.IsSegmentRowDeleted(entry.Reader.ID(), types.RowID(h.rowID)) {
						filtered = append(filtered, h)
					}
				}
				hits = filtered
				cnt = len(hits)
			}
			allHits = append(allHits, hits...)
			total += cnt
		}
	}

	// 2. Search the active memtable using the DSL query for type dispatch.
	mtHits := searchMemtableByQuery(q, idx, snap, plans)
	allHits = append(allHits, mtHits...)

	// 3. Merge and rank: sort by score descending.
	sort.Slice(allHits, func(i, j int) bool {
		if allHits[i].score != allHits[j].score {
			return allHits[i].score > allHits[j].score
		}
		return allHits[i].rowID < allHits[j].rowID
	})

	// 3b. search_after cursor: filter out documents <= searchAfter.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.3
	if len(searchAfter) > 0 {
		filtered := make([]scoredHit, 0, len(allHits))
		for _, h := range allHits {
			if isAfterCursor(h, searchAfter) {
				filtered = append(filtered, h)
			}
		}
		allHits = filtered
	}

	// Update total count.
	total = len(allHits)

	// Apply from/size pagination.
	start := from
	if start > len(allHits) {
		start = len(allHits)
	}
	end := start + size
	if end > len(allHits) {
		end = len(allHits)
	}

	return allHits[start:end], total, nil
}

// isAfterCursor returns true if hit h sorts strictly after the
// searchAfter cursor values. The cursor is (score, rowID) in the
// current sort order (descending score, ascending rowID tie-breaker).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.3
func isAfterCursor(h scoredHit, cursor []any) bool {
	if len(cursor) == 0 {
		return true
	}
	// Compare score (first cursor value).
	cursorScore, ok := toFloat64(cursor[0])
	if !ok {
		return true
	}
	if float64(h.score) > cursorScore {
		return true
	}
	if float64(h.score) < cursorScore {
		return false
	}
	// Scores equal: compare rowID (tie-breaker, ascending).
	if len(cursor) > 1 {
		cursorRow, ok := toFloat64(cursor[1])
		if ok && float64(h.rowID) <= cursorRow {
			return false
		}
	}
	return true
}

// toFloat64 converts an any value to float64.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	default:
		return 0, false
	}
}

// executeSegmentPlan builds an exec pipeline from a per-segment plan and
// drains it, returning scored hits and the hit count.
func executeSegmentPlan(ctx context.Context, p plan.LogicalPlan, entry *storage.SegmentEntry, idx *indexState, snap types.Snapshot) ([]scoredHit, int, error) {
	op, err := buildOperator(ctx, p, entry, idx, snap)
	if err != nil {
		return nil, 0, err
	}
	if op == nil {
		return nil, 0, nil
	}
	defer op.Close()

	pipe := exec.NewPipeline(op)
	batches, err := pipe.Pull(ctx, -1) // unlimited
	if err != nil {
		return nil, 0, err
	}

	var hits []scoredHit
	cnt := 0
	for _, b := range batches {
		for i := 0; i < b.Rows; i++ {
			_, rowID := exec.SplitDocID(b.DocIDs[i])
			score := float32(0)
			if i < len(b.Scores) {
				score = b.Scores[i]
			}
			hits = append(hits, scoredHit{entry: entry, rowID: rowID, score: score})
			cnt++
		}
		exec.Release(b)
	}
	return hits, cnt, nil
}

// buildOperator recursively builds an exec.Operator chain from a plan tree.
func buildOperator(ctx context.Context, p plan.LogicalPlan, entry *storage.SegmentEntry, idx *indexState, snap types.Snapshot) (exec.Operator, error) {
	switch typed := p.(type) {
	case *plan.Scan:
		return buildScan(typed, entry, snap), nil

	case *plan.Filter:
		input, err := buildOperator(ctx, typed.Input, entry, idx, snap)
		if err != nil {
			return nil, err
		}
		return buildFilter(typed, input, idx), nil

	case *plan.BM25:
		return buildBM25(typed, entry, idx), nil

	case *plan.ANN:
		return buildANN(typed, entry, idx), nil

	case *plan.SparseVector:
		return buildSparseVector(typed, entry, idx), nil

	case *plan.TopN:
		input, err := buildOperator(ctx, typed.Input, entry, idx, snap)
		if err != nil {
			return nil, err
		}
		tn, err := exec.NewTopN(input, typed.From, typed.Size)
		if err != nil {
			return nil, err
		}
		return tn, nil

	case *plan.Fusion:
		return buildFusion(typed, entry, idx, snap)

	case *plan.Aggregate:
		// F3: Wire GroupAggregator into the execution pipeline.
		// @spec: 02_spec.md#4.6
		input, err := buildOperator(ctx, typed.Input, entry, idx, snap)
		if err != nil {
			return nil, err
		}
		aggDefs := make([]agg.AggDef, len(typed.Aggs))
		for i, ad := range typed.Aggs {
			aggDefs[i] = agg.AggDef{Name: ad.Name, Type: ad.Type, Params: ad.Params}
		}
		ga := agg.NewGroupAggregator(typed.GroupField, aggDefs)
		collect := func(v any) {
			if s, ok := v.(string); ok {
				ga.CollectString(s, map[string]any{})
			} else {
				ga.Collect(v, map[string]any{})
			}
		}
		result := func() any { return ga.Buckets() }
		return exec.NewHashAggregateOp(input, collect, result), nil

	case *plan.ProjectionScan:
		// F6: ProjectionScan reads pre-computed aggregate state.
		// @spec: 02_spec.md#7.4
		return nil, fmt.Errorf("embedded: ProjectionScan not yet implemented (requires projection storage)")

	default:
		return nil, fmt.Errorf("embedded: unsupported plan node %T", p)
	}
}

// buildScan constructs a ScanOp from a plan.Scan node.
func buildScan(p *plan.Scan, entry *storage.SegmentEntry, snap types.Snapshot) exec.Operator {
	// match_all (FieldType "_id"): the _id column is stored separately
	// via AppendID, not as a regular col_*.bin file. Use matchAllOp
	// to iterate all rows directly without opening a column.
	if p.FieldType == "_id" && p.Field <= 1 {
		return newMatchAllOp(entry.Reader.NumRows(), p.SegID)
	}
	op := exec.NewScan(entry.Reader, p.Field, p.Snapshot, p.SegID)
	// Zone map predicate (R2 block skip) is installed by the optimizer;
	// M0 leaves it as pass-through.
	// Install bitmap filter if present (R1 delete bitmap).
	if p.BmFilter.HasBitmap {
		op.SetBitmapFilter(nil) // nil = pass-through; wired by the planner
	}
	return op
}

// matchAllOp produces all rows in a segment with score 1.0, without
// needing a specific column. Used for match_all queries where the _id
// column is stored separately (via AppendID) and not as col_*.bin.
type matchAllOp struct {
	numRows int
	segID   uint64
	done    bool
	out     *exec.ColumnBatch
}

func newMatchAllOp(numRows int, segID uint64) *matchAllOp {
	return &matchAllOp{numRows: numRows, segID: segID, out: exec.NewBatch(0)}
}

func (o *matchAllOp) Next(ctx context.Context) (*exec.ColumnBatch, error) {
	if o.done {
		return nil, exec.ErrEOF
	}
	o.done = true
	for i := 0; i < o.numRows; i++ {
		if !o.out.AppendRow(o.segID, uint32(i)) {
			break
		}
		o.out.AppendScore(1.0)
	}
	return o.out, nil
}

func (o *matchAllOp) Close() {
	if o.out != nil {
		exec.Release(o.out)
		o.out = nil
	}
}

// buildFilter wraps an input operator with a predicate filter.
func buildFilter(p *plan.Filter, input exec.Operator, idx *indexState) exec.Operator {
	pred := makeRowPredicate(p.Pred, idx)
	return exec.NewPredFilter(input, pred)
}

// buildBM25 constructs a BM25ScoreOp from a plan.BM25 node.
func buildBM25(p *plan.BM25, entry *storage.SegmentEntry, idx *indexState) exec.Operator {
	fid, fd, ok := idx.resolveField(p.Pred.Field)
	if !ok {
		return noopOperator{}
	}
	// Get the loaded inverted index for this field in this segment.
	loaded, ok := entry.Indexes[fid]
	if !ok {
		// No inverted index loaded — fall back to scan.
		return exec.NewScan(entry.Reader, fid, types.Snapshot{}, p.SegID)
	}
	_ = fd // field type check done by translator
	// Analyze the query text: split on whitespace and use the first term
	// (the inverted index stores individual tokens, not full phrases).
	text := p.Pred.Text
	if idx := strings.IndexByte(text, ' '); idx > 0 {
		text = text[:idx]
	}
	// Build the index.Query for the inverted index search.
	q := index.Query{
		Term: []byte(text),
	}
	return exec.NewBM25Score(loaded, q, index.SearchBudget{}, p.SegID)
}

// buildANN constructs an ANNOp from a plan.ANN node.
func buildANN(p *plan.ANN, entry *storage.SegmentEntry, idx *indexState) exec.Operator {
	loaded, ok := entry.Indexes[p.Field]
	if !ok {
		// No HNSW index loaded — return noop (planner should handle
		// brute-force memtable search separately).
		return noopOperator{}
	}
	q := index.Query{
		Vector:   p.Knn.Vector,
		K:        p.Knn.K,
		EFSearch: p.Knn.NumCandidates,
	}
	return exec.NewANN(loaded, q, p.FilterPlan, index.SearchBudget{}, p.SegID)
}

// buildSparseVector constructs the segment-level operator for a sparse
// vector query. M1 uses the memtable brute-force path; segment-level
// inverted-index search (term = field\x00dim, tf = weight×2^8) is a
// follow-up (design 02 §5). For now, return noop so that only the
// memtable path contributes results.
func buildSparseVector(p *plan.SparseVector, entry *storage.SegmentEntry, idx *indexState) exec.Operator {
	_ = p
	_ = entry
	_ = idx
	return noopOperator{}
}

// buildFusion builds a fusion pipeline from a plan.Fusion node.
func buildFusion(p *plan.Fusion, entry *storage.SegmentEntry, idx *indexState, snap types.Snapshot) (exec.Operator, error) {
	// Build each sub-plan's operator chain.
	ops := make([]exec.Operator, 0, len(p.Inputs))
	for _, input := range p.Inputs {
		op, err := buildOperator(context.Background(), input, entry, idx, snap)
		if err != nil {
			return nil, err
		}
		if op != nil {
			ops = append(ops, op)
		}
	}
	if len(ops) == 0 {
		return noopOperator{}, nil
	}
	if len(ops) == 1 {
		tn, err := exec.NewTopN(ops[0], p.From, p.Size)
		if err != nil {
			return nil, err
		}
		return tn, nil
	}

	// Execute each sub-plan, collect results, and fuse.
	// For Phase 0, we use a simple approach: pull each sub-plan, collect
	// ScoredDocs, and fuse using the fusion package.
	var perPath [][]fusion.ScoredDoc
	for _, op := range ops {
		pipe := exec.NewPipeline(op)
		batches, err := pipe.Pull(context.Background(), -1)
		if err != nil {
			return nil, err
		}
		var pathDocs []fusion.ScoredDoc
		for _, b := range batches {
			for i := 0; i < b.Rows; i++ {
				score := float32(0)
				if i < len(b.Scores) {
					score = b.Scores[i]
				}
				pathDocs = append(pathDocs, fusion.ScoredDoc{
					DocID: b.DocIDs[i],
					Score: score,
				})
			}
			exec.Release(b)
		}
		perPath = append(perPath, pathDocs)
	}

	// Fuse candidates.
	fuser, err := fusion.NewFusion(p.Rank)
	if err != nil {
		return nil, fmt.Errorf("embedded: create fusion: %w", err)
	}
	fused, err := fuser.Merge(perPath)
	if err != nil {
		return nil, fmt.Errorf("embedded: fusion merge: %w", err)
	}

	// Create a batch from fused results.
	out := exec.NewBatch(0)
	for _, f := range fused {
		segID, rowID := exec.SplitDocID(f.DocID)
		if !out.AppendRow(segID, rowID) {
			break
		}
		out.AppendScore(f.Score)
	}

	return &batchEmitter{batch: out}, nil
}

// batchEmitter is a simple operator that emits one pre-built batch.
type batchEmitter struct {
	batch *exec.ColumnBatch
	done  bool
}

func (e *batchEmitter) Next(ctx context.Context) (*exec.ColumnBatch, error) {
	if e.done {
		return nil, exec.ErrEOF
	}
	e.done = true
	return e.batch, nil
}

func (e *batchEmitter) Close() {
	if e.batch != nil {
		exec.Release(e.batch)
		e.batch = nil
	}
}

// noopOperator emits nothing.
type noopOperator struct{}

func (noopOperator) Next(context.Context) (*exec.ColumnBatch, error) { return nil, exec.ErrEOF }
func (noopOperator) Close()                                          {}

// makeRowPredicate converts a DSL query into a RowPredicate for the
// PredFilterOp. It evaluates term, terms, range, and exists predicates
// against the actual column data in the batch.
func makeRowPredicate(q dsl.Query, idx *indexState) exec.RowPredicate {
	if q == nil {
		return func(cols []exec.ColumnVec, row int) bool { return true }
	}
	switch typed := q.(type) {
	case *dsl.TermQuery:
		return func(cols []exec.ColumnVec, row int) bool {
			if len(cols) == 0 {
				return false
			}
			return matchTerm(cols[0], row, typed.Value, typed.CaseInsensitive)
		}
	case *dsl.TermsQuery:
		return func(cols []exec.ColumnVec, row int) bool {
			if len(cols) == 0 {
				return false
			}
			for _, v := range typed.Values {
				if matchTerm(cols[0], row, v, false) {
					return true
				}
			}
			return false
		}
	case *dsl.RangeQuery:
		return func(cols []exec.ColumnVec, row int) bool {
			if len(cols) == 0 {
				return false
			}
			return matchRange(cols[0], row, typed)
		}
	case *dsl.ExistsQuery:
		return func(cols []exec.ColumnVec, row int) bool {
			if len(cols) == 0 {
				return false
			}
			return matchExists(cols[0], row)
		}
	default:
		// match_all and other queries: accept all rows.
		return func(cols []exec.ColumnVec, row int) bool { return true }
	}
}

// matchTerm checks if the column value at the given row equals the query value.
// It checks all data arrays to handle cases where the column type doesn't
// match the mapping type.
func matchTerm(col exec.ColumnVec, row int, want any, caseInsensitive bool) bool {
	// Check Strings (keyword/text fields)
	if row < len(col.Strings) {
		s, ok := want.(string)
		if !ok {
			// Try converting want to string
			s = fmt.Sprintf("%v", want)
		}
		if caseInsensitive {
			return strings.EqualFold(col.Strings[row], s)
		}
		return col.Strings[row] == s
	}
	// Check Longs (long/integer fields)
	if row < len(col.Longs) {
		switch n := want.(type) {
		case float64:
			return float64(col.Longs[row]) == n
		case int64:
			return col.Longs[row] == n
		case int:
			return col.Longs[row] == int64(n)
		case string:
			if v, err := strconv.ParseInt(n, 10, 64); err == nil {
				return col.Longs[row] == v
			}
		}
		return false
	}
	// Check Doubles (float/double fields)
	if row < len(col.Doubles) {
		switch n := want.(type) {
		case float64:
			return col.Doubles[row] == n
		case int64:
			return col.Doubles[row] == float64(n)
		case int:
			return col.Doubles[row] == float64(n)
		case string:
			if v, err := strconv.ParseFloat(n, 64); err == nil {
				return col.Doubles[row] == v
			}
		}
		return false
	}
	// Check Bools
	if row < len(col.Bools) {
		b, ok := want.(bool)
		if !ok {
			return false
		}
		return col.Bools[row] == b
	}
	return false
}

// matchRange checks if the column value at the given row falls within
// the range bounds (gt/gte/lt/lte). It checks all data arrays to handle
// cases where the column type doesn't match the mapping type.
func matchRange(col exec.ColumnVec, row int, q *dsl.RangeQuery) bool {
	// Check Doubles first (float/double fields)
	if row < len(col.Doubles) {
		return inRange(col.Doubles[row], q)
	}
	// Check Longs (long/integer fields, or float stored as long)
	if row < len(col.Longs) {
		return inRange(float64(col.Longs[row]), q)
	}
	// Check Strings (keyword/text fields with string comparison)
	if row < len(col.Strings) {
		val := col.Strings[row]
		// Try numeric comparison first
		if v, err := strconv.ParseFloat(val, 64); err == nil {
			return inRange(v, q)
		}
		// Fall back to lexicographic string comparison
		if q.Gte != nil {
			if s := fmt.Sprintf("%v", q.Gte); val < s {
				return false
			}
		}
		if q.Gt != nil {
			if s := fmt.Sprintf("%v", q.Gt); val <= s {
				return false
			}
		}
		if q.Lte != nil {
			if s := fmt.Sprintf("%v", q.Lte); val > s {
				return false
			}
		}
		if q.Lt != nil {
			if s := fmt.Sprintf("%v", q.Lt); val >= s {
				return false
			}
		}
		return true
	}
	return false
}

// inRange checks if a float64 value satisfies the range bounds.
func inRange(val float64, q *dsl.RangeQuery) bool {
	if q.Gte != nil {
		if v := toF64(q.Gte); val < v {
			return false
		}
	}
	if q.Gt != nil {
		if v := toF64(q.Gt); val <= v {
			return false
		}
	}
	if q.Lte != nil {
		if v := toF64(q.Lte); val > v {
			return false
		}
	}
	if q.Lt != nil {
		if v := toF64(q.Lt); val >= v {
			return false
		}
	}
	return true
}

// toF64 converts an any to float64 for range comparison.
func toF64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	default:
		return 0
	}
}

// matchExists checks if the column value at the given row is non-null.
// It checks all data arrays to handle cases where the column type doesn't
// match the mapping type.
func matchExists(col exec.ColumnVec, row int) bool {
	if col.Nulls != nil {
		if col.Nulls.Contains(uint32(row)) {
			return false
		}
	}
	// Check if any data array has a value at this row
	if row < len(col.Doubles) {
		return true
	}
	if row < len(col.Longs) {
		return true
	}
	if row < len(col.Strings) {
		return col.Strings[row] != ""
	}
	if row < len(col.Bools) {
		return true
	}
	return false
}

// searchMemtable searches the active (unflushed) memtable using
// Shard.IterateMemtable. For Phase 0, this implements a basic
// match_all scan that returns all visible memtable rows with score 1.0.
// The mini inverted index (D1/D4) will be wired in a follow-up to
// support text/vector queries on unflushed data.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.5
func searchMemtable(ctx context.Context, plans []plan.LogicalPlan, idx *indexState, snap types.Snapshot) []scoredHit {
	if idx == nil || idx.shard == nil {
		return nil
	}
	// Sparse vector queries use a dedicated brute-force memtable scan.
	if sv := findSparseVectorPlan(plans); sv != nil {
		return searchMemtableSparseVector(sv, idx, snap)
	}
	// Dispatch based on plan type for proper query-aware memtable search.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#memtable-search
	for _, p := range plans {
		switch typed := unwrapPlan(p).(type) {
		case *plan.BM25:
			return searchMemtableBM25(typed, idx, snap)
		case *plan.ANN:
			return searchMemtableKNN(typed, idx, snap)
		case *plan.Fusion:
			return searchMemtableFusion(typed, idx, snap)
		}
	}
	// Default: scan all visible rows (match_all, term, range, etc.).
	return scanMemtable(idx, snap)
}

// unwrapPlan follows TopN wrappers to find the underlying plan node.
func unwrapPlan(p plan.LogicalPlan) plan.LogicalPlan {
	if tn, ok := p.(*plan.TopN); ok && tn != nil {
		return unwrapPlan(tn.Input)
	}
	return p
}

// searchMemtableByQuery dispatches memtable search based on the DSL
// query type, enabling query-aware filtering of unflushed data.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#memtable-search
func searchMemtableByQuery(q dsl.Query, idx *indexState, snap types.Snapshot, plans []plan.LogicalPlan) []scoredHit {
	if idx == nil || idx.shard == nil {
		return nil
	}
	if sv := findSparseVectorPlan(plans); sv != nil {
		return searchMemtableSparseVector(sv, idx, snap)
	}
	for _, p := range plans {
		switch typed := unwrapPlan(p).(type) {
		case *plan.BM25:
			return searchMemtableBM25(typed, idx, snap)
		case *plan.ANN:
			return searchMemtableKNN(typed, idx, snap)
		case *plan.Fusion:
			return searchMemtableFusion(typed, idx, snap)
		}
	}
	if q != nil {
		switch typed := q.(type) {
		case *dsl.MatchAllQuery:
			return scanMemtable(idx, snap)
		case *dsl.MatchQuery:
			return searchMemtableByMatch(typed, idx, snap)
		case *dsl.TermQuery:
			return searchMemtableByTerm(typed, idx, snap)
		case *dsl.BoolQuery:
			return searchMemtableByBool(typed, idx, snap)
		case *dsl.KnnQuery:
			return searchMemtableByKNN(typed, idx, snap)
		case *dsl.HybridQuery:
			return searchMemtableByHybrid(typed, idx, snap)
		case *dsl.ConstantScoreQuery:
			return searchMemtableByConstantScore(typed, idx, snap)
		case *dsl.BoostingQuery:
			return searchMemtableByBoosting(typed, idx, snap)
		case *dsl.DisMaxQuery:
			return searchMemtableByDisMax(typed, idx, snap)
		case *dsl.PrefixQuery:
			return searchMemtableByPrefix(typed, idx, snap)
		case *dsl.RegexpQuery:
			return searchMemtableByRegexp(typed, idx, snap)
		case *dsl.FunctionScoreQuery:
			return searchMemtableByFunctionScore(typed, idx, snap)
		case *dsl.ScriptScoreQuery:
			return searchMemtableByScriptScore(typed, idx, snap)
		case *dsl.GeoDistanceQuery:
			return searchMemtableByGeoDistance(typed, idx, snap)
		case *dsl.GeoBoundingBoxQuery:
			return searchMemtableByGeoBoundingBox(typed, idx, snap)
		case *dsl.MoreLikeThisQuery:
			return searchMemtableByMoreLikeThis(typed, idx, snap)
		case *dsl.SimpleQueryStringQuery:
			return searchMemtableBySimpleQueryString(typed, idx, snap)
		case *dsl.CombinedFieldsQuery:
			return searchMemtableByCombinedFields(typed, idx, snap)
		case *dsl.MatchBoolPrefixQuery:
			return searchMemtableByMatchBoolPrefix(typed, idx, snap)
		case *dsl.DistanceFeatureQuery:
			return searchMemtableByDistanceFeature(typed, idx, snap)
		case *dsl.RankFeatureQuery:
			return searchMemtableByRankFeature(typed, idx, snap)
		case *dsl.WrapperQuery:
			return searchMemtableByQuery(typed.Inner, idx, snap, nil)
		case *dsl.PinnedQuery:
			return searchMemtableByPinned(typed, idx, snap)
		case *dsl.TermsSetQuery:
			return searchMemtableByTermsSet(typed, idx, snap)
		case *dsl.MatchNoneQuery:
			return nil
		case *dsl.ScriptQuery:
			return searchMemtableByScript(typed, idx, snap)
		case *dsl.PercolateQuery:
			return searchMemtableByPercolate(typed, idx, snap)
		case *dsl.GeoShapeQuery:
			return searchMemtableByGeoShape(typed, idx, snap)
		case *dsl.IntervalsQuery:
			return searchMemtableByIntervals(typed, idx, snap)
		case *dsl.NestedQuery:
			return searchMemtableByNested(typed, idx, snap)
		}
	}
	return scanMemtable(idx, snap)
}

func searchMemtableByMatch(q *dsl.MatchQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	if idx.miniInv == nil {
		return scanMemtable(idx, snap)
	}
	idx.mu.RLock()
	fieldID, ok := idx.fieldIDs[q.Field]
	fd := idx.fieldDefs[fieldID]
	idx.mu.RUnlock()
	if !ok || fd == nil {
		return scanMemtable(idx, snap)
	}
	az, err := analyzer.Get(fd.Analyzer, nil)
	if err != nil || az == nil {
		az, _ = analyzer.Get("standard", nil)
	}
	miniHits := idx.miniInv.search(fieldID, q.Text, az)
	if len(miniHits) == 0 {
		return nil
	}
	hitMap := make(map[int32]float32, len(miniHits))
	for _, mh := range miniHits {
		hitMap[mh.rowID] = mh.score
	}
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		if score, ok := hitMap[int32(rowID)]; ok {
			hits = append(hits, scoredHit{rowID: uint32(rowID), score: score, source: source, id: id})
		}
		return true
	})
	return hits
}

func searchMemtableByTerm(q *dsl.TermQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) == nil {
			if docVal, ok := doc[q.Field]; ok {
				if fmt.Sprintf("%v", docVal) == q.Value {
					hits = append(hits, scoredHit{rowID: uint32(rowID), score: 1.0, source: source, id: id})
				}
			}
		}
		return true
	})
	return hits
}

func searchMemtableByBool(q *dsl.BoolQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		for _, sub := range q.Must {
			if !docMatchesQuery(sub, doc) {
				return true
			}
		}
		for _, sub := range q.MustNot {
			if docMatchesQuery(sub, doc) {
				return true
			}
		}
		if len(q.Should) > 0 && len(q.Must) == 0 && len(q.Filter) == 0 {
			matched := false
			for _, sub := range q.Should {
				if docMatchesQuery(sub, doc) {
					matched = true
					break
				}
			}
			if !matched {
				return true
			}
		}
		for _, sub := range q.Filter {
			if !docMatchesQuery(sub, doc) {
				return true
			}
		}
		hits = append(hits, scoredHit{rowID: uint32(rowID), score: 1.0, source: source, id: id})
		return true
	})
	return hits
}

func searchMemtableByKNN(q *dsl.KnnQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	if q == nil || len(q.Vector) == 0 {
		return scanMemtable(idx, snap)
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	type cand struct {
		rowID  uint32
		score  float32
		source []byte
		id     string
	}
	var cands []cand
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) == nil {
			if vecRaw, ok := doc[q.Field]; ok {
				if vec, ok := vecRaw.([]any); ok {
					fv := make([]float32, len(vec))
					valid := true
					for i, v := range vec {
						if f, ok := v.(float64); ok {
							fv[i] = float32(f)
						} else {
							valid = false
							break
						}
					}
					if valid && len(fv) == len(q.Vector) {
						var dot float32
						for i := range q.Vector {
							dot += q.Vector[i] * fv[i]
						}
						cands = append(cands, cand{rowID: uint32(rowID), score: dot, source: source, id: id})
					}
				}
			}
		}
		return true
	})
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > k {
		cands = cands[:k]
	}
	hits := make([]scoredHit, len(cands))
	for i, c := range cands {
		hits[i] = scoredHit{rowID: c.rowID, score: c.score, source: c.source, id: c.id}
	}
	return hits
}

func searchMemtableByHybrid(q *dsl.HybridQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var allHits []scoredHit
	for _, sub := range q.Queries {
		switch typed := sub.(type) {
		case *dsl.MatchQuery:
			allHits = append(allHits, searchMemtableByMatch(typed, idx, snap)...)
		case *dsl.TermQuery:
			allHits = append(allHits, searchMemtableByTerm(typed, idx, snap)...)
		case *dsl.KnnQuery:
			allHits = append(allHits, searchMemtableByKNN(typed, idx, snap)...)
		case *dsl.MatchAllQuery:
			allHits = append(allHits, scanMemtable(idx, snap)...)
		}
	}
	seen := make(map[uint32]float32)
	deduped := make([]scoredHit, 0, len(allHits))
	for _, h := range allHits {
		if ex, ok := seen[h.rowID]; !ok || h.score > ex {
			seen[h.rowID] = h.score
			deduped = append(deduped, h)
		}
	}
	sort.Slice(deduped, func(i, j int) bool { return deduped[i].score > deduped[j].score })
	return deduped
}

// ---------------------------------------------------------------------------
// P0 DSL memtable search functions (es-feature-gap-fill)
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.2
// ---------------------------------------------------------------------------

// searchMemtableByConstantScore wraps the inner filter query in a constant
// score: matching docs get score = Boost.
func searchMemtableByConstantScore(q *dsl.ConstantScoreQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	boost := float32(q.Boost)
	if boost == 0 {
		boost = 1.0
	}
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		if docMatchesQuery(q.Filter, doc) {
			hits = append(hits, scoredHit{rowID: uint32(rowID), score: boost, source: source, id: id})
		}
		return true
	})
	return hits
}

// searchMemtableByBoosting returns docs matching the positive query,
// down-scoring docs that also match the negative query.
func searchMemtableByBoosting(q *dsl.BoostingQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	negBoost := float32(q.NegativeBoost)
	if negBoost == 0 {
		negBoost = 1.0
	}
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		if !docMatchesQuery(q.Positive, doc) {
			return true
		}
		score := float32(1.0)
		if q.Negative != nil && docMatchesQuery(q.Negative, doc) {
			score *= negBoost
		}
		hits = append(hits, scoredHit{rowID: uint32(rowID), score: score, source: source, id: id})
		return true
	})
	return hits
}

// searchMemtableByDisMax returns docs matching any sub-query, with the
// score being the maximum among all sub-queries (plus tie_breaker × rest).
func searchMemtableByDisMax(q *dsl.DisMaxQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		var maxScore, restSum float32
		matched := false
		for _, sub := range q.Queries {
			if docMatchesQuery(sub, doc) {
				matched = true
				sc := float32(1.0) // P0: uniform score for memtable matches
				if sc > maxScore {
					maxScore = sc
				} else {
					restSum += sc
				}
			}
		}
		if matched {
			score := maxScore + float32(q.TieBreaker)*restSum
			if q.Boost > 0 {
				score *= float32(q.Boost)
			}
			hits = append(hits, scoredHit{rowID: uint32(rowID), score: score, source: source, id: id})
		}
		return true
	})
	return hits
}

// searchMemtableByPrefix matches memtable documents whose field value
// starts with the given prefix string.
func searchMemtableByPrefix(q *dsl.PrefixQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		if docVal, ok := doc[q.Field]; ok {
			if strings.HasPrefix(fmt.Sprintf("%v", docVal), q.Value) {
				sc := float32(1.0)
				if q.Boost > 0 {
					sc = float32(q.Boost)
				}
				hits = append(hits, scoredHit{rowID: uint32(rowID), score: sc, source: source, id: id})
			}
		}
		return true
	})
	return hits
}

// searchMemtableByRegexp matches memtable documents whose field value
// matches the given regular expression pattern.
func searchMemtableByRegexp(q *dsl.RegexpQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	re, err := regexp.Compile(q.Pattern)
	if err != nil {
		return nil
	}
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		if docVal, ok := doc[q.Field]; ok {
			if re.MatchString(fmt.Sprintf("%v", docVal)) {
				sc := float32(1.0)
				if q.Boost > 0 {
					sc = float32(q.Boost)
				}
				hits = append(hits, scoredHit{rowID: uint32(rowID), score: sc, source: source, id: id})
			}
		}
		return true
	})
	return hits
}

// ---------------------------------------------------------------------------
// P1 DSL memtable search functions (es-feature-gap-fill)
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.6-2.9
// ---------------------------------------------------------------------------

func searchMemtableByFunctionScore(q *dsl.FunctionScoreQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	// Delegate to inner query if present, otherwise scan all.
	var base []scoredHit
	if q.Query != nil {
		base = searchMemtableByQuery(q.Query, idx, snap, nil)
	} else {
		base = scanMemtable(idx, snap)
	}
	// Apply score functions (P1: simplified — use weight only).
	for i := range base {
		var fs float32 = 1.0
		for _, fn := range q.Functions {
			fs *= float32(fn.Weight)
		}
		if q.Boost > 0 {
			fs *= float32(q.Boost)
		}
		base[i].score = fs
	}
	return base
}

func searchMemtableByScriptScore(q *dsl.ScriptScoreQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	// Keep the inner query's real scores; the script re-scoring is applied
	// uniformly to memtable and segment hits in Engine.Search (the script
	// receives the inner score as _score), so both paths share one
	// implementation.
	return searchMemtableByQuery(q.Query, idx, snap, nil)
}

func searchMemtableByGeoDistance(q *dsl.GeoDistanceQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		geoVal, ok := doc[q.Field]
		if !ok {
			return true
		}
		lat, lon := extractGeoPoint(geoVal)
		if lat == 0 && lon == 0 {
			return true
		}
		dist := haversineMeters(q.Lat, q.Lon, lat, lon)
		if dist <= q.DistanceM {
			hits = append(hits, scoredHit{rowID: uint32(rowID), score: float32(q.Boost), source: source, id: id})
		}
		return true
	})
	return hits
}

func searchMemtableByGeoBoundingBox(q *dsl.GeoBoundingBoxQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		geoVal, ok := doc[q.Field]
		if !ok {
			return true
		}
		lat, lon := extractGeoPoint(geoVal)
		if lat == 0 && lon == 0 {
			return true
		}
		if lat >= q.Bottom && lat <= q.Top && lon >= q.Left && lon <= q.Right {
			hits = append(hits, scoredHit{rowID: uint32(rowID), score: float32(q.Boost), source: source, id: id})
		}
		return true
	})
	return hits
}

// extractGeoPoint extracts lat/lon from a geo_point field value.
func extractGeoPoint(val any) (lat, lon float64) {
	switch v := val.(type) {
	case map[string]any:
		if l, ok := v["lat"].(float64); ok {
			lat = l
		}
		if l, ok := v["lon"].(float64); ok {
			lon = l
		}
	case string:
		fmt.Sscanf(v, "%f,%f", &lat, &lon)
	}
	return
}

// haversineMeters computes the great-circle distance in meters.
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000 // Earth radius in meters
	lat1r := lat1 * 0.017453292519943295
	lat2r := lat2 * 0.017453292519943295
	dlat := lat2r - lat1r
	dlon := (lon2 - lon1) * 0.017453292519943295
	a := 0.5*(1-math.Cos(dlat)) + math.Cos(lat1r)*math.Cos(lat2r)*0.5*(1-math.Cos(dlon))
	return R * 2 * math.Asin(math.Sqrt(a))
}

// ---------------------------------------------------------------------------
// P2 DSL memtable search functions (es-feature-gap-fill)
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.10-2.13
// ---------------------------------------------------------------------------

func searchMemtableByMoreLikeThis(q *dsl.MoreLikeThisQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	// P2 simplified: match like texts against doc fields.
	likeWords := strings.ToLower(strings.Join(q.LikeTexts, " "))
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		fields := q.Fields
		if len(fields) == 0 {
			for k := range doc {
				fields = append(fields, k)
			}
		}
		for _, f := range fields {
			if v, ok := doc[f]; ok {
				if strings.Contains(strings.ToLower(fmt.Sprintf("%v", v)), likeWords) {
					hits = append(hits, scoredHit{rowID: uint32(rowID), score: float32(q.Boost), source: source, id: id})
					return true
				}
			}
		}
		return true
	})
	return hits
}

func searchMemtableBySimpleQueryString(q *dsl.SimpleQueryStringQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	fields := q.Fields
	if len(fields) == 0 {
		fields = []string{"_all"}
	}
	matchQ := &dsl.MatchQuery{Field: fields[0], Text: q.Query, Operator: q.DefaultOperator, Boost: q.Boost}
	return searchMemtableByMatch(matchQ, idx, snap)
}

func searchMemtableByCombinedFields(q *dsl.CombinedFieldsQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	if len(q.Fields) == 0 {
		return scanMemtable(idx, snap)
	}
	matchQ := &dsl.MatchQuery{Field: q.Fields[0], Text: q.Query, Operator: q.Operator, Boost: q.Boost}
	return searchMemtableByMatch(matchQ, idx, snap)
}

func searchMemtableByMatchBoolPrefix(q *dsl.MatchBoolPrefixQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	matchQ := &dsl.MatchQuery{Field: q.Field, Text: q.Query, Operator: "or", Boost: q.Boost}
	return searchMemtableByMatch(matchQ, idx, snap)
}

// ---------------------------------------------------------------------------
// P3 DSL memtable search functions (es-feature-gap-fill)
// ---------------------------------------------------------------------------

func searchMemtableByDistanceFeature(q *dsl.DistanceFeatureQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	pivot, _ := q.Pivot.(float64)
	if pivot <= 0 {
		pivot = 1.0
	}
	origin, _ := q.Origin.(float64)
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		val, ok := doc[q.Field]
		if !ok {
			return true
		}
		fieldVal, _ := val.(float64)
		dist := math.Abs(fieldVal - origin)
		score := float32(q.Boost * pivot / (pivot + dist))
		hits = append(hits, scoredHit{rowID: uint32(rowID), score: score, source: source, id: id})
		return true
	})
	return hits
}

func searchMemtableByRankFeature(q *dsl.RankFeatureQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		val, ok := doc[q.Field]
		if !ok {
			return true
		}
		fieldVal, _ := val.(float64)
		if fieldVal <= 0 {
			fieldVal = 1.0
		}
		score := float32(q.Boost * math.Log(1.0+q.Positive/(fieldVal+q.Negative)))
		hits = append(hits, scoredHit{rowID: uint32(rowID), score: score, source: source, id: id})
		return true
	})
	return hits
}

// ---------------------------------------------------------------------------
// P3 DSL memtable search functions — pinned + terms_set
// ---------------------------------------------------------------------------

func searchMemtableByPinned(q *dsl.PinnedQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	// Execute the organic query first.
	base := searchMemtableByQuery(q.Organic, idx, snap, nil)
	// Promote pinned docs to the top with high score.
	pinnedIDs := make(map[string]bool)
	for _, p := range q.Pinned {
		pinnedIDs[p.ID] = true
	}
	var pinnedHits, organicHits []scoredHit
	for _, h := range base {
		if pinnedIDs[h.id] {
			pinnedHits = append(pinnedHits, scoredHit{rowID: h.rowID, score: 1e10, source: h.source, id: h.id})
		} else {
			organicHits = append(organicHits, h)
		}
	}
	return append(pinnedHits, organicHits...)
}

func searchMemtableByTermsSet(q *dsl.TermsSetQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	// Build a value set for quick lookup.
	valueSet := make(map[string]bool)
	for _, v := range q.Values {
		valueSet[fmt.Sprintf("%v", v)] = true
	}
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		val, ok := doc[q.Field]
		if !ok {
			return true
		}
		// Count how many values from the set match.
		matched := 0
		switch arr := val.(type) {
		case []any:
			for _, e := range arr {
				if valueSet[fmt.Sprintf("%v", e)] {
					matched++
				}
			}
		default:
			if valueSet[fmt.Sprintf("%v", val)] {
				matched = 1
			}
		}
		// Determine minimum required matches.
		minMatch := 1
		if q.MinimumShouldMatchField != "" {
			if minVal, ok := doc[q.MinimumShouldMatchField]; ok {
				if min, ok := minVal.(float64); ok {
					minMatch = int(min)
				}
			}
		}
		if matched >= minMatch {
			hits = append(hits, scoredHit{rowID: uint32(rowID), score: float32(q.Boost), source: source, id: id})
		}
		return true
	})
	return hits
}

// ---------------------------------------------------------------------------
// Final DSL memtable search functions
// ---------------------------------------------------------------------------

func searchMemtableByScript(q *dsl.ScriptQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	scr, err := script.Parse(q.Script)
	if err != nil {
		// Unparseable script matches nothing (ES rejects at parse time).
		return nil
	}
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		if evalScriptBool(scr, doc, q.Params, 0) {
			hits = append(hits, scoredHit{rowID: uint32(rowID), score: float32(q.Boost), source: source, id: id})
		}
		return true
	})
	return hits
}

// evalScriptBool evaluates a compiled script as a boolean filter against
// a document (the script engine path, replacing the former MatchAll stub).
func evalScriptBool(scr *script.Script, doc map[string]any, params map[string]any, score float64) bool {
	v, err := scr.Eval(&script.ScriptCtx{Doc: doc, Score: score, Params: params})
	if err != nil {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case nil:
		return false
	default:
		return true
	}
}

func searchMemtableByPercolate(q *dsl.PercolateQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	// Resolve the document to percolate: inline "document" wins, then the
	// stored "id" (fetched from this shard's current snapshot).
	var doc map[string]any
	if q.Document != nil {
		doc = q.Document
	} else if q.DocumentID != "" && idx.shard != nil {
		if src, _, found := idx.shard.GetDocSource(context.Background(), q.DocumentID, snap); found {
			_ = jsonx.Unmarshal(src, &doc)
		}
	}
	if len(doc) == 0 || idx.percolator == nil {
		return nil
	}
	docJSON, err := jsonx.Marshal(doc)
	if err != nil {
		return nil
	}
	matches, err := idx.percolator.Match(docJSON)
	if err != nil {
		return nil
	}
	// Each match is one registered percolator query: the hit's _id is
	// the query document's ID and _source is the stored query DSL.
	var hits []scoredHit
	for _, queryID := range matches {
		var stored []byte
		if sq := idx.percolator.Lookup(queryID); sq != nil {
			stored = sq.Query
		}
		hits = append(hits, scoredHit{
			id:     queryID,
			score:  float32(q.Boost),
			source: stored,
		})
	}
	return hits
}

func searchMemtableByGeoShape(q *dsl.GeoShapeQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		if _, ok := doc[q.Field]; ok {
			hits = append(hits, scoredHit{rowID: uint32(rowID), score: float32(q.Boost), source: source, id: id})
		}
		return true
	})
	return hits
}

func searchMemtableByIntervals(q *dsl.IntervalsQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	// Simplified: treat intervals as a match on the field.
	return scanMemtable(idx, snap)
}

func searchMemtableByNested(q *dsl.NestedQuery, idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) != nil {
			return true
		}
		// Extract the nested array from the document.
		nestedVal, ok := doc[q.Path]
		if !ok {
			return true
		}
		nestedArr, ok := nestedVal.([]any)
		if !ok {
			return true
		}
		// Check if any nested element matches the inner query.
		for _, elem := range nestedArr {
			if elemMap, ok := elem.(map[string]any); ok {
				if docMatchesQuery(q.Query, elemMap) {
					hits = append(hits, scoredHit{rowID: uint32(rowID), score: float32(q.Boost), source: source, id: id})
					return true
				}
			}
		}
		return true
	})
	return hits
}

func docMatchesQuery(q dsl.Query, doc map[string]any) bool {
	switch typed := q.(type) {
	case *dsl.MatchQuery:
		if docVal, ok := doc[typed.Field]; ok {
			docStr := fmt.Sprintf("%v", docVal)
			for _, term := range strings.Fields(strings.ToLower(typed.Text)) {
				if strings.Contains(strings.ToLower(docStr), term) {
					return true
				}
			}
		}
		return false
	case *dsl.TermQuery:
		if docVal, ok := doc[typed.Field]; ok {
			return fmt.Sprintf("%v", docVal) == typed.Value
		}
		return false
	case *dsl.MatchAllQuery:
		return true
	case *dsl.ExistsQuery:
		_, ok := doc[typed.Field]
		return ok
	case *dsl.ConstantScoreQuery:
		return docMatchesQuery(typed.Filter, doc)
	case *dsl.BoostingQuery:
		return docMatchesQuery(typed.Positive, doc)
	case *dsl.DisMaxQuery:
		for _, sub := range typed.Queries {
			if docMatchesQuery(sub, doc) {
				return true
			}
		}
		return false
	case *dsl.PrefixQuery:
		if docVal, ok := doc[typed.Field]; ok {
			return strings.HasPrefix(fmt.Sprintf("%v", docVal), typed.Value)
		}
		return false
	case *dsl.RegexpQuery:
		if docVal, ok := doc[typed.Field]; ok {
			matched, err := regexp.MatchString(typed.Pattern, fmt.Sprintf("%v", docVal))
			return err == nil && matched
		}
		return false
	case *dsl.FunctionScoreQuery:
		if typed.Query != nil {
			return docMatchesQuery(typed.Query, doc)
		}
		return true
	case *dsl.ScriptScoreQuery:
		return docMatchesQuery(typed.Query, doc)
	case *dsl.GeoDistanceQuery:
		if geoVal, ok := doc[typed.Field]; ok {
			lat, lon := extractGeoPoint(geoVal)
			if lat == 0 && lon == 0 {
				return false
			}
			return haversineMeters(typed.Lat, typed.Lon, lat, lon) <= typed.DistanceM
		}
		return false
	case *dsl.GeoBoundingBoxQuery:
		if geoVal, ok := doc[typed.Field]; ok {
			lat, lon := extractGeoPoint(geoVal)
			return lat >= typed.Bottom && lat <= typed.Top && lon >= typed.Left && lon <= typed.Right
		}
		return false
	case *dsl.MoreLikeThisQuery:
		likeWords := strings.ToLower(strings.Join(typed.LikeTexts, " "))
		fields := typed.Fields
		if len(fields) == 0 {
			return true
		}
		for _, f := range fields {
			if v, ok := doc[f]; ok {
				if strings.Contains(strings.ToLower(fmt.Sprintf("%v", v)), likeWords) {
					return true
				}
			}
		}
		return false
	case *dsl.SimpleQueryStringQuery:
		fields := typed.Fields
		if len(fields) == 0 {
			fields = []string{"_all"}
		}
		for _, f := range fields {
			if v, ok := doc[f]; ok {
				for _, term := range strings.Fields(strings.ToLower(typed.Query)) {
					if strings.Contains(strings.ToLower(fmt.Sprintf("%v", v)), term) {
						return true
					}
				}
			}
		}
		return false
	case *dsl.CombinedFieldsQuery:
		for _, f := range typed.Fields {
			if v, ok := doc[f]; ok {
				for _, term := range strings.Fields(strings.ToLower(typed.Query)) {
					if strings.Contains(strings.ToLower(fmt.Sprintf("%v", v)), term) {
						return true
					}
				}
			}
		}
		return false
	case *dsl.MatchBoolPrefixQuery:
		if docVal, ok := doc[typed.Field]; ok {
			docStr := strings.ToLower(fmt.Sprintf("%v", docVal))
			for _, term := range strings.Fields(strings.ToLower(typed.Query)) {
				if strings.Contains(docStr, term) {
					return true
				}
			}
		}
		return false
	case *dsl.DistanceFeatureQuery:
		_, ok := doc[typed.Field]
		return ok
	case *dsl.RankFeatureQuery:
		_, ok := doc[typed.Field]
		return ok
	case *dsl.WrapperQuery:
		return docMatchesQuery(typed.Inner, doc)
	case *dsl.PinnedQuery:
		return docMatchesQuery(typed.Organic, doc)
	case *dsl.TermsSetQuery:
		if docVal, ok := doc[typed.Field]; ok {
			valueSet := make(map[string]bool)
			for _, v := range typed.Values {
				valueSet[fmt.Sprintf("%v", v)] = true
			}
			matched := 0
			switch arr := docVal.(type) {
			case []any:
				for _, e := range arr {
					if valueSet[fmt.Sprintf("%v", e)] {
						matched++
					}
				}
			default:
				if valueSet[fmt.Sprintf("%v", docVal)] {
					matched = 1
				}
			}
			minMatch := 1
			if typed.MinimumShouldMatchField != "" {
				if minVal, ok := doc[typed.MinimumShouldMatchField]; ok {
					if min, ok := minVal.(float64); ok {
						minMatch = int(min)
					}
				}
			}
			return matched >= minMatch
		}
		return false
	case *dsl.MatchNoneQuery:
		return false
	case *dsl.ScriptQuery:
		// Real script evaluation via the script engine (segment-path
		// post-filter; memtable path handled in searchMemtableByScript).
		scr, err := script.Parse(typed.Script)
		if err != nil {
			return false
		}
		return evalScriptBool(scr, doc, typed.Params, 0)
	case *dsl.PercolateQuery:
		// Percolate hits come from the registered-query table (see
		// searchMemtableByPercolate); ordinary indexed documents never
		// match a percolate query on their own.
		return false
	case *dsl.GeoShapeQuery:
		_, ok := doc[typed.Field]
		return ok
	case *dsl.IntervalsQuery:
		return true // simplified — actual interval matching in searchMemtableByIntervals
	case *dsl.NestedQuery:
		if nestedVal, ok := doc[typed.Path]; ok {
			if arr, ok := nestedVal.([]any); ok {
				for _, elem := range arr {
					if elemMap, ok := elem.(map[string]any); ok {
						if docMatchesQuery(typed.Query, elemMap) {
							return true
						}
					}
				}
			}
		}
		return false
	default:
		return true
	}
}

// findSparseVectorPlan walks the per-segment plan trees looking for a
// *plan.SparseVector leaf (unwrapping TopN/Fusion wrappers). Returns the
// first match, or nil when the query is not a sparse-vector search.
func findSparseVectorPlan(plans []plan.LogicalPlan) *plan.SparseVector {
	for _, p := range plans {
		if sv := findSparseVectorNode(p); sv != nil {
			return sv
		}
	}
	return nil
}

// findSparseVectorNode recursively searches a single plan tree.
func findSparseVectorNode(p plan.LogicalPlan) *plan.SparseVector {
	switch typed := p.(type) {
	case *plan.SparseVector:
		return typed
	case *plan.TopN:
		return findSparseVectorNode(typed.Input)
	case *plan.Fusion:
		for _, in := range typed.Inputs {
			if sv := findSparseVectorNode(in); sv != nil {
				return sv
			}
		}
	}
	return nil
}

// searchMemtableSparseVector performs a brute-force sparse-vector inner-
// product search over all visible memtable rows. Each row's _source JSON
// is parsed to extract the {indices, values} sparse vector for the query
// field, and the score is computed as score = Σ w_q · w_d over shared
// dimensions, scaled by the query boost (design 02 §5).
//
// This is the M1 simple path; the segment-level inverted-index search
// (term = field\x00dim, tf = weight×2^8) is a follow-up.
func searchMemtableSparseVector(sv *plan.SparseVector, idx *indexState, snap types.Snapshot) []scoredHit {
	if sv == nil || sv.Query == nil || len(sv.Query.Embedding) == 0 {
		return nil
	}
	// Resolve the field name from the query so we can look it up in
	// the _source JSON.
	_, fd, ok := idx.resolveField(sv.Query.Field)
	if !ok {
		return nil
	}
	fieldName := sv.Query.Field
	_ = fd

	queryVec := sv.Query.Embedding
	boost := sv.Query.Boost
	if boost == 0 {
		boost = 1.0
	}

	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		docVec := extractSparseVectorFromSource(source, fieldName)
		if len(docVec) == 0 {
			return true // no sparse vector for this field; skip
		}
		score := sparseInnerProduct(queryVec, docVec)
		if score == 0 {
			return true // no shared dimensions; skip
		}
		hits = append(hits, scoredHit{
			rowID:  uint32(rowID),
			score:  score * boost,
			source: source,
			id:     id,
		})
		return true
	})
	return hits
}

// sparseInnerProduct computes Σ w_q · w_d over shared dimensions of the
// query and document sparse vectors (SPLADE-style inner-product scoring,
// design 02 §5).
func sparseInnerProduct(query, doc map[uint32]float32) float32 {
	var score float32
	// Iterate the smaller map for efficiency.
	if len(query) <= len(doc) {
		for dim, wq := range query {
			if wd, ok := doc[dim]; ok {
				score += wq * wd
			}
		}
	} else {
		for dim, wd := range doc {
			if wq, ok := query[dim]; ok {
				score += wq * wd
			}
		}
	}
	return score
}

// extractSparseVectorFromSource parses a raw _source JSON document and
// extracts the sparse_vector field as a map[uint32]float32. The field is
// expected to be stored in the {"indices": [...], "values": [...]}
// format (the same format validated by meta.validSparseVector).
func extractSparseVectorFromSource(source []byte, field string) map[uint32]float32 {
	if len(source) == 0 || field == "" {
		return nil
	}
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(source, &doc); err != nil {
		return nil
	}
	raw, ok := doc[field]
	if !ok {
		return nil
	}
	var sv map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &sv); err != nil {
		return nil
	}
	idxRaw, ok1 := sv["indices"]
	valRaw, ok2 := sv["values"]
	if !ok1 || !ok2 {
		return nil
	}
	var idxs []any
	if err := jsonx.Unmarshal(idxRaw, &idxs); err != nil {
		return nil
	}
	var vals []any
	if err := jsonx.Unmarshal(valRaw, &vals); err != nil {
		return nil
	}
	if len(idxs) != len(vals) || len(idxs) == 0 {
		return nil
	}
	out := make(map[uint32]float32, len(idxs))
	for i, el := range idxs {
		dim, err := dimFromJSONValue(el)
		if err != nil {
			return nil
		}
		w, err := weightFromJSONValue(vals[i])
		if err != nil {
			return nil
		}
		out[dim] = w
	}
	return out
}

// dimFromJSONValue converts a JSON-decoded value to a uint32 dimension.
func dimFromJSONValue(v any) (uint32, error) {
	switch x := v.(type) {
	case float64:
		if x < 0 || x > float64(^uint32(0)) {
			return 0, fmt.Errorf("dimension index out of uint32 range: %v", x)
		}
		return uint32(x), nil
	case jsonx.Number:
		n, err := x.Int64()
		if err != nil {
			return 0, fmt.Errorf("dimension index must be an integer: %w", err)
		}
		if n < 0 || n > int64(^uint32(0)) {
			return 0, fmt.Errorf("dimension index out of uint32 range: %v", n)
		}
		return uint32(n), nil
	default:
		return 0, fmt.Errorf("dimension index must be a number, got %T", v)
	}
}

// weightFromJSONValue converts a JSON-decoded value to a float32 weight.
func weightFromJSONValue(v any) (float32, error) {
	switch x := v.(type) {
	case float64:
		return float32(x), nil
	case jsonx.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, fmt.Errorf("weight must be a number: %w", err)
		}
		return float32(f), nil
	default:
		return 0, fmt.Errorf("weight must be a number, got %T", v)
	}
}

// scanMemtable iterates all visible rows in the memtable and returns
// them as scored hits with score 1.0 (match_all).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#memtable-search
func scanMemtable(idx *indexState, snap types.Snapshot) []scoredHit {
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		hits = append(hits, scoredHit{
			rowID:  uint32(rowID),
			score:  1.0,
			source: source,
			id:     id,
		})
		return true
	})
	return hits
}

// searchMemtableBM25 searches the memtable using the mini inverted index.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#memtable-search
func searchMemtableBM25(p *plan.BM25, idx *indexState, snap types.Snapshot) []scoredHit {
	if p == nil || p.Pred == nil || idx.miniInv == nil {
		return scanMemtable(idx, snap) // fallback to scan all
	}
	fieldName := p.Pred.Field
	queryText := p.Pred.Text
	idx.mu.RLock()
	fieldID, ok := idx.fieldIDs[fieldName]
	fd := idx.fieldDefs[fieldID]
	idx.mu.RUnlock()
	if !ok || fd == nil {
		return scanMemtable(idx, snap)
	}
	// Get analyzer for the field.
	az, err := analyzer.Get(fd.Analyzer, nil)
	if err != nil || az == nil {
		az, _ = analyzer.Get("standard", nil) // fallback
	}
	// Search the mini inverted index.
	miniHits := idx.miniInv.search(fieldID, queryText, az)
	if len(miniHits) == 0 {
		return nil
	}
	// Resolve rowIDs to scoredHits with _id and _source.
	hitMap := make(map[int32]float32, len(miniHits))
	for _, mh := range miniHits {
		hitMap[mh.rowID] = mh.score
	}
	var hits []scoredHit
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		if score, ok := hitMap[int32(rowID)]; ok {
			hits = append(hits, scoredHit{
				rowID:  uint32(rowID),
				score:  score,
				source: source,
				id:     id,
			})
		}
		return true
	})
	return hits
}

// searchMemtableKNN does brute-force vector search on the memtable.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#memtable-search
func searchMemtableKNN(p *plan.ANN, idx *indexState, snap types.Snapshot) []scoredHit {
	if p == nil || p.Knn == nil || len(p.Knn.Vector) == 0 {
		return scanMemtable(idx, snap) // fallback
	}
	queryVec := p.Knn.Vector
	k := p.Knn.K
	if k <= 0 {
		k = 10
	}
	fieldName := p.Knn.Field
	// Collect all memtable rows and compute distances.
	type candidate struct {
		rowID  uint32
		score  float32
		source []byte
		id     string
	}
	var cands []candidate
	idx.shard.IterateMemtable(snap, func(rowID types.RowID, id string, seq types.SeqNo, source []byte) bool {
		// Parse _source JSON to extract the vector field.
		var doc map[string]any
		if jsonx.Unmarshal(source, &doc) == nil {
			if vecRaw, ok := doc[fieldName]; ok {
				if vec, ok := vecRaw.([]any); ok {
					floatVec := make([]float32, len(vec))
					valid := true
					for i, v := range vec {
						if f, ok := v.(float64); ok {
							floatVec[i] = float32(f)
						} else {
							valid = false
							break
						}
					}
					if valid && len(floatVec) == len(queryVec) {
						// Compute dot product (cosine assumes normalized).
						var dot float32
						for i := range queryVec {
							dot += queryVec[i] * floatVec[i]
						}
						cands = append(cands, candidate{
							rowID:  uint32(rowID),
							score:  dot,
							source: source,
							id:     id,
						})
					}
				}
			}
		}
		return true
	})
	// Sort by score descending, keep top K.
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].score > cands[j].score
	})
	if len(cands) > k {
		cands = cands[:k]
	}
	hits := make([]scoredHit, len(cands))
	for i, c := range cands {
		hits[i] = scoredHit{rowID: c.rowID, score: c.score, source: c.source, id: c.id}
	}
	return hits
}

// searchMemtableFusion searches the memtable for each sub-query in a
// hybrid fusion and merges results.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#memtable-search
func searchMemtableFusion(p *plan.Fusion, idx *indexState, snap types.Snapshot) []scoredHit {
	if p == nil {
		return scanMemtable(idx, snap)
	}
	// For each fusion input, search the memtable and collect results.
	var allHits []scoredHit
	for _, input := range p.Inputs {
		var subHits []scoredHit
		switch typed := input.(type) {
		case *plan.BM25:
			subHits = searchMemtableBM25(typed, idx, snap)
		case *plan.ANN:
			subHits = searchMemtableKNN(typed, idx, snap)
		default:
			subHits = scanMemtable(idx, snap)
		}
		allHits = append(allHits, subHits...)
	}
	// Deduplicate by rowID, keeping the highest score.
	seen := make(map[uint32]float32)
	deduped := make([]scoredHit, 0, len(allHits))
	for _, h := range allHits {
		if existing, ok := seen[h.rowID]; !ok || h.score > existing {
			seen[h.rowID] = h.score
			deduped = append(deduped, h)
		}
	}
	// Sort by score descending.
	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].score > deduped[j].score
	})
	return deduped
}

// assembleHits converts scoredHits into esapi SearchHits by resolving
// _id and _source from the segment or memtable.
func assembleHits(hits []scoredHit, segSet *storage.SegmentSet, idx *indexState) []searchHitResult {
	var results []searchHitResult
	for _, h := range hits {
		if h.source != nil {
			// Memtable hit.
			results = append(results, searchHitResult{
				id:     h.id,
				score:  h.score,
				source: h.source,
			})
			continue
		}
		// Segment hit: resolve _id and _source using the direct entry reference.
		if h.entry == nil || h.entry.Reader == nil {
			continue
		}
		id, err := h.entry.Reader.DocID(types.RowID(h.rowID))
		if err != nil {
			continue
		}
		source, err := h.entry.Reader.Source(types.RowID(h.rowID))
		if err != nil {
			continue
		}
		results = append(results, searchHitResult{
			id:     id,
			score:  h.score,
			source: source,
		})
	}
	return results
}

// searchHitResult is the intermediate result before conversion to esapi.SearchHit.
type searchHitResult struct {
	id     string
	score  float32
	source []byte
}

// fieldTypeName returns the string name of a field type for plan node construction.
func fieldTypeName(ft meta.FieldType) string { return string(ft) }
