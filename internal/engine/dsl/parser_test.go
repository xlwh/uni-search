package dsl

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/errors"
)

// TestR14_NestedBoolParse covers the R14 acceptance scenario "嵌套 bool 解析":
// a three-layer nested bool (must → bool → filter/must_not) mixing
// term/terms/range/exists/match/match_phrase parses into an AST whose
// structure and fields correspond one-to-one with the input JSON.
func TestR14_NestedBoolParse(t *testing.T) {
	body := `{
	  "query": {
	    "bool": {
	      "must": [
	        {"match": {"content": {"query": "hello world", "operator": "and", "fuzziness": "AUTO", "minimum_should_match": "75%", "analyzer": "standard", "boost": 2.5}}},
	        {"match_phrase": {"title": {"query": "quick fox", "slop": 2, "analyzer": "standard"}}},
	        {"bool": {
	          "filter": [
	            {"range": {"ts": {"gte": "2026-01-01", "lt": "2026-10-01", "time_zone": "+08:00", "format": "strict_date_optional_time", "boost": 1.5}}},
	            {"range": {"level": {"gte": 1, "lte": 9}}},
	            {"term": {"project_id": "p-42"}}
	          ],
	          "must_not": [
	            {"exists": {"field": "deleted_at"}}
	          ]
	        }}
	      ],
	      "should": [
	        {"term": {"status": {"value": "active", "boost": 3.0, "case_insensitive": true}}},
	        {"terms": {"tags": ["go", "db"], "boost": 2.0}},
	        {"term": {"retry": 3}}
	      ],
	      "minimum_should_match": 2,
	      "boost": 1.25
	    }
	  }
	}`

	q, err := ParseQuery([]byte(body))
	require.NoError(t, err)

	bq, ok := q.(*BoolQuery)
	require.True(t, ok, "root query must be a *BoolQuery")
	assert.Equal(t, 1.25, bq.Boost)
	assert.Equal(t, "2", bq.MinShouldMatch, "numeric minimum_should_match must normalize to a string")
	assert.Empty(t, bq.Filter)
	assert.Empty(t, bq.MustNot)
	require.Len(t, bq.Must, 3)
	require.Len(t, bq.Should, 3)

	// must[0]: full match query.
	mq, ok := bq.Must[0].(*MatchQuery)
	require.True(t, ok)
	assert.Equal(t, "content", mq.Field)
	assert.Equal(t, "hello world", mq.Text)
	assert.Equal(t, "and", mq.Operator)
	assert.Equal(t, "AUTO", mq.Fuzziness)
	assert.Equal(t, "75%", mq.MinimumShouldMatch)
	assert.Equal(t, "standard", mq.Analyzer)
	assert.Equal(t, 2.5, mq.Boost)

	// must[1]: match_phrase.
	pq, ok := bq.Must[1].(*MatchPhraseQuery)
	require.True(t, ok)
	assert.Equal(t, "title", pq.Field)
	assert.Equal(t, "quick fox", pq.Text)
	assert.Equal(t, 2, pq.Slop)
	assert.Equal(t, "standard", pq.Analyzer)

	// must[2]: inner bool with filter/must_not (layer 2 → layer 3).
	inner, ok := bq.Must[2].(*BoolQuery)
	require.True(t, ok)
	assert.Empty(t, inner.Must)
	assert.Empty(t, inner.Should)
	assert.Equal(t, DefaultBoost, inner.Boost)
	require.Len(t, inner.Filter, 3)
	require.Len(t, inner.MustNot, 1)

	// filter[0]: date range.
	rq, ok := inner.Filter[0].(*RangeQuery)
	require.True(t, ok)
	assert.Equal(t, "ts", rq.Field)
	assert.Equal(t, "2026-01-01", rq.Gte)
	assert.Nil(t, rq.Gt)
	assert.Equal(t, "2026-10-01", rq.Lt)
	assert.Nil(t, rq.Lte)
	assert.Equal(t, "+08:00", rq.TimeZone)
	assert.Equal(t, "strict_date_optional_time", rq.Format)
	assert.Equal(t, 1.5, rq.Boost)

	// filter[1]: numeric range bounds decode to float64.
	nrq, ok := inner.Filter[1].(*RangeQuery)
	require.True(t, ok)
	assert.Equal(t, "level", nrq.Field)
	assert.Equal(t, float64(1), nrq.Gte)
	assert.Equal(t, float64(9), nrq.Lte)
	assert.Nil(t, nrq.Gt)
	assert.Nil(t, nrq.Lt)

	// filter[2]: short-form term.
	st, ok := inner.Filter[2].(*TermQuery)
	require.True(t, ok)
	assert.Equal(t, "project_id", st.Field)
	assert.Equal(t, "p-42", st.Value)
	assert.Equal(t, DefaultBoost, st.Boost)
	assert.False(t, st.CaseInsensitive)

	// must_not[0]: exists.
	eq, ok := inner.MustNot[0].(*ExistsQuery)
	require.True(t, ok)
	assert.Equal(t, "deleted_at", eq.Field)

	// should[0]: long-form term.
	stq, ok := bq.Should[0].(*TermQuery)
	require.True(t, ok)
	assert.Equal(t, "status", stq.Field)
	assert.Equal(t, "active", stq.Value)
	assert.Equal(t, 3.0, stq.Boost)
	assert.True(t, stq.CaseInsensitive)

	// should[1]: terms with hoisted boost.
	tsq, ok := bq.Should[1].(*TermsQuery)
	require.True(t, ok)
	assert.Equal(t, "tags", tsq.Field)
	assert.Equal(t, []any{"go", "db"}, tsq.Values)
	assert.Equal(t, 2.0, tsq.Boost)

	// should[2]: numeric term.
	ntq, ok := bq.Should[2].(*TermQuery)
	require.True(t, ok)
	assert.Equal(t, "retry", ntq.Field)
	assert.Equal(t, float64(3), ntq.Value)

	// The same body must round-trip through ParseSearchBody.
	sq, from, size, err := ParseSearchBody([]byte(body))
	require.NoError(t, err)
	assert.Equal(t, 0, from)
	assert.Equal(t, 10, size)
	assert.IsType(t, &BoolQuery{}, sq)
}

