package dsl

import (
	"bytes"
	"encoding/base64"
	"github.com/unisearch-db/unisearch/internal/jsonx"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/unisearch-db/unisearch/internal/errors"
)

// parseCtx tracks the JSON path of the clause currently being parsed so
// that every error message can point at the exact location, e.g.
// query.bool.must[2].match.foo.
type parseCtx struct {
	path string
}

func (c *parseCtx) key(k string) *parseCtx { return &parseCtx{path: c.path + "." + k} }

func (c *parseCtx) idx(i int) *parseCtx {
	return &parseCtx{path: fmt.Sprintf("%s[%d]", c.path, i)}
}

// illegalAt builds a 400 illegal_argument error whose reason embeds the
// JSON path of the offending clause.
func illegalAt(c *parseCtx, format string, args ...any) *errors.APIError {
	return errors.NewIllegalArgument(fmt.Sprintf(format, args...) + " at [" + c.path + "]")
}

func queryRootCtx() *parseCtx { return &parseCtx{path: "query"} }

func knnRootCtx() *parseCtx { return &parseCtx{path: "knn"} }

// ParseQuery parses either a complete search body ({"query": {...}}) or a
// bare query object ({"term": {...}}) into an AST. A search body that
// carries no query clause (and no top-level knn) parses to a nil Query
// with a nil error; deciding the match-all semantics of that case is the
// search layer's business (T16).
func ParseQuery(body []byte) (Query, error) {
	var top map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &top); err != nil || top == nil {
		return nil, errors.NewIllegalArgument("request body must be a valid JSON object")
	}
	if _, ok := top["query"]; ok || isSearchBody(top) {
		q, _, _, err := ParseSearchBody(body)
		return q, err
	}
	return parseQueryMap(queryRootCtx(), top)
}

// ParseSearchBody parses a complete search request body. Interprets
// "query", "knn", "from", "size"; other M1 keys (aggs, highlight, sort,
// pit, search_after) are parsed by ParseSearchBodyFull.
func ParseSearchBody(body []byte) (Query, int, int, error) {
	var top map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &top); err != nil || top == nil {
		return nil, 0, 10, errors.NewIllegalArgument("request body must be a valid JSON object")
	}

	from, size := 0, 10
	var (
		q    Query
		knns []Query
		err  error
	)
	for k, vraw := range top {
		switch k {
		case "query":
			q, err = parseQueryRaw(queryRootCtx(), vraw)
		case "knn":
			knns, err = parseTopKnn(knnRootCtx(), vraw)
		case "from":
			from, err = decodeNonNegativeInt(&parseCtx{path: "from"}, vraw)
		case "size":
			size, err = decodeNonNegativeInt(&parseCtx{path: "size"}, vraw)
		}
		if err != nil {
			return nil, 0, 10, err
		}
	}

	switch {
	case q != nil && len(knns) == 0:
		return q, from, size, nil
	case q == nil && len(knns) == 0:
		return nil, from, size, nil
	case q == nil && len(knns) == 1:
		return knns[0], from, size, nil
	case q == nil:
		// Multiple top-level knn clauses: ES unions their results, ranking
		// each hit by its originating clause; M0 approximates the union with
		// a bool.should requiring exactly one clause to match per hit.
		return &BoolQuery{Should: knns, MinShouldMatch: "1", Boost: DefaultBoost}, from, size, nil
	default:
		// Both a query and a top-level knn clause are present. ES 8.12
		// semantics: the knn search runs first and the query restricts its
		// result set, which is exactly a filtered knn search (the query
		// contributes no score, matching bool.filter semantics). We
		// materialize that as BoolQuery{Filter: [query], Must: [knn]} so
		// the plan layer can push the filter into the ANN search
		// (optimizer rules R1/R4, design 03 §3.2).
		bq := &BoolQuery{Filter: []Query{q}, Boost: DefaultBoost}
		if len(knns) == 1 {
			bq.Must = knns
		} else {
			bq.Should = knns
			bq.MinShouldMatch = "1"
		}
		return bq, from, size, nil
	}
}

// SearchExtras carries M1 search body fields that the engine layer
// consumes beyond the M0 query/from/size triple. Each field is the raw
// JSON (nil when absent in the request).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.8
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3
type SearchExtras struct {
	Aggs           jsonx.RawMessage
	Highlight      jsonx.RawMessage
	Sort           jsonx.RawMessage
	PIT            jsonx.RawMessage
	SearchAfter    jsonx.RawMessage
	ScriptFields   jsonx.RawMessage // M1: script_fields for computed fields
	TrackTotalHits jsonx.RawMessage // M1: false=skip count, int=cap
	TerminateAfter int              // M1: row threshold for early termination (0=disabled)
	Source         jsonx.RawMessage // M1: _source filtering (includes/excludes)
	Rescore        jsonx.RawMessage // P0: rescore query specs
	Collapse       jsonx.RawMessage // P0: collapse field + inner_hits
	PostFilter     jsonx.RawMessage // P0: post-filter query
	RuntimeMappings jsonx.RawMessage // P2: runtime field definitions
}

// ParseSearchBodyFull parses a search body and returns the raw JSON
// for aggs, highlight, sort, pit, and search_after in addition to the
// M0 query/from/size triple.
func ParseSearchBodyFull(body []byte) (q Query, from, size int, extras SearchExtras, err error) {
	var top map[string]jsonx.RawMessage
	if e := jsonx.Unmarshal(body, &top); e != nil || top == nil {
		return nil, 0, 10, SearchExtras{}, errors.NewIllegalArgument("request body must be a valid JSON object")
	}
	q, from, size, err = ParseSearchBody(body)
	if err != nil {
		return nil, 0, 10, SearchExtras{}, err
	}
	for k, vraw := range top {
		switch k {
		case "aggs", "aggregations":
			extras.Aggs = vraw
		case "highlight":
			extras.Highlight = vraw
		case "sort":
			extras.Sort = vraw
		case "pit":
			extras.PIT = vraw
		case "search_after":
			extras.SearchAfter = vraw
		case "script_fields":
			extras.ScriptFields = vraw
		case "track_total_hits":
			extras.TrackTotalHits = vraw
		case "_source":
			extras.Source = vraw
		case "rescore":
			extras.Rescore = vraw
		case "collapse":
			extras.Collapse = vraw
		case "post_filter":
			extras.PostFilter = vraw
		case "runtime_mappings":
			extras.RuntimeMappings = vraw
		case "terminate_after":
			var n int
			if jsonx.Unmarshal(vraw, &n) == nil && n > 0 {
				extras.TerminateAfter = n
			}
		}
	}
	return q, from, size, extras, nil
}

// searchBodyKeys lists top-level keys that only ever occur in a search
// body, never inside a bare query object. Their presence tells ParseQuery
// that the input is a search body, so bodies without a "query" key parse
// to nil instead of misreporting e.g. "size" as an unknown query type.
// Note: "knn" is deliberately included; a bare {"knn": {...}} object has
// the same meaning under both interpretations.
var searchBodyKeys = map[string]bool{
	"from": true, "size": true, "knn": true, "sort": true, "aggs": true,
	"aggregations": true, "post_filter": true, "highlight": true,
	"suggest": true, "rescore": true, "collapse": true, "docvalue_fields": true,
	"stored_fields": true, "script_fields": true, "runtime_mappings": true,
	"track_total_hits": true, "terminate_after": true, "timeout": true,
	"min_score": true, "search_after": true, "pit": true, "indices_boost": true,
	"profile": true, "_source": true, "version": true, "explain": true,
	"track_scores": true, "stats": true,
}

func isSearchBody(top map[string]jsonx.RawMessage) bool {
	for k := range top {
		if searchBodyKeys[k] {
			return true
		}
	}
	return false
}

// parseQueryRaw parses a query clause (a JSON object holding exactly one
// query type) at the given path.
func parseQueryRaw(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, illegalAt(c, "query clause must be a JSON object")
	}
	return parseQueryMap(c, m)
}

func parseQueryMap(c *parseCtx, m map[string]jsonx.RawMessage) (Query, error) {
	if len(m) != 1 {
		return nil, illegalAt(c, "query clause must contain exactly one query type, got %d", len(m))
	}
	for name, raw := range m {
		return dispatchQuery(c, name, raw)
	}
	return nil, nil // unreachable: m has exactly one entry
}

