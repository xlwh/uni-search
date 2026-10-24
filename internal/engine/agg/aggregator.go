// Package agg implements the M1 aggregation engine (design
// 03-query-engine.md §6). It provides vectorized grouping aggregation
// with a flat open-addressing hash table, SIMD column-batch reduction,
// and the core M1 metric aggregations: sum, count, min, max, avg,
// and cardinality (HLL++). Additional metric types (percentiles via
// t-digest, stats, etc.) and pipeline aggregations build on the same
// Aggregator interface.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.3
package agg

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// Aggregator is the interface every metric aggregation implements.
// Collect is called once per matching document; Result returns the
// final aggregation value as a JSON-compatible Go type.
type Aggregator interface {
	// Collect adds one value to the aggregation. The value is the
	// document's field value (or nil when the field is missing).
	Collect(value any)
	// Result returns the final aggregated value.
	Result() any
	// Reset clears the aggregator for reuse.
	Reset()
	// Name returns the aggregation type name (e.g. "sum", "count").
	Name() string
}

// AggregatorFactory constructs an Aggregator from ES aggregation
// parameters. The DSL translator calls the factory matching the
// aggregation type.
type AggregatorFactory func(params map[string]any) (Aggregator, error)

// registry maps aggregation type names to factories.
var registry = make(map[string]AggregatorFactory)

// Register installs an aggregation factory.
func Register(name string, factory AggregatorFactory) {
	registry[name] = factory
}

// NewAggregator looks up and constructs an aggregator by type name.
func NewAggregator(name string, params map[string]any) (Aggregator, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("agg: unknown aggregation type %q", name)
	}
	return f(params)
}

// ---------------------------------------------------------------------------
// Core metric aggregations: sum, count, min, max, avg, cardinality
// ---------------------------------------------------------------------------

// SumAgg computes the sum of numeric values.
type SumAgg struct {
	sum   float64
	count int
}

func newSumAgg(_ map[string]any) (Aggregator, error) { return &SumAgg{}, nil }

func (a *SumAgg) Collect(value any) {
	n, ok := toFloat(value)
	if !ok {
		return
	}
	a.sum += n
	a.count++
}
func (a *SumAgg) Result() any  { return a.sum }
func (a *SumAgg) Reset()       { a.sum = 0; a.count = 0 }
func (a *SumAgg) Name() string { return "sum" }

// CountAgg counts non-null values.
type CountAgg struct {
	count int
}

func newCountAgg(_ map[string]any) (Aggregator, error) { return &CountAgg{}, nil }

func (a *CountAgg) Collect(value any) {
	if value != nil {
		a.count++
	}
}
func (a *CountAgg) Result() any  { return float64(a.count) }
func (a *CountAgg) Reset()       { a.count = 0 }
func (a *CountAgg) Name() string { return "count" }

// MinAgg tracks the minimum value.
type MinAgg struct {
	min   float64
	count int
}

func newMinAgg(_ map[string]any) (Aggregator, error) { return &MinAgg{}, nil }

func (a *MinAgg) Collect(value any) {
	n, ok := toFloat(value)
	if !ok {
		return
	}
	if a.count == 0 || n < a.min {
		a.min = n
	}
	a.count++
}
func (a *MinAgg) Result() any {
	if a.count == 0 {
		return nil
	}
	return a.min
}
func (a *MinAgg) Reset()       { a.min = 0; a.count = 0 }
func (a *MinAgg) Name() string { return "min" }

// MaxAgg tracks the maximum value.
type MaxAgg struct {
	max   float64
	count int
}

func newMaxAgg(_ map[string]any) (Aggregator, error) { return &MaxAgg{}, nil }

func (a *MaxAgg) Collect(value any) {
	n, ok := toFloat(value)
	if !ok {
		return
	}
	if a.count == 0 || n > a.max {
		a.max = n
	}
	a.count++
}
func (a *MaxAgg) Result() any {
	if a.count == 0 {
		return nil
	}
	return a.max
}
func (a *MaxAgg) Reset()       { a.max = 0; a.count = 0 }
func (a *MaxAgg) Name() string { return "max" }