// TestR14_ErrorPath covers the R14 acceptance scenario "错误路径": a match
// query at query.bool.must[2] missing its required text reports the JSON
// path in the error reason; an unknown DSL (intervals) is rejected with the
// 400 + unsupported_capability envelope carrying the feature name.
func TestR14_ErrorPath(t *testing.T) {
	t.Run("missing match text reports json path", func(t *testing.T) {
		body := `{"query": {"bool": {"must": [
		  {"term": {"a": 1}},
		  {"exists": {"field": "b"}},
		  {"match": {"foo": {"operator": "and"}}}
		]}}}`

		q, err := ParseQuery([]byte(body))
		require.Error(t, err)
		assert.Nil(t, q)

		apiErr, ok := err.(*errors.APIError)
		require.True(t, ok, "parse errors must be *errors.APIError, got %T", err)
		assert.Equal(t, 400, apiErr.Status)
		assert.Equal(t, "illegal_argument", apiErr.Type)
		assert.Contains(t, apiErr.Reason, "query.bool.must[2].match.foo")
	})

	t.Run("unknown query intervals is unsupported_capability", func(t *testing.T) {
		_, err := ParseQuery([]byte(`{"query": {"intervals": {"foo": {"match": {"query": "x"}}}}}`))
		require.Error(t, err)

		apiErr, ok := err.(*errors.APIError)
		require.True(t, ok)
		assert.Equal(t, 400, apiErr.Status)
		assert.Equal(t, "unsupported_capability", apiErr.Type)
		require.NotNil(t, apiErr.Unsupported)
		assert.Equal(t, "query.intervals", apiErr.Unsupported.Feature)
		assert.NotEmpty(t, apiErr.Unsupported.Milestone)
		assert.NotEmpty(t, apiErr.Unsupported.Workaround)
	})

	t.Run("unsupported nested inside bool keeps the feature name", func(t *testing.T) {
		_, err := ParseQuery([]byte(`{"query": {"bool": {"must": [{"query_string": {"query": "a:b"}}]}}}`))
		require.Error(t, err)

		apiErr, ok := err.(*errors.APIError)
		require.True(t, ok)
		assert.Equal(t, "unsupported_capability", apiErr.Type)
		assert.Equal(t, 400, apiErr.Status)
		require.NotNil(t, apiErr.Unsupported)
		assert.Equal(t, "query.query_string", apiErr.Unsupported.Feature)
	})
}

