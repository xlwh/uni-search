package embedded

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	"github.com/unisearch-db/unisearch/internal/engine/plan"
	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/index/hnsw"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// translateQuery translates a DSL query into per-segment logical plan trees.
// Returns one plan per segment (each wrapped in TopN). The planner executes
// each plan independently and merges results via MergeSortOp.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.4
func translateQuery(q dsl.Query, idx *indexState, snap types.Snapshot, segSet *storage.SegmentSet, from, size int) ([]plan.LogicalPlan, error) {
	if segSet == nil || len(segSet.Items()) == 0 {
		return nil, nil
	}
	var plans []plan.LogicalPlan
	for _, entry := range segSet.Items() {
		if entry.Reader == nil {
			continue
		}
		segID := uint64(entry.Reader.ID())
		p, err := translateForSegment(q, idx, snap, segID, entry, from, size)
		if err != nil {
			return nil, err
		}
		if p != nil {
			plans = append(plans, p)
		}
	}
	return plans, nil
}

// translateForSegment translates a DSL query into a plan tree for one segment.
func translateForSegment(q dsl.Query, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	if q == nil {
		// match_all: Scan all rows via the _id column (always present).
		fid, _, _ := idx.resolveField("_id")
		if fid == 0 {
			fid = types.FieldID(1) // _id is always FieldID 1
		}
		return &plan.TopN{
			Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
			From:  from,
			Size:  size,
		}, nil
	}

	switch typed := q.(type) {
	case *dsl.MatchAllQuery:
		// match_all: scan all rows via _id field (same as nil query).
		fid, _, _ := idx.resolveField("_id")
		if fid == 0 {
			fid = types.FieldID(1)
		}
		return &plan.TopN{
			Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
			From:  from,
			Size:  size,
		}, nil
	case *dsl.MatchQuery:
		return translateMatch(typed, idx, snap, segID, entry, from, size)
	case *dsl.KnnQuery:
		return translateKnn(typed, idx, snap, segID, entry, from, size)
	case *dsl.SparseVectorQuery:
		return translateSparseVector(typed, idx, snap, segID, entry, from, size)
	case *dsl.TermQuery:
		return translateTerm(typed, idx, snap, segID, entry, from, size)
	case *dsl.TermsQuery:
		return translateTerms(typed, idx, snap, segID, entry, from, size)
	case *dsl.RangeQuery:
		return translateRange(typed, idx, snap, segID, entry, from, size)
	case *dsl.ExistsQuery:
		return translateExists(typed, idx, snap, segID, entry, from, size)
	case *dsl.BoolQuery:
		return translateBool(typed, idx, snap, segID, entry, from, size)
	case *dsl.HybridQuery:
		return translateHybrid(typed, idx, snap, segID, entry, from, size)
	case *dsl.MatchPhraseQuery:
		return translateMatchPhrase(typed, idx, snap, segID, entry, from, size)
	case *dsl.MultiMatchQuery:
		return translateMultiMatch(typed, idx, snap, segID, entry, from, size)
	case *dsl.QueryStringQuery:
		return translateQueryString(typed, idx, snap, segID, entry, from, size)
	case *dsl.FuzzyQuery:
		return translateFuzzy(typed, idx, snap, segID, entry, from, size)
	case *dsl.ConstantScoreQuery:
		return translateConstantScore(typed, idx, snap, segID, entry, from, size)
	case *dsl.BoostingQuery:
		return translateBoosting(typed, idx, snap, segID, entry, from, size)
	case *dsl.DisMaxQuery:
		return translateDisMax(typed, idx, snap, segID, entry, from, size)
	case *dsl.PrefixQuery:
		return translatePrefix(typed, idx, snap, segID, entry, from, size)
	case *dsl.RegexpQuery:
		return translateRegexp(typed, idx, snap, segID, entry, from, size)
	case *dsl.FunctionScoreQuery:
		return translateFunctionScore(typed, idx, snap, segID, entry, from, size)
	case *dsl.ScriptScoreQuery:
		return translateScriptScore(typed, idx, snap, segID, entry, from, size)
	case *dsl.GeoDistanceQuery:
		return translateGeoDistance(typed, idx, snap, segID, entry, from, size)
	case *dsl.GeoBoundingBoxQuery:
		return translateGeoBoundingBox(typed, idx, snap, segID, entry, from, size)
	case *dsl.MoreLikeThisQuery:
		return translateMoreLikeThis(typed, idx, snap, segID, entry, from, size)
	case *dsl.SimpleQueryStringQuery:
		return translateSimpleQueryString(typed, idx, snap, segID, entry, from, size)
	case *dsl.CombinedFieldsQuery:
		return translateCombinedFields(typed, idx, snap, segID, entry, from, size)
	case *dsl.MatchBoolPrefixQuery:
		return translateMatchBoolPrefix(typed, idx, snap, segID, entry, from, size)
	case *dsl.DistanceFeatureQuery:
		return translateDistanceFeature(typed, idx, snap, segID, entry, from, size)
	case *dsl.RankFeatureQuery:
		return translateRankFeature(typed, idx, snap, segID, entry, from, size)
	case *dsl.WrapperQuery:
		return translateForSegment(typed.Inner, idx, snap, segID, entry, from, size)
	case *dsl.PinnedQuery:
		return translateForSegment(typed.Organic, idx, snap, segID, entry, from, size)
	case *dsl.TermsSetQuery:
		return translateTermsSet(typed, idx, snap, segID, entry, from, size)
	case *dsl.MatchNoneQuery:
		return nil, nil // no matches
	case *dsl.ScriptQuery:
		return translateScript(typed, idx, snap, segID, entry, from, size)
	case *dsl.PercolateQuery:
		return translatePercolate(typed, idx, snap, segID, entry, from, size)
	case *dsl.GeoShapeQuery:
		return translateGeoShape(typed, idx, snap, segID, entry, from, size)
	case *dsl.IntervalsQuery:
		return translateIntervals(typed, idx, snap, segID, entry, from, size)
	case *dsl.NestedQuery:
		return translateNested(typed, idx, snap, segID, entry, from, size)
	default:
		return nil, fmt.Errorf("embedded: unsupported query type %T", q)
	}
}

