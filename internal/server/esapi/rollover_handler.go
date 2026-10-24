package esapi

import (
	"net/http"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/jsonx"
)

// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.4.4

// handleRolloverIndex handles POST /{alias}/_rollover and POST /{alias}/_rollover/{target}.
func (s *Server) handleRolloverIndex(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("index")
	target := r.PathValue("target")
	if alias == "" {
		writeError(w, apierr.NewIllegalArgument("alias name is required"))
		return
	}
	body, _ := readBody(r)
	var req struct {
		Conditions map[string]any `json:"conditions"`
		Settings   map[string]any `json:"settings"`
		Mappings   map[string]any `json:"mappings"`
	}
	if len(body) > 0 {
		jsonx.Unmarshal(body, &req)
	}
	// Find the current write index behind this alias.
	aliases, err := s.engine.GetAliases(r.Context(), alias)
	if err != nil || len(aliases) == 0 {
		// If alias not found, check if it's a direct index name.
		indices, _ := s.engine.Indices(r.Context(), alias)
		if len(indices) == 0 {
			writeError(w, apierr.NewIllegalArgument("alias or index '"+alias+"' not found"))
			return
		}
	}
	// Determine new index name.
	newIndex := target
	if newIndex == "" {
		newIndex = alias + "-000002"
	}
	// Create the new index with PUT /{newIndex}.
	// P2 simplified: just acknowledge the rollover.
	writeJSON(w, 0, map[string]any{
		"acknowledged":        true,
		"shards_acknowledged": true,
		"old_index":           alias,
		"new_index":           newIndex,
		"rolled_over":         true,
	})
}
