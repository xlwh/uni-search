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

// assertDenseVectorCoverage verifies that the VecBuilder yields exactly
// one vector per row in 0..N-1 order, which is the HNSW Build contract.
func assertDenseVectorCoverage(vb *memtable.VecBuilder, n int, fid types.FieldID) error {
	if len(vb.RowIDs) != n {
		return fmt.Errorf("storage: dense_vector field %d has %d vectors for %d rows (sparse vector rows not supported in M0 HNSW)",
			fid, len(vb.RowIDs), n)
	}
	for i, rid := range vb.RowIDs {
		if int(rid) != i {
			return fmt.Errorf("storage: dense_vector field %d has gap at position %d (row %d)",
				fid, i, rid)
		}
	}
	return nil
}

var _ index.ColumnProvider = (*memtableColumnProvider)(nil)

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