// translateMatch translates a match query into a BM25 plan node (for text
// fields) or a Scan+Filter (for keyword fields).
func translateMatch(q *dsl.MatchQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, fd, ok := idx.resolveField(q.Field)
	if !ok {
		return nil, fmt.Errorf("embedded: unknown field %q for match query", q.Field)
	}

	// Use RouteForField (D3) to determine the index type.
	kinds := index.RouteForField(fd.Type)
	for _, kind := range kinds {
		if kind == index.KindInverted {
			// Text field → BM25 scoring via inverted index.
			return &plan.TopN{
				Input: &plan.BM25{Pred: q, TopK: from + size, SegID: segID},
				From:  from,
				Size:  size,
			}, nil
		}
	}

	// Non-text field (e.g. keyword) → term-level scan.
	return &plan.TopN{
		Input: &plan.Filter{
			Pred:  q,
			Input: &plan.Scan{SegID: segID, Field: fid, FieldType: string(fd.Type), Snapshot: snap},
		},
		From: from,
		Size: size,
	}, nil
}

// translateKnn translates a knn query into an ANN plan node.
func translateKnn(q *dsl.KnnQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, fd, ok := idx.resolveField(q.Field)
	if !ok {
		// Field not in mapping — fall back to scan all and let
		// postFilterHits filter by _source.
		fid2, _, _ := idx.resolveField("_id")
		if fid2 == 0 {
			fid2 = types.FieldID(1)
		}
		return &plan.TopN{
			Input: &plan.Scan{SegID: segID, Field: fid2, FieldType: "_id", Snapshot: snap},
			From:  from, Size: size,
		}, nil
	}
	if fd.Type != meta.TypeDenseVector {
		return nil, fmt.Errorf("embedded: field %q is not dense_vector", q.Field)
	}

	// Check if the segment has a loaded HNSW index for this field.
	if entry.Indexes != nil {
		if _, hasIndex := entry.Indexes[fid]; !hasIndex {
			// No index loaded for this segment — the planner will fall
			// back to brute-force scan.
		}
	}

	// Build filter plan if the knn query has filters.
	var filterPlan *hnsw.FilterPlan
	if len(q.Filter) > 0 {
		// For M0, use post-filter strategy. R4 optimizer will refine.
		filterPlan = &hnsw.FilterPlan{Strategy: hnsw.StrategyPostFilter}
	}

	k := q.K
	if k < size {
		k = size
	}

	return &plan.TopN{
		Input: &plan.ANN{Field: fid, Knn: q, FilterPlan: filterPlan, SegID: segID},
		From:  from,
		Size:  size,
	}, nil
}

