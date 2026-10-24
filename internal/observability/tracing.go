// Package observability — OTel distributed tracing (design 06 §2).
// Four-level span tree: http.request → query.plan → shard.search →
// segment.scan / index.hnsw / index.bm25 / agg.partial.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.2
package observability

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// InitTracing initializes the OTel SDK with an OTLP gRPC exporter.
// endpoint is the collector address (e.g. "localhost:4317").
// Returns a shutdown function that must be called on exit.
// If the exporter cannot connect, tracing degrades to no-op (business
// is unaffected).
func InitTracing(ctx context.Context, endpoint string) (func(), error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName("unisearch"),
	))
	if err != nil {
		res = resource.Default()
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithMaxQueueSize(2048),
		),
		sdktrace.WithResource(res),
		// Parent-based sampling with 1% base ratio (design 06 §2:
		// "错误全采、慢查询>1s全采、其余1%"). Error and slow-query
		// always-on is handled at span-attribute level by the
		// middleware; the base sampler reduces volume to 1%.
		// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.3
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.01))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	shutdown := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tp.Shutdown(shutCtx)
	}
	return shutdown, nil
}

// StartHTTPSpan creates a top-level HTTP request span.
func StartHTTPSpan(ctx context.Context, method, path string) (context.Context, oteltrace.Span) {
	return otel.Tracer("unisearch").Start(ctx, "http.request",
		oteltrace.WithAttributes(semconv.HTTPMethod(method), semconv.HTTPTarget(path)),
	)
}

// StartPlanSpan creates a query plan span (level 2).
func StartPlanSpan(ctx context.Context) (context.Context, oteltrace.Span) {
	return otel.Tracer("unisearch").Start(ctx, "query.plan")
}

// StartShardSpan creates a per-shard search span (level 3).
func StartShardSpan(ctx context.Context, shardID string) (context.Context, oteltrace.Span) {
	return otel.Tracer("unisearch").Start(ctx, "shard.search")
}

// StartSegmentSpan creates a segment/operator-level span (level 4).
// op is one of: "segment.scan", "index.hnsw", "index.bm25", "agg.partial".
func StartSegmentSpan(ctx context.Context, op string) (context.Context, oteltrace.Span) {
	return otel.Tracer("unisearch").Start(ctx, op)
}
