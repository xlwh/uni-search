package esapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
)

// ---------------------------------------------------------------------------
// Mock engine
// ---------------------------------------------------------------------------

type mockEngine struct {
	writes      []IndexDoc
	writeErr    error
	bulkRes     []WriteResult
	bulkErr     error
	getRes      map[string][]byte
	getSeqNo    map[string]int64
	notFound    bool
	deleteRes   *WriteResult
	deleteErr   error
	searchRes   *SearchResult
	searchErr   error
	countVal    int64
	countErr    error
	mgetResult  []GetResult
	mgetErr     error
	mappingPut  []byte
	mappingGet  []byte
	mappingErr  error
	settingsErr error
	aliasCalls  []struct{ Index, Alias string }
	aliasesMap  map[string][]string
	aliasErr    error
	analyzeRes  []AnalyzeToken
	analyzeErr  error
	refreshErr  error
	indicesRes  []IndexInfo
	indicesErr  error
	healthRes   *ClusterHealth
	healthErr   error
}

func (m *mockEngine) Write(_ context.Context, doc IndexDoc, _ string) (*WriteResult, error) {
	m.writes = append(m.writes, doc)
	if m.writeErr != nil {
		return nil, m.writeErr
	}
	res := &WriteResult{Index: doc.Index, ID: doc.ID, Op: doc.Op, SeqNo: 1, Version: 1, Result: "created"}
	// _create path: a doc that already exists (per mock's getRes)
	// surfaces a 409-style conflict result.
	if doc.Op == OpCreate && m.getRes != nil {
		key := doc.Index + "/" + doc.ID
		if _, exists := m.getRes[key]; exists {
			res.Result = "conflict"
		}
	}
	// if_seq_no optimistic locking: any mismatch → conflict.
	if doc.IfSeqNo != nil {
		if cur, ok := m.getSeqNo[doc.ID]; ok && cur != *doc.IfSeqNo {
			res.Result = "conflict"
		}
	}
	return res, nil
}

func (m *mockEngine) Bulk(_ context.Context, docs []IndexDoc, _ string) ([]WriteResult, error) {
	m.writes = append(m.writes, docs...)
	if m.bulkErr != nil {
		return nil, m.bulkErr
	}
	if m.bulkRes != nil {
		return m.bulkRes, nil
	}
	out := make([]WriteResult, 0, len(docs))
	for _, d := range docs {
		out = append(out, WriteResult{
			Index: d.Index, ID: d.ID, Op: d.Op,
			SeqNo: 1, Version: 1, Result: opDefaultResult(d.Op),
		})
	}
	return out, nil
}

func opDefaultResult(op DocOp) string {
	switch op {
	case OpDelete:
		return "deleted"
	case OpUpdate:
		return "updated"
	default:
		return "created"
	}
}

func (m *mockEngine) Get(_ context.Context, index, id string) ([]byte, int64, int64, bool, error) {
	if m.notFound {
		return nil, 0, 0, true, nil
	}
	key := index + "/" + id
	if src, ok := m.getRes[key]; ok {
		seq := m.getSeqNo[id]
		return src, seq, 1, false, nil
	}
	return nil, 0, 0, true, nil
}

func (m *mockEngine) MGet(_ context.Context, reqs []GetRequest) ([]GetResult, error) {
	if m.mgetErr != nil {
		return nil, m.mgetErr
	}
	if m.mgetResult != nil {
		return m.mgetResult, nil
	}
	out := make([]GetResult, 0, len(reqs))
	for _, r := range reqs {
		key := r.Index + "/" + r.ID
		src, ok := m.getRes[key]
		out = append(out, GetResult{
			Index: r.Index, ID: r.ID,
			Found: ok, Source: src,
		})
	}
	return out, nil
}

