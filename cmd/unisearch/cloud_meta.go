package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/unisearch-db/unisearch/internal/cloud"
	"github.com/unisearch-db/unisearch/internal/config"
	"github.com/unisearch-db/unisearch/internal/observability"
	"github.com/unisearch-db/unisearch/internal/raft"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// runMeta boots a cloud-mode meta node: Raft cluster for metadata
// consensus, CoordinatorEngine for ES API routing, and internal HTTP
// endpoints for shard node registration/heartbeat.
func runMeta(cfg *config.Config) error {
	slog.Info("starting meta node",
		"node_id", cfg.Cloud.NodeID,
		"raft_addr", cfg.Cloud.RaftAddr,
		"listen", cfg.Listen,
	)

	// --- Step 1: Initialize Raft cluster ---
	raftCfg := raft.RaftClusterConfig{
		NodeID:   cfg.Cloud.NodeID,
		DataDir:  cfg.Data.Dir,
		RaftAddr: cfg.Cloud.RaftAddr,
		Peers:    cfg.Cloud.RaftPeers,
	}
	cluster, err := raft.NewRaftCluster(raftCfg)
	if err != nil {
		return fmt.Errorf("meta: init raft: %w", err)
	}

	// Wait for leader election.
	slog.Info("waiting for raft leader election...")
	ready := false
	for i := 0; i < 60; i++ {
		if cluster.IsReady() {
			ready = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		slog.Warn("raft cluster not ready after 30s, continuing anyway")
	}
	slog.Info("raft cluster ready",
		"is_leader", cluster.IsLeader(),
		"leader", cluster.Leader(),
	)

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

	// --- Step 3: Create CoordinatorEngine ---
	coordEngine := cloud.NewCoordinatorEngine(cluster)
	defer func() { _ = coordEngine.Close() }()

	// --- Step 4: Create ES API server ---
	esapiServer := esapi.NewServer(coordEngine)

	// --- Step 5: Create outer mux with internal endpoints ---
	outerMux := http.NewServeMux()
	// Internal endpoints take precedence over the ES API fallback.
	outerMux.HandleFunc("POST /_internal/register", func(w http.ResponseWriter, r *http.Request) {
		handleInternalRegister(w, r, cluster)
	})
	outerMux.HandleFunc("GET /_internal/routes", func(w http.ResponseWriter, r *http.Request) {
		handleInternalRoutes(w, r, cluster)
	})
	outerMux.HandleFunc("POST /_internal/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		handleInternalHeartbeat(w, r, cluster)
	})
	// Fallback: all other requests go to the ES API server.
	outerMux.Handle("/", esapiServer)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           outerMux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// --- Step 6: Start HTTP server ---
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("meta http listening", "addr", cfg.Listen)
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
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("http shutdown error", "err", err)
	}
	slog.Info("closing raft cluster")
	_ = cluster.Close()
	slog.Info("shutdown complete")
	return nil
}

// handleInternalRegister processes shard node registration requests.
func handleInternalRegister(w http.ResponseWriter, r *http.Request, cluster *raft.RaftCluster) {
	var body struct {
		ID   string `json:"id"`
		Addr string `json:"addr"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := cluster.Apply(raft.CmdRegisterNode, raft.NodeInfo{
		ID:       raft.NodeID(body.ID),
		Addr:     body.Addr,
		Role:     body.Role,
		LastSeen: time.Now().Unix(),
	})
	if err != nil {
		slog.Warn("register node failed", "id", body.ID, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("shard node registered", "id", body.ID, "addr", body.Addr, "role", body.Role)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
}

// handleInternalRoutes returns the current cluster routing table.
func handleInternalRoutes(w http.ResponseWriter, r *http.Request, cluster *raft.RaftCluster) {
	members := cluster.GetMembers()
	memberList := make([]map[string]any, len(members))
	for i, m := range members {
		memberList[i] = map[string]any{
			"id":        string(m.ID),
			"addr":      m.Addr,
			"role":      m.Role,
			"last_seen": m.LastSeen,
		}
	}

	routes := cluster.ListRoutes()
	routeList := make([]map[string]any, len(routes))
	for i, rt := range routes {
		rg := make([]string, len(rt.RaftGroup))
		for j, n := range rt.RaftGroup {
			rg[j] = string(n)
		}
		routeList[i] = map[string]any{
			"shard_id":   uint64(rt.ShardID),
			"index_id":   uint32(rt.IndexID),
			"raft_group": rg,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"members":   memberList,
		"routes":    routeList,
		"leader":    cluster.Leader(),
		"is_leader": cluster.IsLeader(),
	})
}

// handleInternalHeartbeat processes shard node heartbeats.
func handleInternalHeartbeat(w http.ResponseWriter, r *http.Request, cluster *raft.RaftCluster) {
	var body struct {
		ID       string `json:"id"`
		LastSeen int64  `json:"last_seen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.LastSeen == 0 {
		body.LastSeen = time.Now().Unix()
	}
	// Find the member and update LastSeen via re-registration.
	members := cluster.GetMembers()
	for _, m := range members {
		if string(m.ID) == body.ID {
			m.LastSeen = body.LastSeen
			_ = cluster.Apply(raft.CmdRegisterNode, m)
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"acknowledged": true,
		"leader_addr":  cluster.Leader(),
	})
}
