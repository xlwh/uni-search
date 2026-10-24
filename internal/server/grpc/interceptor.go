// Package grpc — unary and streaming interceptors for tracing and metrics.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.4
package grpc

import (
	"context"
	"time"

	"github.com/unisearch-db/unisearch/internal/observability"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// TracingInterceptor creates a span for each gRPC unary call.
func TracingInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, span := oteltrace.SpanFromContext(ctx).TracerProvider().Tracer("unisearch").Start(ctx, "grpc."+info.FullMethod)
	defer span.End()
	return handler(ctx, req)
}

// MetricsInterceptor records gRPC request duration and count.
func MetricsInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	duration := time.Since(start).Seconds()

	// Record as query metrics (using method as the "index" label for
	// gRPC calls since there's no URL path to parse).
	observability.QueryDuration.WithLabelValues("default", "_grpc", "grpc").Observe(duration)
	if err != nil {
		observability.QueryErrorsTotal.WithLabelValues("default", "_grpc").Inc()
	}
	return resp, err
}

// ChainUnaryInterceptors chains multiple unary interceptors.
func ChainUnaryInterceptors(interceptors ...grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// Build the chain from the end.
		h := handler
		for i := len(interceptors) - 1; i >= 0; i-- {
			if interceptors[i] == nil {
				continue
			}
			h = wrapInterceptor(interceptors[i], info, h)
		}
		return h(ctx, req)
	}
}

// wrapInterceptor wraps a handler with a single interceptor, capturing
// the info at wrap time.
func wrapInterceptor(ic grpc.UnaryServerInterceptor, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) grpc.UnaryHandler {
	return func(ctx context.Context, req any) (any, error) {
		return ic(ctx, req, info, h)
	}
}
