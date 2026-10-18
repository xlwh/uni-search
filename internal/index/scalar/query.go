package scalar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/index"
)

// Package errors. Scalar indexes are exact filters: their output feeds
// pre-filter planning (§2.6) and filter operators (03 §3.4), so a
// truncated result is never returned silently — budget exhaustion is
// an error the caller retries with a larger budget.
var (
	// ErrBudgetExhausted is returned when a SearchBudget cap truncates
	// the work of a scalar search. No partial row set is returned.
	ErrBudgetExhausted = errors.New("scalar: search budget exhausted before completion")
	// errNilIDLookup guards LookupIDs against a nil lookup surface.
	errNilIDLookup = errors.New("scalar: nil IDLookup")
)

// RangeQuery selects rows whose 1D value lies in the half-open interval
// [Min, Max) (§6.1). For double columns the bounds are the
// order-preserving encodings produced by NewDoubleRangeQuery; for
// long/date/ip columns Min/Max are the raw values. Use math.MinInt64 /
// MaxInt64 for open ends (a value exactly equal to MaxInt64 is then
// excluded, which matches the half-open semantics).
type RangeQuery struct {
	Min int64
	Max int64
}

// NewDoubleRangeQuery builds a RangeQuery over double columns: the
// float bounds are mapped through the same order-preserving bit
// transform the build applies, so [Min, Max) in float space becomes
// [enc(Min), enc(Max)) in the index's long domain, exactly.
func NewDoubleRangeQuery(min, max float64) RangeQuery {
	return RangeQuery{Min: doubleToSortable(min), Max: doubleToSortable(max)}
}

// GeoDistanceQuery selects rows within RadiusM meters of (Lat, Lon)
// (§6.1). Distance is the haversine great-circle distance over the
// mean-earth-radius sphere; rows at exactly RadiusM are included.
type GeoDistanceQuery struct {
	Lat     float64
	Lon     float64
	RadiusM float64
}

// encodeTag prefixes the wire form of scalar queries in
// index.Query.Term so the generic LoadedIndex.Search trait can route
// them (the typed Searcher methods remain the primary entry).
const (
	encodeTagRange byte = 1 + iota
	encodeTagGeo
)

// Encode serializes the query into the index.Query.Term wire form.
func (q RangeQuery) Encode() []byte {
	out := make([]byte, 1+16)
	out[0] = encodeTagRange
	binary.LittleEndian.PutUint64(out[1:9], uint64(q.Min))
	binary.LittleEndian.PutUint64(out[9:17], uint64(q.Max))
	return out
}

// Encode serializes the query into the index.Query.Term wire form.
func (q GeoDistanceQuery) Encode() []byte {
	out := make([]byte, 1+24)
	out[0] = encodeTagGeo
	binary.LittleEndian.PutUint64(out[1:9], math.Float64bits(q.Lat))
	binary.LittleEndian.PutUint64(out[9:17], math.Float64bits(q.Lon))
	binary.LittleEndian.PutUint64(out[17:25], math.Float64bits(q.RadiusM))
	return out
}

// decodeQuery parses the wire form produced by the Encode methods.
func decodeQuery(b []byte) (any, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("scalar: empty encoded query")
	}
	switch b[0] {
	case encodeTagRange:
		if len(b) != 17 {
			return nil, fmt.Errorf("scalar: corrupt range query (%d bytes)", len(b))
		}
		return RangeQuery{
			Min: int64(binary.LittleEndian.Uint64(b[1:9])),
			Max: int64(binary.LittleEndian.Uint64(b[9:17])),
		}, nil
	case encodeTagGeo:
		if len(b) != 25 {
			return nil, fmt.Errorf("scalar: corrupt geo query (%d bytes)", len(b))
		}
		return GeoDistanceQuery{
			Lat:     math.Float64frombits(binary.LittleEndian.Uint64(b[1:9])),
			Lon:     math.Float64frombits(binary.LittleEndian.Uint64(b[9:17])),
			RadiusM: math.Float64frombits(binary.LittleEndian.Uint64(b[17:25])),
		}, nil
	default:
		return nil, fmt.Errorf("scalar: unknown query tag %d", b[0])
	}
}

// Stats reports the work a scalar search performed. It exists so
// callers (and tests) can verify pruning: a pruned search touches far
// fewer rows than the linear scan it replaces (R12).
type Stats struct {
	// RowsTouched counts rows examined: binary-search probes (1D),
	// bounding-box checks and distance computations (2D), plus every
	// emitted row.
	RowsTouched int64
	// DistComps counts exact haversine distance computations (geo only).
	DistComps int64
	// NodesVisited counts BKD tree nodes entered (2D only; leaves and
	// inner nodes). 1D binary search reports 0.
	NodesVisited int32
}

// RangeResult is the outcome of a 1D range search: the matching rows as
// a roaring bitmap (for filter-operator intersection) plus work stats.
type RangeResult struct {
	Rows  *roaring.Bitmap
	Stats Stats
}

// GeoResult is the outcome of a geo distance search.
type GeoResult struct {
	Rows  *roaring.Bitmap
	Stats Stats
}

// RangeSearcher is implemented by loaded BKD indexes over 1D columns.
// The filter operator (T15) obtains it by type assertion:
//
//	if rs, ok := li.(scalar.RangeSearcher); ok { ... }
type RangeSearcher interface {
	// SearchRange returns every row with value in [q.Min, q.Max).
	SearchRange(ctx context.Context, q RangeQuery, budget index.SearchBudget) (*RangeResult, error)
}

// GeoSearcher is implemented by loaded BKD indexes over geo_point
// columns.
type GeoSearcher interface {
	// SearchGeoDistance returns every row within q.RadiusM meters of
	// (q.Lat, q.Lon), computed with exact haversine distance.
	SearchGeoDistance(ctx context.Context, q GeoDistanceQuery, budget index.SearchBudget) (*GeoResult, error)
}

// TermSearcher is implemented by loaded bitmap indexes (exact mode):
// equality lookup returning the value's row bitmap.
type TermSearcher interface {
	// SearchTerm returns the rows whose value equals term. Absent
	// terms yield an empty bitmap, not an error.
	SearchTerm(ctx context.Context, term []byte, budget index.SearchBudget) (*roaring.Bitmap, error)
}

// doubleToSortable maps a float64 to an int64 that preserves the total
// order of the floats (the Lucene sortable-bits transform: negative bit
// patterns XOR every bit except the sign, positives pass through
// unchanged), so double columns share the sorted-int64 array of the
// BKD 1D path (§6.1 "double 定点化"). NaN is rejected by the build, not
// mapped.
func doubleToSortable(f float64) int64 {
	b := int64(math.Float64bits(f))
	if b < 0 {
		b ^= math.MaxInt64 // flip magnitude bits; negatives stay < 0
	}
	return b
}

// sortableToDouble is the inverse of doubleToSortable.
func sortableToDouble(v int64) float64 {
	if v < 0 {
		v ^= math.MaxInt64
	}
	return math.Float64frombits(uint64(v))
}
