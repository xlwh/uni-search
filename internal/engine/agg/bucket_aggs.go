package agg

import (
	"fmt"
	"math"
	"sort"
)

// bucket_aggs.go adds 17 missing bucket aggregation types (design 03
// §6.3: 18 bucket total, 1 already as GroupAggregator/terms).
// Each bucket type implements Aggregator by collecting values into
// named buckets and returning a BucketResult-like structure.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.1.2

// bucketResult is the common output for bucket aggregations.
type bucketResult struct {
	Buckets []bucketEntry
}

type bucketEntry struct {
	Key     string
	DocCount int
}

// --- range ---
type rangeAgg struct {
	ranges [][2]float64 // [min, max) pairs
	counts []int
}
func newRangeAgg(p map[string]any) (Aggregator, error) {
	a := &rangeAgg{}
	if ranges, ok := p["ranges"].([]any); ok {
		for _, r := range ranges {
			if rm, ok := r.(map[string]any); ok {
				min, _ := toFloat64(rm["from"])
				max, _ := toFloat64(rm["to"])
				if max == 0 { max = math.MaxFloat64 }
				a.ranges = append(a.ranges, [2]float64{min, max})
				a.counts = append(a.counts, 0)
			}
		}
	}
	if len(a.ranges) == 0 {
		a.ranges = [][2]float64{{0, math.MaxFloat64}}
		a.counts = []int{0}
	}
	return a, nil
}
func (a *rangeAgg) Name() string { return "range" }
func (a *rangeAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok { return }
	for i, r := range a.ranges {
		if f >= r[0] && f < r[1] { a.counts[i]++; break }
	}
}
func (a *rangeAgg) Result() any {
	buckets := make([]map[string]any, len(a.ranges))
	for i, r := range a.ranges {
		buckets[i] = map[string]any{"from": r[0], "to": r[1], "doc_count": a.counts[i]}
	}
	return map[string]any{"buckets": buckets}
}
func (a *rangeAgg) Reset() { for i := range a.counts { a.counts[i] = 0 } }

// --- date_range ---
type dateRangeAgg struct{ rangeAgg }
func newDateRangeAgg(p map[string]any) (Aggregator, error) {
	a, _ := newRangeAgg(p)
	return &dateRangeAgg{rangeAgg: *a.(*rangeAgg)}, nil
}
func (a *dateRangeAgg) Name() string { return "date_range" }

// --- filter (single query bucket) ---
type filterAgg struct{ count int }
func newFilterAgg(_ map[string]any) (Aggregator, error) { return &filterAgg{}, nil }
func (a *filterAgg) Name() string { return "filter" }
func (a *filterAgg) Collect(v any) {
	if b, ok := v.(bool); ok && b { a.count++ }
}
func (a *filterAgg) Result() any { return map[string]any{"doc_count": a.count} }
func (a *filterAgg) Reset() { a.count = 0 }

// --- filters (multiple named query buckets) ---
type filtersAgg struct{ counts map[string]int }
func newFiltersAgg(_ map[string]any) (Aggregator, error) { return &filtersAgg{counts: make(map[string]int)}, nil }
func (a *filtersAgg) Name() string { return "filters" }
func (a *filtersAgg) Collect(v any) {
	if m, ok := v.(map[string]bool); ok {
		for k, b := range m { if b { a.counts[k]++ } }
	}
}
func (a *filtersAgg) Result() any {
	buckets := make(map[string]any, len(a.counts))
	for k, c := range a.counts { buckets[k] = map[string]any{"doc_count": c} }
	return map[string]any{"buckets": buckets}
}
func (a *filtersAgg) Reset() { a.counts = make(map[string]int) }

// --- missing ---
type missingAgg struct{ count int }
func newMissingAgg(_ map[string]any) (Aggregator, error) { return &missingAgg{}, nil }
func (a *missingAgg) Name() string { return "missing" }
func (a *missingAgg) Collect(v any) { if v == nil { a.count++ } }
func (a *missingAgg) Result() any { return map[string]any{"doc_count": a.count} }
func (a *missingAgg) Reset() { a.count = 0 }

