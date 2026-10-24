// Package observability — HTTP middleware for metrics and tracing.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.1
package observability

import (
	"net/http"
	"strings"
	"time"
)

// statusWriter captures the response status code for metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// WithMetrics is an HTTP middleware that records query duration,
// request count, and error count. It skips /_prometheus/metrics
// to avoid self-instrumentation.
func WithMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip metrics endpoint to avoid self-instrumentation.
		if r.URL.Path == "/_prometheus/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		duration := time.Since(start).Seconds()

		index := extractIndex(r.URL.Path)
		QueryRequestsTotal.WithLabelValues("default", index, "http").Inc()
		QueryDuration.WithLabelValues("default", index, "http").Observe(duration)
		if sw.status >= 400 {
			QueryErrorsTotal.WithLabelValues("default", index, "http").Inc()
		}
	})
}

// WithTracing is an HTTP middleware that creates a top-level span for
// each request. The span is propagated through the request context.
func WithTracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip metrics endpoint.
		if r.URL.Path == "/_prometheus/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		ctx, span := StartHTTPSpan(r.Context(), r.Method, r.URL.Path)
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractIndex parses the index name from the URL path.
// e.g. "/my-index/_search" → "my-index", "/_search" → "_all".
func extractIndex(path string) string {
	path = strings.TrimPrefix(path, "/")
	if path == "" || strings.HasPrefix(path, "_") {
		return "_all"
	}
	// Index is the first path segment before any /_action.
	parts := strings.SplitN(path, "/", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return "_all"
}