func (m *mockEngine) Delete(_ context.Context, index, id string, ifSeq *int64, _ string) (*WriteResult, error) {
	if m.deleteErr != nil {
		return nil, m.deleteErr
	}
	if m.deleteRes != nil {
		return m.deleteRes, nil
	}
	return &WriteResult{Index: index, ID: id, Op: OpDelete, SeqNo: 2, Version: 2, Result: "deleted"}, nil
}

func (m *mockEngine) Search(_ context.Context, _ []string, _ []byte) (*SearchResult, error) {
	return m.searchRes, m.searchErr
}

func (m *mockEngine) Count(_ context.Context, _ []string, _ []byte) (int64, error) {
	return m.countVal, m.countErr
}

func (m *mockEngine) PutMapping(_ context.Context, index string, body []byte) error {
	m.mappingPut = body
	return m.mappingErr
}

func (m *mockEngine) GetMapping(_ context.Context, _ string) ([]byte, error) {
	return m.mappingGet, m.mappingErr
}

func (m *mockEngine) PutSettings(context.Context, string, []byte) error { return m.settingsErr }
func (m *mockEngine) GetSettings(context.Context, string) ([]byte, error) {
	return m.mappingGet, m.mappingErr
}

func (m *mockEngine) PutAlias(_ context.Context, idx, alias string) error {
	m.aliasCalls = append(m.aliasCalls, struct{ Index, Alias string }{idx, alias})
	return m.aliasErr
}

func (m *mockEngine) GetAliases(_ context.Context, _ string) (map[string][]string, error) {
	return m.aliasesMap, m.aliasErr
}

func (m *mockEngine) Analyze(context.Context, string, string, string) ([]AnalyzeToken, error) {
	return m.analyzeRes, m.analyzeErr
}

func (m *mockEngine) Refresh(context.Context, string) error { return m.refreshErr }

func (m *mockEngine) Indices(context.Context, string) ([]IndexInfo, error) {
	return m.indicesRes, m.indicesErr
}

func (m *mockEngine) Health(context.Context) (*ClusterHealth, error) {
	return m.healthRes, m.healthErr
}

// ---------------------------------------------------------------------------
// Spec R17 scenario 1: ES client connectivity — GET /, _cluster/health,
// _cat/health, _cat/indices must succeed with ES 8.x-shaped bodies so
// elasticsearch-py 8.x sees a compatible server.
// ---------------------------------------------------------------------------

func TestR17_ESClientConnectivity(t *testing.T) {
	mock := &mockEngine{
		healthRes: &ClusterHealth{
			ClusterName:         "unisearch",
			Status:              "green",
			NumberOfNodes:       1,
			NumberOfDataNodes:   1,
			ActivePrimaryShards: 1,
			ActiveShards:        1,
		},
		indicesRes: []IndexInfo{
			{Health: "green", Status: "open", Index: "docs", UUID: "abc", Primaries: 1, Replicas: 0, DocsCount: 5},
		},
	}
	srv := NewServer(mock)

	// 1. GET / — version probe must report 8.12.0 + distribution:unisearch.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var root map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &root))
	assert.Equal(t, "unisearch", root["cluster_name"])
	version := root["version"].(map[string]any)
	assert.Equal(t, "8.12.0", version["number"])
	assert.Equal(t, "unisearch", version["distribution"])
	assert.Equal(t, "You Know, for Search", root["tagline"])

	// 2. GET /_cluster/health returns the cluster health snapshot.
	req = httptest.NewRequest(http.MethodGet, "/_cluster/health", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var health ClusterHealth
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &health))
	assert.Equal(t, "green", health.Status)
	assert.Equal(t, "unisearch", health.ClusterName)
	assert.Equal(t, 1, health.NumberOfNodes)

	// 3. GET /_cat/indices returns the index table.
	req = httptest.NewRequest(http.MethodGet, "/_cat/indices?format=json", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var indices []IndexInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &indices))
	require.Len(t, indices, 1)
	assert.Equal(t, "docs", indices[0].Index)
	assert.Equal(t, int64(5), indices[0].DocsCount)
}

