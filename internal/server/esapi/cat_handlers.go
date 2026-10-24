package esapi

import (
	"net/http"
)

// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.5.6

func (s *Server) handleCatAliases(w http.ResponseWriter, r *http.Request) {
	// List all aliases across indices.
	indices, err := s.engine.Indices(r.Context(), "*")
	if err != nil {
		writeJSON(w, 0, []any{})
		return
	}
	var rows []map[string]any
	for _, ii := range indices {
		aliases, _ := s.engine.GetAliases(r.Context(), ii.Index)
		for alias, idxList := range aliases {
			for range idxList {
				rows = append(rows, map[string]any{
					"alias": alias, "index": ii.Index, "filter": "-", "routing.index": "-", "routing.search": "-",
				})
			}
		}
	}
	writeJSON(w, 0, rows)
}

func (s *Server) handleCatSegments(w http.ResponseWriter, r *http.Request) {
	indices, err := s.engine.Indices(r.Context(), "*")
	if err != nil {
		writeJSON(w, 0, []any{})
		return
	}
	var rows []map[string]any
	for _, ii := range indices {
		rows = append(rows, map[string]any{
			"index": ii.Index, "shard": "0", "prirep": "p", "ip": "127.0.0.1",
			"segment": "0", "docs.count": ii.DocsCount, "docs.deleted": ii.DocsDeleted,
			"size": ii.StoreSize, "committed": "true", "search": "true",
		})
	}
	writeJSON(w, 0, rows)
}

func (s *Server) handleCatTemplates(w http.ResponseWriter, r *http.Request) {
	// List index templates from Server.templates sync.Map.
	var rows []map[string]any
	s.templates.Range(func(k, v any) bool {
		rows = append(rows, map[string]any{
			"name": k, "order": 0, "version": 1,
		})
		return true
	})
	writeJSON(w, 0, rows)
}

func (s *Server) handleCatRecovery(w http.ResponseWriter, r *http.Request) {
	// P1: no active recovery operations in embedded mode.
	writeJSON(w, 0, []any{})
}

func (s *Server) handleCatTasks(w http.ResponseWriter, r *http.Request) {
	// P1: no task management in embedded mode.
	writeJSON(w, 0, []any{})
}

func (s *Server) handleCatThreadPool(w http.ResponseWriter, r *http.Request) {
	// P1: return Go runtime info as thread pool proxy.
	writeJSON(w, 0, []map[string]any{
		{"node_name": "node-1", "name": "generic", "active": 0, "queue": 0, "rejected": 0},
	})
}