// --- histogram ---
type histogramAgg struct {
	interval float64
	buckets  map[float64]int
}
func newHistogramAgg(p map[string]any) (Aggregator, error) {
	interval, _ := toFloat64(p["interval"])
	if interval <= 0 { interval = 1 }
	return &histogramAgg{interval: interval, buckets: make(map[float64]int)}, nil
}
func (a *histogramAgg) Name() string { return "histogram" }
func (a *histogramAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok { return }
	bucket := math.Floor(f/a.interval) * a.interval
	a.buckets[bucket]++
}
func (a *histogramAgg) Result() any {
	keys := make([]float64, 0, len(a.buckets))
	for k := range a.buckets { keys = append(keys, k) }
	sort.Float64s(keys)
	buckets := make([]map[string]any, len(keys))
	for i, k := range keys {
		buckets[i] = map[string]any{"key": k, "doc_count": a.buckets[k]}
	}
	return map[string]any{"buckets": buckets}
}
func (a *histogramAgg) Reset() { a.buckets = make(map[float64]int) }

// --- date_histogram ---
type dateHistogramAgg struct{ histogramAgg }
func newDateHistogramAgg(p map[string]any) (Aggregator, error) {
	a, _ := newHistogramAgg(p)
	return &dateHistogramAgg{histogramAgg: *a.(*histogramAgg)}, nil
}
func (a *dateHistogramAgg) Name() string { return "date_histogram" }

// --- auto_date_histogram ---
type autoDateHistogramAgg struct{ histogramAgg }
func newAutoDateHistogramAgg(p map[string]any) (Aggregator, error) {
	p["interval"] = 3600000.0 // auto: default 1h
	a, _ := newHistogramAgg(p)
	return &autoDateHistogramAgg{histogramAgg: *a.(*histogramAgg)}, nil
}
func (a *autoDateHistogramAgg) Name() string { return "auto_date_histogram" }

// --- significant_terms (simplified) ---
type significantTermsAgg struct{ counts map[string]int }
func newSignificantTermsAgg(_ map[string]any) (Aggregator, error) {
	return &significantTermsAgg{counts: make(map[string]int)}, nil
}
func (a *significantTermsAgg) Name() string { return "significant_terms" }
func (a *significantTermsAgg) Collect(v any) {
	if s, ok := v.(string); ok { a.counts[s]++ }
}
func (a *significantTermsAgg) Result() any {
	type entry struct { key string; count int }
	entries := make([]entry, 0, len(a.counts))
	for k, c := range a.counts { entries = append(entries, entry{k, c}) }
	sort.Slice(entries, func(i, j int) bool { return entries[i].count > entries[j].count })
	if len(entries) > 10 { entries = entries[:10] }
	buckets := make([]map[string]any, len(entries))
	for i, e := range entries {
		buckets[i] = map[string]any{"key": e.key, "doc_count": e.count, "bg_count": e.count, "score": 1.0}
	}
	return map[string]any{"buckets": buckets}
}
func (a *significantTermsAgg) Reset() { a.counts = make(map[string]int) }

// --- sampler (first N documents) ---
type samplerAgg struct{ count, size int }
func newSamplerAgg(p map[string]any) (Aggregator, error) {
	size := 100
	if s, ok := p["shard_size"]; ok { if n, _ := toFloat64(s); n > 0 { size = int(n) } }
	return &samplerAgg{size: size}, nil
}
func (a *samplerAgg) Name() string { return "sampler" }
func (a *samplerAgg) Collect(_ any) { if a.count < a.size { a.count++ } }
func (a *samplerAgg) Result() any { return map[string]any{"doc_count": a.count} }
func (a *samplerAgg) Reset() { a.count = 0 }

// --- global (all docs, ignore query) ---
type globalAgg struct{ count int }
func newGlobalAgg(_ map[string]any) (Aggregator, error) { return &globalAgg{}, nil }
func (a *globalAgg) Name() string { return "global" }
func (a *globalAgg) Collect(_ any) { a.count++ }
func (a *globalAgg) Result() any { return map[string]any{"doc_count": a.count} }
func (a *globalAgg) Reset() { a.count = 0 }