// AvgAgg computes the arithmetic mean.
type AvgAgg struct {
	sum   float64
	count int
}

func newAvgAgg(_ map[string]any) (Aggregator, error) { return &AvgAgg{}, nil }

func (a *AvgAgg) Collect(value any) {
	n, ok := toFloat(value)
	if !ok {
		return
	}
	a.sum += n
	a.count++
}
func (a *AvgAgg) Result() any {
	if a.count == 0 {
		return nil
	}
	return a.sum / float64(a.count)
}
func (a *AvgAgg) Reset()       { a.sum = 0; a.count = 0 }
func (a *AvgAgg) Name() string { return "avg" }

// CardinalityAgg uses HLL++ for approximate distinct count.
type CardinalityAgg struct {
	hll        *HLL
	precision int
}

func newCardinalityAgg(params map[string]any) (Aggregator, error) {
	precision := 14 // ES default
	if p, ok := params["precision_threshold"]; ok {
		if pf, ok := toFloat(p); ok && pf > 0 {
			// Map precision_threshold to HLL precision.
			precision = int(math.Log2(pf))
			if precision < 4 {
				precision = 4
			}
			if precision > 18 {
				precision = 18
			}
		}
	}
	return &CardinalityAgg{hll: NewHLL(precision), precision: precision}, nil
}

func (a *CardinalityAgg) Collect(value any) {
	if value == nil {
		return
	}
	h := hashValue(value)
	a.hll.Add(h)
}
func (a *CardinalityAgg) Result() any  { return float64(a.hll.Estimate()) }
func (a *CardinalityAgg) Reset()       { a.hll = NewHLL(a.precision) }
func (a *CardinalityAgg) Name() string { return "cardinality" }

// StatsAgg computes sum, count, min, max, avg in one pass.
type StatsAgg struct {
	sum   float64
	count int
	min   float64
	max   float64
}

func newStatsAgg(_ map[string]any) (Aggregator, error) { return &StatsAgg{}, nil }

func (a *StatsAgg) Collect(value any) {
	n, ok := toFloat(value)
	if !ok {
		return
	}
	if a.count == 0 {
		a.min = n
		a.max = n
	} else {
		if n < a.min {
			a.min = n
		}
		if n > a.max {
			a.max = n
		}
	}
	a.sum += n
	a.count++
}
func (a *StatsAgg) Result() any {
	if a.count == 0 {
		return nil
	}
	return map[string]any{
		"count": float64(a.count),
		"sum":   a.sum,
		"min":   a.min,
		"max":   a.max,
		"avg":   a.sum / float64(a.count),
	}
}
func (a *StatsAgg) Reset()       { a.sum = 0; a.count = 0; a.min = 0; a.max = 0 }
func (a *StatsAgg) Name() string { return "stats" }

// ValueCountAgg counts all values including nulls (doc_count).
type ValueCountAgg struct {
	count int
}

func newValueCountAgg(_ map[string]any) (Aggregator, error) { return &ValueCountAgg{}, nil }

func (a *ValueCountAgg) Collect(value any) {
	a.count++ // counts every document, even with null field
}
func (a *ValueCountAgg) Result() any  { return float64(a.count) }
func (a *ValueCountAgg) Reset()       { a.count = 0 }
func (a *ValueCountAgg) Name() string { return "value_count" }

// ---------------------------------------------------------------------------
// Group aggregator (terms bucket aggregation)
// ---------------------------------------------------------------------------

// GroupAggregator groups documents by a field value and runs metric
// aggregations per group. Uses a specialized AggHashTable for O(1) group
// lookup with type-aware key handling. @spec: 02_spec.md#4.3
type GroupAggregator struct {
	groupField string
	aggDefs    []AggDef
	ht         AggHashTable
}

// AggDef defines one sub-aggregation within a group.
type AggDef struct {
	Name   string
	Type   string
	Params map[string]any
}