// ---------------------------------------------------------------------------
// Spec R17 scenario 2: _bulk mixed-batch — legal and illegal items
// mixed; HTTP 200 with per-item errors; legal items persisted.
// ---------------------------------------------------------------------------

func TestR17_BulkMixedBatch(t *testing.T) {
	mock := &mockEngine{
		bulkRes: []WriteResult{
			{Index: "docs", ID: "1", Op: OpIndex, Result: "created", SeqNo: 1},
			{Index: "docs", ID: "2", Op: OpIndex, Result: "", Err: apierr.NewDocumentParsing("mapping conflict on field foo")},
		},
	}
	srv := NewServer(mock)

	// NDJSON: two index lines + two source lines.
	body := strings.Join([]string{
		`{"index":{"_index":"docs","_id":"1"}}`,
		`{"title":"hello"}`,
		`{"index":{"_index":"docs","_id":"2"}}`,
		`{"title":123}`, // would-be mapping conflict (mock surfaces it)
	}, "\n") + "\n"

	req := httptest.NewRequest(http.MethodPost, "/_bulk", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "bulk HTTP 200 even with per-item errors")
	var resp bulkResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Errors, "errors flag set when any item failed")
	require.Len(t, resp.Items, 2)
	// Item 0: success → 201.
	item0 := resp.Items[0]["index"].(map[string]any)
	assert.Equal(t, "created", item0["result"])
	assert.EqualValues(t, http.StatusCreated, item0["status"])
	// Item 1: failure → 400 + error block.
	item1 := resp.Items[1]["index"].(map[string]any)
	assert.EqualValues(t, http.StatusBadRequest, item1["status"])
	errObj := item1["error"].(map[string]any)
	assert.Equal(t, "document_parsing_exception", errObj["type"])
	assert.Contains(t, errObj["reason"].(string), "mapping conflict")

	// Both writes dispatched to the engine (engine layer owns
	// per-row mapping rejection).
	require.Len(t, mock.writes, 2)
}

// ---------------------------------------------------------------------------
// Spec R17 extras: if_seq_no 409, _create 409, _search response shape.
// ---------------------------------------------------------------------------

