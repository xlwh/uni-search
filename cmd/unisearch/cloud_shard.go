package main

import (
	"context"
	"encoding/json"
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
	"github.com/unisearch-db/unisearch/internal/cloud"
	"github.com/unisearch-db/unisearch/internal/config"
	"github.com/unisearch-db/unisearch/internal/embedded"
	"github.com/unisearch-db/unisearch/internal/objstore"
	"github.com/unisearch-db/unisearch/internal/observability"
	grpcsrv "github.com/unisearch-db/unisearch/internal/server/grpc"
	"google.golang.org/grpc"
)

// runShard boots a cloud-mode shard node: embedded engine for local
// storage, gRPC ShardService for data serving, HTTP health check, and
// meta cluster registration.
func runShard(cfg *config.Config) error {
	slog.Info("starting shard node",
		"node_id", cfg.Cloud.NodeID,
		"listen", cfg.Listen,
		"grpc_addr", cfg.Cloud.GrpcAddr,
		"advertise_addr", cfg.Cloud.AdvertiseAddr,
	)

	// --- Step 1: Initialize embedded engine ---
	engine, err := embedded.New(cfg.Data.Dir)
	if err != nil {
		return fmt.Errorf("shard: open engine: %w", err)
	}
	defer func() { _ = engine.Close() }()

	// --- Step 2: OTel tracing ---
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

	// --- Step 3: Initialize S3 client (MinIO) ---
	if cfg.Cloud.ObjStore.Endpoint != "" {
		s3Cfg := objstore.S3Config{
			Endpoint:  cfg.Cloud.ObjStore.Endpoint,
			AccessKey: cfg.Cloud.ObjStore.AccessKey,
			SecretKey: cfg.Cloud.ObjStore.SecretKey,
			Bucket:    cfg.Cloud.ObjStore.Bucket,
			UseSSL:    cfg.Cloud.ObjStore.UseSSL,
			Region:    cfg.Cloud.ObjStore.Region,
		}
		s3Client, err := objstore.NewMinioS3Client(s3Cfg)
		if err != nil {
			slog.Warn("S3 client init failed, continuing with local storage only", "err", err)
		} else {
			slog.Info("S3 client initialized",
				"endpoint", s3Cfg.Endpoint,
				"bucket", s3Cfg.Bucket,
			)
			_ = s3Client // Reserved for future WAL/segment offload
		}
	}

	// --- Step 4: Create gRPC ShardService server ---
	grpcAddr := cfg.Cloud.GrpcAddr
	if grpcAddr == "" {
		grpcAddr = ":9300"
	}
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("shard: grpc listen %s: %w", grpcAddr, err)
	}
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(grpcsrv.ChainUnaryInterceptors(
			grpcsrv.TracingInterceptor,
			grpcsrv.MetricsInterceptor,
		)),
	)
	gen.RegisterShardServiceServer(grpcServer, grpcsrv.NewShardServer(engine))

	go func() {
		slog.Info("shard grpc listening", "addr", grpcAddr)
		if err := grpcServer.Serve(grpcLis); err != nil {
			slog.Error("grpc serve error", "err", err)
		}
	}()
	defer grpcServer.GracefulStop()

	// --- Step 5: Create HTTP health check server ---
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("GET /_cluster/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cluster_name": "unisearch",
			"status":       "green",
			"node_id":      cfg.Cloud.NodeID,
		})
	})
	httpMux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":    cfg.Cloud.NodeID,
			"role":    "shard",
			"tagline": "You Know, for Search",
		})
	})

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpMux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// --- Step 6: Register with meta cluster ---
	advertiseAddr := cfg.Cloud.AdvertiseAddr
	if advertiseAddr == "" {
		advertiseAddr = cfg.Cloud.NodeID + ":9300"
	}
	metaClient := cloud.NewMetaHTTPClient(cfg.Cloud.MetaAddrs, cfg.Cloud.NodeID)

	// Retry registration with backoff.
	registered := false
	for i := 0; i < 10; i++ {
		if err := metaClient.Register(cfg.Cloud.NodeID, advertiseAddr, "shard"); err != nil {
			slog.Warn("meta registration failed, retrying", "attempt", i+1, "err", err)
			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}
		registered = true
		slog.Info("registered with meta cluster", "node_id", cfg.Cloud.NodeID, "addr", advertiseAddr)
		break
	}
	if !registered {
		slog.Warn("failed to register with meta cluster after retries, continuing")
	}

	// Start heartbeat loop.
	heartbeatDone := make(chan struct{})
	go metaClient.StartHeartbeatLoop(heartbeatDone, 5*time.Second)

	// --- Step 7: Start HTTP server and signal-driven shutdown ---
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("shard http listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		close(heartbeatDone)
		return err
	case sig := <-stop:
		slog.Info("shutdown signal received", "signal", sig)
	}

	close(heartbeatDone)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("http shutdown error", "err", err)
	}
	slog.Info("flushing engine")
	if err := engine.Close(); err != nil {
		slog.Error("flush engine error", "err", err)
	}
	slog.Info("shutdown complete")
	return nil
}
