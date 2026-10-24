package esapi

import (
	"net/http"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
)

// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.4

// handleValidateQuery handles GET/POST /{index}/_validate/query.
func (s *Server) handleValidateQuery(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil || len(body) == 0 {
		writeJSON(w, 0, map[string]any{"valid": false, "error": "empty request body"})
		return
	}
	_, _, _, _, perr := dsl.ParseSearchBodyFull(body)
	if perr != nil {
		writeJSON(w, 0, map[string]any{"valid": false, "error": perr.Error()})
		return
	}
	writeJSON(w, 0, map[string]any{"valid": true})
}

// handleSearchShards handles GET/POST /_search_shards and /{index}/_search_shards.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.7
func (s *Server) handleSearchShards(w http.ResponseWriter, r *http.Request) {
	pattern := r.PathValue("index")
	if pattern == "" {
		pattern = "*"
	}
	indices, err := s.engine.Indices(r.Context(), pattern)
	if err != nil {
		writeJSON(w, 0, map[string]any{"nodes": map[string]any{}, "shards": []any{}})
		return
	}
	var shards []any
	for _, ii := range indices {
		shards = append(shards, map[string]any{
			"index":           ii.Index,
			"shard":           0,
			"primary":         true,
			"node":            "node-1",
			"state":           "STARTED",
			"relocating_node": nil,
		})
	}
	writeJSON(w, 0, map[string]any{
		"nodes": map[string]any{
			"node-1": map[string]any{
				"name":              "node-1",
				"transport_address": "127.0.0.1:9300",
			},
		},
		"shards": shards,
	})
}