// TestR14_KnnAndHybrid covers top-level knn parsing (ES 8.12 clause), the
// hybrid clause with rrf/weighted rank specs, and the query+knn
// combination semantics.
func TestR14_KnnAndHybrid(t *testing.T) {
	t.Run("top-level knn object", func(t *testing.T) {
		body := `{
		  "knn": {
		    "field": "emb",
		    "vector": [0.1, 0.2, 0.3],
		    "k": 10,
		    "num_candidates": 100,
		    "similarity": 0.5,
		    "filter": {"term": {"project_id": "p-42"}}
		  },
		  "from": 5,
		  "size": 20
		}`

		q, from, size, err := ParseSearchBody([]byte(body))
		require.NoError(t, err)
		assert.Equal(t, 5, from)
		assert.Equal(t, 20, size)

		kq, ok := q.(*KnnQuery)
		require.True(t, ok)
		assert.Equal(t, "emb", kq.Field)
		assert.Equal(t, []float32{0.1, 0.2, 0.3}, kq.Vector)
		assert.Equal(t, 10, kq.K)
		assert.Equal(t, 100, kq.NumCandidates)
		assert.Equal(t, 0.5, kq.Similarity)
		assert.Equal(t, DefaultBoost, kq.Boost)
		require.Len(t, kq.Filter, 1)
		fq, ok := kq.Filter[0].(*TermQuery)
		require.True(t, ok)
		assert.Equal(t, "project_id", fq.Field)
		assert.Equal(t, "p-42", fq.Value)
	})

	t.Run("top-level knn accepts the ES query_vector alias", func(t *testing.T) {
		q, _, _, err := ParseSearchBody([]byte(`{"knn": {"field": "emb", "query_vector": [1.0, 2.0], "k": 3, "num_candidates": 30}}`))
		require.NoError(t, err)
		kq, ok := q.(*KnnQuery)
		require.True(t, ok)
		assert.Equal(t, []float32{1, 2}, kq.Vector)
		assert.Equal(t, 3, kq.K)
		assert.Equal(t, 30, kq.NumCandidates)
	})

	t.Run("knn as a query inside hybrid", func(t *testing.T) {
		q, err := ParseQuery([]byte(`{"hybrid": {"queries": [{"knn": {"field": "emb", "vector": [1.0], "k": 5, "num_candidates": 50}}]}}`))
		require.NoError(t, err)
		hq, ok := q.(*HybridQuery)
		require.True(t, ok)
		require.Len(t, hq.Queries, 1)
		_, ok = hq.Queries[0].(*KnnQuery)
		assert.True(t, ok)
	})

	t.Run("hybrid with rrf rank", func(t *testing.T) {
		body := `{"query": {"hybrid": {
		  "queries": [
		    {"match": {"content": "hello"}},
		    {"knn": {"field": "emb", "vector": [1.0, 2.0], "k": 5, "num_candidates": 50}}
		  ],
		  "filter": [{"term": {"status": "active"}}],
		  "rank": {"method": "rrf", "rank_constant": 60, "window_size": 500}
		}}}`

		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		hq, ok := q.(*HybridQuery)
		require.True(t, ok)
		require.Len(t, hq.Queries, 2)

		mq, ok := hq.Queries[0].(*MatchQuery)
		require.True(t, ok)
		assert.Equal(t, "content", mq.Field)
		assert.Equal(t, "hello", mq.Text)
		assert.Equal(t, "or", mq.Operator)
		assert.Equal(t, DefaultBoost, mq.Boost)

		kq, ok := hq.Queries[1].(*KnnQuery)
		require.True(t, ok)
		assert.Equal(t, "emb", kq.Field)
		assert.Equal(t, 5, kq.K)

		require.Len(t, hq.Filter, 1)
		fq, ok := hq.Filter[0].(*TermQuery)
		require.True(t, ok)
		assert.Equal(t, "status", fq.Field)
		assert.Equal(t, "active", fq.Value)

		assert.Equal(t, RankMethodRRF, hq.Rank.Method)
		assert.Equal(t, 60, hq.Rank.RankConst)
		assert.Equal(t, 500, hq.Rank.WindowSize)
		assert.Nil(t, hq.Rank.Weights)
	})

	t.Run("hybrid with weighted rank fills defaults", func(t *testing.T) {
		body := `{"query": {"hybrid": {
		  "queries": [
		    {"match": {"content": "hello"}},
		    {"knn": {"field": "emb", "vector": [1.0], "k": 5, "num_candidates": 50}}
		  ],
		  "rank": {"method": "weighted", "weights": [0.6, 0.4]}
		}}}`

		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		hq, ok := q.(*HybridQuery)
		require.True(t, ok)
		assert.Equal(t, RankMethodWeighted, hq.Rank.Method)
		assert.Equal(t, []float64{0.6, 0.4}, hq.Rank.Weights)
		assert.Equal(t, DefaultRankConstant, hq.Rank.RankConst)
		assert.Equal(t, DefaultWindowSize, hq.Rank.WindowSize)
	})

	t.Run("hybrid without rank defaults to rrf", func(t *testing.T) {
		q, err := ParseQuery([]byte(`{"query": {"hybrid": {"queries": [{"exists": {"field": "a"}}]}}}`))
		require.NoError(t, err)
		hq, ok := q.(*HybridQuery)
		require.True(t, ok)
		assert.Equal(t, RankMethodRRF, hq.Rank.Method)
		assert.Equal(t, DefaultRankConstant, hq.Rank.RankConst)
		assert.Equal(t, DefaultWindowSize, hq.Rank.WindowSize)
	})

	t.Run("query plus top-level knn combines into bool(filter, must)", func(t *testing.T) {
		body := `{
		  "query": {"term": {"status": "active"}},
		  "knn": {"field": "emb", "vector": [0.5], "k": 5, "num_candidates": 50}
		}`

		q, _, _, err := ParseSearchBody([]byte(body))
		require.NoError(t, err)
		bq, ok := q.(*BoolQuery)
		require.True(t, ok, "query+knn must synthesize a *BoolQuery, got %T", q)
		require.Len(t, bq.Filter, 1)
		fq, ok := bq.Filter[0].(*TermQuery)
		require.True(t, ok)
		assert.Equal(t, "status", fq.Field)
		require.Len(t, bq.Must, 1)
		kq, ok := bq.Must[0].(*KnnQuery)
		require.True(t, ok)
		assert.Equal(t, "emb", kq.Field)
		assert.Equal(t, []float32{0.5}, kq.Vector)
		assert.Empty(t, bq.Should)
		assert.Empty(t, bq.MustNot)
	})

	t.Run("multiple top-level knn clauses union through should", func(t *testing.T) {
		body := `{"knn": [
		  {"field": "a", "vector": [1], "k": 1, "num_candidates": 10},
		  {"field": "b", "vector": [2], "k": 1, "num_candidates": 10}
		]}`
		q, _, _, err := ParseSearchBody([]byte(body))
		require.NoError(t, err)
		bq, ok := q.(*BoolQuery)
		require.True(t, ok)
		require.Len(t, bq.Should, 2)
		assert.Equal(t, "1", bq.MinShouldMatch)
	})
}

