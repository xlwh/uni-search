package agg

import (
	"math"
	"sort"
)

// extended_aggs.go adds 10 missing metric aggregation types (design 03
// §6.3: 17 metric total, 8 already in aggregator.go).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.1.1

// toFloat64 converts an any value to float64.
func toFloat64(v any) (float64, bool) {
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
	default:
		return 0, false
	}
}

// --- extended_stats ---
type extendedStatsAgg struct {
	count        int
	sum          float64
	min          float64
	max          float64
	sumOfSquares float64
	variance     float64
	stdDev       float64
}

func newExtendedStatsAgg(_ map[string]any) (Aggregator, error) { return &extendedStatsAgg{}, nil }
func (a *extendedStatsAgg) Name() string                       { return "extended_stats" }
func (a *extendedStatsAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok {
		return
	}
	a.count++
	a.sum += f
	a.sumOfSquares += f * f
	if a.count == 1 || f < a.min {
		a.min = f
	}
	if a.count == 1 || f > a.max {
		a.max = f
	}
}
func (a *extendedStatsAgg) Result() any {
	avg := 0.0
	if a.count > 0 {
		avg = a.sum / float64(a.count)
	}
	if a.count > 1 {
		a.variance = (a.sumOfSquares - float64(a.count)*avg*avg) / float64(a.count-1)
		a.stdDev = math.Sqrt(a.variance)
	}
	return map[string]any{
		"count": a.count, "min": a.min, "max": a.max, "avg": avg, "sum": a.sum,
		"sum_of_squares": a.sumOfSquares, "variance": a.variance, "std_deviation": a.stdDev,
	}
}
func (a *extendedStatsAgg) Reset() { *a = extendedStatsAgg{} }

// --- percentiles ---
type percentilesAgg struct {
	td       *TDigest
	percents []float64
}

func newPercentilesAgg(_ map[string]any) (Aggregator, error) {
	return &percentilesAgg{td: NewTDigest(100), percents: []float64{1, 5, 25, 50, 75, 95, 99}}, nil
}
func (a *percentilesAgg) Name() string { return "percentiles" }
func (a *percentilesAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok {
		return
	}
	a.td.AddWeighted(f, 1)
}
func (a *percentilesAgg) Result() any {
	values := make(map[string]any, len(a.percents))
	for _, p := range a.percents {
		values[fmtPercent(p)] = a.td.Quantile(p / 100)
	}
	return map[string]any{"values": values}
}
func (a *percentilesAgg) Reset() { a.td = NewTDigest(100) }

// --- percentile_ranks ---
type percentileRanksAgg struct {
	values []float64
}

func newPercentileRanksAgg(_ map[string]any) (Aggregator, error) { return &percentileRanksAgg{}, nil }
func (a *percentileRanksAgg) Name() string                       { return "percentile_ranks" }
func (a *percentileRanksAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok {
		return
	}
	a.values = append(a.values, f)
}
func (a *percentileRanksAgg) Result() any {
	sort.Float64s(a.values)
	n := len(a.values)
	result := make(map[string]any)
	for _, target := range []float64{1, 5, 25, 50, 75, 95, 99} {
		rank := 0.0
		for _, v := range a.values {
			if v <= target {
				rank++
			}
		}
		if n > 0 {
			rank = rank / float64(n) * 100
		}
		result[fmtPercent(target)] = rank
	}
	return map[string]any{"values": result}
}
func (a *percentileRanksAgg) Reset() { a.values = nil }

// --- top_hits ---
type topHitsAgg struct {
	hits []topHitEntry
	size int
}
type topHitEntry struct {
	score  float64
	source []byte
	id     string
}

