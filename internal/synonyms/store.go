package synonyms

import "sync"

// SynonymStore manages synonym sets in memory using sync.Map.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.9
type SynonymStore struct {
	m sync.Map // id → *SynonymSet
}

// NewSynonymStore creates an empty SynonymStore.
func NewSynonymStore() *SynonymStore {
	return &SynonymStore{}
}

// Put creates or updates a synonym set.
func (s *SynonymStore) Put(set *SynonymSet) {
	s.m.Store(set.ID, set)
}

// Get retrieves a synonym set by ID. Returns nil, false if not found.
func (s *SynonymStore) Get(id string) (*SynonymSet, bool) {
	v, ok := s.m.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*SynonymSet), true
}

// Delete removes a synonym set by ID.
func (s *SynonymStore) Delete(id string) {
	s.m.Delete(id)
}

// List returns all synonym sets.
func (s *SynonymStore) List() []*SynonymSet {
	var out []*SynonymSet
	s.m.Range(func(_, v any) bool {
		out = append(out, v.(*SynonymSet))
		return true
	})
	return out
}
