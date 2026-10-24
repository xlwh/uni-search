package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/errors"
)

// TestP0_PrefixQueryParse tests prefix query parsing in both short and long form.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.1
func TestP0_PrefixQueryParse(t *testing.T) {
	// Short form.
	t.Run("short form", func(t *testing.T) {
		body := `{"query": {"prefix": {"title": "hel"}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		pq, ok := q.(*PrefixQuery)
		require.True(t, ok, "must be *PrefixQuery")
		assert.Equal(t, "title", pq.Field)
		assert.Equal(t, "hel", pq.Value)
		assert.Equal(t, DefaultBoost, pq.Boost)
	})

	// Long form with boost.
	t.Run("long form with boost", func(t *testing.T) {
		body := `{"query": {"prefix": {"title": {"value": "uni", "boost": 2.5}}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		pq, ok := q.(*PrefixQuery)
		require.True(t, ok)
		assert.Equal(t, "title", pq.Field)
		assert.Equal(t, "uni", pq.Value)
		assert.Equal(t, 2.5, pq.Boost)
	})

	// Missing value.
	t.Run("missing value returns error", func(t *testing.T) {
		body := `{"query": {"prefix": {"title": {"boost": 2.0}}}}`
		_, err := ParseQuery([]byte(body))
		require.Error(t, err)
		apiErr, ok := err.(*errors.APIError)
		require.True(t, ok)
		assert.Contains(t, apiErr.Error(), "requires [value]")
	})
}

// TestP0_RegexpQueryParse tests regexp query parsing.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.2
func TestP0_RegexpQueryParse(t *testing.T) {
	// Short form.
	t.Run("short form", func(t *testing.T) {
		body := `{"query": {"regexp": {"title": "hel.*"}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		rq, ok := q.(*RegexpQuery)
		require.True(t, ok)
		assert.Equal(t, "title", rq.Field)
		assert.Equal(t, "hel.*", rq.Pattern)
		assert.Equal(t, DefaultBoost, rq.Boost)
	})

	// Long form with flags.
	t.Run("long form with flags", func(t *testing.T) {
		body := `{"query": {"regexp": {"title": {"value": "ab.*", "flags": "ALL", "boost": 1.5}}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		rq, ok := q.(*RegexpQuery)
		require.True(t, ok)
		assert.Equal(t, "title", rq.Field)
		assert.Equal(t, "ab.*", rq.Pattern)
		assert.Equal(t, "ALL", rq.Flags)
		assert.Equal(t, 1.5, rq.Boost)
	})

	// Missing value.
	t.Run("missing value returns error", func(t *testing.T) {
		body := `{"query": {"regexp": {"title": {"flags": "ALL"}}}}`
		_, err := ParseQuery([]byte(body))
		require.Error(t, err)
	})
}

// TestP0_ConstantScoreQueryParse tests constant_score query parsing.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.3
func TestP0_ConstantScoreQueryParse(t *testing.T) {
	t.Run("with filter and default boost", func(t *testing.T) {
		body := `{"query": {"constant_score": {"filter": {"term": {"status": "active"}}}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		csq, ok := q.(*ConstantScoreQuery)
		require.True(t, ok)
		assert.Equal(t, DefaultBoost, csq.Boost)
		_, ok = csq.Filter.(*TermQuery)
		assert.True(t, ok, "filter must be a TermQuery")
	})

	t.Run("with filter and custom boost", func(t *testing.T) {
		body := `{"query": {"constant_score": {"filter": {"match": {"title": "hello"}}, "boost": 3.0}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		csq, ok := q.(*ConstantScoreQuery)
		require.True(t, ok)
		assert.Equal(t, 3.0, csq.Boost)
		_, ok = csq.Filter.(*MatchQuery)
		assert.True(t, ok, "filter must be a MatchQuery")
	})

	t.Run("missing filter returns error", func(t *testing.T) {
		body := `{"query": {"constant_score": {"boost": 2.0}}}`
		_, err := ParseQuery([]byte(body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires [filter]")
	})
}

// TestP0_BoostingQueryParse tests boosting query parsing.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.4
func TestP0_BoostingQueryParse(t *testing.T) {
	t.Run("positive and negative", func(t *testing.T) {
		body := `{"query": {"boosting": {"positive": {"match": {"title": "hello"}}, "negative": {"term": {"status": "inactive"}}, "negative_boost": 0.5}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		bq, ok := q.(*BoostingQuery)
		require.True(t, ok)
		assert.NotNil(t, bq.Positive)
		assert.NotNil(t, bq.Negative)
		assert.Equal(t, 0.5, bq.NegativeBoost)
		_, ok = bq.Positive.(*MatchQuery)
		assert.True(t, ok)
		_, ok = bq.Negative.(*TermQuery)
		assert.True(t, ok)
	})

	t.Run("missing positive returns error", func(t *testing.T) {
		body := `{"query": {"boosting": {"negative": {"term": {"status": "inactive"}}}}}`
		_, err := ParseQuery([]byte(body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires [positive]")
	})
}

// TestP0_DisMaxQueryParse tests dis_max query parsing.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.5
func TestP0_DisMaxQueryParse(t *testing.T) {
	t.Run("multiple queries with tie_breaker", func(t *testing.T) {
		body := `{"query": {"dis_max": {"queries": [{"match": {"title": "hello"}}, {"match": {"content": "world"}}], "tie_breaker": 0.3, "boost": 1.5}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		dq, ok := q.(*DisMaxQuery)
		require.True(t, ok)
		require.Len(t, dq.Queries, 2)
		assert.Equal(t, 0.3, dq.TieBreaker)
		assert.Equal(t, 1.5, dq.Boost)
		_, ok = dq.Queries[0].(*MatchQuery)
		assert.True(t, ok)
	})

	t.Run("single query", func(t *testing.T) {
		body := `{"query": {"dis_max": {"queries": [{"term": {"status": "active"}}]}}}`
		q, err := ParseQuery([]byte(body))
		require.NoError(t, err)
		dq, ok := q.(*DisMaxQuery)
		require.True(t, ok)
		require.Len(t, dq.Queries, 1)
		assert.Equal(t, DefaultBoost, dq.Boost)
	})

	t.Run("empty queries returns error", func(t *testing.T) {
		body := `{"query": {"dis_max": {"queries": []}}}`
		_, err := ParseQuery([]byte(body))
		require.Error(t, err)
	})

	t.Run("missing queries returns error", func(t *testing.T) {
		body := `{"query": {"dis_max": {"tie_breaker": 0.3}}}`
		_, err := ParseQuery([]byte(body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires [queries]")
	})
}

// TestP0_UnsupportedRemoved verifies that the 5 new query types are no
// longer in the unsupportedQueries map.
func TestP0_UnsupportedRemoved(t *testing.T) {
	removed := []string{"prefix", "regexp", "constant_score", "boosting", "dis_max"}
	for _, name := range removed {
		_, exists := unsupportedQueries[name]
		assert.False(t, exists, "%q should be removed from unsupportedQueries", name)
	}
}
