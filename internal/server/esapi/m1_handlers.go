// Package esapi m1_handlers.go implements the M1 new REST endpoints
// (design 04-es-compat-layer §2.2/§6). Each handler follows the same
// pattern: parse request → call engine → write ES 8.x JSON response.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.8
package esapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/unisearch-db/unisearch/internal/jsonx"
)

// handleMSearch handles POST /_msearch and POST /{index}/_msearch.
// The body is NDJSON: header\n body\n header\n body\n ...
func (s *Server) handleMSearch(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	lines := splitNDJSON(body)
	if len(lines)%2 != 0 {
		writeError(w, apierrNew("msearch: odd number of NDJSON lines"))
		return
	}
	indices := parseIndices(r)
	responses := make([]map[string]any, 0, len(lines)/2)
	for i := 0; i < len(lines); i += 2 {
		// header line may contain an index override
		headerLine := lines[i]
		queryBody := lines[i+1]
		searchIndices := indices
		// Parse header for index override
		var header map[string]any
		if json.Unmarshal(headerLine, &header) == nil {
			if idx, ok := header["index"].(string); ok && idx != "" {
				searchIndices = []string{idx}
			}
		}
		if len(queryBody) == 0 {
			queryBody = []byte("{}")
		}
		res, err := s.engine.Search(r.Context(), searchIndices, queryBody)
		if err != nil {
			responses = append(responses, map[string]any{
				"error":  err.Error(),
				"status": 400,
			})
			continue
		}
		resp := map[string]any{
			"took":      res.Took,
			"timed_out": res.TimedOut,
			"hits": map[string]any{
				"total":     map[string]any{"value": res.Total, "relation": res.Relation},
				"max_score": res.MaxScore,
				"hits":      reshapeHits(res.Hits),
			},
			"status": 200,
		}
		if res.Aggregations != nil {
			resp["aggregations"] = res.Aggregations
		}
		responses = append(responses, resp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"took": 0, "responses": responses})
}

// handleFieldCaps handles GET /{index}/_field_caps.
func (s *Server) handleFieldCaps(w http.ResponseWriter, r *http.Request) {
	indices := parseIndices(r)
	fieldsMap := map[string]any{}
	for _, idx := range indices {
		mapping, err := s.engine.GetMapping(r.Context(), idx)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(mapping, &m) != nil {
			continue
		}
		idxProps, ok := m[idx].(map[string]any)
		if !ok {
			continue
		}
		mappings, ok := idxProps["mappings"].(map[string]any)
		if !ok {
			continue
		}
		properties, ok := mappings["properties"].(map[string]any)
		if !ok {
			continue
		}
		for field, def := range properties {
			fd, ok := def.(map[string]any)
			if !ok {
				continue
			}
			// ES 8.x _field_caps uses field TYPE as secondary key,
			// not index name. e.g. {"title": {"text": {"type":"text",...}}}
			// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.4
			typeName, _ := fd["type"].(string)
			if typeName == "" {
				typeName = "object"
			}
			fieldsMap[field] = map[string]any{
				typeName: map[string]any{
					"type":                   fd["type"],
					"metadata_field":         false,
					"searchable":             true,
					"aggregatable":           true,
					"indices":                []string{idx},
					"non_searchable_indices": []string{},
				},
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"indices": indices,
		"fields":  fieldsMap,
	})
}

// handleTermsEnum handles GET /{index}/_terms_enum.
// Returns terms for a specific field by searching all documents and
// collecting distinct values (design 04 §6.6).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#terms_enum
func (s *Server) handleTermsEnum(w http.ResponseWriter, r *http.Request) {
	field := r.URL.Query().Get("field")
	prefix := r.URL.Query().Get("prefix")
	index := r.PathValue("index")
	size := 1000
	if sz := r.URL.Query().Get("size"); sz != "" {
		if n, err := strconv.Atoi(sz); err == nil && n > 0 {
			size = n
		}
	}

	if field == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"_shards": map[string]any{"total": 1, "successful": 1, "skipped": 0},
			"terms":   []any{},
			"error":   "field parameter is required",
		})
		return
	}

	// Search all docs with match_all and size large enough to collect terms.
	searchBody := `{"query":{"match_all":{}},"size":` + strconv.Itoa(size) + `}`
	res, err := s.engine.Search(ctxFromReq(r), []string{index}, []byte(searchBody))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"_shards":  map[string]any{"total": 1, "successful": 1, "skipped": 0, "failed": 0},
			"terms":    []any{},
			"complete": false,
		})
		return
	}

	// Collect distinct field values from _source.
	seen := make(map[string]struct{})
	var terms []string
	for _, hit := range res.Hits {
		var src map[string]any
		if jsonx.Unmarshal(hit.Source, &src) != nil {
			continue
		}
		val, ok := src[field]
		if !ok {
			continue
		}
		var term string
		switch v := val.(type) {
		case string:
			term = v
		case float64:
			term = strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			if v {
				term = "true"
			} else {
				term = "false"
			}
		default:
			term = fmt.Sprintf("%v", v)
		}
		if term == "" {
			continue
		}
		// Apply prefix filter.
		if prefix != "" && !strings.HasPrefix(strings.ToLower(term), strings.ToLower(prefix)) {
			continue
		}
		if _, dup := seen[term]; dup {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
		if len(terms) >= size {
			break
		}
	}

	// Convert to response format.
	termList := make([]any, len(terms))
	for i, t := range terms {
		termList[i] = t
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"_shards":  map[string]any{"total": 1, "successful": 1, "skipped": 0, "failed": 0},
		"terms":    termList,
		"complete": len(terms) < size,
	})
}