// TestR14_FromSizePassthrough pins the ParseSearchBody contract: from/size
// are parsed and passed through unvalidated (the from+size <= 10000 gate
// belongs to T16); defaults are 0/10.
func TestR14_FromSizePassthrough(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		_, from, size, err := ParseSearchBody([]byte(`{"query": {"exists": {"field": "a"}}}`))
		require.NoError(t, err)
		assert.Equal(t, 0, from)
		assert.Equal(t, 10, size)
	})

	t.Run("explicit values pass through", func(t *testing.T) {
		_, from, size, err := ParseSearchBody([]byte(`{"query": {"exists": {"field": "a"}}, "from": 40, "size": 20}`))
		require.NoError(t, err)
		assert.Equal(t, 40, from)
		assert.Equal(t, 20, size)
	})

	t.Run("values beyond the ES window still pass through", func(t *testing.T) {
		_, from, size, err := ParseSearchBody([]byte(`{"query": {"exists": {"field": "a"}}, "from": 9991, "size": 20}`))
		require.NoError(t, err)
		assert.Equal(t, 9991, from)
		assert.Equal(t, 20, size)
	})

	t.Run("zero size is legal", func(t *testing.T) {
		_, _, size, err := ParseSearchBody([]byte(`{"size": 0}`))
		require.NoError(t, err)
		assert.Equal(t, 0, size)
	})

	t.Run("negative from is rejected", func(t *testing.T) {
		_, _, _, err := ParseSearchBody([]byte(`{"from": -1}`))
		require.Error(t, err)
		apiErr, ok := err.(*errors.APIError)
		require.True(t, ok)
		assert.Equal(t, "illegal_argument", apiErr.Type)
		assert.Contains(t, apiErr.Reason, "from")
	})

	t.Run("string size is rejected", func(t *testing.T) {
		_, _, _, err := ParseSearchBody([]byte(`{"size": "10"}`))
		require.Error(t, err)
		apiErr, ok := err.(*errors.APIError)
		require.True(t, ok)
		assert.Equal(t, "illegal_argument", apiErr.Type)
		assert.Contains(t, apiErr.Reason, "size")
	})

	t.Run("fractional from is rejected", func(t *testing.T) {
		_, _, _, err := ParseSearchBody([]byte(`{"from": 1.5}`))
		require.Error(t, err)
		apiErr, ok := err.(*errors.APIError)
		require.True(t, ok)
		assert.Equal(t, "illegal_argument", apiErr.Type)
	})

	t.Run("body without query yields nil query", func(t *testing.T) {
		q, from, size, err := ParseSearchBody([]byte(`{"size": 7}`))
		require.NoError(t, err)
		assert.Nil(t, q)
		assert.Equal(t, 7, size)
		assert.Equal(t, 0, from)

		// The same body through ParseQuery also yields (nil, nil).
		pq, err := ParseQuery([]byte(`{"size": 7}`))
		require.NoError(t, err)
		assert.Nil(t, pq)
	})
}

