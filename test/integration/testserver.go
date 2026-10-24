// Package integration provides the test infrastructure for golden
// tests: a TestServer that boots the embedded engine behind an
// httptest.NewServer, and a TestClient that wraps HTTP calls with
// ES-compatible JSON parsing.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.11
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unisearch-db/unisearch/internal/embedded"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// TestServer boots the embedded engine behind httptest.NewServer.
type TestServer struct {
	Engine  *embedded.Engine
	Server  *httptest.Server
	DataDir string
}

// NewTestServer creates a TestServer with a temporary data directory.
func NewTestServer(t *testing.T) *TestServer {
	t.Helper()
	dir := t.TempDir()
	eng, err := embedded.New(dir)
	if err != nil {
		t.Fatalf("integration: create engine: %v", err)
	}
	srv := httptest.NewServer(esapi.NewServer(eng))
	t.Cleanup(func() {
		srv.Close()
		eng.Close()
	})
	return &TestServer{Engine: eng, Server: srv, DataDir: dir}
}

// TestClient wraps HTTP calls against the TestServer.
type TestClient struct {
	baseURL string
	t       *testing.T
}

// NewTestClient creates a client for the given TestServer.
func NewTestClient(t *testing.T, ts *TestServer) *TestClient {
	return &TestClient{baseURL: ts.Server.URL, t: t}
}

// Put sends a PUT request and returns the response body.
func (c *TestClient) Put(path string, body []byte) (int, []byte) {
	req, _ := http.NewRequest(http.MethodPut, c.baseURL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

// Post sends a POST request and returns the response body.
func (c *TestClient) Post(path string, body []byte) (int, []byte) {
	req, _ := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req)
}

// Get sends a GET request and returns the response body.
func (c *TestClient) Get(path string) (int, []byte) {
	req, _ := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	return c.do(req)
}

// Delete sends a DELETE request and returns the response body.
func (c *TestClient) Delete(path string) (int, []byte) {
	req, _ := http.NewRequest(http.MethodDelete, c.baseURL+path, nil)
	return c.do(req)
}

// PostNDJSON sends a POST request with NDJSON content type.
func (c *TestClient) PostNDJSON(path string, body []byte) (int, []byte) {
	req, _ := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	return c.do(req)
}

func (c *TestClient) do(req *http.Request) (int, []byte) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("integration: HTTP request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, body
}

// CreateIndex creates an index with the given mapping.
func (c *TestClient) CreateIndex(index string, mapping []byte) (int, []byte) {
	return c.Put("/"+index, mapping)
}

// IndexDoc indexes a single document.
func (c *TestClient) IndexDoc(index, id string, source []byte) (int, []byte) {
	return c.Put(fmt.Sprintf("/%s/_doc/%s", index, id), source)
}

// Bulk sends a bulk request.
func (c *TestClient) Bulk(ndjson []byte) (int, []byte) {
	return c.PostNDJSON("/_bulk", ndjson)
}

// Search sends a search request.
func (c *TestClient) Search(index string, body []byte) (int, []byte) {
	return c.Post(fmt.Sprintf("/%s/_search", index), body)
}

// Count sends a count request.
func (c *TestClient) Count(index string, body []byte) (int, []byte) {
	return c.Post(fmt.Sprintf("/%s/_count", index), body)
}

// Refresh flushes an index.
func (c *TestClient) Refresh(index string) (int, []byte) {
	return c.Post(fmt.Sprintf("/%s/_refresh", index), []byte(`{}`))
}

// GetDoc retrieves a document by ID.
func (c *TestClient) GetDoc(index, id string) (int, []byte) {
	return c.Get(fmt.Sprintf("/%s/_doc/%s", index, id))
}

// ParseJSON unmarshals the response body as JSON.
func ParseJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("integration: parse JSON: %v\nbody: %s", err, string(body))
	}
	return m
}

// AssertStatus asserts the HTTP status code.
func AssertStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("integration: expected status %d, got %d", want, got)
	}
}

// Context returns a background context for compatibility.
func Context() context.Context { return context.Background() }