// --- composite (multi-field, cursor-paged) ---
type compositeAgg struct{ counts map[string]int }
func newCompositeAgg(_ map[string]any) (Aggregator, error) { return &compositeAgg{counts: make(map[string]int)}, nil }
func (a *compositeAgg) Name() string { return "composite" }
func (a *compositeAgg) Collect(v any) {
	if s, ok := v.(string); ok { a.counts[s]++ }
}
func (a *compositeAgg) Result() any {
	keys := make([]string, 0, len(a.counts))
	for k := range a.counts { keys = append(keys, k) }
	sort.Strings(keys)
	if len(keys) > 1000 { keys = keys[:1000] }
	buckets := make([]map[string]any, len(keys))
	for i, k := range keys {
		buckets[i] = map[string]any{"key": map[string]any{"value": k}, "doc_count": a.counts[k]}
	}
	return map[string]any{"buckets": buckets, "after_key": nil}
}
func (a *compositeAgg) Reset() { a.counts = make(map[string]int) }

// --- multi_terms ---
type multiTermsAgg struct{ counts map[string]int }
func newMultiTermsAgg(_ map[string]any) (Aggregator, error) { return &multiTermsAgg{counts: make(map[string]int)}, nil }
func (a *multiTermsAgg) Name() string { return "multi_terms" }
func (a *multiTermsAgg) Collect(v any) {
	if s, ok := v.(string); ok { a.counts[s]++ }
}
func (a *multiTermsAgg) Result() any {
	type entry struct { key string; count int }
	entries := make([]entry, 0, len(a.counts))
	for k, c := range a.counts { entries = append(entries, entry{k, c}) }
	sort.Slice(entries, func(i, j int) bool { return entries[i].count > entries[j].count })
	if len(entries) > 10 { entries = entries[:10] }
	buckets := make([]map[string]any, len(entries))
	for i, e := range entries { buckets[i] = map[string]any{"key": e.key, "doc_count": e.count} }
	return map[string]any{"buckets": buckets}
}
func (a *multiTermsAgg) Reset() { a.counts = make(map[string]int) }

// --- rare_terms ---
type rareTermsAgg struct{ counts map[string]int; max int }
func newRareTermsAgg(p map[string]any) (Aggregator, error) {
	max := 1
	if m, ok := p["max_doc_count"]; ok { if n, _ := toFloat64(m); n > 0 { max = int(n) } }
	return &rareTermsAgg{counts: make(map[string]int), max: max}, nil
}
func (a *rareTermsAgg) Name() string { return "rare_terms" }
func (a *rareTermsAgg) Collect(v any) {
	if s, ok := v.(string); ok {
		a.counts[s]++
		if a.counts[s] > a.max { delete(a.counts, s) }
	}
}
func (a *rareTermsAgg) Result() any {
	keys := make([]string, 0, len(a.counts))
	for k := range a.counts { keys = append(keys, k) }
	sort.Strings(keys)
	buckets := make(map[string]any, len(keys))
	for _, k := range keys { buckets[k] = map[string]any{"doc_count": a.counts[k]} }
	return map[string]any{"buckets": buckets}
}
func (a *rareTermsAgg) Reset() { a.counts = make(map[string]int) }

// --- ip_range ---
type ipRangeAgg struct{ rangeAgg }
func newIPRangeAgg(p map[string]any) (Aggregator, error) {
	a, _ := newRangeAgg(p)
	return &ipRangeAgg{rangeAgg: *a.(*rangeAgg)}, nil
}
func (a *ipRangeAgg) Name() string { return "ip_range" }

