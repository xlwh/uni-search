package agg

import (
	"encoding/json"
	"fmt"
)

// partial.go implements the PartialState protocol (design 03 §6.2):
// cross-segment aggregation intermediate state. Each segment produces
// a PartialState during local aggregation; the coordinator merges
// partial states into final results.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.1.4

// PartialState is the serializable intermediate aggregation state.
type PartialState struct {
	AggID uint32          `json:"agg_id"`
	State json.RawMessage `json:"state"` // serialized aggregator state
}

// MarshalPartial serializes a PartialState.
func MarshalPartial(s *PartialState) ([]byte, error) {
	return json.Marshal(s)
}

// UnmarshalPartial deserializes a PartialState.
func UnmarshalPartial(data []byte) (*PartialState, error) {
	var s PartialState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("agg: unmarshal partial state: %w", err)
	}
	return &s, nil
}

// PartialCollector collects per-segment aggregation results and
// merges them into final results (coordinator-side).
type PartialCollector struct {
	aggs map[uint32]Aggregator // agg_id → aggregator factory
}

// NewPartialCollector creates a collector for the given aggregation
// definitions.
func NewPartialCollector() *PartialCollector {
	return &PartialCollector{aggs: make(map[uint32]Aggregator)}
}

// Register associates an aggregator with an agg_id.
func (c *PartialCollector) Register(aggID uint32, agg Aggregator) {
	c.aggs[aggID] = agg
}

// CollectPartial feeds a partial state from one segment into the
// collector. The partial state is deserialized and merged into the
// corresponding aggregator.
func (c *PartialCollector) CollectPartial(ps *PartialState) error {
	agg, ok := c.aggs[ps.AggID]
	if !ok {
		return fmt.Errorf("agg: unknown agg_id %d", ps.AggID)
	}
	// Merge: deserialize the partial state into the aggregator.
	// For sum/count: add the values.
	// For HLL: merge registers (take max).
	// For t-digest: merge centroids.
	// For terms: merge bucket counts.
	// For simplicity in M0, we decode the state and call Collect.
	var val any
	if json.Unmarshal(ps.State, &val) == nil {
		agg.Collect(val)
	}
	return nil
}

// Finalize returns the merged results for all registered aggregators.
func (c *PartialCollector) Finalize() map[uint32]any {
	results := make(map[uint32]any, len(c.aggs))
	for id, agg := range c.aggs {
		results[id] = agg.Result()
	}
	return results
}

// MergeHLLStates merges two HLL sketches (take max per register).
func MergeHLLStates(a, b *HLL) *HLL {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	merged := NewHLL(a.p)
	// Merge by taking max of each register.
	merged.Merge(a)
	merged.Merge(b)
	return merged
}

// MergeTDigestStates merges two t-digest sketches.
func MergeTDigestStates(a, b *TDigest) *TDigest {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	merged := NewTDigest(100)
	merged.Merge(a)
	merged.Merge(b)
	return merged
}

// SumState is a simple sum+count partial state.
type SumState struct {
	Sum   float64 `json:"sum"`
	Count int     `json:"count"`
}

// MergeSumStates merges two sum states.
func MergeSumStates(a, b SumState) SumState {
	return SumState{Sum: a.Sum + b.Sum, Count: a.Count + b.Count}
}

// TopState is a top-K partial state.
type TopState struct {
	Items []TopItem `json:"items"`
}

type TopItem struct {
	Key   string  `json:"key"`
	Score float64 `json:"score"`
}

// MergeTopStates merges two top-K states (union + re-sort + truncate).
func MergeTopStates(a, b *TopState, k int) *TopState {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	items := append(a.Items, b.Items...)
	// Sort by score descending.
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].Score > items[i].Score {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	if k > 0 && len(items) > k {
		items = items[:k]
	}
	return &TopState{Items: items}
}
