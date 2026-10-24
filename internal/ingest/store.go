package ingest

import "sync"

// PipelineStore manages pipeline definitions in memory.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.7
type PipelineStore struct {
	m sync.Map // id → *Pipeline
}

func NewPipelineStore() *PipelineStore { return &PipelineStore{} }

func (s *PipelineStore) Put(p *Pipeline) { s.m.Store(p.ID, p) }
func (s *PipelineStore) Get(id string) (*Pipeline, bool) {
	v, ok := s.m.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Pipeline), true
}
func (s *PipelineStore) Delete(id string) { s.m.Delete(id) }
func (s *PipelineStore) List() []*Pipeline {
	var out []*Pipeline
	s.m.Range(func(_, v any) bool { out = append(out, v.(*Pipeline)); return true })
	return out
}