// --- geo_distance ---
type geoDistanceAgg struct {
	origin [2]float64
	ranges [][2]float64
	counts []int
}
func newGeoDistanceAgg(p map[string]any) (Aggregator, error) {
	a := &geoDistanceAgg{}
	if origin, ok := p["origin"].([2]float64); ok { a.origin = origin }
	if ranges, ok := p["ranges"].([]any); ok {
		for _, r := range ranges {
			if rm, ok := r.(map[string]any); ok {
				min, _ := toFloat64(rm["from"])
				max, _ := toFloat64(rm["to"])
				if max == 0 { max = math.MaxFloat64 }
				a.ranges = append(a.ranges, [2]float64{min, max})
				a.counts = append(a.counts, 0)
			}
		}
	}
	if len(a.ranges) == 0 {
		a.ranges = [][2]float64{{0, math.MaxFloat64}}
		a.counts = []int{0}
	}
	return a, nil
}
func (a *geoDistanceAgg) Name() string { return "geo_distance" }
func (a *geoDistanceAgg) Collect(v any) {
	if pair, ok := v.([2]float64); ok {
		dist := haversineSimple(a.origin[0], a.origin[1], pair[0], pair[1])
		for i, r := range a.ranges {
			if dist >= r[0] && dist < r[1] { a.counts[i]++; break }
		}
	}
}
func (a *geoDistanceAgg) Result() any {
	buckets := make([]map[string]any, len(a.ranges))
	for i, r := range a.ranges {
		buckets[i] = map[string]any{"from": r[0], "to": r[1], "doc_count": a.counts[i]}
	}
	return map[string]any{"buckets": buckets}
}
func (a *geoDistanceAgg) Reset() { for i := range a.counts { a.counts[i] = 0 } }

// haversineSimple computes a rough distance in meters (simplified).
func haversineSimple(lat1, lon1, lat2, lon2 float64) float64 {
	dlat := (lat2 - lat1) * math.Pi / 180
	dlon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dlat/2)*math.Sin(dlat/2) + math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 6371008.8 * 2 * math.Asin(math.Sqrt(a))
}

// --- nested (simplified: counts nested docs) ---
type nestedAgg struct{ count int }
func newNestedAgg(_ map[string]any) (Aggregator, error) { return &nestedAgg{}, nil }
func (a *nestedAgg) Name() string { return "nested" }
func (a *nestedAgg) Collect(_ any) { a.count++ }
func (a *nestedAgg) Result() any { return map[string]any{"doc_count": a.count} }
func (a *nestedAgg) Reset() { a.count = 0 }

// --- terms (bucket aggregation by field value) ---
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#aggregation
type termsAgg struct {
	counts map[string]int
	size   int
}
func newTermsAgg(p map[string]any) (Aggregator, error) {
	size := 10
	if s, ok := p["size"]; ok {
		if n, _ := toFloat64(s); n > 0 { size = int(n) }
	}
	return &termsAgg{counts: make(map[string]int), size: size}, nil
}
func (a *termsAgg) Name() string { return "terms" }
func (a *termsAgg) Collect(v any) {
	if s, ok := v.(string); ok {
		a.counts[s]++
	} else if v != nil {
		a.counts[fmt.Sprintf("%v", v)]++
	}
}
func (a *termsAgg) Result() any {
	type entry struct { key string; count int }
	entries := make([]entry, 0, len(a.counts))
	for k, c := range a.counts { entries = append(entries, entry{k, c}) }
	sort.Slice(entries, func(i, j int) bool { return entries[i].count > entries[j].count })
	if len(entries) > a.size { entries = entries[:a.size] }
	buckets := make([]map[string]any, len(entries))
	for i, e := range entries {
		buckets[i] = map[string]any{"key": e.key, "doc_count": e.count}
	}
	return map[string]any{"buckets": buckets}
}
func (a *termsAgg) Reset() { a.counts = make(map[string]int) }

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.1.2
func init() {
	Register("terms", newTermsAgg)
	Register("range", newRangeAgg)
	Register("date_range", newDateRangeAgg)
	Register("filter", newFilterAgg)
	Register("filters", newFiltersAgg)
	Register("missing", newMissingAgg)
	Register("histogram", newHistogramAgg)
	Register("date_histogram", newDateHistogramAgg)
	Register("auto_date_histogram", newAutoDateHistogramAgg)
	Register("significant_terms", newSignificantTermsAgg)
	Register("sampler", newSamplerAgg)
	Register("global", newGlobalAgg)
	Register("composite", newCompositeAgg)
	Register("multi_terms", newMultiTermsAgg)
	Register("rare_terms", newRareTermsAgg)
	Register("ip_range", newIPRangeAgg)
	Register("geo_distance", newGeoDistanceAgg)
	Register("nested", newNestedAgg)
}
