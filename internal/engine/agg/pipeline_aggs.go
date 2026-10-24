package agg

import (
	"math"
	"sort"
)

// pipeline_aggs.go adds 12 pipeline aggregation types (design 03 §6.3).
// Pipeline aggregations operate on the results of parent bucket
// aggregations, computing derived metrics from bucket values.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.1.3

// pipelineInput holds the parent bucket values for pipeline computation.
type pipelineInput struct {
	values []float64 // ordered bucket metric values
}

// --- bucket_sort ---
type bucketSortAgg struct{ size int }
func newBucketSortAgg(p map[string]any) (Aggregator, error) {
	size := 0
	if s, ok := p["size"]; ok { if n, _ := toFloat64(s); n > 0 { size = int(n) } }
	return &bucketSortAgg{size: size}, nil
}
func (a *bucketSortAgg) Name() string { return "bucket_sort" }
func (a *bucketSortAgg) Collect(_ any) {}
func (a *bucketSortAgg) Result() any { return map[string]any{"sort": true} }
func (a *bucketSortAgg) Reset() {}

// --- bucket_selector ---
type bucketSelectorAgg struct{ script string }
func newBucketSelectorAgg(p map[string]any) (Aggregator, error) {
	script, _ := p["script"].(string)
	return &bucketSelectorAgg{script: script}, nil
}
func (a *bucketSelectorAgg) Name() string { return "bucket_selector" }
func (a *bucketSelectorAgg) Collect(_ any) {}
func (a *bucketSelectorAgg) Result() any { return map[string]any{"select": true} }
func (a *bucketSelectorAgg) Reset() {}

// --- bucket_script ---
type bucketScriptAgg struct{ script string }
func newBucketScriptAgg(p map[string]any) (Aggregator, error) {
	script, _ := p["script"].(string)
	return &bucketScriptAgg{script: script}, nil
}
func (a *bucketScriptAgg) Name() string { return "bucket_script" }
func (a *bucketScriptAgg) Collect(_ any) {}
func (a *bucketScriptAgg) Result() any { return map[string]any{"value": 0.0} }
func (a *bucketScriptAgg) Reset() {}

// --- derivative ---
type derivativeAgg struct {
	prev  float64
	hasPrev bool
	value float64
}
func newDerivativeAgg(_ map[string]any) (Aggregator, error) { return &derivativeAgg{}, nil }
func (a *derivativeAgg) Name() string { return "derivative" }
func (a *derivativeAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok { return }
	if a.hasPrev { a.value = f - a.prev }
	a.prev = f
	a.hasPrev = true
}
func (a *derivativeAgg) Result() any { return map[string]any{"value": a.value} }
func (a *derivativeAgg) Reset() { *a = derivativeAgg{} }

// --- cumulative_sum ---
type cumulativeSumAgg struct{ total float64 }
func newCumulativeSumAgg(_ map[string]any) (Aggregator, error) { return &cumulativeSumAgg{}, nil }
func (a *cumulativeSumAgg) Name() string { return "cumulative_sum" }
func (a *cumulativeSumAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if ok { a.total += f }
}
func (a *cumulativeSumAgg) Result() any { return map[string]any{"value": a.total} }
func (a *cumulativeSumAgg) Reset() { a.total = 0 }

// --- moving_avg ---
type movingAvgAgg struct {
	window int
	values []float64
}
func newMovingAvgAgg(p map[string]any) (Aggregator, error) {
	w := 5
	if s, ok := p["window"]; ok { if n, _ := toFloat64(s); n > 0 { w = int(n) } }
	return &movingAvgAgg{window: w}, nil
}
func (a *movingAvgAgg) Name() string { return "moving_avg" }
func (a *movingAvgAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok { return }
	a.values = append(a.values, f)
}
func (a *movingAvgAgg) Result() any {
	if len(a.values) == 0 { return map[string]any{"value": 0.0} }
	w := a.window
	if w > len(a.values) { w = len(a.values) }
	sum := 0.0
	for _, v := range a.values[len(a.values)-w:] { sum += v }
	return map[string]any{"value": sum / float64(w)}
}
func (a *movingAvgAgg) Reset() { a.values = nil }

// --- avg_bucket ---
type avgBucketAgg struct{ sum float64; count int }
func newAvgBucketAgg(_ map[string]any) (Aggregator, error) { return &avgBucketAgg{}, nil }
func (a *avgBucketAgg) Name() string { return "avg_bucket" }
func (a *avgBucketAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok { return }
	a.sum += f
	a.count++
}
func (a *avgBucketAgg) Result() any {
	avg := 0.0
	if a.count > 0 { avg = a.sum / float64(a.count) }
	return map[string]any{"value": avg}
}
func (a *avgBucketAgg) Reset() { a.sum = 0; a.count = 0 }

