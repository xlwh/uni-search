// Package main server.go assembles the embedded-mode HTTP server:
// meta + embedded engine + esapi router + http.Server with a
// graceful-shutdown signal handler (design 00 §5).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	gen "github.com/unisearch-db/unisearch/api/proto/gen"
	"github.com/unisearch-db/unisearch/internal/config"
	"github.com/unisearch-db/unisearch/internal/embedded"
	"github.com/unisearch-db/unisearch/internal/observability"
	"github.com/unisearch-db/unisearch/internal/server/admin"
	grpcsrv "github.com/unisearch-db/unisearch/internal/server/grpc"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
	"github.com/unisearch-db/unisearch/internal/tenant"
	"google.golang.org/grpc"
)

// runEmbedded boots the full single-node stack: meta store, in-
// memory document store, REST router, HTTP listener, and a signal-
// driven graceful shutdown. Returns the process exit code.
func runEmbedded(cfg *config.Config) error {
	slog.Info("opening embedded engine", "data_dir", cfg.Data.Dir)
	engine, err := embedded.New(cfg.Data.Dir)
	if err != nil {
		return fmt.Errorf("open engine: %w", err)
	}
	defer func() { _ = engine.Close() }()

	// OTel tracing initialization (design 06 §2). If the collector is
	// unreachable, tracing degrades to no-op (business unaffected).
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "localhost:4317"
	}
	tracingShutdown, err := observability.InitTracing(context.Background(), otelEndpoint)
	if err != nil {
		slog.Warn("tracing init failed, degrading to no-op", "err", err)
	} else {
		defer tracingShutdown()
		slog.Info("tracing initialized", "endpoint", otelEndpoint)
	}

	srv := esapi.NewServer(engine)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Tenant manager (multi-tenancy quota control).
	tenantMgr := tenant.NewManager()
	defer tenantMgr.Close()

	// Admin web console server (separate port, e.g. :9201).
	var adminSrv *http.Server
	if cfg.Admin.Enabled {
		adminHandler, err := admin.NewAdminServer(engine, tenantMgr, cfg.Listen)
		if err != nil {
			slog.Warn("admin server init failed, skipping admin console", "err", err)
		} else {
			adminSrv = &http.Server{
				Addr:              cfg.Admin.Listen,
				Handler:           adminHandler,
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       60 * time.Second,
			}
			go func() {
				slog.Info("admin console listening", "addr", cfg.Admin.Listen)
				if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Error("admin console serve error", "err", err)
				}
			}()
		}
	}

	// gRPC server (design 05 §7): same-process dual-protocol on a
	// separate port. Default :9300.
	grpcAddr := os.Getenv("UNISEARCH_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":9300"
	}
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		slog.Warn("grpc listener bind failed, skipping gRPC", "addr", grpcAddr, "err", err)
	} else {
		grpcServer := grpc.NewServer(
			grpc.UnaryInterceptor(grpcsrv.ChainUnaryInterceptors(
				grpcsrv.TracingInterceptor,
				grpcsrv.MetricsInterceptor,
			)),
		)
		gen.RegisterShardServiceServer(grpcServer, grpcsrv.NewShardServer(engine))
		gen.RegisterMetaServiceServer(grpcServer, grpcsrv.NewMetaServer())
		go func() {
			slog.Info("grpc listening", "addr", grpcAddr)
			if err := grpcServer.Serve(grpcLis); err != nil {
				slog.Error("grpc serve error", "err", err)
			}
		}()
		defer grpcServer.GracefulStop()
	}

	// Signal-driven graceful shutdown (design 00 §5).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("http listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		slog.Info("shutdown signal received", "signal", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if adminSrv != nil {
		if err := adminSrv.Shutdown(ctx); err != nil {
			slog.Error("admin shutdown error", "err", err)
		}
	}
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("http shutdown error", "err", err)
	}
	slog.Info("flushing docs")
	if err := engine.Close(); err != nil {
		slog.Error("flush docs error", "err", err)
	}
	slog.Info("shutdown complete")
	return nil
}