// TestR14_UnsupportedQueries sweeps the query types named in the task plus
// a few extras: all must surface as 400 + unsupported_capability with the
// "query.<name>" feature.
func TestR14_UnsupportedQueries(t *testing.T) {
	for _, name := range []string{
		"intervals", "query_string", "function_score", "match_bool_prefix",
		"multi_match", "simple_query_string", "script_score", "prefix",
		"wildcard", "regexp", "fuzzy", "ids", "match_all", "match_none",
		"match_phrase_prefix", "sparse_vector", "combined_fields", "terms_set",
		"constant_score", "boosting", "dis_max", "script", "nested",
		"geo_shape", "percolate", "bogus",
	} {
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(`{"query": {%q: {"field": "f"}}}`, name)
			q, err := ParseQuery([]byte(body))
			require.Error(t, err)
			assert.Nil(t, q)

			apiErr, ok := err.(*errors.APIError)
			require.True(t, ok)
			assert.Equal(t, 400, apiErr.Status)
			assert.Equal(t, "unsupported_capability", apiErr.Type)
			require.NotNil(t, apiErr.Unsupported)
			assert.Equal(t, "query."+name, apiErr.Unsupported.Feature)
			assert.NotEmpty(t, apiErr.Unsupported.Milestone)
		})
	}
}

