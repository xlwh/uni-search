package esapi

import (
	"net/http"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
)

// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.4

// handleOpenIndex handles POST /{index}/_open.
func (s *Server) handleOpenIndex(w http.ResponseWriter, r *http.Request) {
	index := r.PathValue("index")
	if index == "" {
		writeError(w, apierr.NewIllegalArgument("index name is required"))
		return
	}
	// Check index exists via Indices.
	indices, err := s.engine.Indices(r.Context(), index)
	if err != nil || len(indices) == 0 {
		writeError(w, apierr.NewIndexNotFound(index))
		return
	}
	// M0: open is a no-op (index is always open in embedded mode).
	// When close is implemented, this clears the closed flag.
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

// handleCloseIndex handles POST /{index}/_close.
func (s *Server) handleCloseIndex(w http.ResponseWriter, r *http.Request) {
	index := r.PathValue("index")
	if index == "" {
		writeError(w, apierr.NewIllegalArgument("index name is required"))
		return
	}
	indices, err := s.engine.Indices(r.Context(), index)
	if err != nil || len(indices) == 0 {
		writeError(w, apierr.NewIndexNotFound(index))
		return
	}
	// M0: close is acknowledged but doesn't actually block writes yet.
	// Full close implementation requires modifying indexState in manager.go.
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

// handleIndexStats handles GET /{index}/_stats and GET /_stats.
func (s *Server) handleIndexStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pattern := r.PathValue("index")
	if pattern == "" {
		pattern = "*"
	}

	indices, err := s.engine.Indices(ctx, pattern)
	if err != nil {
		writeError(w, apierr.NewInternal("failed to list indices: "+err.Error()))
		return
	}

	indicesStats := make(map[string]any)
	for _, ii := range indices {
		indicesStats[ii.Index] = map[string]any{
			"uuid": ii.UUID,
			"primaries": map[string]any{
				"docs":  map[string]any{"count": ii.DocsCount, "deleted": ii.DocsDeleted},
				"store": map[string]any{"size_in_bytes": ii.StoreSize},
			},
			"total": map[string]any{
				"docs":  map[string]any{"count": ii.DocsCount, "deleted": ii.DocsDeleted},
				"store": map[string]any{"size_in_bytes": ii.StoreSize},
			},
		}
	}

	writeJSON(w, 0, map[string]any{
		"_shards": map[string]any{"total": 1, "successful": 1, "failed": 0},
		"indices": indicesStats,
	})
}

// handleSegments handles GET /{index}/_segments and GET /_segments.
func (s *Server) handleSegments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pattern := r.PathValue("index")
	if pattern == "" {
		pattern = "*"
	}

	indices, err := s.engine.Indices(ctx, pattern)
	if err != nil {
		writeError(w, apierr.NewInternal("failed to list indices: "+err.Error()))
		return
	}

	indicesResult := make(map[string]any)
	for _, ii := range indices {
		shards := make(map[string]any)
		segments := make(map[string]any)
		segments["0"] = map[string]any{
			"generation":    0,
			"num_docs":      ii.DocsCount,
			"deleted_docs":  ii.DocsDeleted,
			"size_in_bytes": ii.StoreSize,
			"committed":     true,
			"search":        true,
			"version":       1,
		}
		shards["0"] = map[string]any{
			"routing":  map[string]any{"state": "STARTED", "primary": true, "node": "node-1"},
			"segments": segments,
		}
		indicesResult[ii.Index] = map[string]any{"shards": shards}
	}

	writeJSON(w, 0, map[string]any{
		"_shards": map[string]any{"total": 1, "successful": 1, "failed": 0},
		"indices": indicesResult,
	})
}

// handleResolveIndex handles GET /_resolve/index/{name}.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.4.5
func (s *Server) handleResolveIndex(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, apierr.NewIllegalArgument("index name is required"))
		return
	}
	indices, err := s.engine.Indices(r.Context(), name)
	if err != nil {
		writeError(w, apierr.NewInternal("failed to resolve index: "+err.Error()))
		return
	}
	var idxNames []string
	for _, ii := range indices {
		idxNames = append(idxNames, ii.Index)
	}
	aliases, _ := s.engine.GetAliases(r.Context(), name)
	aliasResult := make(map[string]any)
	if len(aliases) > 0 {
		for aliasName, idxList := range aliases {
			aliasResult[aliasName] = idxList
		}
	}
	writeJSON(w, 0, map[string]any{
		"indices":      idxNames,
		"aliases":      aliasResult,
		"data_streams": []string{},
	})
}

// handleReloadSearchAnalyzers handles POST /{index}/_reload_search_analyzers.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.4.6
func (s *Server) handleReloadSearchAnalyzers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, map[string]any{
		"_shards":                   map[string]any{"total": 1, "successful": 1, "failed": 0},
		"reloaded_search_analyzers": true,
	})
}

// handleShrinkIndex handles POST /{index}/_shrink/{target}.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.4.7
func (s *Server) handleShrinkIndex(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("index")
	target := r.PathValue("target")
	if source == "" || target == "" {
		writeError(w, apierr.NewIllegalArgument("source index and target index are required"))
		return
	}
	// P2: acknowledge shrink (embedded single-shard mode: no-op).
	writeJSON(w, 0, map[string]any{
		"acknowledged":        true,
		"shards_acknowledged": true,
		"index":               target,
	})
}

// handleSplitIndex handles POST /{index}/_split/{target}.
func (s *Server) handleSplitIndex(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("index")
	target := r.PathValue("target")
	if source == "" || target == "" {
		writeError(w, apierr.NewIllegalArgument("source index and target index are required"))
		return
	}
	writeJSON(w, 0, map[string]any{
		"acknowledged":        true,
		"shards_acknowledged": true,
		"index":               target,
	})
}

// handleCloneIndex handles POST /{index}/_clone/{target}.
func (s *Server) handleCloneIndex(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("index")
	target := r.PathValue("target")
	if source == "" || target == "" {
		writeError(w, apierr.NewIllegalArgument("source index and target index are required"))
		return
	}
	writeJSON(w, 0, map[string]any{
		"acknowledged":        true,
		"shards_acknowledged": true,
		"index":               target,
	})
}
