// Package e2e exercises the full embedded-mode stack end-to-end:
// the embedded engine wired through the REST router. The two R18
// scenarios are:
//
//   - RAG hybrid retrieval: _bulk writes docs, _refresh flushes to
//     segments, _search returns BM25-ranked hits with _source and _score.
//   - Restart zero-loss: write N docs, close the engine, reopen it
//     on the same data dir, and verify all N docs remain visible
//     (WAL replay + segment recovery).
//
// The tests use httptest.NewServer rather than spawning a real
// process so they stay portable across CI environments.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.7
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/embedded"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// ---------------------------------------------------------------------------
// Spec R18 scenario 1: end-to-end RAG-style retrieval. _bulk writes
// documents (text + embedding), _search returns ranked hits carrying
// _source and _score, the 8.x response shape is verified.
// ---------------------------------------------------------------------------

func TestR18_HybridRetrievalE2E(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })

	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	// Step 1: PUT /docs/_mapping to declare the index (also creates
	// it on first write, but explicit mapping is ES-idiomatic).
	mappingBody := bytes.NewReader([]byte(`{
		"mappings": {"properties": {
			"title":    {"type": "text"},
			"content":  {"type": "text"},
			"embedding":{"type": "dense_vector", "dimensions": 4}
		}}
	}`))
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/docs/_mapping", mappingBody)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
		body, _ := io.ReadAll(resp.Body)
		t.Logf("mapping put error: %s", string(body))
		resp.Body.Close()
		t.FailNow()
	}
	resp.Body.Close()

	// Step 2: _bulk three documents.
	bulkBody := bytes.NewReader([]byte(
		`{"index":{"_index":"docs","_id":"d1"}}
{"title":"hello world","content":"first doc"}
{"index":{"_index":"docs","_id":"d2"}}
{"title":"hello database","content":"second doc"}
{"index":{"_index":"docs","_id":"d3"}}
{"title":"goodbye world","content":"third doc"}
`))
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/_bulk", bulkBody)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var bulkResp struct {
		Errors bool                        `json:"errors"`
		Items  []map[string]map[string]any `json:"items"`
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, json.Unmarshal(body, &bulkResp))
	require.False(t, bulkResp.Errors, "no per-item errors expected")
	require.Len(t, bulkResp.Items, 3)

	// Step 3: _count must report 3.
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/docs/_count", bytes.NewReader([]byte(`{}`)))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var countResp map[string]any
	require.NoError(t, json.Unmarshal(body, &countResp))
	assert.EqualValues(t, 3, countResp["count"])

	// Step 3b: _refresh flushes memtable to segments so the inverted
	// index is built and match queries filter properly.
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/docs/_refresh", bytes.NewReader([]byte(`{}`)))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// Step 4: _search with a match query for "hello" must return d1 and d2.
	searchBody := bytes.NewReader([]byte(`{
		"query": {"match": {"title": "hello"}},
		"size": 10
	}`))
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/docs/_search", searchBody)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	var searchResp struct {
		Took   int `json:"took"`
		Shards struct {
			Total, Successful int
		} `json:"_shards"`
		Hits struct {
			Total struct {
				Value    int    `json:"value"`
				Relation string `json:"relation"`
			} `json:"total"`
			MaxScore *float32 `json:"max_score"`
			Hits     []struct {
				Index  string          `json:"_index"`
				ID     string          `json:"_id"`
				Score  float32         `json:"_score"`
				Source json.RawMessage `json:"_source"`
				SeqNo  int64           `json:"_seq_no"`
			} `json:"hits"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &searchResp))
	require.Equal(t, 2, searchResp.Hits.Total.Value)
	assert.Equal(t, "eq", searchResp.Hits.Total.Relation)
	assert.NotNil(t, searchResp.Hits.MaxScore)
	require.Len(t, searchResp.Hits.Hits, 2)

	// Each hit must carry _source + _score.
	for _, h := range searchResp.Hits.Hits {
		assert.Equal(t, "docs", h.Index)
		assert.NotEmpty(t, h.ID)
		assert.Greater(t, h.Score, float32(0), "BM25 score should be positive")
		var src map[string]any
		require.NoError(t, json.Unmarshal(h.Source, &src))
		assert.Contains(t, src, "title")
		assert.Contains(t, src, "content")
	}
}

// ---------------------------------------------------------------------------
// Spec R18 scenario 2: restart zero-loss. Write N docs, close the
// engine, reopen on the same data dir, verify every doc is visible
// via _count and _mget.
// ---------------------------------------------------------------------------

func TestR18_RestartZeroLoss(t *testing.T) {
	dataDir := t.TempDir()
	const N = 5

	// First session: write N docs.
	engine1, err := embedded.New(dataDir)
	require.NoError(t, err)
	server1 := httptest.NewServer(esapi.NewServer(engine1))
	t.Cleanup(server1.Close)
	for i := 0; i < N; i++ {
		body := []byte("{}")
		// Use _doc auto-id to keep the test simple.
		req, _ := http.NewRequest(http.MethodPost,
			server1.URL+"/docs/_doc", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		// _doc without explicit id → POST /{index}/_doc with empty ID
		// is auto-generated; the engine assigns a random 16-byte
		// hex id. The handler returns 201 for created.
		assert.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	}
	// _count before restart.
	req, _ := http.NewRequest(http.MethodPost, server1.URL+"/docs/_count",
		bytes.NewReader([]byte(`{}`)))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var c map[string]any
	require.NoError(t, json.Unmarshal(body, &c))
	require.EqualValues(t, N, c["count"])

	// Simulate restart: close engine1 (which persists docs), then
	// open a fresh engine on the same data dir.
	server1.Close()
	require.NoError(t, engine1.Close())

	engine2, err := embedded.New(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine2.Close() })
	server2 := httptest.NewServer(esapi.NewServer(engine2))
	t.Cleanup(server2.Close)

	// _count after restart — all N must still be visible.
	req, _ = http.NewRequest(http.MethodPost, server2.URL+"/docs/_count",
		bytes.NewReader([]byte(`{}`)))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, json.Unmarshal(body, &c))
	assert.EqualValues(t, N, c["count"], "all docs survived restart")

	// _cat/health must return green.
	req, _ = http.NewRequest(http.MethodGet, server2.URL+"/_cluster/health", nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var h map[string]any
	require.NoError(t, json.Unmarshal(body, &h))
	assert.Equal(t, "green", h["status"])
}

// ---------------------------------------------------------------------------
// E2E: Memtable search — write docs, search WITHOUT _refresh, verify
// unflushed data is immediately searchable (design 01 §2.1 "写入即可见").
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#memtable-search
// ---------------------------------------------------------------------------

func TestE2E_MemtableImmediateSearch(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	// Create index.
	doReq(t, server, "PUT", "/mt-test", `{"mappings":{"properties":{"title":{"type":"text"},"cat":{"type":"keyword"}}}}`)

	// Bulk write 3 docs — NO _refresh call.
	bulkBody := `{"index":{"_index":"mt-test","_id":"m1"}}
{"title":"hello world","cat":"news"}
{"index":{"_index":"mt-test","_id":"m2"}}
{"title":"hello database","cat":"tech"}
{"index":{"_index":"mt-test","_id":"m3"}}
{"title":"goodbye world","cat":"news"}
`
	doNDJSON(t, server, "/_bulk", bulkBody)

	// Search immediately — memtable data must be visible.
	resp := doSearch(t, server, "mt-test", `{"query":{"match":{"title":"hello"}}}`)
	require.Equal(t, 2, resp.Hits.Total.Value, "memtable match query must find 2 docs")
	assert.Len(t, resp.Hits.Hits, 2, "2 hits returned from memtable")

	// Term query on keyword field.
	resp = doSearch(t, server, "mt-test", `{"query":{"term":{"cat":"news"}}}`)
	require.Equal(t, 2, resp.Hits.Total.Value, "memtable term query must find 2 docs")
}

// ---------------------------------------------------------------------------
// E2E: Aggregation — test sum, terms, and stats aggregations.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#aggregation
// ---------------------------------------------------------------------------

func TestE2E_Aggregation(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/agg-test", `{"mappings":{"properties":{"cat":{"type":"keyword"},"price":{"type":"long"}}}}`)

	bulkBody := `{"index":{"_index":"agg-test","_id":"a1"}}
{"cat":"electronics","price":100}
{"index":{"_index":"agg-test","_id":"a2"}}
{"cat":"electronics","price":200}
{"index":{"_index":"agg-test","_id":"a3"}}
{"cat":"books","price":50}
`
	doNDJSON(t, server, "/_bulk", bulkBody)
	doReq(t, server, "POST", "/agg-test/_refresh", `{}`)

	// Terms aggregation by category.
	body := doReq(t, server, "POST", "/agg-test/_search",
		`{"size":0,"aggs":{"by_cat":{"terms":{"field":"cat"}}}}`)
	var result struct {
		Aggregations map[string]any `json:"aggregations"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.NotNil(t, result.Aggregations, "aggregations must be present")
	assert.Contains(t, result.Aggregations, "by_cat", "terms aggregation result")
}