func dispatchQuery(c *parseCtx, name string, raw jsonx.RawMessage) (Query, error) {
	switch name {
	case "term":
		return parseTermQuery(c, raw)
	case "terms":
		return parseTermsQuery(c, raw)
	case "range":
		return parseRangeQuery(c, raw)
	case "exists":
		return parseExistsQuery(c, raw)
	case "match":
		return parseMatchQuery(c, raw)
	case "match_phrase":
		return parseMatchPhraseQuery(c, raw)
	case "bool":
		return parseBoolQuery(c, raw)
	case "knn":
		return parseKnnQuery(c, raw)
	case "sparse_vector":
		return parseSparseVectorQuery(c, raw)
	case "hybrid":
		return parseHybridQuery(c, raw)
	case "match_all":
		return parseMatchAllQuery(c, raw)
	case "multi_match":
		return parseMultiMatchQuery(c, raw)
	case "query_string":
		return parseQueryStringQuery(c, raw)
	case "fuzzy":
		return parseFuzzyQuery(c, raw)
	case "match_phrase_prefix":
		return parseMatchPhrasePrefixQuery(c, raw)
	case "wildcard":
		return parseWildcardQuery(c, raw)
	case "ids":
		return parseIdsQuery(c, raw)
	case "prefix":
		return parsePrefixQuery(c, raw)
	case "regexp":
		return parseRegexpQuery(c, raw)
	case "constant_score":
		return parseConstantScoreQuery(c, raw)
	case "boosting":
		return parseBoostingQuery(c, raw)
	case "dis_max":
		return parseDisMaxQuery(c, raw)
	case "function_score":
		return parseFunctionScoreQuery(c, raw)
	case "script_score":
		return parseScriptScoreQuery(c, raw)
	case "geo_distance":
		return parseGeoDistanceQuery(c, raw)
	case "geo_bounding_box":
		return parseGeoBoundingBoxQuery(c, raw)
	case "more_like_this":
		return parseMoreLikeThisQuery(c, raw)
	case "simple_query_string":
		return parseSimpleQueryStringQuery(c, raw)
	case "combined_fields":
		return parseCombinedFieldsQuery(c, raw)
	case "match_bool_prefix":
		return parseMatchBoolPrefixQuery(c, raw)
	case "distance_feature":
		return parseDistanceFeatureQuery(c, raw)
	case "rank_feature":
		return parseRankFeatureQuery(c, raw)
	case "wrapper":
		return parseWrapperQuery(c, raw)
	case "pinned":
		return parsePinnedQuery(c, raw)
	case "terms_set":
		return parseTermsSetQuery(c, raw)
	case "match_none":
		return &MatchNoneQuery{}, nil
	case "script":
		return parseScriptQuery(c, raw)
	case "percolate":
		return parsePercolateQuery(c, raw)
	case "geo_shape":
		return parseGeoShapeQuery(c, raw)
	case "intervals":
		return parseIntervalsQuery(c, raw)
	case "nested":
		return parseNestedQuery(c, raw)
	default:
		return nil, newUnsupportedQuery(name)
	}
}

// unsupportedQueries maps DSL names that are recognized from Elasticsearch
// but not implemented in the M0 subset to their planned milestone and a
// suggested ES-compatible workaround. Anything not listed here is still
// rejected as unsupported, with a generic message.
var unsupportedQueries = map[string]struct {
	milestone  string
	workaround string
}{
	"wildcard":            {"v1.2 (M1)", "enumerate the matching terms client-side and use a terms query"},
	"ids":                 {"v1.2 (M1)", "use a terms query on the _id field"},
	"semantic":            {"v1.3 (M2)", "embed the text client-side and use match plus knn in a hybrid query"},
}

// newUnsupportedQuery rejects a query type outside the M0 subset with the
// 400 + unsupported_capability envelope (errors registry, 00 §6.1).
func newUnsupportedQuery(name string) *errors.APIError {
	if info, ok := unsupportedQueries[name]; ok {
		return errors.NewUnsupported("query."+name, info.milestone, info.workaround)
	}
	return errors.NewUnsupported("query."+name, "v1.2 (M1)", "check the supported query DSL subset in the documentation")
}

// parseMatchAllQuery parses {"match_all": {}} — matches all documents.
func parseMatchAllQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	// match_all accepts an empty object or an object with boost.
	return &MatchAllQuery{}, nil
}

// parseMultiMatchQuery parses {"multi_match": {"query": "...", "fields": [...]}}.
// Translates to a bool.should of match queries across fields.
func parseMultiMatchQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return nil, illegalAt(c, "multi_match: %v", err)
	}
	q := &MultiMatchQuery{}
	if v, ok := m["query"]; ok {
		jsonx.Unmarshal(v, &q.Text)
	}
	if v, ok := m["fields"]; ok {
		jsonx.Unmarshal(v, &q.Fields)
	}
	if v, ok := m["type"]; ok {
		jsonx.Unmarshal(v, &q.Type)
	}
	if q.Type == "" {
		q.Type = "best_fields"
	}
	return q, nil
}

// parseQueryStringQuery parses {"query_string": {"query": "hello AND world"}}.
// Supports basic AND/OR/NOT and bare term syntax.
func parseQueryStringQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return nil, illegalAt(c, "query_string: %v", err)
	}
	q := &QueryStringQuery{}
	if v, ok := m["query"]; ok {
		jsonx.Unmarshal(v, &q.Query)
	}
	return q, nil
}

// parseFuzzyQuery parses {"fuzzy": {"field": {"value": "term", "fuzziness": "AUTO"}}}.
func parseFuzzyQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return nil, illegalAt(c, "fuzzy: %v", err)
	}
	field, vraw, err := singleField(c, "fuzzy", m)
	if err != nil {
		return nil, err
	}
	q := &FuzzyQuery{Field: field}
	// Value can be a string or an object with value/fuzziness.
	var s string
	if err := jsonx.Unmarshal(vraw, &s); err == nil {
		q.Value = s
	} else {
		var opts map[string]jsonx.RawMessage
		if err := jsonx.Unmarshal(vraw, &opts); err != nil {
			return nil, illegalAt(c, "fuzzy: field value must be string or object")
		}
		if v, ok := opts["value"]; ok {
			jsonx.Unmarshal(v, &q.Value)
		}
		if v, ok := opts["fuzziness"]; ok {
			jsonx.Unmarshal(v, &q.Fuzziness)
		}
	}
	if q.Fuzziness == "" {
		q.Fuzziness = "AUTO"
	}
	return q, nil
}

// parseMatchPhrasePrefixQuery parses {"match_phrase_prefix": {"<field>": {"query": "text"}}}.
// M0 maps this to a MatchQuery with the same text (prefix matching is
// approximated by the standard analyzer tokenization).
func parseMatchPhrasePrefixQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return nil, illegalAt(c, "match_phrase_prefix: %v", err)
	}
	field, vraw, err := singleField(c, "match_phrase_prefix", m)
	if err != nil {
		return nil, err
	}
	var body map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(vraw, &body); err != nil {
		// Shorthand: {"match_phrase_prefix": {"field": "text"}}
		var text string
		if jsonx.Unmarshal(vraw, &text) == nil {
			return &MatchQuery{Field: field, Text: text, Operator: "or"}, nil
		}
		return nil, illegalAt(c, "match_phrase_prefix: expected object or string")
	}
	q := &MatchQuery{Field: field, Operator: "or"}
	for k, v := range body {
		switch k {
		case "query", "text":
			jsonx.Unmarshal(v, &q.Text)
		case "boost":
			decodeFloat(c.key("boost"), v)
		default:
			// ignore unknown fields for compatibility
		}
	}
	if q.Text == "" {
		return nil, illegalAt(c, "match_phrase_prefix requires a [query]")
	}
	return q, nil
}

// parseWildcardQuery parses {"wildcard": {"<field>": "pattern*"}}.
// M0 maps this to a MatchQuery with the wildcard pattern converted to
// plain text (stripping * and ? wildcards) for approximate matching.
func parseWildcardQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return nil, illegalAt(c, "wildcard: %v", err)
	}
	field, vraw, err := singleField(c, "wildcard", m)
	if err != nil {
		return nil, err
	}
	// Accept both {"field": "pattern"} and {"field": {"value": "pattern"}}.
	var pattern string
	var body map[string]jsonx.RawMessage
	if jsonx.Unmarshal(vraw, &body) == nil {
		if v, ok := body["value"]; ok {
			jsonx.Unmarshal(v, &pattern)
		}
	} else {
		jsonx.Unmarshal(vraw, &pattern)
	}
	if pattern == "" {
		return nil, illegalAt(c, "wildcard requires a value")
	}
	// If no wildcard characters, use exact term match (works on keyword fields).
	if !strings.ContainsAny(pattern, "*?") {
		return &TermQuery{Field: field, Value: pattern}, nil
	}
	// Strip wildcard characters for approximate match.
	text := strings.ReplaceAll(pattern, "*", "")
	text = strings.ReplaceAll(text, "?", "")
	if text == "" {
		text = pattern // fallback to original pattern
	}
	return &MatchQuery{Field: field, Text: text, Operator: "or"}, nil
}

// parseIdsQuery parses {"ids": {"values": ["id1", "id2", ...]}}.
// M0 maps this to a TermsQuery on the _id field.
func parseIdsQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return nil, illegalAt(c, "ids: %v", err)
	}
	vraw, ok := m["values"]
	if !ok {
		return nil, illegalAt(c, "ids requires [values]")
	}
	var vals []any
	if err := jsonx.Unmarshal(vraw, &vals); err != nil {
		return nil, illegalAt(c, "ids: [values] must be an array")
	}
	if len(vals) == 0 {
		return nil, illegalAt(c, "ids: [values] must not be empty")
	}
	return &TermsQuery{Field: "_id", Values: vals}, nil
}

// singleField extracts the single field-name key from a leaf query object
// ({"<type>": {"<field>": ...}}); leaf queries in this subset accept
// exactly one field.
func singleField(c *parseCtx, typeName string, m map[string]jsonx.RawMessage) (string, jsonx.RawMessage, error) {
	if len(m) != 1 {
		return "", nil, illegalAt(c, "[%s] query requires exactly one field, got %d", typeName, len(m))
	}
	for k, v := range m {
		if k == "" {
			return "", nil, illegalAt(c, "[%s] field name must not be empty", typeName)
		}
		return k, v, nil
	}
	return "", nil, nil // unreachable: m has exactly one entry
}

func parseTermQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("term")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	field, fraw, err := singleField(nodeCtx, "term", m)
	if err != nil {
		return nil, err
	}
	fctx := nodeCtx.key(field)
	q := &TermQuery{Field: field, Boost: DefaultBoost}

	// Short form: {"term": {"field": "value"}}.
	if firstByte(fraw) != '{' {
		if firstByte(fraw) == 'n' {
			return nil, illegalAt(fctx, "[term] value must not be null")
		}
		if q.Value, err = decodeScalar(fctx, fraw); err != nil {
			return nil, err
		}
		return q, nil
	}

	obj, err := decodeObject(fctx, fraw)
	if err != nil {
		return nil, err
	}
	for k, vraw := range obj {
		switch k {
		case "value":
			if firstByte(vraw) == 'n' {
				return nil, illegalAt(fctx.key("value"), "[term] value must not be null")
			}
			if q.Value, err = decodeScalar(fctx.key("value"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			if q.Boost, err = decodeFloat(fctx.key("boost"), vraw); err != nil {
				return nil, err
			}
		case "case_insensitive":
			if q.CaseInsensitive, err = decodeBool(fctx.key("case_insensitive"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(fctx, "[term] query does not support [%s]", k)
		}
	}
	if q.Value == nil {
		return nil, illegalAt(fctx, "[term] requires a [value]")
	}
	return q, nil
}

func parseTermsQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("terms")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}

	q := &TermsQuery{Boost: DefaultBoost}
	fieldCount := 0
	for k, vraw := range m {
		if k == "boost" {
			// boost is hoisted next to the field key, per the ES terms syntax
			// {"terms": {"field": [...], "boost": 2.0}}.
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil {
				return nil, err
			}
			continue
		}
		fieldCount++
		fctx := nodeCtx.key(k)
		switch firstByte(vraw) {
		case '[':
			var arr []jsonx.RawMessage
			if err := jsonx.Unmarshal(vraw, &arr); err != nil {
				return nil, illegalAt(fctx, "[terms] requires an array of values")
			}
			vals := make([]any, 0, len(arr))
			for i, el := range arr {
				v, err := decodeScalar(fctx.idx(i), el)
				if err != nil {
					return nil, err
				}
				vals = append(vals, v)
			}
			q.Field, q.Values = k, vals
		case '{':
			// ES terms lookup references another document; not implemented.
			return nil, errors.NewUnsupported("query.terms.lookup", "v1.3 (M2)",
				"retrieve the reference values client-side and inline them as a terms array")
		default:
			return nil, illegalAt(fctx, "[terms] requires an array of values, got %s", jsonKindName(vraw))
		}
	}
	if fieldCount != 1 {
		return nil, illegalAt(nodeCtx, "[terms] query requires exactly one field, got %d", fieldCount)
	}
	return q, nil
}

func parseRangeQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("range")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	field, fraw, err := singleField(nodeCtx, "range", m)
	if err != nil {
		return nil, err
	}
	fctx := nodeCtx.key(field)

	obj, err := decodeObject(fctx, fraw)
	if err != nil {
		return nil, err
	}

	q := &RangeQuery{Field: field, Boost: DefaultBoost}
	bounds := 0
	for k, vraw := range obj {
		switch k {
		case "gt", "gte", "lt", "lte":
			v, err := decodeRangeBound(fctx.key(k), vraw)
			if err != nil {
				return nil, err
			}
			bounds++
			switch k {
			case "gt":
				q.Gt = v
			case "gte":
				q.Gte = v
			case "lt":
				q.Lt = v
			case "lte":
				q.Lte = v
			}
		case "time_zone":
			if q.TimeZone, err = decodeString(fctx.key("time_zone"), vraw); err != nil {
				return nil, err
			}
		case "format":
			if q.Format, err = decodeString(fctx.key("format"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			if q.Boost, err = decodeFloat(fctx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(fctx, "[range] query does not support [%s]", k)
		}
	}
	if bounds == 0 {
		return nil, illegalAt(fctx, "[range] requires at least one of [gt], [gte], [lt], [lte]")
	}
	return q, nil
}

// decodeRangeBound accepts numbers or date strings; the date-math literal
// grammar itself is validated at plan time (design 03 §2.2).
func decodeRangeBound(c *parseCtx, raw jsonx.RawMessage) (any, error) {
	switch firstByte(raw) {
	case '"':
		s, err := decodeString(c, raw)
		if err != nil {
			return nil, err
		}
		if s == "" {
			return nil, illegalAt(c, "range bound must not be an empty string")
		}
		return s, nil
	case 'n', '{', '[':
		return nil, illegalAt(c, "range bound must be a number or a date string, got %s", jsonKindName(raw))
	default:
		var f float64
		if err := jsonx.Unmarshal(raw, &f); err != nil {
			return nil, illegalAt(c, "range bound must be a number or a date string, got %s", jsonKindName(raw))
		}
		return f, nil
	}
}

func parseExistsQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("exists")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &ExistsQuery{}
	for k, vraw := range m {
		if k != "field" {
			return nil, illegalAt(nodeCtx, "[exists] query does not support [%s]", k)
		}
		if q.Field, err = decodeNonEmptyString(nodeCtx.key("field"), vraw); err != nil {
			return nil, err
		}
	}
	if q.Field == "" {
		return nil, illegalAt(nodeCtx, "[exists] requires a [field]")
	}
	return q, nil
}

func parseMatchQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("match")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	field, fraw, err := singleField(nodeCtx, "match", m)
	if err != nil {
		return nil, err
	}
	fctx := nodeCtx.key(field)

	q := &MatchQuery{Field: field, Boost: DefaultBoost, Operator: "or"}

	// Short form: {"match": {"field": "text"}}.
	if firstByte(fraw) != '{' {
		if q.Text, err = decodeText(fctx, fraw); err != nil {
			return nil, err
		}
		return q, nil
	}

	obj, err := decodeObject(fctx, fraw)
	if err != nil {
		return nil, err
	}
	seenText := false
	for k, vraw := range obj {
		switch k {
		case "query", "text":
			// "query" is the ES key; "text" is accepted as an alias.
			if seenText {
				return nil, illegalAt(fctx, "[match] accepts only one of [query] or [text]")
			}
			seenText = true
			if q.Text, err = decodeText(fctx, vraw); err != nil {
				return nil, err
			}
		case "operator":
			if q.Operator, err = decodeOperator(fctx.key("operator"), vraw); err != nil {
				return nil, err
			}
		case "fuzziness":
			if q.Fuzziness, err = decodeFuzziness(fctx.key("fuzziness"), vraw); err != nil {
				return nil, err
			}
		case "minimum_should_match":
			if q.MinimumShouldMatch, err = decodeMinimumShouldMatch(fctx.key("minimum_should_match"), vraw); err != nil {
				return nil, err
			}
		case "analyzer":
			if q.Analyzer, err = decodeNonEmptyString(fctx.key("analyzer"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			if q.Boost, err = decodeFloat(fctx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(fctx, "[match] query does not support [%s]", k)
		}
	}
	if !seenText {
		return nil, illegalAt(fctx, "[match] requires a [query] (or [text])")
	}
	return q, nil
}

func parseMatchPhraseQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("match_phrase")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	field, fraw, err := singleField(nodeCtx, "match_phrase", m)
	if err != nil {
		return nil, err
	}
	fctx := nodeCtx.key(field)

	q := &MatchPhraseQuery{Field: field}

	// Short form: {"match_phrase": {"field": "text"}}.
	if firstByte(fraw) != '{' {
		if q.Text, err = decodeText(fctx, fraw); err != nil {
			return nil, err
		}
		return q, nil
	}

	obj, err := decodeObject(fctx, fraw)
	if err != nil {
		return nil, err
	}
	seenText := false
	for k, vraw := range obj {
		switch k {
		case "query", "text":
			if seenText {
				return nil, illegalAt(fctx, "[match_phrase] accepts only one of [query] or [text]")
			}
			seenText = true
			if q.Text, err = decodeText(fctx, vraw); err != nil {
				return nil, err
			}
		case "slop":
			if q.Slop, err = decodeNonNegativeInt(fctx.key("slop"), vraw); err != nil {
				return nil, err
			}
		case "analyzer":
			if q.Analyzer, err = decodeNonEmptyString(fctx.key("analyzer"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(fctx, "[match_phrase] query does not support [%s]", k)
		}
	}
	if !seenText {
		return nil, illegalAt(fctx, "[match_phrase] requires a [query] (or [text])")
	}
	return q, nil
}

func parseBoolQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("bool")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}

	q := &BoolQuery{Boost: DefaultBoost}
	for k, vraw := range m {
		switch k {
		case "must", "should", "filter", "must_not":
			clauses, err := parseClauseArray(nodeCtx.key(k), vraw)
			if err != nil {
				return nil, err
			}
			switch k {
			case "must":
				q.Must = clauses
			case "should":
				q.Should = clauses
			case "filter":
				q.Filter = clauses
			case "must_not":
				q.MustNot = clauses
			}
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil {
				return nil, err
			}
		case "minimum_should_match":
			if q.MinShouldMatch, err = decodeMinimumShouldMatch(nodeCtx.key("minimum_should_match"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(nodeCtx, "[bool] query does not support [%s]", k)
		}
	}
	return q, nil
}

// parseClauseArray parses an array of query clauses (bool.must, hybrid
// queries/filter, ...) supporting arbitrary nesting.
func parseClauseArray(c *parseCtx, raw jsonx.RawMessage) ([]Query, error) {
	if firstByte(raw) != '[' {
		return nil, illegalAt(c, "expected an array of query clauses, got %s", jsonKindName(raw))
	}
	var arr []jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &arr); err != nil {
		return nil, illegalAt(c, "expected an array of query clauses")
	}
	if len(arr) == 0 {
		return nil, nil
	}
	clauses := make([]Query, 0, len(arr))
	for i, el := range arr {
		q, err := parseQueryRaw(c.idx(i), el)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, q)
	}
	return clauses, nil
}

// parseKnnQuery parses the knn query form ({"knn": {...}} inside a query
// object, ES 8.14+).
func parseKnnQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	return parseKnnObject(c.key("knn"), raw)
}

// parseTopKnn parses the ES 8.12 top-level "knn" search clause, which is
// either a single knn object or an array of knn objects.
func parseTopKnn(c *parseCtx, raw jsonx.RawMessage) ([]Query, error) {
	if firstByte(raw) == '[' {
		var arr []jsonx.RawMessage
		if err := jsonx.Unmarshal(raw, &arr); err != nil {
			return nil, illegalAt(c, "knn clause must be an object or an array of objects")
		}
		knns := make([]Query, 0, len(arr))
		for i, el := range arr {
			q, err := parseKnnObject(c.idx(i), el)
			if err != nil {
				return nil, err
			}
			knns = append(knns, q)
		}
		return knns, nil
	}
	q, err := parseKnnObject(c, raw)
	if err != nil {
		return nil, err
	}
	return []Query{q}, nil
}

// parseKnnObject parses a knn clause body. The object layout is shared by
// the top-level clause and the query form: {"field", "vector",
// "k", "num_candidates", "filter", "similarity", "boost"}. "vector" is the
// UniSearch spelling; "query_vector" is accepted as the ES 8.12 alias.
// ES 8.12 does not allow boost on the top-level clause; we accept it
// uniformly and leave stricter rejection to the search layer.
func parseKnnObject(nodeCtx *parseCtx, raw jsonx.RawMessage) (*KnnQuery, error) {
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}

	// ES 8.x format: {"field_name": {"vector": [...], "k": N}}
	// ES 7.x format: {"field": "field_name", "vector": [...], "k": N}
	// Detect ES 8.x format: a key that's not a known KNN param and whose
	// value is an object containing "vector" or "query_vector".
	knownKnnKeys := map[string]bool{
		"field": true, "vector": true, "query_vector": true,
		"k": true, "num_candidates": true, "filter": true,
		"similarity": true, "boost": true,
	}
	for k, vraw := range m {
		if !knownKnnKeys[k] && firstByte(vraw) == '{' {
			// ES 8.x format: k is the field name, vraw is the inner object.
			inner, err := decodeObject(nodeCtx.key(k), vraw)
			if err != nil {
				return nil, err
			}
			// Parse the inner object as the KNN parameters.
			merged := make(map[string]jsonx.RawMessage, len(inner)+1)
			merged["field"] = jsonx.RawMessage(strconv.Quote(k)) // set field name
			for ik, iv := range inner {
				merged[ik] = iv
			}
			return parseKnnParams(nodeCtx, merged)
		}
	}

	// ES 7.x format: parse directly.
	return parseKnnParams(nodeCtx, m)
}

// parseKnnParams parses KNN query parameters from a flat map.
func parseKnnParams(nodeCtx *parseCtx, m map[string]jsonx.RawMessage) (*KnnQuery, error) {
	q := &KnnQuery{Boost: DefaultBoost}
	seenVector := false
	var err error
	for k, vraw := range m {
		switch k {
		case "field":
			fv, err := decodeNonEmptyString(nodeCtx.key("field"), vraw)
			if err != nil {
				return nil, err
			}
			// Handle quoted field name from ES 8.x format.
			q.Field = strings.Trim(fv, `"`)
		case "vector", "query_vector":
			if seenVector {
				return nil, illegalAt(nodeCtx, "[knn] accepts only one of [vector] or [query_vector]")
			}
			seenVector = true
			if q.Vector, err = decodeVector(nodeCtx.key(k), vraw); err != nil {
				return nil, err
			}
		case "k":
			if q.K, err = decodePositiveInt(nodeCtx.key("k"), vraw); err != nil {
				return nil, err
			}
		case "num_candidates":
			if q.NumCandidates, err = decodePositiveInt(nodeCtx.key("num_candidates"), vraw); err != nil {
				return nil, err
			}
		case "filter":
			fq, err := parseQueryRaw(nodeCtx.key("filter"), vraw)
			if err != nil {
				return nil, err
			}
			q.Filter = []Query{fq}
		case "similarity":
			if q.Similarity, err = decodeFloat(nodeCtx.key("similarity"), vraw); err != nil {
				return nil, err
			}
			if q.Similarity < 0 {
				return nil, illegalAt(nodeCtx.key("similarity"), "[similarity] must not be negative, got %v", q.Similarity)
			}
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(nodeCtx, "[knn] query does not support [%s]", k)
		}
	}
	if q.Field == "" {
		return nil, illegalAt(nodeCtx, "[knn] requires a [field]")
	}
	if !seenVector {
		return nil, illegalAt(nodeCtx, "[knn] requires a [vector]")
	}
	if q.K == 0 {
		return nil, illegalAt(nodeCtx, "[knn] requires a positive [k]")
	}
	return q, nil
}

// decodeVector decodes the query vector: a non-empty array of numbers.
func decodeVector(c *parseCtx, raw jsonx.RawMessage) ([]float32, error) {
	if firstByte(raw) != '[' {
		return nil, illegalAt(c, "vector must be a non-empty array of numbers, got %s", jsonKindName(raw))
	}
	var arr []jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &arr); err != nil {
		return nil, illegalAt(c, "vector must be a non-empty array of numbers")
	}
	if len(arr) == 0 {
		return nil, illegalAt(c, "vector must be a non-empty array of numbers")
	}
	vec := make([]float32, 0, len(arr))
	for i, el := range arr {
		if firstByte(el) == 'n' {
			return nil, illegalAt(c.idx(i), "vector element must be a number, got null")
		}
		var f float64
		if err := jsonx.Unmarshal(el, &f); err != nil {
			return nil, illegalAt(c.idx(i), "vector element must be a number, got %s", jsonKindName(el))
		}
		vec = append(vec, float32(f))
	}
	return vec, nil
}

