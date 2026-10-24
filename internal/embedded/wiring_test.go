package embedded

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// TestScriptQuery_FiltersByScript verifies the script query filters
// documents through the real script engine instead of matching all
// documents (the former stub behavior).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.6
func TestScriptQuery_FiltersByScript(t *testing.T) {
	dir := t.TempDir()
	eng, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()

	require.NoError(t, eng.PutMapping(ctx, "t", []byte(`{"mappings":{"properties":{"price":{"type":"double"}}}}`)))
	for i, price := range []float64{10, 50, 200} {
		_, err := eng.Write(ctx, esapi.IndexDoc{
			Op: esapi.OpIndex, Index: "t", ID: string(rune('a' + i)),
			Source: []byte(`{"price":` + floatStr(price) + `}`),
		}, "")
		require.NoError(t, err)
	}

	// memtable path: only docs with price > 100.
	res, err := eng.Search(ctx, []string{"t"}, []byte(
		`{"query":{"script":{"script":{"source":"doc['price'].value > 100"}}},"size":10}`))
	require.NoError(t, err)
	require.Equal(t, 1, res.Total, "script filter should keep only price=200")
	assert.Equal(t, "c", res.Hits[0].ID)

	// Segment path (after flush): postFilterHits re-evaluates the script.
	require.NoError(t, eng.Refresh(ctx, "t"))
	res, err = eng.Search(ctx, []string{"t"}, []byte(
		`{"query":{"script":{"script":{"source":"doc['price'].value > 100"}}},"size":10}`))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Total, "script filter on segment path should keep only price=200")
}

// TestScriptScore_ReScoresWithScript verifies script_score replaces the
// inner score with the script result (former stub assigned uniform 1.0).
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.7
func TestScriptScore_ReScoresWithScript(t *testing.T) {
	dir := t.TempDir()
	eng, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()

	require.NoError(t, eng.PutMapping(ctx, "t", []byte(`{"mappings":{"properties":{"price":{"type":"double"}}}}`)))
	_, err = eng.Write(ctx, esapi.IndexDoc{
		Op: esapi.OpIndex, Index: "t", ID: "a",
		Source: []byte(`{"price":42}`),
	}, "")
	require.NoError(t, err)

	res, err := eng.Search(ctx, []string{"t"}, []byte(
		`{"query":{"script_score":{"query":{"match_all":{}},"script":{"source":"doc['price'].value * 2"}}},"size":10}`))
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.InDelta(t, 84.0, res.Hits[0].Score, 0.001, "script_score should set score = price*2 = 84")

	// min_score filtering drops the hit.
	res, err = eng.Search(ctx, []string{"t"}, []byte(
		`{"query":{"script_score":{"query":{"match_all":{}},"script":{"source":"doc['price'].value"},"min_score":100}},"size":10}`))
	require.NoError(t, err)
	assert.Equal(t, 0, res.Total, "min_score=100 should drop the price=42 hit")
}

// TestScriptFields_UseScriptEngine verifies script_fields evaluate via
// the script engine (the former stub only supported field * N via regex).
func TestScriptFields_UseScriptEngine(t *testing.T) {
	dir := t.TempDir()
	eng, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()

	require.NoError(t, eng.PutMapping(ctx, "t", []byte(`{"mappings":{"properties":{"price":{"type":"double"},"qty":{"type":"long"}}}}`)))
	_, err = eng.Write(ctx, esapi.IndexDoc{
		Op: esapi.OpIndex, Index: "t", ID: "a",
		Source: []byte(`{"price":10,"qty":3}`),
	}, "")
	require.NoError(t, err)

	// Ternary + two-field arithmetic — beyond the old regex stub.
	res, err := eng.Search(ctx, []string{"t"}, []byte(
		`{"query":{"match_all":{}},"size":10,"script_fields":{"total":{"script":{"source":"doc['price'].value * doc['qty'].value"}},"tag":{"script":{"source":"doc['price'].value > 5 ? 'expensive' : 'cheap'"}}}}`))
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	require.NotNil(t, res.Hits[0].Fields)
	assert.InDelta(t, 30.0, res.Hits[0].Fields["total"].(float64), 0.001)
	assert.Equal(t, "expensive", res.Hits[0].Fields["tag"])
}

