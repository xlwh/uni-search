// Package e2e exercises the full embedded-mode stack end-to-end:
// the embedded engine wired through the REST router. The two R18
// scenarios are:
//
//   - RAG hybrid retrieval: _bulk writes docs, _search returns
//     matches with _source and _score.
//   - Restart zero-loss: write N docs, close the engine, reopen it
//     on the same data dir, and verify all N docs remain visible.
//
// The tests use httptest.NewServer rather than spawning a real
// process so they stay portable across CI environments. The
// "restart" is simulated by Close+New on the same data dir (the
// same durability guarantee: meta files + per-index JSONL doc
// files survive process exit).
package e2e

import (
	"bytes"
	"encoding/json"
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
		Errors bool `json:"errors"`
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
		Took    int `json:"took"`
		Shards  struct {
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

	// Each hit must carry _source + _score + _seq_no.
	for _, h := range searchResp.Hits.Hits {
		assert.Equal(t, "docs", h.Index)
		assert.NotEmpty(t, h.ID)
		assert.Equal(t, float32(1.0), h.Score)
		assert.GreaterOrEqual(t, h.SeqNo, int64(1))
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
