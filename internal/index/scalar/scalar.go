// Package scalar implements the scalar index family of design
// 02-index-engine.md §6: the BKD index over 1D numeric columns (long /
// date / ip natively, double via an order-preserving encoding) and 2D
// geo_point columns (§6.1), and the low-cardinality bitmap index with
// the optional Bloom fallback (§6.3/§6.4). The package registers
// index.KindBKD and index.KindBitmap in init (§1.2).
//
// The registry's ColumnProvider is still in its M0 vector-only minimal
// form (internal/index/registry.go), so scalar columns reach Build
// through the narrow extension interfaces below: Build type-asserts the
// provider it receives and fails with a clear error when the column is
// not scalar. The storage engine (T13+) implements these interfaces on
// top of the col_*.bin block readers without either side importing the
// other's internals (00 §4 dependency rule).
//
// Query surface: range and geo distance queries are expressed as the
// request structures in query.go (RangeQuery / GeoDistanceQuery) and
// consumed through the Searcher interfaces implemented by the loaded
// indexes; they return roaring bitmaps so the filter operator (T15) can
// intersect them across fields and segments. The generic
// index.LoadedIndex.Search trait is honored by encoding the same
// requests into index.Query.Term (see query.go Encode methods).
package scalar

import (
	"context"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// LongColumn extends index.ColumnProvider with per-row int64 access for
// the long family (long/date/ip; §6.1). Values are iterated in row
// order; iteration stops on the first error returned by fn.
type LongColumn interface {
	index.ColumnProvider
	// ForEachLong invokes fn once per row with the row's int64 value.
	ForEachLong(fn func(rowID int32, v int64) error) error
}

// DoubleColumn extends index.ColumnProvider with per-row float64 access
// (double columns; §6.1). Double values enter the BKD 1D sorted array
// through an order-preserving bit transform, so range semantics are
// exact (see NewDoubleRangeQuery).
type DoubleColumn interface {
	index.ColumnProvider
	// ForEachDouble invokes fn once per row with the row's float64 value.
	ForEachDouble(fn func(rowID int32, v float64) error) error
}

// GeoPointColumn extends index.ColumnProvider with per-row (lat, lon)
// pairs for geo_point columns (§6.1). Coordinates are in degrees;
// lat ∈ [-90,90], lon ∈ [-180,180].
type GeoPointColumn interface {
	index.ColumnProvider
	// ForEachGeoPoint invokes fn once per row with the row's coordinates.
	ForEachGeoPoint(fn func(rowID int32, lat, lon float64) error) error
}

// BytesColumn extends index.ColumnProvider with per-row term access
// for keyword-family columns (§6.3). v is only valid for the duration
// of the call.
type BytesColumn interface {
	index.ColumnProvider
	// ForEachValue invokes fn once per row with the row's value.
	ForEachValue(fn func(rowID int32, v []byte) error) error
}

// IDLookup is the _id point-lookup surface built by the storage engine
// (T4: internal/storage/segment id.hash, design 01 §4.4). The concrete
// segment reader satisfies it directly:
//
//	var lk scalar.IDLookup = seg // segment.Segment.LookupID
//
// The scalar package only wires this surface into the row-bitmap filter
// contract consumed by the query engine (T15); it never re-implements
// the hash table (T12.3 "_id 哈希接入" = reuse, not rewrite).
type IDLookup interface {
	// LookupID resolves a document _id to its RowID. found is false for
	// absent ids; the storage layer applies delete/versioning checks.
	LookupID(ctx context.Context, id string) (row types.RowID, found bool, err error)
}

// LookupIDs resolves a batch of _ids to a row bitmap through the
// storage-provided _id hash index (§6.5 GET/UPDATE location flow). The
// result is the union of all found rows; absent ids contribute nothing.
// Rows are uint32 per the roaring contract; delete-bitmap checks stay
// with the caller, matching the storage layer's versioned-read path.
func LookupIDs(ctx context.Context, lk IDLookup, ids []string) (*roaring.Bitmap, error) {
	out := roaring.New()
	if lk == nil {
		return nil, errNilIDLookup
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, found, err := lk.LookupID(ctx, id)
		if err != nil {
			return nil, err
		}
		if found {
			out.Add(uint32(row))
		}
	}
	return out, nil
}
