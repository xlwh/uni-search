package memtable

import (
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	apierrors "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// Row is one parsed document ready for columnar storage. Values maps field
// IDs to the Go-native representation of design 01 §1.2, one value per
// field per row:
//
//	string (keyword/text), int64 (long/date), float64 (double),
//	bool (boolean), uint32 (ip), GeoPoint (geo_point),
//	[]float32 (dense_vector), map[uint32]float32 (sparse_vector).
//
// Multi-valued fields are flattened by the write path before they reach
// the memtable (v1 scope). Source is the raw _source JSON, kept verbatim;
// compression happens at flush time (§4.4).
type Row struct {
	ID     string
	SeqNo  types.SeqNo
	Source []byte
	Values map[types.FieldID]any

	// ClientID and Sequence enable idempotent writes (design 01 §2.3).
	// When ClientID is non-empty, the MemTable deduplicates requests
	// with the same (ClientID, Sequence) pair via an LRU window.
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.1
	ClientID string
	Sequence uint64
}

// RowView is the read-only projection used by the scan path (design 01
// §2.1: memtable reads are simple row iteration, not vectorized).
type RowView struct {
	RowID  types.RowID // row number within its owning table
	ID     string
	SeqNo  types.SeqNo
	Source []byte
}

// GeoPoint is the Go-native geo_point value (design 01 §1.2).
type GeoPoint struct {
	Lon float64
	Lat float64
}

// Table is the columnar write buffer (design 01 §2.1). Values are
// aggregated per FieldID into typed column/vector builders; each builder
// tracks the RowID of every appended value, so sparse rows (a field absent
// from a row simply has no entry) and repeated RowIDs (multi-value,
// appended by the write path as separate entries) both stay resolvable.
//
// Tables are single-writer under the owning MemTable's lock and become
// immutable once frozen; the flush worker therefore reads frozen tables
// without synchronization.
type Table struct {
	rows    int
	columns map[types.FieldID]*ColumnBuilder
	vectors map[types.FieldID]*VecBuilder
	sources [][]byte
	ids     []string
	seqNos  []types.SeqNo
	pkIndex map[string]types.RowID
	bytes   atomic.Int64
	created time.Time
}

func newTable() *Table {
	return &Table{
		columns: make(map[types.FieldID]*ColumnBuilder),
		vectors: make(map[types.FieldID]*VecBuilder),
		pkIndex: make(map[string]types.RowID),
		created: time.Now(),
	}
}

// NumRows returns the number of appended rows.
func (t *Table) NumRows() int { return t.rows }

// Bytes returns the memory-usage estimate used by the §2.2 freeze trigger.
func (t *Table) Bytes() int64 { return t.bytes.Load() }

// CreatedAt returns when the table was created (age-based freeze trigger).
func (t *Table) CreatedAt() time.Time { return t.created }

// ID returns the _id of the given row.
func (t *Table) ID(row types.RowID) string { return t.ids[row] }

// SeqNo returns the MVCC version of the given row.
func (t *Table) SeqNo(row types.RowID) types.SeqNo { return t.seqNos[row] }

// Source returns the raw _source JSON of the given row (shared, do not
// mutate).
func (t *Table) Source(row types.RowID) []byte { return t.sources[row] }

// RowView materializes the read projection of a row.
func (t *Table) RowView(row types.RowID) RowView {
	return RowView{RowID: row, ID: t.ids[row], SeqNo: t.seqNos[row], Source: t.sources[row]}
}

// LookupID resolves _id to its row through the primary-key index (O(1)),
// returning the latest row appended for the id (§2.1: point update
// targeting). Use lookupVisible for MVCC-filtered reads.
func (t *Table) LookupID(id string) (types.RowID, bool) {
	rid, ok := t.pkIndex[id]
	return rid, ok
}

// FieldIDs returns the sorted union of scalar-column and vector field IDs
// present in this table, for the flush path (T4/T6).
func (t *Table) FieldIDs() []types.FieldID {
	out := make([]types.FieldID, 0, len(t.columns)+len(t.vectors))
	for f := range t.columns {
		out = append(out, f)
	}
	for f := range t.vectors {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Column returns the scalar column builder for the field, or nil.
func (t *Table) Column(field types.FieldID) *ColumnBuilder { return t.columns[field] }

// Vector returns the vector column builder for the field, or nil.
func (t *Table) Vector(field types.FieldID) *VecBuilder { return t.vectors[field] }

// collect appends every row visible to snap to dst, in append order.
func (t *Table) collect(snap types.Snapshot, dst []RowView) []RowView {
	for i := 0; i < t.rows; i++ {
		if snap.Visible(t.seqNos[i]) {
			dst = append(dst, t.RowView(types.RowID(i)))
		}
	}
	return dst
}

// lookupVisible finds the newest row with the given _id that is visible to
// snap. The pk index serves the common fast path; when the newest version
// is not yet visible to an older snapshot, the fallback scan returns the
// newest visible version.
func (t *Table) lookupVisible(id string, snap types.Snapshot) (types.RowID, bool) {
	if rid, ok := t.pkIndex[id]; ok && snap.Visible(t.seqNos[rid]) {
		return rid, true
	}
	for i := t.rows - 1; i >= 0; i-- {
		if t.ids[i] == id && snap.Visible(t.seqNos[i]) {
			return types.RowID(i), true
		}
	}
	return 0, false
}

// appendRow validates the whole row before mutating any state so a
// rejected row never leaves partially appended column entries behind.
func (t *Table) appendRow(row *Row) error {
	for fid, v := range row.Values {
		if err := t.checkValue(fid, v); err != nil {
			return err
		}
	}
	rid := types.RowID(t.rows)
	for fid, v := range row.Values {
		if err := t.appendValue(fid, rid, v); err != nil {
			return err // unreachable after checkValue; kept as a guard
		}
	}
	t.sources = append(t.sources, row.Source)
	t.ids = append(t.ids, row.ID)
	t.seqNos = append(t.seqNos, row.SeqNo)
	t.pkIndex[row.ID] = rid
	t.rows++
	t.bytes.Add(row.sizeEstimate())
	return nil
}

func (t *Table) checkValue(fid types.FieldID, v any) error {
	switch v.(type) {
	case []float32, map[uint32]float32:
		if b := t.vectors[fid]; b != nil {
			return b.check(v)
		}
		return nil
	case string, int64, float64, bool, uint32, GeoPoint:
		if c := t.columns[fid]; c != nil {
			return c.check(v)
		}
		return nil
	default:
		return apierrors.NewDocumentParsing(fmt.Sprintf(
			"memtable: unsupported value type %T for field %d", v, fid))
	}
}

func (t *Table) appendValue(fid types.FieldID, rid types.RowID, v any) error {
	switch v.(type) {
	case []float32, map[uint32]float32:
		b := t.vectors[fid]
		if b == nil {
			b = &VecBuilder{field: fid}
			t.vectors[fid] = b
		}
		return b.append(rid, v)
	case string, int64, float64, bool, uint32, GeoPoint:
		c := t.columns[fid]
		if c == nil {
			c = &ColumnBuilder{field: fid}
			t.columns[fid] = c
		}
		return c.append(rid, v)
	default:
		return apierrors.NewDocumentParsing(fmt.Sprintf(
			"memtable: unsupported value type %T for field %d", v, fid))
	}
}

// sizeEstimate approximates the row's memory footprint for the §2.2 byte
// trigger.
func (r *Row) sizeEstimate() int64 {
	n := int64(len(r.Source) + len(r.ID) + 16) // row slots + overhead
	for _, v := range r.Values {
		n += 8 + valueSize(v)
	}
	return n
}

func valueSize(v any) int64 {
	switch x := v.(type) {
	case string:
		return int64(len(x))
	case int64:
		return 8
	case float64:
		return 8
	case bool:
		return 1
	case uint32:
		return 4
	case GeoPoint:
		return 16
	case []float32:
		return 4*int64(len(x)) + 8
	case map[uint32]float32:
		return 12*int64(len(x)) + 8
	default:
		return 16
	}
}

// valueKind tags a column's Go-native element type so mixed-type appends
// into one field are rejected instead of silently corrupting the column.
type valueKind uint8

const (
	kindNone valueKind = iota
	kindString
	kindLong
	kindDouble
	kindBool
	kindIP
	kindGeo
	kindDenseVec
	kindSparseVec
)

func (k valueKind) String() string {
	switch k {
	case kindString:
		return "string"
	case kindLong:
		return "int64"
	case kindDouble:
		return "float64"
	case kindBool:
		return "bool"
	case kindIP:
		return "uint32"
	case kindGeo:
		return "geo_point"
	case kindDenseVec:
		return "dense_vector"
	case kindSparseVec:
		return "sparse_vector"
	default:
		return "unset"
	}
}

func scalarKindOf(v any) (valueKind, bool) {
	switch v.(type) {
	case string:
		return kindString, true
	case int64:
		return kindLong, true
	case float64:
		return kindDouble, true
	case bool:
		return kindBool, true
	case uint32:
		return kindIP, true
	case GeoPoint:
		return kindGeo, true
	}
	return kindNone, false
}

// ColumnBuilder accumulates one scalar column in the Go-native
// representation of its field type (design 01 §1.2). Exactly one typed
// slice is populated, fixed by the first appended value; RowIDs records
// which row each entry belongs to (ascending, may repeat for multi-value).
// Exported slices are read-only for callers; mutation happens through
// Table appends only.
type ColumnBuilder struct {
	field  types.FieldID
	kind   valueKind
	RowIDs []types.RowID

	Strings []string   // keyword / text
	Longs   []int64    // long / date (epoch millis)
	Doubles []float64  // double
	Bools   []bool     // boolean
	IPs     []uint32   // ip (IPv4 numeric form; v1 scope)
	Points  []GeoPoint // geo_point
}

func (c *ColumnBuilder) check(v any) error {
	k, ok := scalarKindOf(v)
	if !ok {
		return apierrors.NewDocumentParsing(fmt.Sprintf(
			"memtable: unsupported column value type %T for field %d", v, c.field))
	}
	if c.kind != kindNone && c.kind != k {
		return apierrors.NewDocumentParsing(fmt.Sprintf(
			"memtable: field %d already stores %s values, got %T", c.field, c.kind, v))
	}
	return nil
}

func (c *ColumnBuilder) append(rid types.RowID, v any) error {
	if err := c.check(v); err != nil {
		return err
	}
	if c.kind == kindNone {
		c.kind, _ = scalarKindOf(v)
	}
	c.RowIDs = append(c.RowIDs, rid)
	switch x := v.(type) {
	case string:
		c.Strings = append(c.Strings, x)
	case int64:
		c.Longs = append(c.Longs, x)
	case float64:
		c.Doubles = append(c.Doubles, x)
	case bool:
		c.Bools = append(c.Bools, x)
	case uint32:
		c.IPs = append(c.IPs, x)
	case GeoPoint:
		c.Points = append(c.Points, x)
	}
	return nil
}

// VecBuilder accumulates one vector column (dense or sparse, design 01
// §1.2). As with ColumnBuilder, RowIDs keeps entries aligned to rows.
type VecBuilder struct {
	field  types.FieldID
	kind   valueKind
	RowIDs []types.RowID

	Dense  [][]float32          // dense_vector
	Sparse []map[uint32]float32 // sparse_vector
}

func (b *VecBuilder) check(v any) error {
	k := kindNone
	switch v.(type) {
	case []float32:
		k = kindDenseVec
	case map[uint32]float32:
		k = kindSparseVec
	}
	if k == kindNone {
		return apierrors.NewDocumentParsing(fmt.Sprintf(
			"memtable: unsupported vector value type %T for field %d", v, b.field))
	}
	if b.kind != kindNone && b.kind != k {
		return apierrors.NewDocumentParsing(fmt.Sprintf(
			"memtable: vector field %d already stores %s values, got %T", b.field, b.kind, v))
	}
	return nil
}

func (b *VecBuilder) append(rid types.RowID, v any) error {
	if err := b.check(v); err != nil {
		return err
	}
	if b.kind == kindNone {
		switch v.(type) {
		case []float32:
			b.kind = kindDenseVec
		case map[uint32]float32:
			b.kind = kindSparseVec
		}
	}
	b.RowIDs = append(b.RowIDs, rid)
	switch x := v.(type) {
	case []float32:
		b.Dense = append(b.Dense, x)
	case map[uint32]float32:
		b.Sparse = append(b.Sparse, x)
	}
	return nil
}
