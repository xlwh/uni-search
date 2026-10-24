package esapi

import (
	"fmt"
	"net/http"

	apierr "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/internal/ingest"
	"github.com/unisearch-db/unisearch/internal/jsonx"
)

// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.7

func (s *Server) handlePutPipeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, apierr.NewIllegalArgument("pipeline id is required"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid request body: "+err.Error()))
		return
	}
	var req struct {
		Description string              `json:"description"`
		Processors  []map[string]any    `json:"processors"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid JSON: "+err.Error()))
		return
	}
	if len(req.Processors) == 0 {
		writeError(w, apierr.NewIllegalArgument("processors must not be empty"))
		return
	}
	var procs []ingest.Processor
	for i, pconf := range req.Processors {
		// ES format: each processor is {"type_name": {config}}
		var typeName string
		var config map[string]any
		for k, v := range pconf {
			typeName = k
			if cfg, ok := v.(map[string]any); ok {
				config = cfg
			}
			break // only one key per processor
		}
		if typeName == "" {
			writeError(w, apierr.NewIllegalArgument("processor[" + itoa(i) + "] missing type"))
			return
		}
		proc, perr := ingest.NewProcessor(typeName, config)
		if perr != nil {
			writeError(w, apierr.NewIllegalArgument("processor["+itoa(i)+"]: "+perr.Error()))
			return
		}
		procs = append(procs, proc)
	}
	s.pipelineStore.Put(&ingest.Pipeline{
		ID: id, Description: req.Description, Processors: procs,
	})
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

func (s *Server) handleGetPipelines(w http.ResponseWriter, r *http.Request) {
	pipelines := s.pipelineStore.List()
	results := make([]map[string]any, 0, len(pipelines))
	for _, p := range pipelines {
		procs := make([]map[string]any, 0, len(p.Processors))
		for _, proc := range p.Processors {
			procs = append(procs, map[string]any{"type": proc.Type()})
		}
		results = append(results, map[string]any{
			"id": p.ID, "description": p.Description, "processors": procs,
		})
	}
	writeJSON(w, 0, map[string]any{"count": len(results), "results": results})
}

func (s *Server) handleGetPipeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.pipelineStore.Get(id)
	if !ok {
		writeError(w, apierr.NewIllegalArgument("pipeline '"+id+"' not found"))
		return
	}
	procs := make([]map[string]any, 0, len(p.Processors))
	for _, proc := range p.Processors {
		procs = append(procs, map[string]any{"type": proc.Type()})
	}
	writeJSON(w, 0, map[string]any{"id": p.ID, "description": p.Description, "processors": procs})
}

func (s *Server) handleDeletePipeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.pipelineStore.Get(id); !ok {
		writeError(w, apierr.NewIllegalArgument("pipeline '"+id+"' not found"))
		return
	}
	s.pipelineStore.Delete(id)
	writeJSON(w, 0, map[string]any{"acknowledged": true})
}

func (s *Server) handleSimulatePipeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, err := readBody(r)
	if err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid request body: "+err.Error()))
		return
	}
	var req struct {
		Doc map[string]any `json:"doc"`
	}
	if err := jsonx.Unmarshal(body, &req); err != nil {
		writeError(w, apierr.NewIllegalArgument("invalid JSON: "+err.Error()))
		return
	}
	if id != "" {
		p, ok := s.pipelineStore.Get(id)
		if !ok {
			writeError(w, apierr.NewIllegalArgument("pipeline '"+id+"' not found"))
			return
		}
		if err := p.Execute(req.Doc); err != nil {
			writeError(w, apierr.NewIllegalArgument("pipeline execution failed: "+err.Error()))
			return
		}
	}
	writeJSON(w, 0, map[string]any{"doc": req.Doc})
}

func itoa(i int) string {
	if i == 0 { return "0" }
	var b []byte
	for i > 0 { b = append([]byte{byte('0' + i%10)}, b...); i /= 10 }
	return string(b)
}

// applyPipeline executes the named ingest pipeline on the document,
// modifying its Source in-place. Called by write handlers before
// dispatching to Engine.Write.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.7
func (s *Server) applyPipeline(doc *IndexDoc) error {
	if doc.Pipeline == "" || doc.Op == OpDelete {
		return nil
	}
	pipeline, ok := s.pipelineStore.Get(doc.Pipeline)
	if !ok {
		return fmt.Errorf("pipeline '%s' not found", doc.Pipeline)
	}
	var sourceMap map[string]any
	if err := jsonx.Unmarshal(doc.Source, &sourceMap); err != nil {
		return fmt.Errorf("invalid document source: %w", err)
	}
	if err := pipeline.Execute(sourceMap); err != nil {
		return fmt.Errorf("pipeline execution failed: %w", err)
	}
	modified, err := jsonx.Marshal(sourceMap)
	if err != nil {
		return fmt.Errorf("failed to re-serialize document: %w", err)
	}
	doc.Source = modified
	return nil
}