// parseSparseVectorQuery parses {"sparse_vector": {...}}. The clause body
// follows the same dual layout as knn:
//
//   ES 8.x style: {"<field>": {"embedding": {"indices": [...], "values": [...]}, "boost": 1.0}}
//   Flat style:   {"field": "<field>", "embedding": {"indices": [...], "values": [...]}, "boost": 1.0}
//
// The embedding is a {dim: weight} map encoded as parallel "indices" and
// "values" arrays (design 02 §5, same format as the document _source).
func parseSparseVectorQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("sparse_vector")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}

	// Detect ES 8.x style: a key that is not a known param and whose value
	// is an object containing "embedding".
	knownKeys := map[string]bool{
		"field": true, "embedding": true, "boost": true,
	}
	for k, vraw := range m {
		if !knownKeys[k] && firstByte(vraw) == '{' {
			inner, err := decodeObject(nodeCtx.key(k), vraw)
			if err != nil {
				return nil, err
			}
			merged := make(map[string]jsonx.RawMessage, len(inner)+1)
			merged["field"] = jsonx.RawMessage(strconv.Quote(k))
			for ik, iv := range inner {
				merged[ik] = iv
			}
			return parseSparseVectorParams(nodeCtx, merged)
		}
	}

	// Flat style: parse directly.
	return parseSparseVectorParams(nodeCtx, m)
}