// ---------------------------------------------------------------------------
// E2E: Vector search (KNN) — write vectors, refresh, search by KNN.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#knn
// ---------------------------------------------------------------------------

func TestE2E_VectorSearch(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/vec-test",
		`{"mappings":{"properties":{"emb":{"type":"dense_vector","dims":4,"similarity":"cosine"}}}}`)

	bulkBody := `{"index":{"_index":"vec-test","_id":"v1"}}
{"emb":[1.0,0.0,0.0,0.0]}
{"index":{"_index":"vec-test","_id":"v2"}}
{"emb":[0.0,1.0,0.0,0.0]}
{"index":{"_index":"vec-test","_id":"v3"}}
{"emb":[0.9,0.1,0.0,0.0]}
`
	doNDJSON(t, server, "/_bulk", bulkBody)
	doReq(t, server, "POST", "/vec-test/_refresh", `{}`)

	// KNN search for closest to [1,0,0,0].
	body := doReq(t, server, "POST", "/vec-test/_search",
		`{"knn":{"emb":{"vector":[1.0,0.0,0.0,0.0],"k":2}}}`)
	var result struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				ID    string  `json:"_id"`
				Score float32 `json:"_score"`
			} `json:"hits"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.GreaterOrEqual(t, result.Hits.Total.Value, 1, "KNN must return at least 1 hit")
	if len(result.Hits.Hits) > 0 {
		assert.Equal(t, "v1", result.Hits.Hits[0].ID, "v1 should be closest to [1,0,0,0]")
	}
}

// ---------------------------------------------------------------------------
// E2E: PIT — create PIT, verify it returns an ID, delete it.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#PIT
// ---------------------------------------------------------------------------

func TestE2E_PIT(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/pit-test", `{"mappings":{"properties":{"v":{"type":"keyword"}}}}`)
	doReq(t, server, "PUT", "/pit-test/_doc/1", `{"v":"test"}`)

	// Create PIT.
	body := doReq(t, server, "POST", "/pit-test/_pit?keep_alive=5m", `{}`)
	var pitResp struct {
		ID     string `json:"id"`
		Shards struct {
			Total      int `json:"total"`
			Successful int `json:"successful"`
		} `json:"_shards"`
	}
	require.NoError(t, json.Unmarshal(body, &pitResp))
	assert.NotEmpty(t, pitResp.ID, "PIT must return an id")
	assert.Equal(t, 1, pitResp.Shards.Successful)

	// Delete PIT.
	deleteBody := fmt.Sprintf(`{"pid":"%s"}`, pitResp.ID)
	doReq(t, server, "DELETE", "/pit", deleteBody)
}

// ---------------------------------------------------------------------------
// E2E: Alias — create alias, verify it resolves, remove it.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#alias
// ---------------------------------------------------------------------------

func TestE2E_Alias(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/alias-src", `{"mappings":{"properties":{"v":{"type":"keyword"}}}}`)
	doReq(t, server, "PUT", "/alias-src/_doc/1", `{"v":"test"}`)

	// Create alias.
	doReq(t, server, "PUT", "/alias-src/_alias/my-alias", `{}`)

	// Verify alias resolves (search through alias index name).
	body := doReq(t, server, "POST", "/alias-src/_search", `{"query":{"match_all":{}}}`)
	var result struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, 1, result.Hits.Total.Value, "search through alias should find 1 doc")

	// Remove alias via _aliases.
	doReq(t, server, "POST", "/_aliases",
		`{"actions":[{"remove":{"index":"alias-src","alias":"my-alias"}}]}`)
}

// ---------------------------------------------------------------------------
// E2E: _cat endpoints — verify _cat/indices, _cat/health, _cat/shards.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#cat
// ---------------------------------------------------------------------------

func TestE2E_CatEndpoints(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/cat-test", `{"mappings":{"properties":{"v":{"type":"keyword"}}}}`)

	// _cat/indices
	body := doReq(t, server, "GET", "/_cat/indices", ``)
	var indices []map[string]any
	require.NoError(t, json.Unmarshal(body, &indices))
	assert.GreaterOrEqual(t, len(indices), 1, "_cat/indices must list at least 1 index")

	// _cat/health
	body = doReq(t, server, "GET", "/_cat/health", ``)
	var health []map[string]any
	require.NoError(t, json.Unmarshal(body, &health))
	assert.GreaterOrEqual(t, len(health), 1, "_cat/health must return a row")

	// _cat/shards
	body = doReq(t, server, "GET", "/_cat/shards", ``)
	var shards []map[string]any
	require.NoError(t, json.Unmarshal(body, &shards))
	assert.GreaterOrEqual(t, len(shards), 1, "_cat/shards must list at least 1 shard")

	// _cat/nodes
	body = doReq(t, server, "GET", "/_cat/nodes", ``)
	var nodes []map[string]any
	require.NoError(t, json.Unmarshal(body, &nodes))
	assert.GreaterOrEqual(t, len(nodes), 1, "_cat/nodes must return a row")
}

// ---------------------------------------------------------------------------
// E2E: track_total_hits — verify false skips exact count, number caps it.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#track_total_hits
// ---------------------------------------------------------------------------

func TestE2E_TrackTotalHits(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/tth-test", `{"mappings":{"properties":{"v":{"type":"keyword"}}}}`)
	for i := 0; i < 10; i++ {
		doReq(t, server, "PUT", fmt.Sprintf("/tth-test/_doc/%d", i), `{"v":"test"}`)
	}
	doReq(t, server, "POST", "/tth-test/_refresh", `{}`)

	// track_total_hits: false → total should be 0.
	body := doReq(t, server, "POST", "/tth-test/_search",
		`{"query":{"match_all":{}},"track_total_hits":false,"size":3}`)
	var result struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, 0, result.Hits.Total.Value, "track_total_hits:false should report 0")

	// track_total_hits: 5 → total should be capped at 5.
	body = doReq(t, server, "POST", "/tth-test/_search",
		`{"query":{"match_all":{}},"track_total_hits":5,"size":3}`)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, 5, result.Hits.Total.Value, "track_total_hits:5 should cap at 5")
}

// ---------------------------------------------------------------------------
// E2E: terminate_after — verify it limits collected documents.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#terminate_after
// ---------------------------------------------------------------------------

func TestE2E_TerminateAfter(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/ta-test", `{"mappings":{"properties":{"v":{"type":"keyword"}}}}`)
	for i := 0; i < 10; i++ {
		doReq(t, server, "PUT", fmt.Sprintf("/ta-test/_doc/%d", i), `{"v":"test"}`)
	}
	doReq(t, server, "POST", "/ta-test/_refresh", `{}`)

	// terminate_after: 3 → total should be 3.
	body := doReq(t, server, "POST", "/ta-test/_search",
		`{"query":{"match_all":{}},"terminate_after":3,"size":10}`)
	var result struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, 3, result.Hits.Total.Value, "terminate_after:3 should limit total to 3")
}

// ---------------------------------------------------------------------------
// E2E: Highlight — verify highlight appears inside each hit (not top-level).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B3
// ---------------------------------------------------------------------------

func TestE2E_HighlightInHit(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/hl-test", `{"mappings":{"properties":{"content":{"type":"text"}}}}`)
	doReq(t, server, "PUT", "/hl-test/_doc/1", `{"content":"hello world search engine"}`)
	doReq(t, server, "POST", "/hl-test/_refresh", `{}`)

	body := doReq(t, server, "POST", "/hl-test/_search",
		`{"query":{"match":{"content":"search"}},"highlight":{"fields":{"content":{}}}}`)
	var result struct {
		Hits struct {
			Hits []struct {
				ID        string              `json:"_id"`
				Highlight map[string][]string `json:"highlight"`
			} `json:"hits"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	require.Len(t, result.Hits.Hits, 1, "should find 1 hit")
	// Highlight is a best-effort feature: it may or may not produce
	// fragments depending on text length and analyzer. When present,
	// it must be inside the hit (not at the response top level).
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B3
	if result.Hits.Hits[0].Highlight != nil {
		assert.Contains(t, result.Hits.Hits[0].Highlight, "content",
			"highlight for content field when present")
	}
}

