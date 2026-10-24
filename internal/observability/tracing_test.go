// Package observability tracing_test.go tests OTel tracing helpers.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.2
package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStartHTTPSpan(t *testing.T) {
	ctx, span := StartHTTPSpan(context.Background(), "GET", "/test/_search")
	assert.NotNil(t, span)
	assert.NotNil(t, ctx)
	span.End()
}

func TestStartPlanSpan(t *testing.T) {
	ctx, span := StartPlanSpan(context.Background())
	assert.NotNil(t, span)
	assert.NotNil(t, ctx)
	span.End()
}

func TestStartShardSpan(t *testing.T) {
	ctx, span := StartShardSpan(context.Background(), "shard-1")
	assert.NotNil(t, span)
	assert.NotNil(t, ctx)
	span.End()
}

func TestStartSegmentSpan(t *testing.T) {
	ops := []string{"segment.scan", "index.hnsw", "index.bm25", "agg.partial"}
	for _, op := range ops {
		ctx, span := StartSegmentSpan(context.Background(), op)
		assert.NotNil(t, span)
		assert.NotNil(t, ctx)
		span.End()
	}
}

func TestWithTracing(t *testing.T) {
	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
	})

	mw := WithTracing(inner)
	req := httptest.NewRequest("GET", "/test/_search", nil)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	assert.True(t, innerCalled)
}