// TestPercolate_EndToEnd verifies percolator queries register on write
// (docs with a "query" field) and percolate search returns the matching
// queries — not all documents (the former stub behavior).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#percolate
func TestPercolate_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	eng, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()

	require.NoError(t, eng.PutMapping(ctx, "queries", []byte(`{"mappings":{"properties":{"query":{"type":"object"}}}}`)))

	// Register two percolator queries and one ordinary document.
	_, err = eng.Write(ctx, esapi.IndexDoc{
		Op: esapi.OpIndex, Index: "queries", ID: "q-error",
		Source: []byte(`{"query":{"match":{"message":"error"}}}`),
	}, "")
	require.NoError(t, err)
	_, err = eng.Write(ctx, esapi.IndexDoc{
		Op: esapi.OpIndex, Index: "queries", ID: "q-prod",
		Source: []byte(`{"query":{"term":{"env":"prod"}}}`),
	}, "")
	require.NoError(t, err)
	_, err = eng.Write(ctx, esapi.IndexDoc{
		Op: esapi.OpIndex, Index: "queries", ID: "plain-doc",
		Source: []byte(`{"message":"ordinary"}`),
	}, "")
	require.NoError(t, err)

	idx, ok := eng.manager.getIndex("queries")
	require.True(t, ok)
	assert.Equal(t, 2, idx.percolator.Count(), "two docs carry a query field")

	// Percolate a doc matching only q-error.
	res, err := eng.Search(ctx, []string{"queries"}, []byte(
		`{"query":{"percolate":{"field":"query","document":{"message":"error in pipeline","env":"dev"}}},"size":10}`))
	require.NoError(t, err)
	require.Equal(t, 1, res.Total, "only q-error should match")
	assert.Equal(t, "q-error", res.Hits[0].ID)
	assert.Contains(t, string(res.Hits[0].Source), `"match"`)

	// Percolate a doc matching only q-prod.
	res, err = eng.Search(ctx, []string{"queries"}, []byte(
		`{"query":{"percolate":{"field":"query","document":{"message":"fine","env":"prod"}}},"size":10}`))
	require.NoError(t, err)
	require.Equal(t, 1, res.Total)
	assert.Equal(t, "q-prod", res.Hits[0].ID)

	// Deleting the query doc unregisters it.
	_, err = eng.Delete(ctx, "queries", "q-error", nil, "")
	require.NoError(t, err)
	res, err = eng.Search(ctx, []string{"queries"}, []byte(
		`{"query":{"percolate":{"field":"query","document":{"message":"error","env":"dev"}}},"size":10}`))
	require.NoError(t, err)
	assert.Equal(t, 0, res.Total, "deleted percolator query should no longer match")
}

// TestSuggest_TermSuggester verifies the suggest section is parsed and
// answered (formerly silently dropped) using the term dictionary built
// from the index's analyzed text fields.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.5
func TestSuggest_TermSuggester(t *testing.T) {
	dir := t.TempDir()
	eng, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()

	require.NoError(t, eng.PutMapping(ctx, "t", []byte(`{"mappings":{"properties":{"title":{"type":"text"}}}}`)))
	_, err = eng.Write(ctx, esapi.IndexDoc{
		Op: esapi.OpIndex, Index: "t", ID: "a",
		Source: []byte(`{"title":"hello world"}`),
	}, "")
	require.NoError(t, err)

	res, err := eng.Search(ctx, []string{"t"}, []byte(
		`{"size":0,"suggest":{"my-suggest":{"text":"helo","term":{"field":"title"}}}}`))
	require.NoError(t, err)
	require.NotNil(t, res.Suggest, "suggest section should produce a response block")

	entries, ok := res.Suggest["my-suggest"].([]suggestEntry)
	require.True(t, ok, "suggest block should be a list of entries")
	require.Len(t, entries, 1)
	assert.Equal(t, "helo", entries[0].Text)
	require.NotEmpty(t, entries[0].Options, "expected 'hello' suggestion")
	assert.Equal(t, "hello", entries[0].Options[0].Text)
}

// floatStr renders a float the way JSON needs it.
func floatStr(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
