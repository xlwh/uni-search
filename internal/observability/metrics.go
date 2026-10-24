// Package observability provides Prometheus metrics and OpenTelemetry
// tracing for UniSearch (design 06 §1/§2). Metrics cover five groups:
// write, query, segments, index, and tenant quota.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.1
package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Write group metrics.
var (
	WriteDocsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "unisearch_write_docs_total",
		Help: "Total documents written.",
	}, []string{"tenant", "index", "shard"})

	WriteBatchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "unisearch_write_batch_duration_seconds",
		Help:    "Write batch duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"tenant", "index", "shard"}) // @spec: deep-gap-remediation 06§1

	WriteRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "unisearch_write_rejected_total",
		Help: "Write requests rejected by backpressure.",
	}, []string{"tenant", "index", "shard"}) // @spec: deep-gap-remediation 06§1
)

// Query group metrics.
var (
	QueryRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "unisearch_query_requests_total",
		Help: "Total query requests.",
	}, []string{"tenant", "index", "phase"}) // @spec: deep-gap-remediation 06§1

	QueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "unisearch_query_duration_seconds",
		Help:    "Query duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"tenant", "index", "phase"})

	QueryRowsScanned = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "unisearch_query_rows_scanned_total",
		Help:    "Rows scanned per query.",
		Buckets: prometheus.ExponentialBuckets(10, 10, 6),
	}, []string{"tenant", "index", "phase"}) // @spec: deep-gap-remediation 06§1

	QueryErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "unisearch_query_errors_total",
		Help: "Query errors total.",
	}, []string{"tenant", "index", "phase"}) // @spec: deep-gap-remediation 06§1
)

// Segment group metrics.
var (
	SegmentsOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "unisearch_segments_open",
		Help: "Open segments count.",
	}, []string{"index", "shard", "tier"})

	FlushDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "unisearch_flush_duration_seconds",
		Help:    "Flush duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"index", "shard", "tier"}) // @spec: deep-gap-remediation 06§1

	MergeRunning = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "unisearch_merge_running",
		Help: "Running merge count.",
	}, []string{"index", "shard", "tier"}) // @spec: deep-gap-remediation 06§1

	MergeBytesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "unisearch_merge_bytes_total",
		Help: "Total bytes merged.",
	}, []string{"index", "shard", "tier"}) // @spec: deep-gap-remediation 06§1

	DeletesPending = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "unisearch_deletes_pending",
		Help: "Pending delete bitmap cardinality.",
	}, []string{"index", "shard", "tier"}) // @spec: deep-gap-remediation 06§1
)

// Index group metrics.
var (
	HNSWSearchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "unisearch_hnsw_search_duration_seconds",
		Help:    "HNSW search duration.",
		Buckets: prometheus.DefBuckets,
	}, []string{"field"})

	HNSWNodesVisited = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "unisearch_hnsw_nodes_visited",
		Help:    "HNSW nodes visited per query.",
		Buckets: prometheus.ExponentialBuckets(10, 2, 10),
	}, []string{"field"})

	BM25WANDPrunedRatio = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "unisearch_bm25_wand_pruned_ratio",
		Help:    "WAND pruning ratio (0=none pruned, 1=all pruned).",
		Buckets: prometheus.LinearBuckets(0, 0.1, 11),
	}, []string{"field"})
)

// Tenant quota group metrics.
var (
	TenantQuotaUsageRatio = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "unisearch_tenant_quota_usage_ratio",
		Help: "Tenant quota usage ratio (0-1).",
	}, []string{"tenant", "dim"})

	TenantThrottledTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "unisearch_tenant_throttled_total",
		Help: "Tenant throttled events total.",
	}, []string{"tenant", "dim"})
)

// MetricsHandler returns the Prometheus exposition HTTP handler.
// Register at GET /_prometheus/metrics (design 06 §1).
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}