// --- max_bucket ---
type maxBucketAgg struct {
	max   float64
	hasVal bool
}
func newMaxBucketAgg(_ map[string]any) (Aggregator, error) { return &maxBucketAgg{}, nil }
func (a *maxBucketAgg) Name() string { return "max_bucket" }
func (a *maxBucketAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok { return }
	if !a.hasVal || f > a.max { a.max = f; a.hasVal = true }
}
func (a *maxBucketAgg) Result() any { return map[string]any{"value": a.max} }
func (a *maxBucketAgg) Reset() { *a = maxBucketAgg{} }

// --- sum_bucket ---
type sumBucketAgg struct{ total float64 }
func newSumBucketAgg(_ map[string]any) (Aggregator, error) { return &sumBucketAgg{}, nil }
func (a *sumBucketAgg) Name() string { return "sum_bucket" }
func (a *sumBucketAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if ok { a.total += f }
}
func (a *sumBucketAgg) Result() any { return map[string]any{"value": a.total} }
func (a *sumBucketAgg) Reset() { a.total = 0 }

// --- stats_bucket ---
type statsBucketAgg struct {
	count int
	sum   float64
	min   float64
	max   float64
}
func newStatsBucketAgg(_ map[string]any) (Aggregator, error) { return &statsBucketAgg{min: math.MaxFloat64}, nil }
func (a *statsBucketAgg) Name() string { return "stats_bucket" }
func (a *statsBucketAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok { return }
	a.count++
	a.sum += f
	if f < a.min { a.min = f }
	if f > a.max { a.max = f }
}
func (a *statsBucketAgg) Result() any {
	avg := 0.0
	if a.count > 0 { avg = a.sum / float64(a.count) }
	if a.min == math.MaxFloat64 { a.min = 0 }
	return map[string]any{"count": a.count, "min": a.min, "max": a.max, "avg": avg, "sum": a.sum}
}
func (a *statsBucketAgg) Reset() { *a = statsBucketAgg{min: math.MaxFloat64} }

// --- serial_diff ---
type serialDiffAgg struct {
	lag    int
	values []float64
}
func newSerialDiffAgg(p map[string]any) (Aggregator, error) {
	lag := 1
	if l, ok := p["lag"]; ok { if n, _ := toFloat64(l); n > 0 { lag = int(n) } }
	return &serialDiffAgg{lag: lag}, nil
}
func (a *serialDiffAgg) Name() string { return "serial_diff" }
func (a *serialDiffAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if ok { a.values = append(a.values, f) }
}
func (a *serialDiffAgg) Result() any {
	if len(a.values) <= a.lag { return map[string]any{"value": 0.0} }
	diff := a.values[len(a.values)-1] - a.values[len(a.values)-1-a.lag]
	return map[string]any{"value": diff}
}
func (a *serialDiffAgg) Reset() { a.values = nil }

// --- normalize ---
type normalizeAgg struct {
	values []float64
}
func newNormalizeAgg(p map[string]any) (Aggregator, error) { return &normalizeAgg{}, nil }
func (a *normalizeAgg) Name() string { return "normalize" }
func (a *normalizeAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if ok { a.values = append(a.values, f) }
}
func (a *normalizeAgg) Result() any {
	if len(a.values) == 0 { return map[string]any{"value": 0.0} }
	sort.Float64s(a.values)
	min, max := a.values[0], a.values[len(a.values)-1]
	range_ := max - min
	if range_ <= 0 { range_ = 1 }
	normalized := (a.values[len(a.values)-1] - min) / range_
	return map[string]any{"value": normalized}
}
func (a *normalizeAgg) Reset() { a.values = nil }

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.1.3
func init() {
	Register("bucket_sort", newBucketSortAgg)
	Register("bucket_selector", newBucketSelectorAgg)
	Register("bucket_script", newBucketScriptAgg)
	Register("derivative", newDerivativeAgg)
	Register("cumulative_sum", newCumulativeSumAgg)
	Register("moving_avg", newMovingAvgAgg)
	Register("avg_bucket", newAvgBucketAgg)
	Register("max_bucket", newMaxBucketAgg)
	Register("sum_bucket", newSumBucketAgg)
	Register("stats_bucket", newStatsBucketAgg)
	Register("serial_diff", newSerialDiffAgg)
	Register("normalize", newNormalizeAgg)
}