func newTopHitsAgg(_ map[string]any) (Aggregator, error) { return &topHitsAgg{size: 10}, nil }
func (a *topHitsAgg) Name() string                       { return "top_hits" }
func (a *topHitsAgg) Collect(v any) {
	if entry, ok := v.(topHitEntry); ok {
		a.hits = append(a.hits, entry)
		sort.Slice(a.hits, func(i, j int) bool { return a.hits[i].score > a.hits[j].score })
		if len(a.hits) > a.size {
			a.hits = a.hits[:a.size]
		}
	}
}
func (a *topHitsAgg) Result() any {
	hitsList := make([]map[string]any, 0, len(a.hits))
	for _, h := range a.hits {
		hitsList = append(hitsList, map[string]any{"_id": h.id, "_score": h.score, "_source": h.source})
	}
	return map[string]any{"hits": map[string]any{
		"total": map[string]any{"value": len(a.hits), "relation": "eq"}, "hits": hitsList,
	}}
}
func (a *topHitsAgg) Reset() { a.hits = nil }

// --- weighted_avg ---
type weightedAvgAgg struct {
	sum  float64
	wsum float64
}

func newWeightedAvgAgg(_ map[string]any) (Aggregator, error) { return &weightedAvgAgg{}, nil }
func (a *weightedAvgAgg) Name() string                       { return "weighted_avg" }
func (a *weightedAvgAgg) Collect(v any) {
	if pair, ok := v.([2]float64); ok {
		a.sum += pair[0] * pair[1]
		a.wsum += pair[1]
	}
}
func (a *weightedAvgAgg) Result() any {
	avg := 0.0
	if a.wsum != 0 {
		avg = a.sum / a.wsum
	}
	return map[string]any{"value": avg}
}
func (a *weightedAvgAgg) Reset() { *a = weightedAvgAgg{} }

// --- median_absolute_deviation ---
type madAgg struct {
	values []float64
}

func newMADAgg(_ map[string]any) (Aggregator, error) { return &madAgg{}, nil }
func (a *madAgg) Name() string                       { return "median_absolute_deviation" }
func (a *madAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok {
		return
	}
	a.values = append(a.values, f)
}
func (a *madAgg) Result() any {
	if len(a.values) == 0 {
		return map[string]any{"value": 0.0}
	}
	sort.Float64s(a.values)
	median := a.values[len(a.values)/2]
	devs := make([]float64, len(a.values))
	for i, v := range a.values {
		devs[i] = math.Abs(v - median)
	}
	sort.Float64s(devs)
	return map[string]any{"value": devs[len(devs)/2]}
}
func (a *madAgg) Reset() { a.values = nil }

// --- geo_centroid ---
type geoCentroidAgg struct {
	latSum, lonSum float64
	count          int
}

func newGeoCentroidAgg(_ map[string]any) (Aggregator, error) { return &geoCentroidAgg{}, nil }
func (a *geoCentroidAgg) Name() string                       { return "geo_centroid" }
func (a *geoCentroidAgg) Collect(v any) {
	if pair, ok := v.([2]float64); ok {
		a.latSum += pair[0]
		a.lonSum += pair[1]
		a.count++
	}
}
func (a *geoCentroidAgg) Result() any {
	if a.count == 0 {
		return map[string]any{"location": nil}
	}
	return map[string]any{
		"location": map[string]any{"lat": a.latSum / float64(a.count), "lon": a.lonSum / float64(a.count)},
		"count":    a.count,
	}
}
func (a *geoCentroidAgg) Reset() { *a = geoCentroidAgg{} }

// --- geo_bounds ---
type geoBoundsAgg struct {
	top, bottom, left, right float64
	hasData                  bool
}

func newGeoBoundsAgg(_ map[string]any) (Aggregator, error) {
	return &geoBoundsAgg{top: -90, bottom: 90, left: 180, right: -180}, nil
}
func (a *geoBoundsAgg) Name() string { return "geo_bounds" }
func (a *geoBoundsAgg) Collect(v any) {
	if pair, ok := v.([2]float64); ok {
		lat, lon := pair[0], pair[1]
		if lat > a.top {
			a.top = lat
		}
		if lat < a.bottom {
			a.bottom = lat
		}
		if lon < a.left {
			a.left = lon
		}
		if lon > a.right {
			a.right = lon
		}
		a.hasData = true
	}
}
func (a *geoBoundsAgg) Result() any {
	if !a.hasData {
		return map[string]any{"bounds": nil}
	}
	return map[string]any{"bounds": map[string]any{
		"top_left":     map[string]any{"lat": a.top, "lon": a.left},
		"bottom_right": map[string]any{"lat": a.bottom, "lon": a.right},
	}}
}
func (a *geoBoundsAgg) Reset() { *a = geoBoundsAgg{top: -90, bottom: 90, left: 180, right: -180} }