// translateSparseVector translates a sparse_vector query into a
// SparseVector plan node. M1 uses brute-force memtable scan; segment-level
// inverted-index search (term = field\x00dim, tf = weight×2^8) is a
// follow-up (design 02 §5).
func translateSparseVector(q *dsl.SparseVectorQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, fd, ok := idx.resolveField(q.Field)
	if !ok {
		// Field not in mapping — fall back to scan all and let
		// postFilterHits filter by _source.
		fid2, _, _ := idx.resolveField("_id")
		if fid2 == 0 {
			fid2 = types.FieldID(1)
		}
		return &plan.TopN{
			Input: &plan.Scan{SegID: segID, Field: fid2, FieldType: "_id", Snapshot: snap},
			From:  from, Size: size,
		}, nil
	}
	if fd.Type != meta.TypeSparseVector {
		return nil, fmt.Errorf("embedded: field %q is not sparse_vector", q.Field)
	}

	return &plan.TopN{
		Input: &plan.SparseVector{Field: fid, Query: q, SegID: segID},
		From:  from,
		Size:  size,
	}, nil
}

// translateTerm translates a term query. For keyword/text fields, it uses
// the inverted index (like a match query with the exact term). For numeric
// fields, it falls back to Scan+Filter.
func translateTerm(q *dsl.TermQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, fd, ok := idx.resolveField(q.Field)
	if !ok {
		return nil, fmt.Errorf("embedded: unknown field %q for term query", q.Field)
	}
	// For keyword/text fields, use the inverted index for exact term match.
	if fd != nil && (fd.Type == meta.TypeKeyword || fd.Type == meta.TypeText) {
		termStr, _ := q.Value.(string)
		if termStr == "" {
			termStr = fmt.Sprintf("%v", q.Value)
		}
		return &plan.TopN{
			Input: &plan.BM25{
				SegID: segID,
				Pred:  &dsl.MatchQuery{Field: q.Field, Text: termStr},
			},
			From: from,
			Size: size,
		}, nil
	}
	// For numeric/boolean fields, use Scan+Filter with column data.
	return &plan.TopN{
		Input: &plan.Filter{
			Pred:  q,
			Input: &plan.Scan{SegID: segID, Field: fid, FieldType: string(getFieldType(idx, q.Field)), Snapshot: snap},
		},
		From: from,
		Size: size,
	}, nil
}

// translateTerms translates a terms query. For keyword/text fields, it uses
// the inverted index via a bool.should of match queries. For numeric fields,
// it falls back to Scan+Filter.
func translateTerms(q *dsl.TermsQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, fd, ok := idx.resolveField(q.Field)
	if !ok {
		return nil, fmt.Errorf("embedded: unknown field %q for terms query", q.Field)
	}
	// For keyword/text fields, use the inverted index via bool.should of match queries.
	if fd != nil && (fd.Type == meta.TypeKeyword || fd.Type == meta.TypeText) {
		shoulds := make([]dsl.Query, 0, len(q.Values))
		for _, v := range q.Values {
			s, _ := v.(string)
			if s == "" {
				s = fmt.Sprintf("%v", v)
			}
			shoulds = append(shoulds, &dsl.MatchQuery{Field: q.Field, Text: s})
		}
		if len(shoulds) == 1 {
			return &plan.TopN{
				Input: &plan.BM25{
					SegID: segID,
					Pred:  shoulds[0].(*dsl.MatchQuery),
				},
				From: from, Size: size,
			}, nil
		}
		// Build a bool.should query and translate it.
		boolQ := &dsl.BoolQuery{Should: shoulds}
		return translateBool(boolQ, idx, snap, segID, entry, from, size)
	}
	// For numeric/boolean fields, use Scan+Filter.
	return &plan.TopN{
		Input: &plan.Filter{
			Pred:  q,
			Input: &plan.Scan{SegID: segID, Field: fid, FieldType: string(getFieldType(idx, q.Field)), Snapshot: snap},
		},
		From: from,
		Size: size,
	}, nil
}