// groupBucket holds one group's aggregators.
type groupBucket struct {
	key      string
	aggs     []Aggregator
	docCount int
}

// NewGroupAggregator creates a terms-style group aggregation with the
// default Go map backend (backward compatible). @spec: 02_spec.md#4.3
func NewGroupAggregator(groupField string, defs []AggDef) *GroupAggregator {
	return &GroupAggregator{
		groupField: groupField,
		aggDefs:    defs,
		ht:         newGoMapBackend(defs),
	}
}

// NewGroupAggregatorWithKeyType creates a group aggregation with a
// specialized hash table selected by key type and estimated group count.
// @spec: 02_spec.md#4.4
func NewGroupAggregatorWithKeyType(groupField string, keyType meta.FieldType, defs []AggDef, estimatedGroups int64) *GroupAggregator {
	return &GroupAggregator{
		groupField: groupField,
		aggDefs:    defs,
		ht:         SelectHashTable(keyType, false, estimatedGroups, defs),
	}
}

// Collect adds a document to the appropriate group. groupKey is the
// group key value (string for keyword/text, int64 for long/date/ip);
// fieldValues maps sub-agg field names to values. @spec: 02_spec.md#4.3
func (g *GroupAggregator) Collect(groupKey any, fieldValues map[string]any) {
	bucket, _ := g.ht.FindOrInsert(groupKey)
	for i, def := range g.aggDefs {
		if i >= len(bucket.aggs) || bucket.aggs[i] == nil {
			continue
		}
		bucket.aggs[i].Collect(fieldValues[def.Name])
	}
	bucket.docCount++
}

// CollectString is a convenience method for string keys (backward compatible
// with existing callers that pass string group keys).
func (g *GroupAggregator) CollectString(groupKey string, fieldValues map[string]any) {
	g.Collect(groupKey, fieldValues)
}

// Buckets returns the group results sorted by descending doc_count
// (ES terms aggregation default order). @spec: 02_spec.md#4.3
func (g *GroupAggregator) Buckets() []BucketResult {
	out := make([]BucketResult, 0, g.ht.Len())
	g.ht.Iterate(func(key any, bucket *groupBucket) bool {
		result := BucketResult{Key: bucket.key}
		result.SubAggs = make(map[string]any, len(bucket.aggs))
		for i, def := range g.aggDefs {
			if i < len(bucket.aggs) && bucket.aggs[i] != nil {
				result.SubAggs[def.Name] = bucket.aggs[i].Result()
			}
		}
		result.DocCount = float64(bucket.docCount)
		out = append(out, result)
		return true
	})
	// Sort by descending doc_count, then by key ascending.
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocCount != out[j].DocCount {
			return out[i].DocCount > out[j].DocCount
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Reset clears all groups.
func (g *GroupAggregator) Reset() {
	g.ht.Reset()
}

// BucketResult is one group's result.
type BucketResult struct {
	Key     string
	DocCount float64
	SubAggs  map[string]any
}

// MarshalJSON renders a bucket as ES-style JSON.
func (b BucketResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"key":       b.Key,
		"doc_count": b.DocCount,
		"aggregations": b.SubAggs,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// toFloat converts a Go value to float64. Returns false for non-numeric.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case nil:
		return 0, false
	default:
		return 0, false
	}
}

// hashValue produces a 64-bit hash for cardinality estimation.
// Uses FNV-1a for simplicity; M2 can swap to xxhash.
func hashValue(v any) uint64 {
	s := fmt.Sprintf("%v", v)
	var h uint64 = 14695981039346656037
	for _, b := range []byte(s) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}

// init registers the core aggregations.
func init() {
	Register("sum", newSumAgg)
	Register("count", newCountAgg)
	Register("min", newMinAgg)
	Register("max", newMaxAgg)
	Register("avg", newAvgAgg)
	Register("stats", newStatsAgg)
	Register("value_count", newValueCountAgg)
	Register("cardinality", newCardinalityAgg)
}
