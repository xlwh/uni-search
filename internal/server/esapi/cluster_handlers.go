package esapi

import (
	"net/http"
	"runtime"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
)

// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.5

// handleClusterState handles GET /_cluster/state and GET /_cluster/state/{metric}.
func (s *Server) handleClusterState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	health, err := s.engine.Health(ctx)
	if err != nil {
		writeError(w, apierr.NewInternal("failed to get cluster health: "+err.Error()))
		return
	}

	// Build routing table from indices.
	indices, err := s.engine.Indices(ctx, "*")
	if err != nil {
		writeError(w, apierr.NewInternal("failed to list indices: "+err.Error()))
		return
	}

	routingTable := make(map[string]any)
	metadata := make(map[string]any)
	for _, ii := range indices {
		routingTable[ii.Index] = map[string]any{
			"shards": map[string]any{
				"0": []map[string]any{{
					"state":   "STARTED",
					"primary": true,
					"node":    "node-1",
					"index":   ii.Index,
					"shard":   0,
				}},
			},
		}
		metadata[ii.Index] = map[string]any{
			"version": 1,
			"settings": map[string]any{
				"index": map[string]any{
					"number_of_shards":   "1",
					"number_of_replicas": "0",
				},
			},
		}
	}

	writeJSON(w, 0, map[string]any{
		"cluster_name":  health.ClusterName,
		"cluster_uuid":  "unisearch-uuid",
		"version":       1,
		"state_uuid":    "state-1",
		"master_node":   "node-1",
		"blocks":        map[string]any{},
		"nodes": map[string]any{
			"node-1": map[string]any{
				"name":             "node-1",
				"transport_address": "127.0.0.1:9300",
				"roles":             []string{"master", "data", "ingest"},
			},
		},
		"routing_table": map[string]any{"indices": routingTable},
		"metadata":      map[string]any{"indices": metadata},
	})
}

// handleClusterStats handles GET /_cluster/stats.
func (s *Server) handleClusterStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	indices, err := s.engine.Indices(ctx, "*")
	if err != nil {
		writeError(w, apierr.NewInternal("failed to list indices: "+err.Error()))
		return
	}

	var totalDocs int64
	var totalDeleted int64
	for _, ii := range indices {
		totalDocs += ii.DocsCount
		totalDeleted += ii.DocsDeleted
	}

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	writeJSON(w, 0, map[string]any{
		"_nodes": map[string]any{"total": 1, "successful": 1, "failed": 0},
		"cluster_name": "unisearch",
		"status":       "green",
		"indices": map[string]any{
			"count":  len(indices),
			"docs":   map[string]any{"count": totalDocs, "deleted": totalDeleted},
			"shards": map[string]any{"total": len(indices), "primaries": len(indices)},
		},
		"nodes": map[string]any{
			"count":     1,
			"versions":  []string{"8.12.0"},
			"os":        map[string]any{"name": runtime.GOOS, "arch": runtime.GOARCH, "available_processors": runtime.NumCPU()},
			"process":   map[string]any{"cpu": map[string]any{"percent": 0}},
			"jvm":       map[string]any{"mem": map[string]any{"heap_used_in_bytes": memStats.Alloc, "heap_max_in_bytes": memStats.Sys}},
			"fs":        map[string]any{"total_in_bytes": 0},
		},
	})
}

// handleNodesStats handles GET /_nodes/stats.
func (s *Server) handleNodesStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	indices, err := s.engine.Indices(ctx, "*")
	if err != nil {
		writeError(w, apierr.NewInternal("failed to list indices: "+err.Error()))
		return
	}

	var totalDocs int64
	for _, ii := range indices {
		totalDocs += ii.DocsCount
	}

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	writeJSON(w, 0, map[string]any{
		"_nodes":       map[string]any{"total": 1, "successful": 1, "failed": 0},
		"cluster_name":  "unisearch",
		"nodes": map[string]any{
			"node-1": map[string]any{
				"timestamp":         0,
				"name":              "node-1",
				"transport_address": "127.0.0.1:9300",
				"host":              "127.0.0.1",
				"ip":                "127.0.0.1",
				"roles":             []string{"master", "data", "ingest"},
				"indices": map[string]any{
					"docs":   map[string]any{"count": totalDocs, "deleted": 0},
					"shards": map[string]any{"total": len(indices)},
				},
				"os": map[string]any{
					"cpu":    map[string]any{"percent": 0},
					"mem":    map[string]any{"total_in_bytes": memStats.Sys, "used_in_bytes": memStats.Alloc},
				},
				"process": map[string]any{"cpu": map[string]any{"percent": 0}},
				"jvm":     map[string]any{"mem": map[string]any{"heap_used_in_bytes": memStats.Alloc}},
				"thread_pool": map[string]any{},
			},
		},
	})
}

// handleNodesInfo handles GET /_nodes/info and GET /_nodes.
func (s *Server) handleNodesInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, map[string]any{
		"_nodes":       map[string]any{"total": 1, "successful": 1, "failed": 0},
		"cluster_name":  "unisearch",
		"nodes": map[string]any{
			"node-1": map[string]any{
				"name":              "node-1",
				"transport_address": "127.0.0.1:9300",
				"host":              "127.0.0.1",
				"ip":                "127.0.0.1",
				"version":           "8.12.0",
				"build_flavor":      "default",
				"roles":             []string{"master", "data", "ingest"},
				"os": map[string]any{
					"name":                runtime.GOOS,
					"arch":                runtime.GOARCH,
					"available_processors": runtime.NumCPU(),
				},
				"jvm": map[string]any{
					"version":  "go-runtime",
					"vm_name":  "go",
				},
				"process": map[string]any{"id": 1},
			},
		},
	})
}

// handleGetClusterSettings handles GET /_cluster/settings.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.5.5
func (s *Server) handleGetClusterSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, map[string]any{
		"persistent": map[string]any{},
		"transient":  map[string]any{},
		"defaults":   map[string]any{},
	})
}

// handlePutClusterSettings handles PUT /_cluster/settings.
func (s *Server) handlePutClusterSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, map[string]any{
		"acknowledged": true,
		"persistent":   map[string]any{},
		"transient":    map[string]any{},
	})
}

// P3 cluster handlers (es-feature-gap-fill)
func (s *Server) handleNodesUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, map[string]any{
		"_nodes":       map[string]any{"total": 1, "successful": 1, "failed": 0},
		"cluster_name":  "unisearch",
		"nodes": map[string]any{
			"node-1": map[string]any{
				"timestamp": 0,
				"rest_actions": []map[string]any{},
			},
		},
	})
}

func (s *Server) handleNodesCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, map[string]any{
		"_nodes":       map[string]any{"total": 1, "successful": 1, "failed": 0},
		"capabilities": map[string]any{
			"rest_actions":         map[string]any{"metrics": map[string]any{"_all": true}},
			"metric_descriptors":   map[string]any{},
		},
	})
}

func (s *Server) handleRemoteInfo(w http.ResponseWriter, r *http.Request) {
	// P3: no remote clusters in embedded mode.
	writeJSON(w, 0, map[string]any{})
}