// translateRange translates a range query into a match_all scan.
// The post-filter in Search() handles the actual range check by
// inspecting each hit's _source.
func translateRange(q *dsl.RangeQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, _, ok := idx.resolveField(q.Field)
	if !ok {
		return nil, fmt.Errorf("embedded: unknown field %q for range query", q.Field)
	}
	_ = fid
	// Use match_all scan; post-filter checks range bounds in _source.
	idField, _, _ := idx.resolveField("_id")
	if idField == 0 {
		idField = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: idField, FieldType: "_id", Snapshot: snap},
		From:  from,
		Size:  size,
	}, nil
}

// translateExists translates an exists query into a match_all scan.
// The post-filter in Search() handles the actual field existence check
// by inspecting each hit's _source.
func translateExists(q *dsl.ExistsQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, _, ok := idx.resolveField(q.Field)
	if !ok {
		return nil, nil
	}
	_ = fid
	// Use match_all scan; post-filter checks field existence in _source.
	idField, _, _ := idx.resolveField("_id")
	if idField == 0 {
		idField = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: idField, FieldType: "_id", Snapshot: snap},
		From:  from,
		Size:  size,
	}, nil
}

// translateBool translates a bool query by recursively translating clauses.
// For must: translate the first clause only (post-filter handles the rest).
// For should: use match_all scan (post-filter keeps docs matching any should).
// For must_not: translate first must (post-filter handles must_not).
// The post-filter in Search() checks each hit's _source against the
// original DSL query for correct bool semantics.
func translateBool(q *dsl.BoolQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	if len(q.Must) > 0 {
		// Only translate the first must clause; the post-filter in Search()
		// handles additional must and must_not clauses by checking _source.
		return translateForSegment(q.Must[0], idx, snap, segID, entry, from, size)
	}
	if len(q.Should) > 0 {
		// For should-only queries, use match_all scan so all docs are
		// returned; the post-filter keeps docs matching at least one should.
		fid, _, _ := idx.resolveField("_id")
		if fid == 0 {
			fid = types.FieldID(1)
		}
		return &plan.TopN{
			Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
			From:  from,
			Size:  size,
		}, nil
	}
	if len(q.Filter) > 0 {
		return translateForSegment(q.Filter[0], idx, snap, segID, entry, from, size)
	}
	// Empty bool → match_all.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from,
		Size:  size,
	}, nil
}

// translateHybrid translates a hybrid query into a Fusion plan node.
func translateHybrid(q *dsl.HybridQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	var inputs []plan.LogicalPlan
	windowSize := q.Rank.WindowSize
	if windowSize <= 0 {
		windowSize = dsl.DefaultWindowSize
	}
	for _, sub := range q.Queries {
		subPlan, err := translateForSegment(sub, idx, snap, segID, entry, 0, windowSize)
		if err != nil {
			return nil, err
		}
		if subPlan != nil {
			// Strip the TopN wrapper to get the raw scorer.
			if topN, ok := subPlan.(*plan.TopN); ok {
				inputs = append(inputs, topN.Input)
			} else {
				inputs = append(inputs, subPlan)
			}
		}
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) == 1 {
		return &plan.TopN{Input: inputs[0], From: from, Size: size}, nil
	}
	return &plan.Fusion{Inputs: inputs, Rank: q.Rank, From: from, Size: size}, nil
}

// translateMatchPhrase translates a match_phrase query into a BM25 node
// (M0 treats phrase as a match with position awareness handled by the
// inverted index).
func translateMatchPhrase(q *dsl.MatchPhraseQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, fd, ok := idx.resolveField(q.Field)
	if !ok {
		return nil, fmt.Errorf("embedded: unknown field %q for matchphrase query", q.Field)
	}
	_ = fid // used by the BM25 scorer via the inverted index
	_ = fd
	// Reuse BM25 with a MatchQuery wrapper for phrase scoring.
	matchQ := &dsl.MatchQuery{Field: q.Field, Text: q.Text, Operator: "and"}
	return &plan.TopN{
		Input: &plan.BM25{Pred: matchQ, TopK: from + size, SegID: segID},
		From:  from,
		Size:  size,
	}, nil
}