// handleExplain handles GET /{index}/_explain/{id}.
// Executes the query and reports whether the document matched, with
// a simplified scoring breakdown (design 04 §6.6).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.3
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	indices := parseIndices(r)
	id := r.PathValue("id")
	index := ""
	if len(indices) > 0 {
		index = indices[0]
	}
	// Read optional query body.
	body, _ := readBody(r)
	if len(body) == 0 {
		body = []byte(`{"query":{"match_all":{}}}`)
	}
	// Execute the search with the given query.
	res, err := s.engine.Search(ctxFromReq(r), indices, body)
	if err != nil {
		writeError(w, err)
		return
	}
	// Find the target document in the results.
	var matched bool
	var score float32
	for _, hit := range res.Hits {
		if hit.ID == id {
			matched = true
			score = hit.Score
			break
		}
	}
	if !matched {
		// Check if the document exists at all.
		_, _, _, notFound, _ := s.engine.Get(r.Context(), index, id)
		if notFound {
			writeJSON(w, http.StatusOK, map[string]any{
				"_index": index, "_id": id, "matched": false,
				"explanation": map[string]any{
					"value": 0.0, "description": "document not found",
				},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"_index": index, "_id": id, "matched": false,
			"explanation": map[string]any{
				"value":       0.0,
				"description": "document does not match the query",
			},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"_index":  index,
		"_id":     id,
		"matched": true,
		"explanation": map[string]any{
			"value":       score,
			"description": "sum of:",
			"details": []map[string]any{
				{
					"value":       score,
					"description": "score from query execution",
					"type":        "sum",
				},
			},
		},
	})
}

// handleDeleteByQuery handles POST /{index}/_delete_by_query.
func (s *Server) handleDeleteByQuery(w http.ResponseWriter, r *http.Request) {
	indices := parseIndices(r)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	// Execute the query to find matching documents, then delete them.
	index := ""
	if len(indices) > 0 {
		index = indices[0]
	}
	res, err := s.engine.Search(r.Context(), indices, body)
	if err != nil {
		writeError(w, err)
		return
	}
	deleted := 0
	for _, hit := range res.Hits {
		if _, derr := s.engine.Delete(r.Context(), index, hit.ID, nil, ""); derr == nil {
			deleted++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"took":      0,
		"timed_out": false,
		"deleted":   deleted,
		"failures":  []any{},
		"_shards":   map[string]any{"total": 1, "successful": 1, "skipped": 0},
	})
}

// handleUpdateByQuery handles POST /{index}/_update_by_query.
// Searches for matching documents and re-indexes them (ES semantics:
// documents are immutable, update = delete + insert with same _id).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.3
func (s *Server) handleUpdateByQuery(w http.ResponseWriter, r *http.Request) {
	indices := parseIndices(r)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	index := ""
	if len(indices) > 0 {
		index = indices[0]
	}
	res, err := s.engine.Search(ctxFromReq(r), indices, body)
	if err != nil {
		writeError(w, err)
		return
	}
	updated := 0
	var failures []any
	for _, hit := range res.Hits {
		// Re-index the document with the same _id (update = re-index).
		_, werr := s.engine.Write(ctxFromReq(r), IndexDoc{
			Op:     OpIndex,
			Index:  index,
			ID:     hit.ID,
			Source: hit.Source,
		}, "")
		if werr != nil {
			failures = append(failures, map[string]any{
				"_id":   hit.ID,
				"error": werr.Error(),
			})
		} else {
			updated++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"took":      res.Took,
		"timed_out": res.TimedOut,
		"updated":   updated,
		"failures":  failures,
		"_shards":   map[string]any{"total": res.Shards.Total, "successful": res.Shards.Successful, "skipped": res.Shards.Skipped},
	})
}

// handleReindex handles POST /_reindex.
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Source struct {
			Index string `json:"index"`
		} `json:"source"`
		Dest struct {
			Index string `json:"index"`
		} `json:"dest"`
	}
	json.Unmarshal(body, &req)
	created, updated := 0, 0
	if req.Source.Index != "" && req.Dest.Index != "" {
		// Search source, index into dest.
		res, err := s.engine.Search(r.Context(), []string{req.Source.Index}, []byte("{}"))
		if err == nil {
			for _, hit := range res.Hits {
				_, err := s.engine.Write(r.Context(), IndexDoc{
					Op: OpIndex, Index: req.Dest.Index, ID: hit.ID, Source: hit.Source,
				}, "")
				if err == nil {
					created++
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"took":      0,
		"timed_out": false,
		"created":   created,
		"updated":   updated,
		"failures":  []any{},
	})
}

// handleIndexTemplate handles PUT/GET /_index_template.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.3
func (s *Server) handleIndexTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	switch r.Method {
	case "PUT":
		body, _ := readBody(r)
		if name == "" {
			// Try to extract name from body
			var m map[string]any
			if jsonx.Unmarshal(body, &m) == nil {
				if n, ok := m["name"].(string); ok {
					name = n
				}
			}
		}
		if name != "" {
			s.templates.Store(name, body)
		}
		writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
	case "GET":
		templates := make([]any, 0)
		s.templates.Range(func(key, value any) bool {
			templates = append(templates, map[string]any{
				"name":     key,
				"template": jsonx.RawMessage(value.([]byte)),
			})
			return true
		})
		writeJSON(w, http.StatusOK, map[string]any{"index_templates": templates})
	}
}

// handleILM handles PUT/GET /_ilm/policy.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.3.3
func (s *Server) handleILM(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	switch r.Method {
	case "PUT":
		body, _ := readBody(r)
		if name == "" {
			var m map[string]any
			if jsonx.Unmarshal(body, &m) == nil {
				if n, ok := m["name"].(string); ok {
					name = n
				}
			}
		}
		if name == "" {
			name = "default"
		}
		s.ilmPolicies.Store(name, body)
		writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
	case "GET":
		policies := make(map[string]any)
		s.ilmPolicies.Range(func(key, value any) bool {
			policies[key.(string)] = jsonx.RawMessage(value.([]byte))
			return true
		})
		writeJSON(w, http.StatusOK, policies)
	}
}

// splitNDJSON splits NDJSON body into individual JSON lines.
func splitNDJSON(body []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			if i > start {
				lines = append(lines, body[start:i])
			}
			start = i + 1
		}
	}
	if start < len(body) {
		lines = append(lines, body[start:])
	}
	return lines
}

// apierrNew creates a simple APIError from a message.
func apierrNew(msg string) error {
	return &simpleError{msg: msg}
}

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }
