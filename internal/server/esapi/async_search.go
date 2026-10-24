package esapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
)

// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.8

// asyncSearchEntry holds the state and result of one async search.
type asyncSearchEntry struct {
	id             string
	result         *SearchResult
	isRunning      bool
	isPartial      bool
	searchErr      *apierr.APIError
	startTime      time.Time
	expirationTime time.Time
	indices        []string
	body           []byte
}

// AsyncSearchManager manages async search submissions, result storage,
// and expiration cleanup. Follows the PITManager pattern.
type AsyncSearchManager struct {
	mu      sync.Mutex
	entries map[string]*asyncSearchEntry
	engine  Engine
	max     int // max concurrent entries (default 10000)
}

// NewAsyncSearchManager creates an AsyncSearchManager bound to the engine.
func NewAsyncSearchManager(engine Engine) *AsyncSearchManager {
	m := &AsyncSearchManager{
		engine:  engine,
		entries: make(map[string]*asyncSearchEntry),
		max:     10000,
	}
	go m.cleanupLoop()
	return m
}

// Submit starts an async search and returns the entry ID. If the search
// completes within waitTimeout, the result is available immediately.
func (m *AsyncSearchManager) Submit(indices []string, body []byte, keepAlive time.Duration) string {
	id := generateAsyncSearchID()
	entry := &asyncSearchEntry{
		id:             id,
		isRunning:      true,
		isPartial:      true,
		startTime:      time.Now(),
		expirationTime: time.Now().Add(keepAlive),
		indices:        indices,
		body:           body,
	}
	m.mu.Lock()
	if len(m.entries) >= m.max {
		// Evict oldest expired entry.
		for k, e := range m.entries {
			if time.Now().After(e.expirationTime) {
				delete(m.entries, k)
				break
			}
		}
	}
	m.entries[id] = entry
	m.mu.Unlock()

	// Launch background search.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		result, err := m.engine.Search(ctx, entry.indices, entry.body)
		m.mu.Lock()
		defer m.mu.Unlock()
		entry.isRunning = false
		entry.isPartial = false
		if err != nil {
			if apiErr, ok := err.(*apierr.APIError); ok {
				entry.searchErr = apiErr
			} else {
				entry.searchErr = apierr.NewInternal(err.Error())
			}
		} else {
			entry.result = result
		}
	}()

	return id
}

// Get returns the entry by ID, or nil if not found.
func (m *AsyncSearchManager) Get(id string) *asyncSearchEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[id]
}

// Delete removes an entry by ID.
func (m *AsyncSearchManager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, id)
}

// cleanupLoop periodically removes expired entries.
func (m *AsyncSearchManager) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		now := time.Now()
		for k, e := range m.entries {
			if now.After(e.expirationTime) && !e.isRunning {
				delete(m.entries, k)
			}
		}
		m.mu.Unlock()
	}
}

func generateAsyncSearchID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

// handleSubmitAsyncSearch handles POST /_async_search.
func (s *Server) handleSubmitAsyncSearch(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid request body: "+err.Error()))
		return
	}
	keepAlive := 5 * time.Minute
	if ka := r.URL.Query().Get("keep_alive"); ka != "" {
		if d, err := time.ParseDuration(ka); err == nil {
			keepAlive = d
		}
	}
	indices := parseIndices(r)
	id := s.asyncSearchMgr.Submit(indices, body, keepAlive)
	entry := s.asyncSearchMgr.Get(id)
	resp := map[string]any{
		"id":                     id,
		"is_running":             entry.isRunning,
		"is_partial":             entry.isPartial,
		"start_time_in_millis":   entry.startTime.UnixMilli(),
		"expiration_time_in_millis": entry.expirationTime.UnixMilli(),
	}
	if entry.result != nil {
		resp["took"] = entry.result.Took
		resp["timed_out"] = entry.result.TimedOut
		resp["hits"] = entry.result.Hits
		resp["total"] = entry.result.Total
	}
	writeJSON(w, 0, resp)
}

// handleGetAsyncSearch handles GET /_async_search/{id}.
func (s *Server) handleGetAsyncSearch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("rest")
	entry := s.asyncSearchMgr.Get(id)
	if entry == nil {
		writeError(w, apierr.NewIllegalArgument("async search '"+id+"' not found"))
		return
	}
	resp := map[string]any{
		"id":                     id,
		"is_running":             entry.isRunning,
		"is_partial":             entry.isPartial,
		"start_time_in_millis":   entry.startTime.UnixMilli(),
		"expiration_time_in_millis": entry.expirationTime.UnixMilli(),
	}
	if entry.searchErr != nil {
		resp["error"] = entry.searchErr.Error()
	}
	if entry.result != nil {
		resp["took"] = entry.result.Took
		resp["timed_out"] = entry.result.TimedOut
		resp["hits"] = entry.result.Hits
		resp["total"] = entry.result.Total
		resp["max_score"] = entry.result.MaxScore
	}
	writeJSON(w, 0, resp)
}

// handleGetAsyncSearchStatus handles GET /_async_search/status/{id}.
func (s *Server) handleGetAsyncSearchStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry := s.asyncSearchMgr.Get(id)
	if entry == nil {
		writeError(w, apierr.NewIllegalArgument("async search '"+id+"' not found"))
		return
	}
	writeJSON(w, 0, map[string]any{
		"id":             id,
		"is_running":     entry.isRunning,
		"is_partial":     entry.isPartial,
		"start_time_in_millis": entry.startTime.UnixMilli(),
	})
}

// handleDeleteAsyncSearch handles DELETE /_async_search/{id}.
func (s *Server) handleDeleteAsyncSearch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("rest")
	s.asyncSearchMgr.Delete(id)
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

// makeAsyncSearchGetHandler returns a handler for intercepted GET /_async_search/{id}.
func (s *Server) makeAsyncSearchGetHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entry := s.asyncSearchMgr.Get(id)
		if entry == nil {
			writeError(w, apierr.NewIllegalArgument("async search '"+id+"' not found"))
			return
		}
		resp := map[string]any{
			"id":                     id,
			"is_running":             entry.isRunning,
			"is_partial":             entry.isPartial,
			"start_time_in_millis":   entry.startTime.UnixMilli(),
			"expiration_time_in_millis": entry.expirationTime.UnixMilli(),
		}
		if entry.searchErr != nil {
			resp["error"] = entry.searchErr.Error()
		}
		if entry.result != nil {
			resp["took"] = entry.result.Took
			resp["timed_out"] = entry.result.TimedOut
			resp["hits"] = entry.result.Hits
			resp["total"] = entry.result.Total
			resp["max_score"] = entry.result.MaxScore
		}
		writeJSON(w, 0, resp)
	}
}

// makeAsyncSearchDeleteHandler returns a handler for intercepted DELETE /_async_search/{id}.
func (s *Server) makeAsyncSearchDeleteHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.asyncSearchMgr.Delete(id)
		writeJSON(w, 0, map[string]any{"acknowledged": true})
	}
}