// translateMultiMatch translates a multi_match query into a bool.should
// of match queries across the specified fields.
func translateMultiMatch(q *dsl.MultiMatchQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	if len(q.Fields) == 0 || q.Text == "" {
		return nil, fmt.Errorf("embedded: multi_match requires fields and query text")
	}
	// Build a bool.should of match queries for each field.
	boolQ := &dsl.BoolQuery{Should: make([]dsl.Query, len(q.Fields))}
	for i, field := range q.Fields {
		boolQ.Should[i] = &dsl.MatchQuery{Field: field, Text: q.Text, Operator: "or"}
	}
	return translateBool(boolQ, idx, snap, segID, entry, from, size)
}

// translateQueryString parses a simple query string (AND/OR/NOT) and
// translates to bool queries. Supports: "term", "term1 AND term2",
// "term1 OR term2", "term1 NOT term2".
func translateQueryString(q *dsl.QueryStringQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	if q.Query == "" {
		return nil, fmt.Errorf("embedded: query_string requires non-empty query")
	}
	// Find a default text field for the match queries.
	defaultField := findDefaultTextField(idx)

	upperQ := strings.ToUpper(q.Query)
	hasAnd := strings.Contains(upperQ, " AND ")
	hasOr := strings.Contains(upperQ, " OR ")
	hasNot := strings.Contains(upperQ, " NOT ")

	if hasAnd {
		parts := regexpAnd.Split(q.Query, -1)
		boolQ := &dsl.BoolQuery{Must: make([]dsl.Query, len(parts))}
		for i, p := range parts {
			boolQ.Must[i] = &dsl.MatchQuery{Text: strings.TrimSpace(p), Field: defaultField, Operator: "or"}
		}
		return translateBool(boolQ, idx, snap, segID, entry, from, size)
	}
	if hasNot {
		parts := regexpNotSplit.Split(q.Query, -1)
		boolQ := &dsl.BoolQuery{}
		if len(parts) > 0 {
			boolQ.Must = []dsl.Query{&dsl.MatchQuery{Text: strings.TrimSpace(parts[0]), Field: defaultField, Operator: "or"}}
		}
		for i := 1; i < len(parts); i++ {
			boolQ.MustNot = append(boolQ.MustNot, &dsl.MatchQuery{Text: strings.TrimSpace(parts[i]), Field: defaultField, Operator: "or"})
		}
		return translateBool(boolQ, idx, snap, segID, entry, from, size)
	}
	if hasOr {
		parts := regexpOr.Split(q.Query, -1)
		boolQ := &dsl.BoolQuery{Should: make([]dsl.Query, len(parts))}
		for i, p := range parts {
			boolQ.Should[i] = &dsl.MatchQuery{Text: strings.TrimSpace(p), Field: defaultField, Operator: "or"}
		}
		return translateBool(boolQ, idx, snap, segID, entry, from, size)
	}
	// Bare terms: treat as a single match query on default field.
	return translateMatch(&dsl.MatchQuery{Text: q.Query, Field: defaultField, Operator: "or"}, idx, snap, segID, entry, from, size)
}

// regexpAnd splits on AND (case-insensitive).
var regexpAnd = regexp.MustCompile("(?i) AND ")

// regexpOr splits on OR (case-insensitive).
var regexpOr = regexp.MustCompile("(?i) OR ")

// findDefaultTextField returns the first text field name in the index,
// or "title" as a fallback.
func findDefaultTextField(idx *indexState) string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for name, fid := range idx.fieldIDs {
		if fd, ok := idx.fieldDefs[fid]; ok && fd.Type == meta.TypeText {
			return name
		}
	}
	return "title"
}

// regexpNotSplit splits on NOT (case-insensitive).
var regexpNotSplit = regexp.MustCompile(`(?i) NOT `)