// --- boxplot ---
type boxplotAgg struct {
	values []float64
}

func newBoxplotAgg(_ map[string]any) (Aggregator, error) { return &boxplotAgg{}, nil }
func (a *boxplotAgg) Name() string                       { return "boxplot" }
func (a *boxplotAgg) Collect(v any) {
	f, ok := toFloat64(v)
	if !ok {
		return
	}
	a.values = append(a.values, f)
}
func (a *boxplotAgg) Result() any {
	if len(a.values) == 0 {
		return map[string]any{"min": 0, "q1": 0, "median": 0, "q3": 0, "max": 0}
	}
	sort.Float64s(a.values)
	n := len(a.values)
	q := func(p float64) float64 { return a.values[int(p*float64(n-1))] }
	return map[string]any{"min": a.values[0], "q1": q(0.25), "median": q(0.50), "q3": q(0.75), "max": a.values[n-1]}
}
func (a *boxplotAgg) Reset() { a.values = nil }

// --- string_stats ---
type stringStatsAgg struct {
	count    int
	minLen   int
	maxLen   int
	totalLen int
}

func newStringStatsAgg(_ map[string]any) (Aggregator, error) {
	return &stringStatsAgg{minLen: math.MaxInt32}, nil
}
func (a *stringStatsAgg) Name() string { return "string_stats" }
func (a *stringStatsAgg) Collect(v any) {
	s, ok := v.(string)
	if !ok {
		return
	}
	n := len(s)
	a.count++
	a.totalLen += n
	if n < a.minLen {
		a.minLen = n
	}
	if n > a.maxLen {
		a.maxLen = n
	}
}
func (a *stringStatsAgg) Result() any {
	avgLen := 0.0
	if a.count > 0 {
		avgLen = float64(a.totalLen) / float64(a.count)
	}
	if a.minLen == math.MaxInt32 {
		a.minLen = 0
	}
	return map[string]any{"count": a.count, "min_length": a.minLen, "max_length": a.maxLen, "avg_length": avgLen}
}
func (a *stringStatsAgg) Reset() { *a = stringStatsAgg{minLen: math.MaxInt32} }

// fmtPercent formats a percentile value as a string key.
func fmtPercent(p float64) string {
	if p == math.Trunc(p) {
		return intToStr(int(p)) + ".0"
	}
	return floatToStr(p)
}
func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
func floatToStr(f float64) string {
	intPart := int(f)
	fracPart := f - float64(intPart)
	if fracPart < 0 {
		fracPart = -fracPart
	}
	fracStr := ""
	for i := 0; i < 6 && fracPart > 0; i++ {
		fracPart *= 10
		d := int(fracPart)
		fracStr += intToStr(d)
		fracPart -= float64(d)
	}
	return intToStr(intPart) + "." + fracStr
}

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.1.1
func init() {
	Register("extended_stats", newExtendedStatsAgg)
	Register("percentiles", newPercentilesAgg)
	Register("percentile_ranks", newPercentileRanksAgg)
	Register("top_hits", newTopHitsAgg)
	Register("weighted_avg", newWeightedAvgAgg)
	Register("median_absolute_deviation", newMADAgg)
	Register("geo_centroid", newGeoCentroidAgg)
	Register("geo_bounds", newGeoBoundsAgg)
	Register("boxplot", newBoxplotAgg)
	Register("string_stats", newStringStatsAgg)
}
