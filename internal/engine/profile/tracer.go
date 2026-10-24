// Package profile implements the M1 Profile API (design 03 §11).
// It traces query execution phases and renders ES-aligned profile
// output: query type, description, time_in_nanos, breakdown.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.7
package profile

import (
	"encoding/json"
	"time"
)

// Tracer records execution timing for query phases.
type Tracer struct {
	phases []Phase
}

// Phase is one timing entry in the profile.
type Phase struct {
	Type        string
	Description string
	TimeNanos   int64
	Breakdown   map[string]int64
}

// New creates an empty tracer.
func New() *Tracer {
	return &Tracer{}
}

// Start begins timing a phase and returns a stop function.
func (t *Tracer) Start(phaseType, description string) func() {
	start := time.Now()
	return func() {
		t.phases = append(t.phases, Phase{
			Type:        phaseType,
			Description: description,
			TimeNanos:   time.Since(start).Nanoseconds(),
			Breakdown:   make(map[string]int64),
		})
	}
}

// Render produces the ES 8.x profile JSON structure.
func (t *Tracer) Render() map[string]any {
	queries := make([]map[string]any, len(t.phases))
	for i, p := range t.phases {
		queries[i] = map[string]any{
			"type":           p.Type,
			"description":    p.Description,
			"time_in_nanos":  p.TimeNanos,
			"breakdown":      p.Breakdown,
		}
	}
	return map[string]any{
		"profile": map[string]any{
			"shards": []map[string]any{
				{
					"id":      "[unisearch][0]",
					"searches": []map[string]any{
						{
							"query":        queries,
							"rewrite_time": int64(0),
							"collector":    []map[string]any{},
						},
					},
				},
			},
		},
	}
}

// RenderJSON returns the profile as JSON bytes.
func (t *Tracer) RenderJSON() ([]byte, error) {
	return json.Marshal(t.Render())
}