// TestR14_TypeValidation checks that malformed values produce an
// illegal_argument APIError whose reason carries the JSON path of the
// offending clause (or, for recognized-but-missing features, an
// unsupported_capability error with the feature name).
func TestR14_TypeValidation(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantPath    string // required substring of the error reason
		wantFeature string // set: expect unsupported_capability instead
	}{
		{"knn vector element not numeric", `{"query": {"knn": {"field": "v", "vector": ["a", 1], "k": 5}}}`, "query.knn.vector[0]", ""},
		{"knn vector null element", `{"query": {"knn": {"field": "v", "vector": [0.1, null], "k": 5}}}`, "query.knn.vector[1]", ""},
		{"knn vector empty", `{"query": {"knn": {"field": "v", "vector": [], "k": 5}}}`, "query.knn.vector", ""},
		{"knn vector not array", `{"query": {"knn": {"field": "v", "vector": 1.0, "k": 5}}}`, "query.knn.vector", ""},
		{"knn k zero", `{"query": {"knn": {"field": "v", "vector": [0.1], "k": 0}}}`, "query.knn.k", ""},
		{"knn k negative", `{"query": {"knn": {"field": "v", "vector": [0.1], "k": -3}}}`, "query.knn.k", ""},
		{"knn num_candidates zero", `{"query": {"knn": {"field": "v", "vector": [0.1], "k": 2, "num_candidates": 0}}}`, "query.knn.num_candidates", ""},
		{"knn missing vector", `{"query": {"knn": {"field": "v", "k": 1}}}`, "query.knn", ""},
		{"knn missing field", `{"query": {"knn": {"vector": [1.0], "k": 1}}}`, "query.knn", ""},
		{"knn missing k", `{"query": {"knn": {"field": "v", "vector": [1.0]}}}`, "query.knn", ""},
		{"knn vector and query_vector conflict", `{"query": {"knn": {"field": "v", "vector": [1], "query_vector": [2], "k": 1}}}`, "query.knn", ""},
		{"knn unknown key", `{"query": {"knn": {"field": "v", "vector": [1], "k": 1, "ef": 100}}}`, "query.knn", ""},
		{"range boolean bound", `{"query": {"range": {"ts": {"gt": true}}}}`, "query.range.ts.gt", ""},
		{"range null bound", `{"query": {"range": {"ts": {"gte": null}}}}`, "query.range.ts.gte", ""},
		{"range object bound", `{"query": {"range": {"ts": {"lt": {"a": 1}}}}}`, "query.range.ts.lt", ""},
		{"range without bounds", `{"query": {"range": {"ts": {"format": "x"}}}}`, "query.range.ts", ""},
		{"range empty date bound", `{"query": {"range": {"ts": {"gt": ""}}}}`, "query.range.ts.gt", ""},
		{"range unknown key", `{"query": {"range": {"ts": {"gte": 1, "from": 0}}}}`, "query.range.ts", ""},
		{"term object value", `{"query": {"term": {"f": {"value": {"a": 1}}}}}`, "query.term.f.value", ""},
		{"term null value", `{"query": {"term": {"f": {"value": null}}}}`, "query.term.f.value", ""},
		{"term array short form", `{"query": {"term": {"f": [1, 2]}}}`, "query.term.f", ""},
		{"term missing value", `{"query": {"term": {"f": {"boost": 2}}}}`, "query.term.f", ""},
		{"term unknown key", `{"query": {"term": {"f": {"value": 1, "boostr": 1}}}}`, "query.term.f", ""},
		{"terms value not array", `{"query": {"terms": {"f": "abc"}}}`, "query.terms.f", ""},
		{"terms terms lookup object", `{"query": {"terms": {"f": {"index": "i", "id": "1"}}}}`, "", "query.terms.lookup"},
		{"terms zero fields", `{"query": {"terms": {"boost": 1.0}}}`, "query.terms", ""},
		{"terms two fields", `{"query": {"terms": {"a": [1], "b": [2]}}}`, "query.terms", ""},
		{"match bad operator", `{"query": {"match": {"f": {"query": "x", "operator": "xor"}}}}`, "query.match.f.operator", ""},
		{"match fuzziness out of range", `{"query": {"match": {"f": {"query": "x", "fuzziness": 5}}}}`, "query.match.f.fuzziness", ""},
		{"match bad minimum_should_match", `{"query": {"match": {"f": {"query": "x", "minimum_should_match": "abc"}}}}`, "query.match.f.minimum_should_match", ""},
		{"match no text long form", `{"query": {"match": {"f": {"analyzer": "std"}}}}`, "query.match.f", ""},
		{"match query and text conflict", `{"query": {"match": {"f": {"query": "a", "text": "b"}}}}`, "query.match.f", ""},
		{"match unknown key", `{"query": {"match": {"f": {"query": "a", "boostr": 1}}}}`, "query.match.f", ""},
		{"match object text", `{"query": {"match": {"f": {"query": {"a": 1}}}}}`, "query.match.f", ""},
		{"match_phrase negative slop", `{"query": {"match_phrase": {"f": {"query": "x", "slop": -1}}}}`, "query.match_phrase.f.slop", ""},
		{"match_phrase unsupported boost", `{"query": {"match_phrase": {"f": {"query": "x", "boost": 2}}}}`, "query.match_phrase.f", ""},
		{"bool clause not object", `{"query": {"bool": {"must": [{"term": {"a": 1}}, "bogus"]}}}`, "query.bool.must[1]", ""},
		{"bool clause array", `{"query": {"bool": {"must": [{"term": {"a": 1}}, []]}}}`, "query.bool.must[1]", ""},
		{"bool must not array", `{"query": {"bool": {"must": {"term": {"a": 1}}}}}`, "query.bool.must", ""},
		{"bool must null", `{"query": {"bool": {"must": null}}}`, "query.bool.must", ""},
		{"bool unknown key", `{"query": {"bool": {"mst": []}}}`, "query.bool", ""},
		{"bool bad minimum_should_match", `{"query": {"bool": {"minimum_should_match": "x%"}}}`, "query.bool.minimum_should_match", ""},
		{"exists missing field", `{"query": {"exists": {}}}`, "query.exists", ""},
		{"exists empty field", `{"query": {"exists": {"field": ""}}}`, "query.exists.field", ""},
		{"exists unknown key", `{"query": {"exists": {"field": "a", "extra": 1}}}`, "query.exists", ""},
		{"two query types", `{"query": {"term": {"a": 1}, "match": {"b": "x"}}}`, "query", ""},
		{"empty query object", `{"query": {}}`, "query", ""},
		{"hybrid empty queries", `{"query": {"hybrid": {"queries": []}}}`, "query.hybrid", ""},
		{"hybrid queries not array", `{"query": {"hybrid": {"queries": {"term": {"a": 1}}}}}`, "query.hybrid.queries", ""},
		{"hybrid unknown rank method", `{"query": {"hybrid": {"queries": [{"term": {"a": 1}}], "rank": {"method": "bogus"}}}}`, "query.hybrid.rank.method", ""},
		{"hybrid rerank unsupported", `{"query": {"hybrid": {"queries": [{"term": {"a": 1}}], "rank": {"method": "rerank"}}}}`, "", "hybrid.rank.rerank"},
		{"hybrid weights length mismatch", `{"query": {"hybrid": {"queries": [{"term": {"a": 1}}, {"term": {"b": 2}}], "rank": {"method": "weighted", "weights": [1.0]}}}}`, "query.hybrid", ""},
		{"hybrid unknown key", `{"query": {"hybrid": {"queries": [{"term": {"a": 1}}], "fusion": "rrf"}}}`, "query.hybrid", ""},
		{"non-object query", `{"query": "match_all"}`, "query", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := ParseQuery([]byte(tt.body))
			require.Error(t, err)
			assert.Nil(t, q)

			apiErr, ok := err.(*errors.APIError)
			require.True(t, ok)
			assert.Equal(t, 400, apiErr.Status)
			if tt.wantFeature != "" {
				assert.Equal(t, "unsupported_capability", apiErr.Type)
				require.NotNil(t, apiErr.Unsupported)
				assert.Equal(t, tt.wantFeature, apiErr.Unsupported.Feature)
			} else {
				assert.Equal(t, "illegal_argument", apiErr.Type)
				assert.Contains(t, apiErr.Reason, tt.wantPath)
			}
		})
	}
}

