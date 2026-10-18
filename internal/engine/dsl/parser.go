package dsl

import (
	"bytes"
	"encoding/json"
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
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, errors.NewIllegalArgument("request body must be a valid JSON object")
	}
	if _, ok := top["query"]; ok || isSearchBody(top) {
		q, _, _, err := ParseSearchBody(body)
		return q, err
	}
	return parseQueryMap(queryRootCtx(), top)
}

// ParseSearchBody parses a complete search request body. Only the keys
// this milestone understands are interpreted: "query", "knn", "from" and
// "size"; everything else (sort, aggs, highlight, ...) is left untouched
// for the search layer (T16). from/size are passed through unvalidated:
// enforcing from+size <= 10000 belongs to the search layer (design 03 §8)
// which owns the ES-compatible error copy. Defaults: from 0, size 10.
func ParseSearchBody(body []byte) (Query, int, int, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
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

func isSearchBody(top map[string]json.RawMessage) bool {
	for k := range top {
		if searchBodyKeys[k] {
			return true
		}
	}
	return false
}

// parseQueryRaw parses a query clause (a JSON object holding exactly one
// query type) at the given path.
func parseQueryRaw(c *parseCtx, raw json.RawMessage) (Query, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, illegalAt(c, "query clause must be a JSON object")
	}
	return parseQueryMap(c, m)
}

func parseQueryMap(c *parseCtx, m map[string]json.RawMessage) (Query, error) {
	if len(m) != 1 {
		return nil, illegalAt(c, "query clause must contain exactly one query type, got %d", len(m))
	}
	for name, raw := range m {
		return dispatchQuery(c, name, raw)
	}
	return nil, nil // unreachable: m has exactly one entry
}

func dispatchQuery(c *parseCtx, name string, raw json.RawMessage) (Query, error) {
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
	case "hybrid":
		return parseHybridQuery(c, raw)
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
	"match_all":           {"v1.2 (M1)", "omit the query clause; a search without a query matches all documents"},
	"match_none":          {"v1.2 (M1)", "use a term query with a value that never occurs"},
	"match_bool_prefix":   {"v1.2 (M1)", "expand the final prefix into terms client-side and use a bool.should of term queries"},
	"match_phrase_prefix": {"v1.2 (M1)", "use match_phrase with the complete final term"},
	"multi_match":         {"v1.2 (M1)", "combine one match query per field inside a bool.should query"},
	"combined_fields":     {"v1.2 (M1)", "combine one match query per field inside a bool.should query"},
	"intervals":           {"v1.2 (M1)", "rewrite as match_phrase or nested bool queries"},
	"query_string":        {"v1.2 (M1)", "rewrite as match/match_phrase/bool queries"},
	"simple_query_string": {"v1.2 (M1)", "rewrite as match/match_phrase/bool queries"},
	"prefix":              {"v1.2 (M1)", "enumerate the matching terms client-side and use a terms query"},
	"wildcard":            {"v1.2 (M1)", "enumerate the matching terms client-side and use a terms query"},
	"regexp":              {"v1.2 (M1)", "enumerate the matching terms client-side and use a terms query"},
	"fuzzy":               {"v1.2 (M1)", "use a match query with fuzziness (for example AUTO)"},
	"ids":                 {"v1.2 (M1)", "use a terms query on the _id field"},
	"terms_set":           {"v1.2 (M1)", "compute the matching values client-side and use a terms query"},
	"constant_score":      {"v1.2 (M1)", "put the inner query into bool.filter, which does not affect scoring"},
	"boosting":            {"v1.2 (M1)", "use bool with must plus must_not, or hybrid with weighted rank"},
	"dis_max":             {"v1.2 (M1)", "use bool.should with boost on the individual queries"},
	"function_score":      {"v1.2 (M1)", "use per-query boost, or hybrid rank with weights"},
	"script_score":        {"v1.2 (M1)", "use knn similarity or hybrid rank instead of scripts"},
	"script":              {"v1.2 (M1)", "precompute the predicate into an indexed field"},
	"more_like_this":      {"v1.2 (M1)", "use match or match_phrase with representative terms"},
	"nested":              {"v1.2 (M1)", "query the flattened nested fields directly"},
	"wrapper":             {"v1.2 (M1)", "send the unwrapped query object directly"},
	"pinned":              {"v1.2 (M1)", "use bool.should with the pinned documents boosted"},
	"distance_feature":    {"v1.2 (M1)", "use a range query on the date or numeric field"},
	"rank_feature":        {"v1.2 (M1)", "fold the feature into a numeric boost via hybrid weights"},
	"geo_distance":        {"v1.2 (M1)", "use range queries on the indexed lat/lon subfields"},
	"geo_bounding_box":    {"v1.2 (M1)", "use range queries on the indexed lat/lon subfields"},
	"geo_shape":           {"v1.3 (M2)", "use geo_point with distance queries, or restructure the data"},
	"sparse_vector":       {"v1.2 (M1)", "use knn on dense vectors"},
	"semantic":            {"v1.3 (M2)", "embed the text client-side and use match plus knn in a hybrid query"},
	"percolate":           {"v1.4 (M3)", "store the queries as documents and match them client-side"},
}