// translateFuzzy translates a fuzzy query into a match query with fuzziness.
// The fuzzy query is mapped to a match query with the fuzziness parameter set.
func translateFuzzy(q *dsl.FuzzyQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, fd, ok := idx.resolveField(q.Field)
	if !ok {
		return nil, fmt.Errorf("embedded: unknown field %q for fuzzy query", q.Field)
	}
	_ = fid
	_ = fd
	// Reuse BM25 with a MatchQuery wrapper with fuzziness.
	matchQ := &dsl.MatchQuery{Field: q.Field, Text: q.Value, Fuzziness: q.Fuzziness, Operator: "or"}
	return &plan.TopN{
		Input: &plan.BM25{Pred: matchQ, TopK: from + size, SegID: segID},
		From:  from,
		Size:  size,
	}, nil
}

// getFieldType returns the meta.FieldType for a field name.
func getFieldType(idx *indexState, field string) meta.FieldType {
	_, fd, ok := idx.resolveField(field)
	if !ok {
		return meta.TypeKeyword // default
	}
	return fd.Type
}

// ---------------------------------------------------------------------------
// P0 DSL query translators (es-feature-gap-fill)
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.2
// ---------------------------------------------------------------------------

// translateConstantScore wraps the inner query in a filter context.
// The inner query is translated normally; the plan layer applies a constant
// score equal to Boost instead of the BM25 score.
func translateConstantScore(q *dsl.ConstantScoreQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Delegate to the inner query's translation. The constant score
	// semantics (score = boost) are applied by the post-filter stage
	// which checks _source against docMatchesQuery.
	return translateForSegment(q.Filter, idx, snap, segID, entry, from, size)
}

// translateBoosting translates the positive query as the primary plan.
// The negative query is applied as a post-filter that down-scores matching
// documents by multiplying with NegativeBoost.
func translateBoosting(q *dsl.BoostingQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Translate the positive query as the primary plan.
	return translateForSegment(q.Positive, idx, snap, segID, entry, from, size)
}

// translateDisMax translates a dis_max query. For P0 we translate the first
// sub-query as the primary plan and rely on the post-filter stage to merge
// scores from all sub-queries using the dis_max formula. When multiple
// sub-queries exist, we use a Fusion plan node to combine them.
func translateDisMax(q *dsl.DisMaxQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	if len(q.Queries) == 1 {
		return translateForSegment(q.Queries[0], idx, snap, segID, entry, from, size)
	}
	// P2: Use bool.should to match docs from any sub-query.
	bq := &dsl.BoolQuery{Should: q.Queries, Boost: q.Boost}
	return translateBool(bq, idx, snap, segID, entry, from, size)
}

