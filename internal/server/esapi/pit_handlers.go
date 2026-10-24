package esapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// handleCreatePIT handles POST /{index}/_pit?keep_alive=5m
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.4
func (s *Server) handleCreatePIT(w http.ResponseWriter, r *http.Request) {
	index := r.PathValue("index")
	keepAlive := 5 * time.Minute
	if ka := r.URL.Query().Get("keep_alive"); ka != "" {
		if d, err := time.ParseDuration(ka); err == nil {
			keepAlive = d
		}
	}
	// Create a snapshot from the engine's current visibility watermark.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.1
	snap, err := s.engine.CurrentSnapshot(ctxFromReq(r), []string{index})
	if err != nil {
		snap = types.NewSnapshot(0) // fallback
	}
	pitID, err := s.pitMgr.Create(index, snap)
	if err != nil {
		writeError(w, err)
		return
	}
	_ = keepAlive // PIT manager handles TTL internally
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      pitID,
		"pit_id":  pitID,
		"_shards": map[string]any{"total": 1, "successful": 1, "skipped": 0},
	})
}

// handleDeletePIT handles DELETE /pit?pid={id}
func (s *Server) handleDeletePIT(w http.ResponseWriter, r *http.Request) {
	pitID := r.URL.Query().Get("pid")
	if pitID == "" {
		body, _ := readBody(r)
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			// Accept both "pid" and "pit_id" (ES API uses "pit_id").
			if id, ok := m["pit_id"].(string); ok {
				pitID = id
			} else if id, ok := m["pid"].(string); ok {
				pitID = id
			}
		}
	}
	if pitID == "" {
		writeError(w, apierrNew("pit: missing pid parameter"))
		return
	}
	if err := s.pitMgr.Delete(pitID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"succeeded": true,
		"_shards":   map[string]any{"total": 1, "successful": 1},
	})
}

// handleScroll handles POST /_search/scroll
func (s *Server) handleScroll(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		writeError(w, apierrNew("scroll: invalid body"))
		return
	}
	scrollID, _ := m["scroll_id"].(string)
	if scrollID == "" {
		writeError(w, apierrNew("scroll: missing scroll_id"))
		return
	}
	keepAlive := 5 * time.Minute
	if ka, ok := m["scroll"].(string); ok {
		if d, err := time.ParseDuration(ka); err == nil {
			keepAlive = d
		}
	}
	_ = keepAlive
	// Advance the scroll: get the next page of results.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.2
	cursor, snap, index, err := s.scrollMgr.AdvanceScroll(scrollID)
	if err != nil {
		// Fallback: decode the scroll_id directly (the initial search
		// handler may not have created a PIT, so AdvanceScroll fails
		// on the empty PITID). Use the cursor's stored index instead.
		var decErr error
		cursor, decErr = decodeScrollID(scrollID)
		if decErr != nil {
			writeError(w, err)
			return
		}
		if cursor == nil {
			writeError(w, apierrNew("scroll: invalid scroll_id"))
			return
		}
		index = cursor.Index
		snap = types.Snapshot{}
	}
	_ = snap

	// Execute a real search using the stored body + search_after cursor.
	searchBody := cursor.Body
	if len(searchBody) == 0 {
		searchBody = []byte(`{"query":{"match_all":{}}}`)
	}
	// Inject search_after into the search body if we have sort values.
	if len(cursor.SortValues) > 0 {
		var m map[string]json.RawMessage
		if json.Unmarshal(searchBody, &m) == nil {
			sa, _ := json.Marshal(cursor.SortValues)
			m["search_after"] = sa
			searchBody, _ = json.Marshal(m)
		}
	}

	res, searchErr := s.engine.Search(ctxFromReq(r), []string{index}, searchBody)
	if searchErr != nil {
		writeError(w, searchErr)
		return
	}

	// Build the next scroll_id from the last hit's sort values.
	var nextSort []any
	if len(res.Hits) > 0 {
		nextSort = res.Hits[len(res.Hits)-1].SortValues
	}
	nextCursor := &scrollCursor{
		PITID:      cursor.PITID,
		SortValues: nextSort,
		From:       cursor.From + len(res.Hits),
		Body:       cursor.Body,
		Index:      cursor.Index,
		Size:       cursor.Size,
	}
	newScrollID := encodeScrollID(nextCursor)

	writeJSON(w, http.StatusOK, map[string]any{
		"_scroll_id": newScrollID,
		"took":       res.Took,
		"timed_out":  res.TimedOut,
		"_shards":    map[string]any{"total": res.Shards.Total, "successful": res.Shards.Successful, "skipped": res.Shards.Skipped, "failed": res.Shards.Failed},
		"hits": map[string]any{
			"total":     map[string]any{"value": res.Total, "relation": res.Relation},
			"max_score": res.MaxScore,
			"hits":      reshapeHits(res.Hits),
		},
	})
}

// handleDeleteScroll handles DELETE /_search/scroll
func (s *Server) handleDeleteScroll(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if scrollID, ok := m["scroll_id"].(string); ok {
			s.scrollMgr.DeleteScroll(scrollID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"succeeded": true,
	})
}

// parseKeepAlive extracts the keep_alive duration from query params.
func parseKeepAlive(r *http.Request) time.Duration {
	ka := r.URL.Query().Get("keep_alive")
	if ka == "" {
		return 5 * time.Minute
	}
	if d, err := time.ParseDuration(ka); err == nil {
		return d
	}
	return 5 * time.Minute
}

// parseSizeParam extracts a size query parameter with default.
func parseSizeParam(r *http.Request, def int) int {
	s := r.URL.Query().Get("size")
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}