// newUnsupportedQuery rejects a query type outside the M0 subset with the
// 400 + unsupported_capability envelope (errors registry, 00 §6.1).
func newUnsupportedQuery(name string) *errors.APIError {
	if info, ok := unsupportedQueries[name]; ok {
		return errors.NewUnsupported("query."+name, info.milestone, info.workaround)
	}
	return errors.NewUnsupported("query."+name, "v1.2 (M1)", "check the supported query DSL subset in the documentation")
}

// singleField extracts the single field-name key from a leaf query object
// ({"<type>": {"<field>": ...}}); leaf queries in this subset accept
// exactly one field.
func singleField(c *parseCtx, typeName string, m map[string]json.RawMessage) (string, json.RawMessage, error) {
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

func parseTermQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
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

func parseTermsQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
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
			var arr []json.RawMessage
			if err := json.Unmarshal(vraw, &arr); err != nil {
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

func parseRangeQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
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
func decodeRangeBound(c *parseCtx, raw json.RawMessage) (any, error) {
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
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, illegalAt(c, "range bound must be a number or a date string, got %s", jsonKindName(raw))
		}
		return f, nil
	}
}

func parseExistsQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
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

func parseMatchQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
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

func parseMatchPhraseQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
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

func parseBoolQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
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
func parseClauseArray(c *parseCtx, raw json.RawMessage) ([]Query, error) {
	if firstByte(raw) != '[' {
		return nil, illegalAt(c, "expected an array of query clauses, got %s", jsonKindName(raw))
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
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
func parseKnnQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
	return parseKnnObject(c.key("knn"), raw)
}

// parseTopKnn parses the ES 8.12 top-level "knn" search clause, which is
// either a single knn object or an array of knn objects.
func parseTopKnn(c *parseCtx, raw json.RawMessage) ([]Query, error) {
	if firstByte(raw) == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
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
func parseKnnObject(nodeCtx *parseCtx, raw json.RawMessage) (*KnnQuery, error) {
	m, err := decodeObject(nodeCtx, raw)
	if err != nil {
		return nil, err
	}

	q := &KnnQuery{Boost: DefaultBoost}
	seenVector := false
	for k, vraw := range m {
		switch k {
		case "field":
			if q.Field, err = decodeNonEmptyString(nodeCtx.key("field"), vraw); err != nil {
				return nil, err
			}
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
func decodeVector(c *parseCtx, raw json.RawMessage) ([]float32, error) {
	if firstByte(raw) != '[' {
		return nil, illegalAt(c, "vector must be a non-empty array of numbers, got %s", jsonKindName(raw))
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
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
		if err := json.Unmarshal(el, &f); err != nil {
			return nil, illegalAt(c.idx(i), "vector element must be a number, got %s", jsonKindName(el))
		}
		vec = append(vec, float32(f))
	}
	return vec, nil
}

// parseHybridQuery parses the UniSearch-specific hybrid clause: several
// retrieval paths plus a shared filter, fused by a RankSpec (design 03 §5).
func parseHybridQuery(c *parseCtx, raw json.RawMessage) (Query, error) {
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
func parseRankSpec(c *parseCtx, raw json.RawMessage) (RankSpec, error) {
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
			case "relative_score", "rerank":
				return RankSpec{}, errors.NewUnsupported("hybrid.rank."+s, "v1.2 (M1)",
					"use method rrf or weighted")
			default:
				return RankSpec{}, illegalAt(c.key("method"), "[rank] method must be [rrf] or [weighted], got [%s]", s)
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

func decodeFloatArray(c *parseCtx, raw json.RawMessage) ([]float64, error) {
	if firstByte(raw) != '[' {
		return nil, illegalAt(c, "expected an array of numbers, got %s", jsonKindName(raw))
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
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

func decodeObject(c *parseCtx, raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, illegalAt(c, "expected a JSON object, got %s", jsonKindName(raw))
	}
	return m, nil
}

func decodeString(c *parseCtx, raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", illegalAt(c, "expected a string, got %s", jsonKindName(raw))
	}
	return s, nil
}

func decodeNonEmptyString(c *parseCtx, raw json.RawMessage) (string, error) {
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
func decodeScalar(c *parseCtx, raw json.RawMessage) (any, error) {
	switch firstByte(raw) {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, illegalAt(c, "expected a scalar value")
		}
		return s, nil
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, illegalAt(c, "expected a scalar value")
		}
		return b, nil
	case '{', '[', 'n', 0:
		return nil, illegalAt(c, "expected a scalar value, got %s", jsonKindName(raw))
	default:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, illegalAt(c, "expected a scalar value, got %s", jsonKindName(raw))
		}
		return f, nil
	}
}

// decodeText decodes the query text of match-style queries. ES coerces
// numbers to text, so both strings and numbers are accepted; booleans,
// null, objects and arrays are not.
func decodeText(c *parseCtx, raw json.RawMessage) (string, error) {
	switch firstByte(raw) {
	case '"':
		return decodeString(c, raw)
	case 't', 'f', '{', '[', 'n', 0:
		return "", illegalAt(c, "query text must be a string or a number, got %s", jsonKindName(raw))
	default:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return "", illegalAt(c, "query text must be a string or a number, got %s", jsonKindName(raw))
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
}

func decodeFloat(c *parseCtx, raw json.RawMessage) (float64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, illegalAt(c, "expected a number, got %s", jsonKindName(raw))
	}
	return f, nil
}

func decodeBool(c *parseCtx, raw json.RawMessage) (bool, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, illegalAt(c, "expected a boolean, got %s", jsonKindName(raw))
	}
	return b, nil
}

func decodeInt(c *parseCtx, raw json.RawMessage) (int, error) {
	// Guard the first byte: encoding/json happily decodes a quoted number
	// literal into json.Number, but ES rejects string-typed integer
	// parameters, and so must we.
	if b := firstByte(raw); b != '-' && (b < '0' || b > '9') {
		return 0, illegalAt(c, "expected an integer, got %s", jsonKindName(raw))
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, illegalAt(c, "expected an integer, got %s", jsonKindName(raw))
	}
	i64, err := n.Int64()
	if err != nil {
		return 0, illegalAt(c, "expected an integer, got [%s]", n.String())
	}
	return int(i64), nil
}

func decodeNonNegativeInt(c *parseCtx, raw json.RawMessage) (int, error) {
	n, err := decodeInt(c, raw)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, illegalAt(c, "expected a non-negative integer, got %d", n)
	}
	return n, nil
}

func decodePositiveInt(c *parseCtx, raw json.RawMessage) (int, error) {
	n, err := decodeInt(c, raw)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, illegalAt(c, "expected a positive integer, got %d", n)
	}
	return n, nil
}

func decodeOperator(c *parseCtx, raw json.RawMessage) (string, error) {
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
func decodeFuzziness(c *parseCtx, raw json.RawMessage) (string, error) {
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
		if err := json.Unmarshal(raw, &f); err != nil {
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

func decodeMinimumShouldMatch(c *parseCtx, raw json.RawMessage) (string, error) {
	var s string
	switch firstByte(raw) {
	case '"':
		var err error
		if s, err = decodeString(c, raw); err != nil {
			return "", err
		}
	default:
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
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

func firstByte(raw json.RawMessage) byte {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return 0
	}
	return t[0]
}

func jsonKindName(raw json.RawMessage) string {
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