// TestR14_MatchLiteralsAndAliases pins literal normalization and the alias
// forms accepted by leaf queries.
func TestR14_MatchLiteralsAndAliases(t *testing.T) {
	t.Run("match text alias and numeric coercion", func(t *testing.T) {
		q, err := ParseQuery([]byte(`{"query": {"match": {"n": 42.5}}}`))
		require.NoError(t, err)
		mq := q.(*MatchQuery)
		assert.Equal(t, "n", mq.Field)
		assert.Equal(t, "42.5", mq.Text)

		q, err = ParseQuery([]byte(`{"query": {"match": {"f": {"text": "alias"}}}}`))
		require.NoError(t, err)
		assert.Equal(t, "alias", q.(*MatchQuery).Text)
	})

	t.Run("empty match text is legal", func(t *testing.T) {
		_, err := ParseQuery([]byte(`{"query": {"match": {"f": ""}}}`))
		require.NoError(t, err)
	})

	t.Run("fuzziness literals", func(t *testing.T) {
		for _, tc := range []struct {
			input string
			want  string
		}{
			{`{"query": {"match": {"f": {"query": "x", "fuzziness": "auto"}}}}`, "AUTO"},
			{`{"query": {"match": {"f": {"query": "x", "fuzziness": "AUTO:1,5"}}}}`, "AUTO:1,5"},
			{`{"query": {"match": {"f": {"query": "x", "fuzziness": 2}}}}`, "2"},
			{`{"query": {"match": {"f": {"query": "x", "fuzziness": 0}}}}`, "0"},
			{`{"query": {"match": {"f": {"query": "x", "fuzziness": 1.5}}}}`, "1.5"},
		} {
			q, err := ParseQuery([]byte(tc.input))
			require.NoError(t, err)
			assert.Equal(t, tc.want, q.(*MatchQuery).Fuzziness)
		}
	})

	t.Run("minimum_should_match literals", func(t *testing.T) {
		for _, tc := range []struct {
			input string
			want  string
		}{
			{`{"query": {"bool": {"should": [{"term": {"a": 1}}], "minimum_should_match": 2}}}`, "2"},
			{`{"query": {"bool": {"should": [{"term": {"a": 1}}], "minimum_should_match": "75%"}}}`, "75%"},
			{`{"query": {"bool": {"should": [{"term": {"a": 1}}], "minimum_should_match": "-1"}}}`, "-1"},
			{`{"query": {"bool": {"should": [{"term": {"a": 1}}], "minimum_should_match": "2<-25% 9<-3"}}}`, "2<-25% 9<-3"},
		} {
			q, err := ParseQuery([]byte(tc.input))
			require.NoError(t, err)
			assert.Equal(t, tc.want, q.(*BoolQuery).MinShouldMatch)
		}
	})

	t.Run("operator is case-insensitive", func(t *testing.T) {
		q, err := ParseQuery([]byte(`{"query": {"match": {"f": {"query": "x", "operator": "AND"}}}}`))
		require.NoError(t, err)
		assert.Equal(t, "and", q.(*MatchQuery).Operator)
	})

	t.Run("term boolean and null-free scalars", func(t *testing.T) {
		q, err := ParseQuery([]byte(`{"query": {"term": {"flag": true}}}`))
		require.NoError(t, err)
		assert.Equal(t, true, q.(*TermQuery).Value)
	})

	t.Run("empty bool is legal", func(t *testing.T) {
		q, err := ParseQuery([]byte(`{"query": {"bool": {}}}`))
		require.NoError(t, err)
		bq := q.(*BoolQuery)
		assert.Empty(t, bq.Must)
		assert.Empty(t, bq.Should)
		assert.Empty(t, bq.Filter)
		assert.Empty(t, bq.MustNot)
		assert.Equal(t, DefaultBoost, bq.Boost)
	})

	t.Run("direct query object without search body wrapper", func(t *testing.T) {
		q, err := ParseQuery([]byte(`{"term": {"a": 1}}`))
		require.NoError(t, err)
		assert.Equal(t, "a", q.(*TermQuery).Field)

		q, err = ParseQuery([]byte(`{"bool": {"filter": [{"exists": {"field": "x"}}]}}`))
		require.NoError(t, err)
		require.Len(t, q.(*BoolQuery).Filter, 1)
	})
}

