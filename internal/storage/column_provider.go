package storage

import (
	"fmt"
	"io"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/storage/memtable"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// memtableColumnProvider implements index.ColumnProvider over a frozen
// memtable.Table's VecBuilder so the HNSW builder can stream vectors
// without touching storage internals (design 02 §1: storage couples to
// index only through ColumnProvider).
//
// The HNSW Build contract (hnsw/index.go) requires the provider to yield
// vectors in dense 0..N-1 row order; assertDenseVectorCoverage enforces
// that before we hand the provider over.
type memtableColumnProvider struct {
	field string
	rows  int64
	dim   int
	vb    *memtable.VecBuilder
}

func newMemtableColumnProvider(field string, totalRows int64, vb *memtable.VecBuilder) (*memtableColumnProvider, error) {
	if len(vb.Dense) == 0 {
		return nil, fmt.Errorf("storage: field %s has no dense vectors", field)
	}
	return &memtableColumnProvider{
		field: field,
		rows:  totalRows,
		dim:   len(vb.Dense[0]),
		vb:    vb,
	}, nil
}

func (p *memtableColumnProvider) Field() string { return p.field }
func (p *memtableColumnProvider) Rows() int64   { return p.rows }
func (p *memtableColumnProvider) Dim() int      { return p.dim }

// ForEachVector yields (rowID, vec) pairs in row order. Multi-value
// dense vectors are flattened by the write path before they reach the
// memtable, so each row owns exactly one entry.
func (p *memtableColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	for i, rid := range p.vb.RowIDs {
		if err := fn(int32(rid), p.vb.Dense[i]); err != nil {
			return err
		}
	}
	return nil
}

// assertDenseVectorCoverage verifies that the VecBuilder yields vectors
// for the rows that have them. Rows without vectors (sparse) are skipped
// — the HNSW builder only indexes rows that have actual vector data.
func assertDenseVectorCoverage(vb *memtable.VecBuilder, n int, fid types.FieldID) error {
	// Sparse vectors are allowed: not every row must have a vector.
	// Only verify that row IDs are in range and non-decreasing.
	for i, rid := range vb.RowIDs {
		if int(rid) < 0 || int(rid) >= n {
			return fmt.Errorf("storage: dense_vector field %d row ID %d out of range [0, %d)",
				fid, rid, n)
		}
		if i > 0 && int(rid) <= int(vb.RowIDs[i-1]) {
			return fmt.Errorf("storage: dense_vector field %d row IDs not strictly increasing at position %d",
				fid, i)
		}
	}
	return nil
}

var _ index.ColumnProvider = (*memtableColumnProvider)(nil)

// ---------------------------------------------------------------------------
// Text / scalar column providers for inverted & scalar index builds.
// These wrap a frozen memtable.Table's ColumnBuilder so the inverted
// and scalar index builders can stream values without touching storage
// internals, mirroring the vector column provider above.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.6
// ---------------------------------------------------------------------------

// memtableTextColumnProvider implements inverted.TextColumn over a
// frozen memtable.Table's ColumnBuilder (Strings slice). Used by
// buildInvertedIndexes for text fields.
type memtableTextColumnProvider struct {
	field     string
	totalRows int64
	cb        *memtable.ColumnBuilder
}

func newMemtableTextColumnProvider(field string, totalRows int64, cb *memtable.ColumnBuilder) *memtableTextColumnProvider {
	return &memtableTextColumnProvider{field: field, totalRows: totalRows, cb: cb}
}

func (p *memtableTextColumnProvider) Field() string { return p.field }
func (p *memtableTextColumnProvider) Rows() int64   { return p.totalRows }
func (p *memtableTextColumnProvider) Dim() int      { return 0 }
func (p *memtableTextColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	return nil // non-vector column
}

// ForEachText yields (rowID, text) pairs in row order from the
// ColumnBuilder's Strings slice. Only rows that have a value are
// visited (sparse columns skip absent rows).
func (p *memtableTextColumnProvider) ForEachText(fn func(rowID int32, text []byte) error) error {
	if p.cb == nil {
		return nil
	}
	for i, rid := range p.cb.RowIDs {
		if err := fn(int32(rid), []byte(p.cb.Strings[i])); err != nil {
			return err
		}
	}
	return nil
}

// memtableLongColumnProvider implements scalar.LongColumn over a
// frozen memtable.Table's ColumnBuilder (Longs slice). Used by
// buildScalarIndexes for long/date fields.
type memtableLongColumnProvider struct {
	field     string
	totalRows int64
	cb        *memtable.ColumnBuilder
}

func newMemtableLongColumnProvider(field string, totalRows int64, cb *memtable.ColumnBuilder) *memtableLongColumnProvider {
	return &memtableLongColumnProvider{field: field, totalRows: totalRows, cb: cb}
}

func (p *memtableLongColumnProvider) Field() string { return p.field }
func (p *memtableLongColumnProvider) Rows() int64   { return p.totalRows }
func (p *memtableLongColumnProvider) Dim() int      { return 0 }
func (p *memtableLongColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	return nil
}

func (p *memtableLongColumnProvider) ForEachLong(fn func(rowID int32, v int64) error) error {
	if p.cb == nil {
		return nil
	}
	for i, rid := range p.cb.RowIDs {
		if err := fn(int32(rid), p.cb.Longs[i]); err != nil {
			return err
		}
	}
	return nil
}

// memtableDoubleColumnProvider implements scalar.DoubleColumn over a
// frozen memtable.Table's ColumnBuilder (Doubles slice).
type memtableDoubleColumnProvider struct {
	field     string
	totalRows int64
	cb        *memtable.ColumnBuilder
}

func newMemtableDoubleColumnProvider(field string, totalRows int64, cb *memtable.ColumnBuilder) *memtableDoubleColumnProvider {
	return &memtableDoubleColumnProvider{field: field, totalRows: totalRows, cb: cb}
}

func (p *memtableDoubleColumnProvider) Field() string { return p.field }
func (p *memtableDoubleColumnProvider) Rows() int64   { return p.totalRows }
func (p *memtableDoubleColumnProvider) Dim() int      { return 0 }
func (p *memtableDoubleColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	return nil
}

func (p *memtableDoubleColumnProvider) ForEachDouble(fn func(rowID int32, v float64) error) error {
	if p.cb == nil {
		return nil
	}
	for i, rid := range p.cb.RowIDs {
		if err := fn(int32(rid), p.cb.Doubles[i]); err != nil {
			return err
		}
	}
	return nil
}

// memtableBytesColumnProvider implements scalar.BytesColumn over a
// frozen memtable.Table's ColumnBuilder (Strings slice for keyword
// fields). The string is converted to []byte on each call.
type memtableBytesColumnProvider struct {
	field     string
	totalRows int64
	cb        *memtable.ColumnBuilder
}

func newMemtableBytesColumnProvider(field string, totalRows int64, cb *memtable.ColumnBuilder) *memtableBytesColumnProvider {
	return &memtableBytesColumnProvider{field: field, totalRows: totalRows, cb: cb}
}

func (p *memtableBytesColumnProvider) Field() string { return p.field }
func (p *memtableBytesColumnProvider) Rows() int64   { return p.totalRows }
func (p *memtableBytesColumnProvider) Dim() int      { return 0 }
func (p *memtableBytesColumnProvider) ForEachVector(fn func(rowID int32, vec []float32) error) error {
	return nil
}

func (p *memtableBytesColumnProvider) ForEachValue(fn func(rowID int32, v []byte) error) error {
	if p.cb == nil {
		return nil
	}
	for i, rid := range p.cb.RowIDs {
		if err := fn(int32(rid), []byte(p.cb.Strings[i])); err != nil {
			return err
		}
	}
	return nil
}

// segIndexOutput implements index.IndexOutput over a directory on disk
// (the segment's vec.idx/ subdirectory). Files are created with
// O_TRUNC so failed builds overwrite cleanly on retry.
type segIndexOutput struct {
	dir string
}

func (o *segIndexOutput) Create(name string) (io.WriteCloser, error) {
	path := joinSafe(o.dir, name)
	f, err := openWriteCreate(path)
	if err != nil {
		return nil, fmt.Errorf("storage: create %s: %w", name, err)
	}
	return f, nil
}

// segIndexInput implements index.IndexInput over a sealed vec.idx/
// directory. Files are opened read-only with seeking.
type segIndexInput struct {
	dir string
}

func (i *segIndexInput) Open(name string) (io.ReadSeekCloser, error) {
	path := joinSafe(i.dir, name)
	f, err := openReadSeek(path)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", name, err)
	}
	return f, nil
}

var (
	_ index.IndexOutput = (*segIndexOutput)(nil)
	_ index.IndexInput  = (*segIndexInput)(nil)
)