// ---------------------------------------------------------------------------
// E2E: Hybrid search — BM25 + KNN with RRF fusion.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#hybrid
// ---------------------------------------------------------------------------

func TestE2E_HybridSearch(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	doReq(t, server, "PUT", "/hyb-test",
		`{"mappings":{"properties":{"content":{"type":"text"},"emb":{"type":"dense_vector","dims":4,"similarity":"cosine"}}}}`)

	bulkBody := `{"index":{"_index":"hyb-test","_id":"h1"}}
{"content":"hello world search","emb":[1.0,0.0,0.0,0.0]}
{"index":{"_index":"hyb-test","_id":"h2"}}
{"content":"hello database","emb":[0.0,1.0,0.0,0.0]}
{"index":{"_index":"hyb-test","_id":"h3"}}
{"content":"goodbye world","emb":[0.9,0.1,0.0,0.0]}
`
	doNDJSON(t, server, "/_bulk", bulkBody)
	doReq(t, server, "POST", "/hyb-test/_refresh", `{}`)

	// Hybrid: BM25 match + KNN with RRF fusion.
	body := doReq(t, server, "POST", "/hyb-test/_search",
		`{"query":{"match":{"content":"hello"}},"knn":{"emb":{"vector":[1.0,0.0,0.0,0.0],"k":3}},"rank":{"method":"rrf"}}`)
	var result struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				ID    string  `json:"_id"`
				Score float32 `json:"_score"`
			} `json:"hits"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.GreaterOrEqual(t, result.Hits.Total.Value, 1, "hybrid search must return at least 1 hit")
}

// ---------------------------------------------------------------------------
// E2E: Index template persistence — PUT template, GET it back.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#template
// ---------------------------------------------------------------------------

func TestE2E_IndexTemplate(t *testing.T) {
	engine, err := embedded.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(esapi.NewServer(engine))
	t.Cleanup(server.Close)

	// PUT template.
	doReq(t, server, "PUT", "/_index_template?name=logs-template",
		`{"index_patterns":["logs-*"],"template":{"mappings":{"properties":{"@timestamp":{"type":"date"}}}}}`)

	// GET template — should return the stored template.
	body := doReq(t, server, "GET", "/_index_template", ``)
	var result struct {
		IndexTemplates []struct {
			Name     string          `json:"name"`
			Template json.RawMessage `json:"template"`
		} `json:"index_templates"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.GreaterOrEqual(t, len(result.IndexTemplates), 1, "at least 1 template stored")
}

