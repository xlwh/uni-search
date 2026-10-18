package segment

import (
	"context"
	"fmt"
	"io"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// TypedZoneMap is the decoded form of the raw ZoneMap bytes: Min/Max hold
// the concrete Go type of the column (string / int64 / float64 / bool) and
// are nil when the block has no non-null value. Consumers use it to decide
// whether a predicate overlaps a block without decoding it (design 01 §4.2,
// query-engine rule R2).
type TypedZoneMap struct {
	Min     any // string | int64 | float64 | bool; nil when no non-null rows
	Max     any
	HasNull bool
}

// BlockMeta is the metadata of the block an iterator currently points to.
// It is available without decoding the block payload.
type BlockMeta struct {
	Index     int
	Encoding  Encoding
	NumRows   uint32
	NullCount uint32
	Zone      TypedZoneMap
}

// ColumnIterator streams a stored column block by block (design 01 §10:
// ScanColumn returns a block-streaming reader). The consumer consults
// Meta().Zone before each block and either decodes it with Next or skips
// it with Skip; skipped blocks are never decoded, which Decoded verifies.
type ColumnIterator interface {
	// NumBlocks is the total number of blocks in the column.
	NumBlocks() int
	// BlockIndex is the position of the next block to consume.
	BlockIndex() int
	// Done reports whether every block has been consumed.
	Done() bool
	// Meta returns the metadata of the next block without decoding it.
	Meta() BlockMeta
	// Next decodes the next block and advances. It returns io.EOF once
	// Done() is true.
	Next(ctx context.Context) (*BlockData, error)
	// Skip advances past the next block without decoding it.
	Skip()
	// Decoded returns how many blocks were actually decoded via Next; the
	// zone-map block-skipping verification counter of R4.
	Decoded() int
	// FieldType returns the column's declared type so consumers can
	// route the typed lane without inspecting BlockData on the hot
	// path.
	FieldType() meta.FieldType
}

// ColumnReader parses one col_{field_id}.bin file (as produced by
// ColumnWriter.Finish) and hands out block iterators.
type ColumnReader struct {
	data        []byte
	hdr         *ColumnHeader
	regionStart int // absolute offset of the block data region
}

// ReadColumnFile parses a complete column file image. The reader keeps the
// whole file in memory (page-cache semantics, design 01 §10); mmap-backed
// loading can substitute the slice later without touching this API.
func ReadColumnFile(data []byte) (*ColumnReader, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("segment: truncated column file")
	}
	if readU64LE(data, 0) != colMagic {
		return nil, fmt.Errorf("segment: bad column file magic")
	}
	hLen := int(readU32LE(data, 8))
	if 12+hLen > len(data) {
		return nil, fmt.Errorf("segment: truncated column header")
	}
	hdr, err := UnmarshalColumnHeader(data[12 : 12+hLen])
	if err != nil {
		return nil, err
	}
	return &ColumnReader{data: data, hdr: hdr, regionStart: 12 + hLen}, nil
}

// Header returns the parsed column header.
func (r *ColumnReader) Header() *ColumnHeader { return r.hdr }

// NumRows returns the total row count of the column.
func (r *ColumnReader) NumRows() uint64 { return r.hdr.NumRows }

// NumBlocks returns the number of blocks.
func (r *ColumnReader) NumBlocks() int { return len(r.hdr.Blocks) }

// FieldType returns the column type.
func (r *ColumnReader) FieldType() meta.FieldType { return r.hdr.Type }

// NewIterator returns a fresh block iterator over the column.
func (r *ColumnReader) NewIterator() ColumnIterator {
	return &colIter{r: r}
}

// decodeBlockAt decodes block i (framed payload) — used by the iterator.
func (r *ColumnReader) decodeBlockAt(i int) (*BlockData, error) {
	bi := r.hdr.Blocks[i]
	off := r.regionStart + int(bi.Offset)
	if off+4 > len(r.data) {
		return nil, fmt.Errorf("segment: block %d start out of range", i)
	}
	pLen := int(readU32LE(r.data, off))
	off += 4
	if pLen < 0 || off+pLen > len(r.data) {
		return nil, fmt.Errorf("segment: block %d payload out of range", i)
	}
	return decodeBlock(r.hdr.Type, bi.Encoding, r.data[off:off+pLen], int(bi.NumRows))
}

// colIter is the concrete ColumnIterator over a ColumnReader.
type colIter struct {
	r       *ColumnReader
	cur     int
	decoded int
}

func (it *colIter) NumBlocks() int  { return len(it.r.hdr.Blocks) }
func (it *colIter) BlockIndex() int { return it.cur }

func (it *colIter) Done() bool { return it.cur >= len(it.r.hdr.Blocks) }

func (it *colIter) Meta() BlockMeta {
	if it.Done() {
		return BlockMeta{Index: it.cur}
	}
	bi := it.r.hdr.Blocks[it.cur]
	return BlockMeta{
		Index:     it.cur,
		Encoding:  bi.Encoding,
		NumRows:   bi.NumRows,
		NullCount: bi.NullCount,
		Zone: TypedZoneMap{
			Min:     deserializeZoneValue(it.r.hdr.Type, bi.Zone.Min),
			Max:     deserializeZoneValue(it.r.hdr.Type, bi.Zone.Max),
			HasNull: bi.Zone.HasNull,
		},
	}
}

func (it *colIter) Next(ctx context.Context) (*BlockData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if it.Done() {
		return nil, io.EOF
	}
	blk, err := it.r.decodeBlockAt(it.cur)
	if err != nil {
		return nil, err
	}
	it.decoded++
	it.cur++
	return blk, nil
}

func (it *colIter) Skip() {
	if !it.Done() {
		it.cur++
	}
}

func (it *colIter) Decoded() int { return it.decoded }

func (it *colIter) FieldType() meta.FieldType { return it.r.hdr.Type }

// emptyColumnIterator is returned for MVCC-pruned segment scans: the
// segment is not visible to the snapshot, so there are no blocks to scan.
type emptyColumnIterator struct{}

func (emptyColumnIterator) NumBlocks() int                 { return 0 }
func (emptyColumnIterator) BlockIndex() int                { return 0 }
func (emptyColumnIterator) Done() bool                     { return true }
func (emptyColumnIterator) Meta() BlockMeta                { return BlockMeta{Index: 0} }
func (emptyColumnIterator) Next(context.Context) (*BlockData, error) {
	return nil, io.EOF
}
func (emptyColumnIterator) Skip()          {}
func (emptyColumnIterator) Decoded() int   { return 0 }
func (emptyColumnIterator) FieldType() meta.FieldType { return meta.TypeLong }

// Ensure the concrete types satisfy the interface.
var (
	_ ColumnIterator = (*colIter)(nil)
	_ ColumnIterator = emptyColumnIterator{}
)