// translatePrefix translates a prefix query. For text fields it uses BM25
// with the prefix as a match query (simplified P0: the inverted index will
// match terms starting with the prefix). For keyword fields it uses a
// Scan+Filter with a prefix predicate.
func translatePrefix(q *dsl.PrefixQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Use _id Scan + postFilterHits for all field types.
	// The postFilterHits stage checks _source using docMatchesQuery,
	// which correctly handles prefix/regexp/geo/etc. matching.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}
func translateRegexp(q *dsl.RegexpQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Use _id Scan + postFilterHits for all field types.
	// The postFilterHits stage checks _source using docMatchesQuery,
	// which correctly handles prefix/regexp/geo/etc. matching.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}
func translateFunctionScore(q *dsl.FunctionScoreQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	if q.Query != nil {
		return translateForSegment(q.Query, idx, snap, segID, entry, from, size)
	}
	// No inner query: match_all + apply functions.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}

// translateScriptScore delegates to the inner query; the script
// re-scoring (and min_score filtering) is applied to the merged hits
// in Engine.Search so the memtable and segment paths share one
// implementation.
func translateScriptScore(q *dsl.ScriptScoreQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	return translateForSegment(q.Query, idx, snap, segID, entry, from, size)
}

// translateGeoDistance translates a geo_distance query into a Scan+Filter
// plan. P1 uses a simple scan with the geo predicate; future versions can
// leverage the BKD 2D index for efficient spatial queries.
func translateGeoDistance(q *dsl.GeoDistanceQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Use _id Scan + postFilterHits for all field types.
	// The postFilterHits stage checks _source using docMatchesQuery,
	// which correctly handles prefix/regexp/geo/etc. matching.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}
func translateGeoBoundingBox(q *dsl.GeoBoundingBoxQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Use _id Scan + postFilterHits for all field types.
	// The postFilterHits stage checks _source using docMatchesQuery,
	// which correctly handles prefix/regexp/geo/etc. matching.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}
func translateMoreLikeThis(q *dsl.MoreLikeThisQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	if len(q.LikeTexts) > 0 {
		fields := q.Fields
		if len(fields) == 0 {
			fields = []string{"_all"}
		}
		matchQ := &dsl.MatchQuery{Field: fields[0], Text: q.LikeTexts[0], Operator: "or", Boost: q.Boost}
		return translateMatch(matchQ, idx, snap, segID, entry, from, size)
	}
	return nil, nil
}

func translateSimpleQueryString(q *dsl.SimpleQueryStringQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fields := q.Fields
	if len(fields) == 0 {
		fields = []string{"_all"}
	}
	matchQ := &dsl.MatchQuery{Field: fields[0], Text: q.Query, Operator: q.DefaultOperator, Boost: q.Boost}
	return translateMatch(matchQ, idx, snap, segID, entry, from, size)
}

func translateCombinedFields(q *dsl.CombinedFieldsQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	if len(q.Fields) > 0 {
		matchQ := &dsl.MatchQuery{Field: q.Fields[0], Text: q.Query, Operator: q.Operator, Boost: q.Boost}
		return translateMatch(matchQ, idx, snap, segID, entry, from, size)
	}
	return nil, nil
}

func translateMatchBoolPrefix(q *dsl.MatchBoolPrefixQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	matchQ := &dsl.MatchQuery{Field: q.Field, Text: q.Query, Operator: "or", Boost: q.Boost}
	return translateMatch(matchQ, idx, snap, segID, entry, from, size)
}

// ---------------------------------------------------------------------------
// P3 DSL query translators (es-feature-gap-fill)
// ---------------------------------------------------------------------------

func translateDistanceFeature(q *dsl.DistanceFeatureQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Use _id Scan + postFilterHits for all field types.
	// The postFilterHits stage checks _source using docMatchesQuery,
	// which correctly handles prefix/regexp/geo/etc. matching.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}
func translateRankFeature(q *dsl.RankFeatureQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Use _id Scan + postFilterHits for all field types.
	// The postFilterHits stage checks _source using docMatchesQuery,
	// which correctly handles prefix/regexp/geo/etc. matching.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}
func translateTermsSet(q *dsl.TermsSetQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Use _id Scan + postFilterHits for all field types.
	// The postFilterHits stage checks _source using docMatchesQuery,
	// which correctly handles prefix/regexp/geo/etc. matching.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}
func translateScript(q *dsl.ScriptQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Filter{Pred: q, Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap}},
		From:  from, Size: size,
	}, nil
}

func translatePercolate(q *dsl.PercolateQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Filter{Pred: q, Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap}},
		From:  from, Size: size,
	}, nil
}

func translateGeoShape(q *dsl.GeoShapeQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	// Use _id Scan + postFilterHits for all field types.
	// The postFilterHits stage checks _source using docMatchesQuery,
	// which correctly handles prefix/regexp/geo/etc. matching.
	fid, _, _ := idx.resolveField("_id")
	if fid == 0 {
		fid = types.FieldID(1)
	}
	return &plan.TopN{
		Input: &plan.Scan{SegID: segID, Field: fid, FieldType: "_id", Snapshot: snap},
		From:  from, Size: size,
	}, nil
}
func translateIntervals(q *dsl.IntervalsQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	matchQ := &dsl.MatchQuery{Field: q.Field, Text: fmt.Sprintf("%v", q.Rules), Operator: "or", Boost: q.Boost}
	return translateMatch(matchQ, idx, snap, segID, entry, from, size)
}

func translateNested(q *dsl.NestedQuery, idx *indexState, snap types.Snapshot, segID uint64, entry *storage.SegmentEntry, from, size int) (plan.LogicalPlan, error) {
	return translateForSegment(q.Query, idx, snap, segID, entry, from, size)
}