// parseSparseVectorParams parses sparse vector query parameters from a flat
// map.
func parseSparseVectorParams(nodeCtx *parseCtx, m map[string]jsonx.RawMessage) (*SparseVectorQuery, error) {
	q := &SparseVectorQuery{Boost: float32(DefaultBoost)}
	seenEmbedding := false
	var err error
	for k, vraw := range m {
		switch k {
		case "field":
			fv, err := decodeNonEmptyString(nodeCtx.key("field"), vraw)
			if err != nil {
				return nil, err
			}
			q.Field = strings.Trim(fv, `"`)
		case "embedding":
			seenEmbedding = true
			if q.Embedding, err = decodeSparseEmbedding(nodeCtx.key("embedding"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			b, err := decodeFloat(nodeCtx.key("boost"), vraw)
			if err != nil {
				return nil, err
			}
			q.Boost = float32(b)
		default:
			return nil, illegalAt(nodeCtx, "[sparse_vector] query does not support [%s]", k)
		}
	}
	if q.Field == "" {
		return nil, illegalAt(nodeCtx, "[sparse_vector] requires a [field]")
	}
	if !seenEmbedding {
		return nil, illegalAt(nodeCtx, "[sparse_vector] requires an [embedding]")
	}
	if len(q.Embedding) == 0 {
		return nil, illegalAt(nodeCtx, "[sparse_vector] [embedding] must not be empty")
	}
	return q, nil
}

// decodeSparseEmbedding decodes the query sparse vector: a JSON object with
// parallel "indices" (uint32) and "values" (float) arrays of equal length.
func decodeSparseEmbedding(c *parseCtx, raw jsonx.RawMessage) (map[uint32]float32, error) {
	m, err := decodeObject(c, raw)
	if err != nil {
		return nil, err
	}
	idxRaw, ok1 := m["indices"]
	valRaw, ok2 := m["values"]
	if !ok1 || !ok2 {
		return nil, illegalAt(c, "[embedding] must contain [indices] and [values] arrays")
	}
	var idxs []any
	if err := jsonx.Unmarshal(idxRaw, &idxs); err != nil {
		return nil, illegalAt(c.key("indices"), "expected a JSON array of integers")
	}
	var vals []any
	if err := jsonx.Unmarshal(valRaw, &vals); err != nil {
		return nil, illegalAt(c.key("values"), "expected a JSON array of numbers")
	}
	if len(idxs) != len(vals) {
		return nil, illegalAt(c, "[indices] and [values] must have equal length, got %d and %d", len(idxs), len(vals))
	}
	if len(idxs) == 0 {
		return nil, illegalAt(c, "[embedding] must not be empty")
	}
	out := make(map[uint32]float32, len(idxs))
	for i, el := range idxs {
		dim, err := dimFromJSON(el)
		if err != nil {
			return nil, illegalAt(c.idx(i), "index element %d: %v", i, err)
		}
		w, err := weightFromJSON(vals[i])
		if err != nil {
			return nil, illegalAt(c.idx(i), "value element %d: %v", i, err)
		}
		out[dim] = w
	}
	return out, nil
}

// dimFromJSON converts a JSON value to a uint32 dimension index.
func dimFromJSON(v any) (uint32, error) {
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

// weightFromJSON converts a JSON value to a float32 weight.
func weightFromJSON(v any) (float32, error) {
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

// parseHybridQuery parses the UniSearch-specific hybrid clause: several
// retrieval paths plus a shared filter, fused by a RankSpec (design 03 §5).
func parseHybridQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("hybrid")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}

	q := &HybridQuery{Rank: defaultRankSpec()}
	for k, vraw := range m {
		switch k {
		case "queries":
			if q.Queries, err = parseClauseArray(nodeCtx.key("queries"), vraw); err != nil {
				return nil, err
			}
		case "filter":
			if q.Filter, err = parseClauseArray(nodeCtx.key("filter"), vraw); err != nil {
				return nil, err
			}
		case "rank":
			if q.Rank, err = parseRankSpec(nodeCtx.key("rank"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(nodeCtx, "[hybrid] query does not support [%s]", k)
		}
	}
	if len(q.Queries) == 0 {
		return nil, illegalAt(nodeCtx, "[hybrid] requires a non-empty [queries] array")
	}
	if len(q.Rank.Weights) > 0 && len(q.Rank.Weights) != len(q.Queries) {
		return nil, illegalAt(nodeCtx,
			"[hybrid] rank weights length %d must match the number of queries %d",
			len(q.Rank.Weights), len(q.Queries))
	}
	return q, nil
}

func defaultRankSpec() RankSpec {
	return RankSpec{
		Method:     RankMethodRRF,
		RankConst:  DefaultRankConstant,
		WindowSize: DefaultWindowSize,
	}
}

// parseRankSpec parses the rank object: {"method": "rrf"|"weighted",
// "rank_constant": 60, "weights": [...], "window_size": 500}. Omitted
// members fall back to the documented defaults.
func parseRankSpec(c *parseCtx, raw jsonx.RawMessage) (RankSpec, error) {
	m, err := decodeObject(c, raw)
	if err != nil {
		return RankSpec{}, err
	}

	spec := defaultRankSpec()
	for k, vraw := range m {
		switch k {
		case "method":
			s, err := decodeString(c.key("method"), vraw)
			if err != nil {
				return RankSpec{}, err
			}
			switch RankMethod(s) {
			case RankMethodRRF:
				spec.Method = RankMethodRRF
			case RankMethodWeighted:
				spec.Method = RankMethodWeighted
			case RankMethodDBSF:
				spec.Method = RankMethodDBSF
			case "relative_score", "rerank":
				return RankSpec{}, errors.NewUnsupported("hybrid.rank."+s, "v1.2 (M1)",
					"use method rrf or weighted")
			default:
				return RankSpec{}, illegalAt(c.key("method"), "[rank] method must be [rrf], [weighted], or [dbsf], got [%s]", s)
			}
		case "rank_constant":
			if spec.RankConst, err = decodePositiveInt(c.key("rank_constant"), vraw); err != nil {
				return RankSpec{}, err
			}
		case "weights":
			if spec.Weights, err = decodeFloatArray(c.key("weights"), vraw); err != nil {
				return RankSpec{}, err
			}
		case "window_size":
			if spec.WindowSize, err = decodePositiveInt(c.key("window_size"), vraw); err != nil {
				return RankSpec{}, err
			}
		default:
			return RankSpec{}, illegalAt(c, "[rank] does not support [%s]", k)
		}
	}
	return spec, nil
}

func decodeFloatArray(c *parseCtx, raw jsonx.RawMessage) ([]float64, error) {
	if firstByte(raw) != '[' {
		return nil, illegalAt(c, "expected an array of numbers, got %s", jsonKindName(raw))
	}
	var arr []jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &arr); err != nil {
		return nil, illegalAt(c, "expected an array of numbers")
	}
	out := make([]float64, 0, len(arr))
	for i, el := range arr {
		if firstByte(el) == 'n' {
			return nil, illegalAt(c.idx(i), "expected a number, got null")
		}
		f, err := decodeFloat(c.idx(i), el)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Primitive decoders. All of them report illegal_argument errors carrying
// the JSON path of the offending value.
// ---------------------------------------------------------------------------

func decodeObject(c *parseCtx, raw jsonx.RawMessage) (map[string]jsonx.RawMessage, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, illegalAt(c, "expected a JSON object, got %s", jsonKindName(raw))
	}
	return m, nil
}

func decodeString(c *parseCtx, raw jsonx.RawMessage) (string, error) {
	var s string
	if err := jsonx.Unmarshal(raw, &s); err != nil {
		return "", illegalAt(c, "expected a string, got %s", jsonKindName(raw))
	}
	return s, nil
}

func decodeNonEmptyString(c *parseCtx, raw jsonx.RawMessage) (string, error) {
	s, err := decodeString(c, raw)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", illegalAt(c, "expected a non-empty string")
	}
	return s, nil
}

// decodeScalar decodes a JSON string/number/bool into a Go value. Objects,
// arrays and null are rejected.
func decodeScalar(c *parseCtx, raw jsonx.RawMessage) (any, error) {
	switch firstByte(raw) {
	case '"':
		var s string
		if err := jsonx.Unmarshal(raw, &s); err != nil {
			return nil, illegalAt(c, "expected a scalar value")
		}
		return s, nil
	case 't', 'f':
		var b bool
		if err := jsonx.Unmarshal(raw, &b); err != nil {
			return nil, illegalAt(c, "expected a scalar value")
		}
		return b, nil
	case '{', '[', 'n', 0:
		return nil, illegalAt(c, "expected a scalar value, got %s", jsonKindName(raw))
	default:
		var f float64
		if err := jsonx.Unmarshal(raw, &f); err != nil {
			return nil, illegalAt(c, "expected a scalar value, got %s", jsonKindName(raw))
		}
		return f, nil
	}
}

// decodeText decodes the query text of match-style queries. ES coerces
// numbers to text, so both strings and numbers are accepted; booleans,
// null, objects and arrays are not.
func decodeText(c *parseCtx, raw jsonx.RawMessage) (string, error) {
	switch firstByte(raw) {
	case '"':
		return decodeString(c, raw)
	case 't', 'f', '{', '[', 'n', 0:
		return "", illegalAt(c, "query text must be a string or a number, got %s", jsonKindName(raw))
	default:
		var f float64
		if err := jsonx.Unmarshal(raw, &f); err != nil {
			return "", illegalAt(c, "query text must be a string or a number, got %s", jsonKindName(raw))
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
}

func decodeFloat(c *parseCtx, raw jsonx.RawMessage) (float64, error) {
	var f float64
	if err := jsonx.Unmarshal(raw, &f); err != nil {
		return 0, illegalAt(c, "expected a number, got %s", jsonKindName(raw))
	}
	return f, nil
}

func decodeBool(c *parseCtx, raw jsonx.RawMessage) (bool, error) {
	var b bool
	if err := jsonx.Unmarshal(raw, &b); err != nil {
		return false, illegalAt(c, "expected a boolean, got %s", jsonKindName(raw))
	}
	return b, nil
}

func decodeInt(c *parseCtx, raw jsonx.RawMessage) (int, error) {
	// Guard the first byte: encoding/json happily decodes a quoted number
	// literal into jsonx.Number, but ES rejects string-typed integer
	// parameters, and so must we.
	if b := firstByte(raw); b != '-' && (b < '0' || b > '9') {
		return 0, illegalAt(c, "expected an integer, got %s", jsonKindName(raw))
	}
	var n jsonx.Number
	if err := jsonx.Unmarshal(raw, &n); err != nil {
		return 0, illegalAt(c, "expected an integer, got %s", jsonKindName(raw))
	}
	i64, err := n.Int64()
	if err != nil {
		return 0, illegalAt(c, "expected an integer, got [%s]", n.String())
	}
	return int(i64), nil
}

func decodeNonNegativeInt(c *parseCtx, raw jsonx.RawMessage) (int, error) {
	n, err := decodeInt(c, raw)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, illegalAt(c, "expected a non-negative integer, got %d", n)
	}
	return n, nil
}

func decodePositiveInt(c *parseCtx, raw jsonx.RawMessage) (int, error) {
	n, err := decodeInt(c, raw)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, illegalAt(c, "expected a positive integer, got %d", n)
	}
	return n, nil
}

func decodeOperator(c *parseCtx, raw jsonx.RawMessage) (string, error) {
	s, err := decodeString(c, raw)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(s) {
	case "and":
		return "and", nil
	case "or":
		return "or", nil
	}
	return "", illegalAt(c, "[operator] must be [and] or [or], got [%s]", s)
}

// decodeFuzziness validates the fuzziness literal: AUTO, AUTO:low[,high]
// or a number within [0, 2] (the fuzziness micro-parser, design 03 §2.2).
func decodeFuzziness(c *parseCtx, raw jsonx.RawMessage) (string, error) {
	switch firstByte(raw) {
	case '"':
		s, err := decodeString(c, raw)
		if err != nil {
			return "", err
		}
		if !validFuzziness(s) {
			return "", illegalAt(c, "[fuzziness] must be AUTO or a number in [0, 2], got [%s]", s)
		}
		return strings.ToUpper(s), nil
	case 't', 'f', '{', '[', 'n', 0:
		return "", illegalAt(c, "[fuzziness] must be AUTO or a number in [0, 2], got %s", jsonKindName(raw))
	default:
		var f float64
		if err := jsonx.Unmarshal(raw, &f); err != nil {
			return "", illegalAt(c, "[fuzziness] must be AUTO or a number in [0, 2], got %s", jsonKindName(raw))
		}
		if f < 0 || f > 2 {
			return "", illegalAt(c, "[fuzziness] must be AUTO or a number in [0, 2], got [%v]", f)
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
}

func validFuzziness(s string) bool {
	u := strings.ToUpper(s)
	if u == "AUTO" {
		return true
	}
	if rest, ok := strings.CutPrefix(u, "AUTO:"); ok {
		parts := strings.Split(rest, ",")
		bounds := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return false
			}
			bounds = append(bounds, n)
		}
		if len(bounds) > 2 {
			return false
		}
		if len(bounds) == 2 && bounds[0] > bounds[1] {
			return false
		}
		return true
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f >= 0 && f <= 2
}

// msmToken matches one minimum_should_match token: an integer, a
// percentage, or a "N<V" combination bucket (the minimum_should_match
// micro-parser, design 03 §2.2).
var msmToken = regexp.MustCompile(`^(\d+<[+-]?\d+%?|[+-]?\d+%?)$`)

func decodeMinimumShouldMatch(c *parseCtx, raw jsonx.RawMessage) (string, error) {
	var s string
	switch firstByte(raw) {
	case '"':
		var err error
		if s, err = decodeString(c, raw); err != nil {
			return "", err
		}
	default:
		var n jsonx.Number
		if err := jsonx.Unmarshal(raw, &n); err != nil {
			return "", illegalAt(c, "[minimum_should_match] must be a number or a string, got %s", jsonKindName(raw))
		}
		s = n.String()
	}
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", illegalAt(c, "[minimum_should_match] must not be empty")
	}
	for _, f := range fields {
		if !msmToken.MatchString(f) {
			return "", illegalAt(c, "[minimum_should_match] is malformed, got [%s]", s)
		}
	}
	return s, nil
}

func firstByte(raw jsonx.RawMessage) byte {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return 0
	}
	return t[0]
}

func jsonKindName(raw jsonx.RawMessage) string {
	switch firstByte(raw) {
	case '{':
		return "object"
	case '[':
		return "array"
	case 'n':
		return "null"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	default:
		return "number"
	}
}

// ---------------------------------------------------------------------------
// P0 DSL query parsers (es-feature-gap-fill)
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2
// ---------------------------------------------------------------------------

// parsePrefixQuery parses {"prefix": {"field": "pre"}} or
// {"prefix": {"field": {"value": "pre", "boost": 2.0}}}.
func parsePrefixQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("prefix")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	field, fraw, err := singleField(nodeCtx, "prefix", m)
	if err != nil {
		return nil, err
	}
	fctx := nodeCtx.key(field)
	q := &PrefixQuery{Field: field, Boost: DefaultBoost}

	// Short form: {"prefix": {"field": "value"}}.
	if firstByte(fraw) != '{' {
		if firstByte(fraw) == 'n' {
			return nil, illegalAt(fctx, "[prefix] value must not be null")
		}
		var s string
		if s, err = decodeString(fctx, fraw); err != nil {
			return nil, err
		}
		q.Value = s
		return q, nil
	}

	// Long form: {"prefix": {"field": {"value": "pre", "boost": 2.0}}}.
	obj, err := decodeObject(fctx, fraw)
	if err != nil {
		return nil, err
	}
	for k, vraw := range obj {
		switch k {
		case "value":
			if q.Value, err = decodeString(fctx.key("value"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			if q.Boost, err = decodeFloat(fctx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(fctx.key(k), "[prefix] unknown parameter [%s]", k)
		}
	}
	if q.Value == "" {
		return nil, illegalAt(fctx, "[prefix] requires [value]")
	}
	return q, nil
}

// parseRegexpQuery parses {"regexp": {"field": "ab.*"}} or
// {"regexp": {"field": {"value": "ab.*", "flags": "ALL", "boost": 2.0}}}.
func parseRegexpQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("regexp")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	field, fraw, err := singleField(nodeCtx, "regexp", m)
	if err != nil {
		return nil, err
	}
	fctx := nodeCtx.key(field)
	q := &RegexpQuery{Field: field, Boost: DefaultBoost}

	// Short form: {"regexp": {"field": "pattern"}}.
	if firstByte(fraw) != '{' {
		if firstByte(fraw) == 'n' {
			return nil, illegalAt(fctx, "[regexp] value must not be null")
		}
		var s string
		if s, err = decodeString(fctx, fraw); err != nil {
			return nil, err
		}
		q.Pattern = s
		return q, nil
	}

	// Long form: {"regexp": {"field": {"value": "ab.*", "flags": "ALL"}}}.
	obj, err := decodeObject(fctx, fraw)
	if err != nil {
		return nil, err
	}
	for k, vraw := range obj {
		switch k {
		case "value":
			if q.Pattern, err = decodeString(fctx.key("value"), vraw); err != nil {
				return nil, err
			}
		case "flags":
			if q.Flags, err = decodeString(fctx.key("flags"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			if q.Boost, err = decodeFloat(fctx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(fctx.key(k), "[regexp] unknown parameter [%s]", k)
		}
	}
	if q.Pattern == "" {
		return nil, illegalAt(fctx, "[regexp] requires [value]")
	}
	return q, nil
}

// parseConstantScoreQuery parses {"constant_score": {"filter": {...}, "boost": 2.0}}.
func parseConstantScoreQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("constant_score")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &ConstantScoreQuery{Boost: DefaultBoost}
	for k, vraw := range m {
		switch k {
		case "filter":
			inner, err := parseQueryRaw(nodeCtx.key("filter"), vraw)
			if err != nil {
				return nil, err
			}
			q.Filter = inner
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(nodeCtx.key(k), "[constant_score] unknown parameter [%s]", k)
		}
	}
	if q.Filter == nil {
		return nil, illegalAt(nodeCtx, "[constant_score] requires [filter]")
	}
	return q, nil
}

// parseBoostingQuery parses {"boosting": {"positive": {...}, "negative": {...}, "negative_boost": 0.5}}.
func parseBoostingQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("boosting")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &BoostingQuery{NegativeBoost: 1.0}
	for k, vraw := range m {
		switch k {
		case "positive":
			inner, err := parseQueryRaw(nodeCtx.key("positive"), vraw)
			if err != nil {
				return nil, err
			}
			q.Positive = inner
		case "negative":
			inner, err := parseQueryRaw(nodeCtx.key("negative"), vraw)
			if err != nil {
				return nil, err
			}
			q.Negative = inner
		case "negative_boost":
			if q.NegativeBoost, err = decodeFloat(nodeCtx.key("negative_boost"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(nodeCtx.key(k), "[boosting] unknown parameter [%s]", k)
		}
	}
	if q.Positive == nil {
		return nil, illegalAt(nodeCtx, "[boosting] requires [positive]")
	}
	return q, nil
}

// parseDisMaxQuery parses {"dis_max": {"queries": [{...}, ...], "tie_breaker": 0.3, "boost": 1.5}}.
func parseDisMaxQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("dis_max")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &DisMaxQuery{Boost: DefaultBoost}
	for k, vraw := range m {
		switch k {
		case "queries":
			var arr []jsonx.RawMessage
			if err := jsonx.Unmarshal(vraw, &arr); err != nil {
				return nil, illegalAt(nodeCtx.key("queries"), "[dis_max] queries must be an array")
			}
			if len(arr) == 0 {
				return nil, illegalAt(nodeCtx.key("queries"), "[dis_max] queries must not be empty")
			}
			for i, item := range arr {
				inner, err := parseQueryRaw(nodeCtx.key("queries").idx(i), item)
				if err != nil {
					return nil, err
				}
				q.Queries = append(q.Queries, inner)
			}
		case "tie_breaker":
			if q.TieBreaker, err = decodeFloat(nodeCtx.key("tie_breaker"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(nodeCtx.key(k), "[dis_max] unknown parameter [%s]", k)
		}
	}
	if len(q.Queries) == 0 {
		return nil, illegalAt(nodeCtx, "[dis_max] requires [queries]")
	}
	return q, nil
}

// ---------------------------------------------------------------------------
// P1 DSL query parsers (es-feature-gap-fill)
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.6-2.9
// ---------------------------------------------------------------------------

// parseFunctionScoreQuery parses {"function_score": {"query": {...}, "functions": [...]}}.
func parseFunctionScoreQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("function_score")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &FunctionScoreQuery{Boost: DefaultBoost, MaxBoost: 3.4028235e38, ScoreMode: "multiply"}
	for k, vraw := range m {
		switch k {
		case "query":
			inner, err := parseQueryRaw(nodeCtx.key("query"), vraw)
			if err != nil {
				return nil, err
			}
			q.Query = inner
		case "functions":
			var arr []jsonx.RawMessage
			if err := jsonx.Unmarshal(vraw, &arr); err != nil {
				return nil, illegalAt(nodeCtx.key("functions"), "must be an array")
			}
			for i, item := range arr {
				fn, err := parseScoreFunction(nodeCtx.key("functions").idx(i), item)
				if err != nil {
					return nil, err
				}
				q.Functions = append(q.Functions, fn)
			}
		case "max_boost":
			if q.MaxBoost, err = decodeFloat(nodeCtx.key("max_boost"), vraw); err != nil {
				return nil, err
			}
		case "score_mode":
			if q.ScoreMode, err = decodeString(nodeCtx.key("score_mode"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil {
				return nil, err
			}
		case "min_score":
			if q.MinScore, err = decodeFloat(nodeCtx.key("min_score"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(nodeCtx.key(k), "[function_score] unknown parameter [%s]", k)
		}
	}
	if q.Query == nil && len(q.Functions) == 0 {
		return nil, illegalAt(nodeCtx, "[function_score] requires [query] or [functions]")
	}
	return q, nil
}

func parseScoreFunction(c *parseCtx, raw jsonx.RawMessage) (ScoreFunction, error) {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return ScoreFunction{}, illegalAt(c, "score function must be a JSON object")
	}
	fn := ScoreFunction{Weight: 1.0}
	for k, vraw := range m {
		switch k {
		case "weight":
			w, e := decodeFloat(c.key("weight"), vraw)
			if e != nil {
				return ScoreFunction{}, e
			}
			fn.Weight = w
		case "random_score":
			fn.Type = "random_score"
			var rs struct {
				Seed  string `json:"seed"`
				Field string `json:"field"`
			}
			if jsonx.Unmarshal(vraw, &rs) == nil {
				fn.Seed = rs.Seed
				fn.Field = rs.Field
			}
		case "field_value_factor":
			fn.Type = "field_value_factor"
			var fvf struct {
				Field    string  `json:"field"`
				Factor   float64 `json:"factor"`
				Modifier string  `json:"modifier"`
				Missing  float64 `json:"missing"`
			}
			if jsonx.Unmarshal(vraw, &fvf) == nil {
				fn.Field = fvf.Field
				fn.Factor = fvf.Factor
				fn.Modifier = fvf.Modifier
				fn.Missing = fvf.Missing
			}
		case "script_score":
			fn.Type = "script_score"
			var ss struct {
				Source string         `json:"source"`
				Params map[string]any `json:"params"`
			}
			if jsonx.Unmarshal(vraw, &ss) == nil {
				fn.Script = ss.Source
				fn.Params = ss.Params
			}
		case "exp", "gauss", "linear":
			fn.Type = k
			var decay struct {
				Origin any     `json:"origin"`
				Scale  any     `json:"scale"`
				Offset any     `json:"offset"`
				Decay  float64 `json:"decay"`
			}
			if jsonx.Unmarshal(vraw, &decay) == nil {
				fn.Origin = decay.Origin
				fn.Scale = decay.Scale
				fn.Offset = decay.Offset
				fn.Decay = decay.Decay
			}
		default:
			return ScoreFunction{}, illegalAt(c.key(k), "unknown score function [%s]", k)
		}
	}
	return fn, nil
}

// parseScriptScoreQuery parses {"script_score": {"query": {...}, "script": {"source": "..."}}}.
func parseScriptScoreQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("script_score")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &ScriptScoreQuery{}
	for k, vraw := range m {
		switch k {
		case "query":
			inner, err := parseQueryRaw(nodeCtx.key("query"), vraw)
			if err != nil {
				return nil, err
			}
			q.Query = inner
		case "script":
			var s struct {
				Source string         `json:"source"`
				Params map[string]any `json:"params"`
			}
			if jsonx.Unmarshal(vraw, &s) == nil {
				q.Script = s.Source
				q.Params = s.Params
			}
		case "min_score":
			if q.MinScore, err = decodeFloat(nodeCtx.key("min_score"), vraw); err != nil {
				return nil, err
			}
		default:
			return nil, illegalAt(nodeCtx.key(k), "[script_score] unknown parameter [%s]", k)
		}
	}
	if q.Query == nil {
		return nil, illegalAt(nodeCtx, "[script_score] requires [query]")
	}
	if q.Script == "" {
		return nil, illegalAt(nodeCtx, "[script_score] requires [script]")
	}
	return q, nil
}

// parseGeoDistanceQuery parses {"geo_distance": {"distance": "200km", "field": {"lat": 40, "lon": -70}}}.
func parseGeoDistanceQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("geo_distance")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &GeoDistanceQuery{Boost: DefaultBoost}
	var distanceStr string
	for k, vraw := range m {
		switch k {
		case "distance":
			if distanceStr, err = decodeString(nodeCtx.key("distance"), vraw); err != nil {
				return nil, err
			}
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			// The remaining key is the field name with lat/lon value.
			q.Field = k
			var coords struct {
				Lat float64 `json:"lat"`
				Lon float64 `json:"lon"`
			}
			if jsonx.Unmarshal(vraw, &coords) == nil && (coords.Lat != 0 || coords.Lon != 0) {
				q.Lat = coords.Lat
				q.Lon = coords.Lon
			} else if s, err := decodeString(nodeCtx.key(k), vraw); err == nil {
				// "lat,lon" string format.
				fmt.Sscanf(s, "%f,%f", &q.Lat, &q.Lon)
			}
		}
	}
	if q.Field == "" {
		return nil, illegalAt(nodeCtx, "[geo_distance] requires a field name")
	}
	if distanceStr == "" {
		return nil, illegalAt(nodeCtx, "[geo_distance] requires [distance]")
	}
	q.Distance = distanceStr
	q.DistanceM = parseDistanceToMeters(distanceStr)
	if q.DistanceM <= 0 {
		return nil, illegalAt(nodeCtx, "[geo_distance] invalid distance format: %s", distanceStr)
	}
	return q, nil
}

// parseGeoBoundingBoxQuery parses {"geo_bounding_box": {"field": {"top_left": {"lat": 40, "lon": -70}, "bottom_right": {"lat": 30, "lon": -80}}}}.
func parseGeoBoundingBoxQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("geo_bounding_box")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &GeoBoundingBoxQuery{Boost: DefaultBoost}
	for k, vraw := range m {
		switch k {
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil {
				return nil, err
			}
		default:
			q.Field = k
			var box struct {
				TopLeft    map[string]float64 `json:"top_left"`
				BottomRight map[string]float64 `json:"bottom_right"`
				Top        float64             `json:"top"`
				Left       float64             `json:"left"`
				Bottom     float64             `json:"bottom"`
				Right      float64             `json:"right"`
			}
			if jsonx.Unmarshal(vraw, &box) == nil {
				if box.TopLeft != nil {
					q.Top = box.TopLeft["lat"]
					q.Left = box.TopLeft["lon"]
				} else {
					q.Top = box.Top
					q.Left = box.Left
				}
				if box.BottomRight != nil {
					q.Bottom = box.BottomRight["lat"]
					q.Right = box.BottomRight["lon"]
				} else {
					q.Bottom = box.Bottom
					q.Right = box.Right
				}
			}
		}
	}
	if q.Field == "" {
		return nil, illegalAt(nodeCtx, "[geo_bounding_box] requires a field name")
	}
	return q, nil
}

// parseDistanceToMeters converts a distance string like "200km" to meters.
func parseDistanceToMeters(s string) float64 {
	s = strings.TrimSpace(strings.ToLower(s))
	// Try common unit suffixes.
	for _, unit := range []struct {
		suffix string
		meters float64
	}{
		{"km", 1000}, {"kilometers", 1000},
		{"m", 1}, {"meters", 1},
		{"mi", 1609.344}, {"miles", 1609.344},
		{"yd", 0.9144}, {"yards", 0.9144},
		{"ft", 0.3048}, {"feet", 0.3048},
		{"nmi", 1852}, {"nauticalmiles", 1852},
		{"cm", 0.01}, {"centimeters", 0.01},
		{"mm", 0.001}, {"millimeters", 0.001},
	} {
		if strings.HasSuffix(s, unit.suffix) {
			numStr := strings.TrimSuffix(s, unit.suffix)
			var num float64
			if _, err := fmt.Sscanf(numStr, "%f", &num); err == nil {
				return num * unit.meters
			}
		}
	}
	// No suffix: assume meters.
	var num float64
	if _, err := fmt.Sscanf(s, "%f", &num); err == nil {
		return num
	}
	return -1
}

// ---------------------------------------------------------------------------
// P2 DSL query parsers (es-feature-gap-fill)
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.10-2.13
// ---------------------------------------------------------------------------

func parseMoreLikeThisQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("more_like_this")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &MoreLikeThisQuery{Boost: DefaultBoost, MinTermFreq: 2, MaxQueryTerms: 25, MinDocFreq: 5}
	for k, vraw := range m {
		switch k {
		case "fields":
			var fs []string
			if jsonx.Unmarshal(vraw, &fs) == nil { q.Fields = fs }
		case "like":
			var like []any
			if jsonx.Unmarshal(vraw, &like) == nil {
				for _, l := range like {
					if s, ok := l.(string); ok { q.LikeTexts = append(q.LikeTexts, s) }
				}
			}
		case "unlike":
			var unlike []any
			if jsonx.Unmarshal(vraw, &unlike) == nil {
				for _, u := range unlike {
					if s, ok := u.(string); ok { q.UnlikeTexts = append(q.UnlikeTexts, s) }
				}
			}
		case "min_term_freq":
			if q.MinTermFreq, err = decodeInt(nodeCtx.key("min_term_freq"), vraw); err != nil { return nil, err }
		case "max_query_terms":
			if q.MaxQueryTerms, err = decodeInt(nodeCtx.key("max_query_terms"), vraw); err != nil { return nil, err }
		case "min_doc_freq":
			if q.MinDocFreq, err = decodeInt(nodeCtx.key("min_doc_freq"), vraw); err != nil { return nil, err }
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[more_like_this] unknown parameter [%s]", k)
		}
	}
	if len(q.LikeTexts) == 0 {
		return nil, illegalAt(nodeCtx, "[more_like_this] requires [like]")
	}
	return q, nil
}

func parseSimpleQueryStringQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("simple_query_string")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &SimpleQueryStringQuery{Boost: DefaultBoost, DefaultOperator: "or"}
	for k, vraw := range m {
		switch k {
		case "query":
			if q.Query, err = decodeString(nodeCtx.key("query"), vraw); err != nil { return nil, err }
		case "fields":
			var fs []string
			if jsonx.Unmarshal(vraw, &fs) == nil { q.Fields = fs }
		case "default_operator":
			if q.DefaultOperator, err = decodeString(nodeCtx.key("default_operator"), vraw); err != nil { return nil, err }
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[simple_query_string] unknown parameter [%s]", k)
		}
	}
	if q.Query == "" {
		return nil, illegalAt(nodeCtx, "[simple_query_string] requires [query]")
	}
	return q, nil
}

func parseCombinedFieldsQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("combined_fields")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	q := &CombinedFieldsQuery{Boost: DefaultBoost, Operator: "or"}
	for k, vraw := range m {
		switch k {
		case "query":
			if q.Query, err = decodeString(nodeCtx.key("query"), vraw); err != nil { return nil, err }
		case "fields":
			var fs []string
			if jsonx.Unmarshal(vraw, &fs) == nil { q.Fields = fs }
		case "operator":
			if q.Operator, err = decodeString(nodeCtx.key("operator"), vraw); err != nil { return nil, err }
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[combined_fields] unknown parameter [%s]", k)
		}
	}
	if q.Query == "" || len(q.Fields) == 0 {
		return nil, illegalAt(nodeCtx, "[combined_fields] requires [query] and [fields]")
	}
	return q, nil
}

func parseMatchBoolPrefixQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("match_bool_prefix")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}
	field, fraw, err := singleField(nodeCtx, "match_bool_prefix", m)
	if err != nil {
		return nil, err
	}
	fctx := nodeCtx.key(field)
	q := &MatchBoolPrefixQuery{Field: field, Boost: DefaultBoost}
	if firstByte(fraw) != '{' {
		if q.Query, err = decodeString(fctx, fraw); err != nil { return nil, err }
		return q, nil
	}
	obj, err := decodeObject(fctx, fraw)
	if err != nil { return nil, err }
	for k, vraw := range obj {
		switch k {
		case "query":
			if q.Query, err = decodeString(fctx.key("query"), vraw); err != nil { return nil, err }
		case "boost":
			if q.Boost, err = decodeFloat(fctx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(fctx.key(k), "[match_bool_prefix] unknown parameter [%s]", k)
		}
	}
	if q.Query == "" {
		return nil, illegalAt(fctx, "[match_bool_prefix] requires [query]")
	}
	return q, nil
}

// ---------------------------------------------------------------------------
// P3 DSL query parsers (es-feature-gap-fill)
// ---------------------------------------------------------------------------

func parseDistanceFeatureQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("distance_feature")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	q := &DistanceFeatureQuery{Boost: DefaultBoost}
	for k, vraw := range m {
		switch k {
		case "field":
			if q.Field, err = decodeString(nodeCtx.key("field"), vraw); err != nil { return nil, err }
		case "origin":
			jsonx.Unmarshal(vraw, &q.Origin)
		case "pivot":
			jsonx.Unmarshal(vraw, &q.Pivot)
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[distance_feature] unknown parameter [%s]", k)
		}
	}
	if q.Field == "" { return nil, illegalAt(nodeCtx, "[distance_feature] requires [field]") }
	return q, nil
}

func parseRankFeatureQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("rank_feature")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	q := &RankFeatureQuery{Boost: DefaultBoost, Positive: 1.0}
	for k, vraw := range m {
		switch k {
		case "field":
			if q.Field, err = decodeString(nodeCtx.key("field"), vraw); err != nil { return nil, err }
		case "positive":
			if q.Positive, err = decodeFloat(nodeCtx.key("positive"), vraw); err != nil { return nil, err }
		case "negative":
			if q.Negative, err = decodeFloat(nodeCtx.key("negative"), vraw); err != nil { return nil, err }
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[rank_feature] unknown parameter [%s]", k)
		}
	}
	if q.Field == "" { return nil, illegalAt(nodeCtx, "[rank_feature] requires [field]") }
	return q, nil
}

func parseWrapperQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("wrapper")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	var encoded string
	for k, vraw := range m {
		switch k {
		case "query":
			if encoded, err = decodeString(nodeCtx.key("query"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[wrapper] unknown parameter [%s]", k)
		}
	}
	if encoded == "" { return nil, illegalAt(nodeCtx, "[wrapper] requires [query]") }
	decoded, derr := base64.StdEncoding.DecodeString(encoded)
	if derr != nil {
		return nil, illegalAt(nodeCtx, "[wrapper] invalid base64: %v", derr)
	}
	inner, perr := parseQueryRaw(nodeCtx, decoded)
	if perr != nil { return nil, perr }
	return &WrapperQuery{Query: encoded, Inner: inner}, nil
}

func parsePinnedQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("pinned")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	q := &PinnedQuery{Boost: DefaultBoost}
	for k, vraw := range m {
		switch k {
		case "organic":
			inner, err := parseQueryRaw(nodeCtx.key("organic"), vraw)
			if err != nil { return nil, err }
			q.Organic = inner
		case "pinned":
			var arr []struct {
				ID   string `json:"_id"`
				Type string `json:"_type"`
			}
			if jsonx.Unmarshal(vraw, &arr) == nil {
				for _, p := range arr {
					q.Pinned = append(q.Pinned, PinnedDoc{ID: p.ID, Type: p.Type})
				}
			}
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[pinned] unknown parameter [%s]", k)
		}
	}
	if q.Organic == nil {
		return nil, illegalAt(nodeCtx, "[pinned] requires [organic]")
	}
	return q, nil
}

func parseTermsSetQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("terms_set")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	field, fraw, err := singleField(nodeCtx, "terms_set", m)
	if err != nil { return nil, err }
	q := &TermsSetQuery{Field: field, Boost: DefaultBoost}
	var obj map[string]jsonx.RawMessage
	if jsonx.Unmarshal(fraw, &obj) == nil {
		for k, vraw := range obj {
			switch k {
			case "terms":
				jsonx.Unmarshal(vraw, &q.Values)
			case "minimum_should_match_field":
				if q.MinimumShouldMatchField, err = decodeString(nodeCtx.key("minimum_should_match_field"), vraw); err != nil { return nil, err }
			case "boost":
				if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
			default:
				return nil, illegalAt(nodeCtx.key(k), "[terms_set] unknown parameter [%s]", k)
			}
		}
	} else {
		// Short form: {"terms_set": {"field": ["v1","v2"]}}
		jsonx.Unmarshal(fraw, &q.Values)
	}
	if len(q.Values) == 0 {
		return nil, illegalAt(nodeCtx, "[terms_set] requires [terms]")
	}
	return q, nil
}

// ---------------------------------------------------------------------------
// Final DSL query parsers — match_none/script/percolate/geo_shape/intervals/nested
// ---------------------------------------------------------------------------

func parseScriptQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("script")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	q := &ScriptQuery{Boost: DefaultBoost}
	for k, vraw := range m {
		switch k {
		case "source":
			if q.Script, err = decodeString(nodeCtx.key("source"), vraw); err != nil { return nil, err }
		case "params":
			jsonx.Unmarshal(vraw, &q.Params)
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[script] unknown parameter [%s]", k)
		}
	}
	if q.Script == "" {
		return nil, illegalAt(nodeCtx, "[script] requires [source]")
	}
	return q, nil
}

func parsePercolateQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("percolate")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	q := &PercolateQuery{Boost: DefaultBoost}
	for k, vraw := range m {
		switch k {
		case "field":
			if q.Field, err = decodeString(nodeCtx.key("field"), vraw); err != nil { return nil, err }
		case "document":
			jsonx.Unmarshal(vraw, &q.Document)
		case "document_id":
			if q.DocumentID, err = decodeString(nodeCtx.key("document_id"), vraw); err != nil { return nil, err }
		case "index":
			if q.Index, err = decodeString(nodeCtx.key("index"), vraw); err != nil { return nil, err }
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[percolate] unknown parameter [%s]", k)
		}
	}
	if q.Field == "" {
		return nil, illegalAt(nodeCtx, "[percolate] requires [field]")
	}
	return q, nil
}

func parseGeoShapeQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("geo_shape")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	q := &GeoShapeQuery{Boost: DefaultBoost, Relation: "intersects"}
	for k, vraw := range m {
		switch k {
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		case "relation":
			if q.Relation, err = decodeString(nodeCtx.key("relation"), vraw); err != nil { return nil, err }
		default:
			q.Field = k
			jsonx.Unmarshal(vraw, &q.Shape)
		}
	}
	if q.Field == "" {
		return nil, illegalAt(nodeCtx, "[geo_shape] requires a field name")
	}
	return q, nil
}

func parseIntervalsQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("intervals")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	field, fraw, err := singleField(nodeCtx, "intervals", m)
	if err != nil { return nil, err }
	q := &IntervalsQuery{Field: field, Boost: DefaultBoost, Rules: make(map[string]any)}
	var ruleMap map[string]jsonx.RawMessage
	if jsonx.Unmarshal(fraw, &ruleMap) == nil {
		for k, vraw := range ruleMap {
			var rule any
			if jsonx.Unmarshal(vraw, &rule) == nil {
				q.Rules[k] = rule
			}
		}
	}
	return q, nil
}

func parseNestedQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
	nodeCtx := c.key("nested")
	m, err := decodeObject(nodeCtx, raw)
	if err != nil { return nil, err }
	q := &NestedQuery{Boost: DefaultBoost, ScoreMode: "avg"}
	for k, vraw := range m {
		switch k {
		case "path":
			if q.Path, err = decodeString(nodeCtx.key("path"), vraw); err != nil { return nil, err }
		case "query":
			inner, err := parseQueryRaw(nodeCtx.key("query"), vraw)
			if err != nil { return nil, err }
			q.Query = inner
		case "score_mode":
			if q.ScoreMode, err = decodeString(nodeCtx.key("score_mode"), vraw); err != nil { return nil, err }
		case "boost":
			if q.Boost, err = decodeFloat(nodeCtx.key("boost"), vraw); err != nil { return nil, err }
		default:
			return nil, illegalAt(nodeCtx.key(k), "[nested] unknown parameter [%s]", k)
		}
	}
	if q.Path == "" || q.Query == nil {
		return nil, illegalAt(nodeCtx, "[nested] requires [path] and [query]")
	}
	return q, nil
}
