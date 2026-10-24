// Package synonyms provides synonym set management for ES-compatible
// _synonyms API.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.9
package synonyms

import "time"

// SynonymSet represents a named collection of synonym rules.
type SynonymSet struct {
	ID        string    `json:"id"`
	Synonyms  []string  `json:"synonyms"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