// FuzzParseQuery feeds arbitrary bytes and JSON shapes into both entry
// points. The invariants: never panic, never return a non-APIError error,
// never return a query alongside an error. The fixed seed corpus covers
// empty objects, deep nesting, very long arrays, invalid UTF-8 and
// truncated JSON.
func FuzzParseQuery(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"query": {"bool": {"must": [{"term": {"f": {"value": 1}}}], "filter": [{"range": {"t": {"gte": "2026-01-01"}}}]}}}`))
	f.Add(deepNestedBool(500))
	f.Add(longTermsBody(20000))
	f.Add([]byte{'"', 0xff, 0xfe, 0x00, '\\'})
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Add([]byte(`{"query": {"match": {"f": `))
	f.Add([]byte(`{"query": {"bool": {"must": [`))
	f.Add([]byte(`{"knn": {"field": "v", "vector": [1, 2], "k": 1}}`))
	f.Add([]byte(`{"query": {"hybrid": {"queries": [{"match": {"a": "b"}}], "rank": {"method": "rrf"}}}}`))
	f.Add([]byte(`{"query": {"bool": {"must": [{"match": {"f": {"query": "x", "fuzziness": "AUTO:2,1"}}}]}}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		q, err := ParseQuery(data)
		if err != nil {
			if _, ok := err.(*errors.APIError); !ok {
				t.Fatalf("ParseQuery returned non-APIError %T: %v", err, err)
			}
			if q != nil {
				t.Fatal("ParseQuery returned both a query and an error")
			}
		} else if q != nil {
			q.queryNode() // parsed values must implement the AST marker
		}

		if q2, from, size, err2 := ParseSearchBody(data); err2 != nil {
			if _, ok := err2.(*errors.APIError); !ok {
				t.Fatalf("ParseSearchBody returned non-APIError %T: %v", err2, err2)
			}
			if q2 != nil {
				t.Fatal("ParseSearchBody returned both a query and an error")
			}
		} else {
			_ = from
			_ = size
		}
	})
}

// deepNestedBool builds a bool clause nested depth levels deep.
func deepNestedBool(depth int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"query": `)
	for i := 0; i < depth; i++ {
		sb.WriteString(`{"bool": {"must": [`)
	}
	sb.WriteString(`{"term": {"a": 1}}`)
	for i := 0; i < depth; i++ {
		sb.WriteString(`]}}`)
	}
	sb.WriteString(`}`)
	return []byte(sb.String())
}

// longTermsBody builds a terms query holding n values.
func longTermsBody(n int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"query": {"terms": {"f": [`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"v`)
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte('"')
	}
	sb.WriteString(`]}}`)
	return []byte(sb.String())
}
