package esapi

import (
	"net/http"
	"strings"
	"time"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/jsonx"
	"github.com/unisearch-db/unisearch/internal/synonyms"
)

// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.9

func (s *Server) handlePutSynonyms(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("rest")
	if id == "" {
		writeError(w, apierr.NewIllegalArgument("synonyms id is required"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid request body: "+err.Error()))
		return
	}
	var req struct {
		Synonyms []string `json:"synonyms"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid JSON: "+err.Error()))
		return
	}
	if len(req.Synonyms) == 0 {
		writeError(w, apierr.NewIllegalArgument("synonyms must not be empty"))
		return
	}
	now := time.Now().UTC()
	s.synonymStore.Put(&synonyms.SynonymSet{
		ID:        id,
		Synonyms:  req.Synonyms,
		CreatedAt: now,
		UpdatedAt: now,
	})
	writeJSON(w, 0, map[string]any{"acknowledged": true, "id": id})
}

func (s *Server) handleGetSynonyms(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("rest")
	if id != "" {
		set, ok := s.synonymStore.Get(id)
		if !ok {
			writeError(w, apierr.NewIllegalArgument("synonyms set '"+id+"' not found"))
			return
		}
		writeJSON(w, 0, set)
		return
	}
	sets := s.synonymStore.List()
	writeJSON(w, 0, map[string]any{"count": len(sets), "results": sets})
}

func (s *Server) handleDeleteSynonyms(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("rest")
	if id == "" {
		writeError(w, apierr.NewIllegalArgument("synonyms id is required"))
		return
	}
	if _, ok := s.synonymStore.Get(id); !ok {
		writeError(w, apierr.NewIllegalArgument("synonyms set '"+id+"' not found"))
		return
	}
	s.synonymStore.Delete(id)
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

// makeSynonymPutHandler returns a handler that processes PUT /_synonyms/{id}
// with the id extracted from the intercepted path (bypassing ServeMux).
func (s *Server) makeSynonymPutHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.serveSynonymPut(w, r, id)
	}
}

// makeSynonymGetHandler returns a handler that processes GET /_synonyms/{id}.
func (s *Server) makeSynonymGetHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.serveSynonymGet(w, r, id)
	}
}

// makeSynonymDeleteHandler returns a handler that processes DELETE /_synonyms/{id}.
func (s *Server) makeSynonymDeleteHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.serveSynonymDelete(w, r, id)
	}
}

func (s *Server) serveSynonymPut(w http.ResponseWriter, r *http.Request, id string) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid request body: "+err.Error()))
		return
	}
	var req struct {
		Synonyms []string `json:"synonyms"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid JSON: "+err.Error()))
		return
	}
	if len(req.Synonyms) == 0 {
		writeError(w, apierr.NewIllegalArgument("synonyms must not be empty"))
		return
	}
	now := time.Now().UTC()
	s.synonymStore.Put(&synonyms.SynonymSet{
		ID:        id,
		Synonyms:  req.Synonyms,
		CreatedAt: now,
		UpdatedAt: now,
	})
	writeJSON(w, 0, map[string]any{"acknowledged": true, "id": id})
}

func (s *Server) serveSynonymGet(w http.ResponseWriter, r *http.Request, id string) {
	set, ok := s.synonymStore.Get(id)
	if !ok {
		writeError(w, apierr.NewIllegalArgument("synonyms set '"+id+"' not found"))
		return
	}
	writeJSON(w, 0, set)
}

func (s *Server) serveSynonymDelete(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.synonymStore.Get(id); !ok {
		writeError(w, apierr.NewIllegalArgument("synonyms set '"+id+"' not found"))
		return
	}
	s.synonymStore.Delete(id)
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

// ---------------------------------------------------------------------------
// P1: Single synonym rule management (es-feature-gap-fill)
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.9.1
// ---------------------------------------------------------------------------

func (s *Server) makePutSynonymRuleHandler(setID, ruleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		set, ok := s.synonymStore.Get(setID)
		if !ok {
			writeError(w, apierr.NewIllegalArgument("synonyms set '"+setID+"' not found"))
			return
		}
		body, err := readBody(r)
		if err != nil {
			writeError(w, apierr.NewIllegalArgument("invalid body: "+err.Error()))
			return
		}
		var req struct {
			Synonyms string `json:"synonyms"`
		}
		if jsonx.Unmarshal(body, &req) != nil || req.Synonyms == "" {
			writeError(w, apierr.NewIllegalArgument("synonyms rule is required"))
			return
		}
		// P1: append or replace the rule in the set's synonym list.
		ruleKey := ruleID
		found := false
		for i, syn := range set.Synonyms {
			if strings.HasPrefix(syn, ruleKey+":") || syn == ruleKey {
				set.Synonyms[i] = ruleKey + ": " + req.Synonyms
				found = true
				break
			}
		}
		if !found {
			set.Synonyms = append(set.Synonyms, ruleKey+": "+req.Synonyms)
		}
		set.UpdatedAt = time.Now().UTC()
		writeJSON(w, 0, map[string]any{"acknowledged": true})
	}
}

func (s *Server) makeGetSynonymRuleHandler(setID, ruleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		set, ok := s.synonymStore.Get(setID)
		if !ok {
			writeError(w, apierr.NewIllegalArgument("synonyms set '"+setID+"' not found"))
			return
		}
		for _, syn := range set.Synonyms {
			if strings.HasPrefix(syn, ruleID+":") || syn == ruleID {
				writeJSON(w, 0, map[string]any{
					"id": setID, "rule_id": ruleID, "synonyms": syn,
				})
				return
			}
		}
		writeError(w, apierr.NewIllegalArgument("synonym rule '"+ruleID+"' not found in set '"+setID+"'"))
	}
}

func (s *Server) makeDeleteSynonymRuleHandler(setID, ruleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		set, ok := s.synonymStore.Get(setID)
		if !ok {
			writeError(w, apierr.NewIllegalArgument("synonyms set '"+setID+"' not found"))
			return
		}
		for i, syn := range set.Synonyms {
			if strings.HasPrefix(syn, ruleID+":") || syn == ruleID {
				set.Synonyms = append(set.Synonyms[:i], set.Synonyms[i+1:]...)
				set.UpdatedAt = time.Now().UTC()
				writeJSON(w, 0, map[string]any{"acknowledged": true})
				return
			}
		}
		writeError(w, apierr.NewIllegalArgument("synonym rule '"+ruleID+"' not found in set '"+setID+"'"))
	}
}
