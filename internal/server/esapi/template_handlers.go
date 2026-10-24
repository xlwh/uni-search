package esapi

import (
	"fmt"
	"net/http"
	"strings"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/jsonx"
)

// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.5

// renderTemplate performs simple {{var}} substitution on a template body.
func renderTemplate(template []byte, params map[string]any) []byte {
	s := string(template)
	for k, v := range params {
		placeholder := "{{" + k + "}}"
		s = strings.ReplaceAll(s, placeholder, toString(v))
	}
	return []byte(s)
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strings.TrimRight(strings.TrimRight(
			fmt.Sprintf("%f", t), "0"), ".")
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	case bool:
		if t { return "true" }
		return "false"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// handleSearchTemplate handles POST /_search/template and POST /{index}/_search/template.
func (s *Server) handleSearchTemplate(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid body: "+err.Error()))
		return
	}
	var req struct {
		Inline       string         `json:"inline"`
		Params       map[string]any `json:"params"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid JSON: "+err.Error()))
		return
	}
	if req.Inline == "" {
		writeError(w, apierr.NewIllegalArgument("[inline] template is required"))
		return
	}
	// Render the template.
	rendered := renderTemplate([]byte(req.Inline), req.Params)
	// Execute the search with the rendered body.
	indices := parseIndices(r)
	result, err := s.engine.Search(r.Context(), indices, rendered)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 0, result)
}

// handleRenderTemplate handles POST /_render/template (render only, no search).
func (s *Server) handleRenderTemplate(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid body: "+err.Error()))
		return
	}
	var req struct {
		Inline string         `json:"inline"`
		Params map[string]any `json:"params"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid JSON: "+err.Error()))
		return
	}
	if req.Inline == "" {
		writeError(w, apierr.NewIllegalArgument("[inline] template is required"))
		return
	}
	rendered := renderTemplate([]byte(req.Inline), req.Params)
	var renderedObj map[string]any
	jsonx.Unmarshal(rendered, &renderedObj)
	writeJSON(w, 0, map[string]any{"rendered": renderedObj})
}

// ---------------------------------------------------------------------------
// _scripts stored template CRUD (es-feature-gap-fill P2)
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.5
// ---------------------------------------------------------------------------

func (s *Server) makePutScriptHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			writeError(w, apierr.NewIllegalArgument("invalid body: "+err.Error()))
			return
		}
		s.templates.Store(id, body)
		writeJSON(w, 0, map[string]any{"acknowledged": true})
	}
}

func (s *Server) makeGetScriptHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		val, ok := s.templates.Load(id)
		if !ok {
			writeError(w, apierr.NewIllegalArgument("stored script '"+id+"' not found"))
			return
		}
		writeJSON(w, 0, map[string]any{"id": id, "script": val})
	}
}

func (s *Server) makeDeleteScriptHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.templates.Load(id); !ok {
			writeError(w, apierr.NewIllegalArgument("stored script '"+id+"' not found"))
			return
		}
		s.templates.Delete(id)
		writeJSON(w, 0, map[string]any{"acknowledged": true})
	}
}
