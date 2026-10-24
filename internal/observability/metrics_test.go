// Package observability metrics_test.go tests Prometheus metrics.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.1
package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMetricsHandler(t *testing.T) {
	handler := MetricsHandler()
	assert.NotNil(t, handler)

	// Verify it serves Prometheus exposition format
	req := httptest.NewRequest("GET", "/_prometheus/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// Response should contain Prometheus metrics (go_* are always
	// present from the Go collector; unisearch_* metrics appear only
	// after first use with label values since they're label vectors).
	body := w.Body.String()
	assert.Contains(t, body, "go_goroutines") // Go runtime metric
}

func TestMetricsRegistration(t *testing.T) {
	// Verify unisearch metrics are registered by using them once.
	// QueryRequestsTotal has 3 labels: tenant, index, phase.
	QueryRequestsTotal.WithLabelValues("test", "test-idx", "search").Inc()
	// WriteDocsTotal has 3 labels: tenant, index, shard.
	WriteDocsTotal.WithLabelValues("test", "test-idx", "0").Inc()

	handler := MetricsHandler()
	req := httptest.NewRequest("GET", "/_prometheus/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	assert.Contains(t, body, "unisearch_query_requests_total")
	assert.Contains(t, body, "unisearch_write_docs_total")
}

func TestWithMetrics(t *testing.T) {
	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := WithMetrics(inner)
	req := httptest.NewRequest("GET", "/test-index/_search", nil)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	assert.True(t, innerCalled)
}

func TestWithMetricsSkipsMetricsEndpoint(t *testing.T) {
	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
	})

	mw := WithMetrics(inner)
	req := httptest.NewRequest("GET", "/_prometheus/metrics", nil)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	// Inner handler should still be called (metrics endpoint serves normally)
	assert.True(t, innerCalled)
}

func TestExtractIndex(t *testing.T) {
	assert.Equal(t, "my-index", extractIndex("/my-index/_search"))
	assert.Equal(t, "my-index", extractIndex("/my-index/_doc/1"))
	assert.Equal(t, "_all", extractIndex("/_search"))
	assert.Equal(t, "_all", extractIndex("/_cluster/health"))
	assert.Equal(t, "_all", extractIndex("/"))
}
