package esapi

import (
	"context"
	"net/http"
	"sort"
	"strings"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/jsonx"
)

// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.10

// TermVectorsResponse is the ES _termvectors response structure.
type TermVectorsResponse struct {
	Index       string                     `json:"_index"`
	ID          string                     `json:"_id"`
	Found       bool                       `json:"found"`
	TermVectors map[string]FieldTermVector `json:"term_vectors"`
}

// FieldTermVector holds term statistics for one field.
type FieldTermVector struct {
	FieldStatistics *FieldStatistics    `json:"field_statistics,omitempty"`
	Terms           map[string]TermInfo `json:"terms"`
}

// FieldStatistics holds field-level statistics.
type FieldStatistics struct {
	DocCount   int `json:"doc_count"`
	SumDocFreq int `json:"sum_doc_freq"`
	SumTTT     int `json:"sum_ttf"`
}

// TermInfo holds per-term statistics.
type TermInfo struct {
	TermFreq    int   `json:"term_freq"`
	DocFreq     int   `json:"doc_freq,omitempty"`
	TTF         int   `json:"ttf,omitempty"`
	Positions   []int `json:"positions,omitempty"`
	StartOffset []int `json:"start_offset,omitempty"`
	EndOffset   []int `json:"end_offset,omitempty"`
}

// handleTermVectors handles GET/POST /{index}/_termvectors/{id}.
func (s *Server) handleTermVectors(w http.ResponseWriter, r *http.Request) {
	index := r.PathValue("index")
	id := r.PathValue("id")
	if index == "" || id == "" {
		writeError(w, apierr.NewIllegalArgument("index and id are required"))
		return
	}

	ctx := r.Context()

	// Fetch the document source.
	source, _, _, notFound, err := s.engine.Get(ctx, index, id)
	if err != nil {
		writeError(w, apierr.NewInternal("failed to get document: "+err.Error()))
		return
	}
	if notFound {
		writeJSON(w, 0, &TermVectorsResponse{
			Index: index, ID: id, Found: false, TermVectors: map[string]FieldTermVector{},
		})
		return
	}

	// Parse the source JSON.
	var doc map[string]any
	if err := jsonx.Unmarshal(source, &doc); err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid document source JSON"))
		return
	}

	// Determine which fields to analyze.
	fieldsParam := r.URL.Query().Get("fields")
	var fieldFilter map[string]bool
	if fieldsParam != "" {
		fieldFilter = make(map[string]bool)
		for _, f := range strings.Split(fieldsParam, ",") {
			fieldFilter[strings.TrimSpace(f)] = true
		}
	}

	// P1: Control which information to include.
	includeOffsets := r.URL.Query().Get("offsets") != "false"
	includePositions := r.URL.Query().Get("positions") != "false"
	includeTermStats := r.URL.Query().Get("term_statistics") == "true"
	includeFieldStats := r.URL.Query().Get("field_statistics") == "true"

	// Build term vectors by analyzing each text field.
	termVectors := make(map[string]FieldTermVector)
	for field, value := range doc {
		if field == "_id" {
			continue
		}
		if fieldFilter != nil && !fieldFilter[field] {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}

		tokens, err := s.engine.Analyze(ctx, index, "", text)
		if err != nil || len(tokens) == 0 {
			continue
		}

		terms := make(map[string]TermInfo)
		for _, tok := range tokens {
			ti := terms[tok.Token]
			ti.TermFreq++
			if includePositions {
				if ti.Positions == nil {
					ti.Positions = []int{}
				}
				ti.Positions = append(ti.Positions, tok.Position)
			}
			if includeOffsets {
				if ti.StartOffset == nil {
					ti.StartOffset = []int{}
					ti.EndOffset = []int{}
				}
				ti.StartOffset = append(ti.StartOffset, tok.StartOffset)
				ti.EndOffset = append(ti.EndOffset, tok.EndOffset)
			}
			terms[tok.Token] = ti
		}

		// P1: term_statistics — set DocFreq (simplified: 1 per term).
		if includeTermStats {
			for k := range terms {
				t := terms[k]
				t.DocFreq = 1
				t.TTF = t.TermFreq
				terms[k] = t
			}
		}

		ftv := FieldTermVector{Terms: terms}
		// P1: field_statistics.
		if includeFieldStats {
			ftv.FieldStatistics = &FieldStatistics{
				DocCount:   1,
				SumDocFreq: len(terms),
				SumTTT:     len(tokens),
			}
		}
		termVectors[field] = ftv
	}

	writeJSON(w, 0, &TermVectorsResponse{
		Index:       index,
		ID:          id,
		Found:       true,
		TermVectors: termVectors,
	})
}

// handleMTermVectors handles GET/POST /_mtermvectors.
func (s *Server) handleMTermVectors(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid request body: "+err.Error()))
		return
	}

	var req struct {
		Docs []struct {
			Index string `json:"_index"`
			ID    string `json:"_id"`
		} `json:"docs"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid JSON: "+err.Error()))
		return
	}

	ctx := r.Context()
	results := make([]TermVectorsResponse, 0, len(req.Docs))
	for _, doc := range req.Docs {
		index := doc.Index
		id := doc.ID
		if index == "" {
			index = r.PathValue("index")
		}
		if index == "" || id == "" {
			results = append(results, TermVectorsResponse{Found: false})
			continue
		}

		source, _, _, notFound, err := s.engine.Get(ctx, index, id)
		if err != nil || notFound {
			results = append(results, TermVectorsResponse{
				Index: index, ID: id, Found: false,
				TermVectors: map[string]FieldTermVector{},
			})
			continue
		}

		var docMap map[string]any
		if jsonx.Unmarshal(source, &docMap) != nil {
			results = append(results, TermVectorsResponse{
				Index: index, ID: id, Found: false,
				TermVectors: map[string]FieldTermVector{},
			})
			continue
		}

		termVectors := buildTermVectors(ctx, s.engine, index, docMap, nil)
		results = append(results, TermVectorsResponse{
			Index: index, ID: id, Found: true, TermVectors: termVectors,
		})
	}

	writeJSON(w, 0, map[string]any{"docs": results})
}

// buildTermVectors constructs term vectors for a document by analyzing
// each string field.
func buildTermVectors(ctx context.Context, engine Engine, index string, doc map[string]any, fieldFilter map[string]bool) map[string]FieldTermVector {
	termVectors := make(map[string]FieldTermVector)
	for field, value := range doc {
		if field == "_id" {
			continue
		}
		if fieldFilter != nil && !fieldFilter[field] {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		tokens, err := engine.Analyze(ctx, index, "", text)
		if err != nil || len(tokens) == 0 {
			continue
		}
		terms := make(map[string]TermInfo)
		for _, tok := range tokens {
			ti := terms[tok.Token]
			ti.TermFreq++
			if ti.Positions == nil {
				ti.Positions = []int{}
			}
			ti.Positions = append(ti.Positions, tok.Position)
			if ti.StartOffset == nil {
				ti.StartOffset = []int{}
				ti.EndOffset = []int{}
			}
			ti.StartOffset = append(ti.StartOffset, tok.StartOffset)
			ti.EndOffset = append(ti.EndOffset, tok.EndOffset)
			terms[tok.Token] = ti
		}
		termVectors[field] = FieldTermVector{Terms: terms}
	}
	return termVectors
}

// sortedFields returns field names in sorted order (for deterministic output).
func sortedFields(m map[string]FieldTermVector) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