func TestR17_DocIfSeqNoConflict(t *testing.T) {
	mock := &mockEngine{
		getSeqNo: map[string]int64{"1": 5}, // current seq is 5; client sends 4 → conflict.
	}
	srv := NewServer(mock)

	// PUT /docs/_doc/1?if_seq_no=4 with any body.
	req := httptest.NewRequest(http.MethodPut, "/docs/_doc/1?if_seq_no=4", bytes.NewReader([]byte(`{"x":1}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "version_conflict_engine_exception", errObj["type"])
}

func TestR17_CreateConflict(t *testing.T) {
	mock := &mockEngine{
		getRes:   map[string][]byte{"docs/existing": {0x7b, 0x7d}},
		getSeqNo: map[string]int64{"existing": 1},
	}
	srv := NewServer(mock)

	req := httptest.NewRequest(http.MethodPost, "/docs/_create/existing", bytes.NewReader([]byte(`{"x":1}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "version_conflict_engine_exception", errObj["type"])
}

func TestR17_SearchResponseShape(t *testing.T) {
	maxScore := float32(1.5)
	mock := &mockEngine{
		searchRes: &SearchResult{
			Took:     12,
			Shards:   ShardStats{Total: 1, Successful: 1},
			Total:    2,
			Relation: "eq",
			MaxScore: &maxScore,
			Hits: []SearchHit{
				{Index: "docs", ID: "1", Score: 1.5, Source: []byte(`{"title":"a"}`), SeqNo: 1, Primary: 1},
				{Index: "docs", ID: "2", Score: 0.8, Source: []byte(`{"title":"b"}`), SeqNo: 2, Primary: 1},
			},
		},
	}
	srv := NewServer(mock)

	req := httptest.NewRequest(http.MethodPost, "/docs/_search", bytes.NewReader([]byte(`{"query":{"match_all":{}}}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.EqualValues(t, 12, body["took"])
	assert.Equal(t, false, body["timed_out"])

	shards := body["_shards"].(map[string]any)
	assert.EqualValues(t, 1, shards["total"])
	assert.EqualValues(t, 1, shards["successful"])

	hits := body["hits"].(map[string]any)
	total := hits["total"].(map[string]any)
	assert.EqualValues(t, 2, total["value"])
	assert.Equal(t, "eq", total["relation"])
	assert.EqualValues(t, 1.5, hits["max_score"])
	hitsArr := hits["hits"].([]any)
	require.Len(t, hitsArr, 2)
	first := hitsArr[0].(map[string]any)
	assert.Equal(t, "docs", first["_index"])
	assert.Equal(t, "1", first["_id"])
	assert.EqualValues(t, 1.5, first["_score"])
	src := first["_source"].(map[string]any)
	assert.Equal(t, "a", src["title"])
	assert.EqualValues(t, 1, first["_seq_no"])
}

func TestR17_CountResponse(t *testing.T) {
	mock := &mockEngine{countVal: 42}
	srv := NewServer(mock)
	req := httptest.NewRequest(http.MethodPost, "/docs/_count", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.EqualValues(t, 42, body["count"])
}

func TestR17_MGetResponse(t *testing.T) {
	mock := &mockEngine{
		getRes: map[string][]byte{
			"docs/1": []byte(`{"a":1}`),
			"docs/2": []byte(`{"a":2}`),
		},
	}
	srv := NewServer(mock)
	req := httptest.NewRequest(http.MethodPost, "/_mget",
		bytes.NewReader([]byte(`{"docs":[{"_index":"docs","_id":"1"},{"_index":"docs","_id":"2"},{"_index":"docs","_id":"3"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	docs := body["docs"].([]any)
	require.Len(t, docs, 3)
	d0 := docs[0].(map[string]any)
	assert.True(t, d0["found"].(bool))
	d2 := docs[2].(map[string]any)
	assert.False(t, d2["found"].(bool))
}

func TestR17_RefreshNoOp(t *testing.T) {
	mock := &mockEngine{}
	srv := NewServer(mock)
	req := httptest.NewRequest(http.MethodPost, "/docs/_refresh", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	shards := body["_shards"].(map[string]any)
	assert.EqualValues(t, 1, shards["successful"])
}

// ---------------------------------------------------------------------------
// Panic recovery — a handler that panics must surface as 500 APIError
// rather than kill the process.
// ---------------------------------------------------------------------------

func TestR17_PanicRecovery(t *testing.T) {
	mock := &mockEngine{
		searchErr: panicSentinel{},
	}
	srv := NewServer(mock)
	req := httptest.NewRequest(http.MethodPost, "/docs/_search", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "internal_error", errObj["type"])
}

// panicSentinel: an error whose .Error() panics — surfaces the
// recovery path through Engine.Search returning a panicking error
// rather than the happy path. We actually trigger the panic
// directly here.
type panicSentinel struct{}

func (panicSentinel) Error() string { panic("boom from search") }

// ---------------------------------------------------------------------------
// Unsupported endpoint test (routes not registered return 404; the
// design's unsupported.go is a sentinel handler for explicit
// `unsupported_capability` errors — M0 returns 404 for unregistered
// paths which is consistent with ES).
// ---------------------------------------------------------------------------

func TestR17_UnregisteredRoute404(t *testing.T) {
	srv := NewServer(&mockEngine{})
	req := httptest.NewRequest(http.MethodGet, "/_xpack/security/user", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ---------------------------------------------------------------------------
// Compile-time assertion that mockEngine satisfies Engine.
// ---------------------------------------------------------------------------

var _ Engine = (*mockEngine)(nil)
