// Package esapi percolate_handler.go implements the _percolate endpoint
// (ES 1.x-style convenience API): it matches the submitted document
// against the percolator queries registered in the index (documents
// carrying a "query" field) and returns the matching query IDs.
// Internally it rewrites the request into a search with a percolate
// query so both endpoints share one implementation.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#percolate
package esapi

import (
	"net/http"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/jsonx"
)

// handlePercolate implements POST /{index}/_percolate with body
// {"doc": {...}} (optionally "field": "query" to select the stored
// percolator field name, ES default "query").
func (s *Server) handlePercolate(w http.ResponseWriter, r *http.Request) {
	index := r.PathValue("index")
	if index == "" {
		writeError(w, apierr.NewIllegalArgument("_percolate requires an index"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("failed to read body: "+err.Error()))
		return
	}

	// Rewrite {"doc": {...}} into a search body with a percolate query.
	var req struct {
		Doc   map[string]any `json:"doc"`
		Field string         `json:"field"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil || req.Doc == nil {
		writeError(w, apierr.NewIllegalArgument(`_percolate requires a "doc" object`))
		return
	}
	field := req.Field
	if field == "" {
		field = "query"
	}
	searchBody := map[string]any{
		"query": map[string]any{
			"percolate": map[string]any{
				"field":    field,
				"document": req.Doc,
			},
		},
		"size": 100,
	}
	sb, err := jsonx.Marshal(searchBody)
	if err != nil {
		writeError(w, err)
		return
	}

	res, err := s.engine.Search(ctxFromReq(r), []string{index}, sb)
	if err != nil {
		writeError(w, err)
		return
	}

	// ES 1.x _percolate response shape: one "matches" entry per
	// registered query that matched the document.
	matches := make([]map[string]any, 0, len(res.Hits))
	for _, h := range res.Hits {
		matches = append(matches, map[string]any{
			"_index": index,
			"_id":    h.ID,
			"_score": h.Score,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"took":    res.Took,
		"total":   res.Total,
		"matches": matches,
	})
}