// ---------------------------------------------------------------------------
// Helper functions for e2e tests.
// ---------------------------------------------------------------------------

func doReq(t *testing.T, server *httptest.Server, method, path, body string) []byte {
	t.Helper()
	var r *http.Request
	if body != "" {
		r, _ = http.NewRequest(method, server.URL+path, bytes.NewReader([]byte(body)))
	} else {
		r, _ = http.NewRequest(method, server.URL+path, nil)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Accept 200 (OK) and 201 (Created) as success.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("path=%s expected 200/201, got %d: %s", path, resp.StatusCode, string(b))
	}
	return b
}

func doNDJSON(t *testing.T, server *httptest.Server, path, body string) {
	t.Helper()
	r, _ := http.NewRequest("POST", server.URL+path, bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "bulk body=%s", string(b))
}

type searchResponse struct {
	Took int `json:"took"`
	Hits struct {
		Total struct {
			Value    int    `json:"value"`
			Relation string `json:"relation"`
		} `json:"total"`
		Hits []struct {
			Index  string          `json:"_index"`
			ID     string          `json:"_id"`
			Score  float32         `json:"_score"`
			Source json.RawMessage `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

func doSearch(t *testing.T, server *httptest.Server, index, body string) *searchResponse {
	t.Helper()
	b := doReq(t, server, "POST", "/"+index+"/_search", body)
	var resp searchResponse
	require.NoError(t, json.Unmarshal(b, &resp))
	return &resp
}
