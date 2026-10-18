package segment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// SegmentReader is the storage engine's read interface over one immutable
// segment directory (design 01 §10). ScanColumn streams the stored column
// block by block; the MVCC snapshot prunes whole segments by
// max_seq_no <= ReadUpTo (design 01 §8).
type SegmentReader interface {
	ID() types.SegmentID
	NumRows() int
	// ScanColumn returns a block-streaming iterator over the stored column;
	// blocks stay page-cache resident and are only decoded on Next.
	ScanColumn(ctx context.Context, field types.FieldID, snap types.Snapshot) (ColumnIterator, error)
	// LookupID resolves a document _id to its row (point-lookup path via
	// id.hash).
	LookupID(ctx context.Context, id string) (types.RowID, bool, error)
	// Source returns the _source bytes of one row (decompresses src.zst).
	Source(row types.RowID) ([]byte, error)
	Footer() *SegmentFooter
	Close() error
}

// Segment is the on-disk SegmentReader implementation: an open handle to
// one published seg_{id} directory. Column files and the _id/_source
// side files are loaded lazily and cached; the footer itself is parsed at
// open. All methods are safe for concurrent use.
type Segment struct {
	dir    string
	footer *SegmentFooter

	mu      sync.Mutex
	closed  bool
	cols    map[types.FieldID]*ColumnReader
	idHash  *idHashTable
	rowIDs  []string // row-ordered _id vector, lazy from id.hash
	srcDec  []byte   // decompressed src.zst stream, lazy
	srcOpen bool
}

// OpenSegment opens a published segment directory (seg_{segment_id}).
func OpenSegment(dir string) (*Segment, error) {
	data, err := os.ReadFile(filepath.Join(dir, footerFileName))
	if err != nil {
		return nil, fmt.Errorf("segment: read footer: %w", err)
	}
	if len(data) < 12 {
		return nil, fmt.Errorf("segment: truncated footer in %s", dir)
	}
	if readU64LE(data, 0) != footerMagic {
		return nil, fmt.Errorf("segment: bad footer magic in %s", dir)
	}
	l := int(readU32LE(data, 8))
	if 12+l > len(data) {
		return nil, fmt.Errorf("segment: truncated footer payload in %s", dir)
	}
	footer, err := UnmarshalSegmentFooter(data[12 : 12+l])
	if err != nil {
		return nil, err
	}
	if footer.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("segment: format version %d newer than supported %d",
			footer.FormatVersion, FormatVersion)
	}
	return &Segment{
		dir:    dir,
		footer: footer,
		cols:   map[types.FieldID]*ColumnReader{},
	}, nil
}

// ID returns the segment identifier.
func (s *Segment) ID() types.SegmentID { return s.footer.SegmentID }

// NumRows returns the row count of the segment.
func (s *Segment) NumRows() int { return int(s.footer.NumRows) }

// Footer returns the parsed segment footer (resident in memory).
func (s *Segment) Footer() *SegmentFooter { return s.footer }

// ScanColumn returns a block-streaming iterator over the stored column.
// The MVCC snapshot prunes the whole segment when
// snap.ReadUpTo < footer.MaxSeqNo (design 01 §8); otherwise every block is
// offered with its zone map for block skipping (03 §3.2 rule R2).
func (s *Segment) ScanColumn(ctx context.Context, field types.FieldID, snap types.Snapshot) (ColumnIterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snap.ReadUpTo < s.footer.MaxSeqNo {
		return emptyColumnIterator{}, nil
	}
	r, err := s.column(field)
	if err != nil {
		return nil, err
	}
	return r.NewIterator(), nil
}

// column lazily loads and caches the parsed column file of a field.
func (s *Segment) column(field types.FieldID) (*ColumnReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("segment: %s closed", s.dir)
	}
	if r, ok := s.cols[field]; ok {
		return r, nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, colFileName(field)))
	if err != nil {
		return nil, fmt.Errorf("segment: field %d not stored in %s: %w", field, s.dir, err)
	}
	r, err := ReadColumnFile(data)
	if err != nil {
		return nil, err
	}
	s.cols[field] = r
	return r, nil
}

// Column returns the parsed ColumnReader for a field. It is the
// un-snapshot public accessor used by the merge path (which reads
// every row regardless of MVCC visibility — the new segment inherits
// the source segments' seq range so post-merge visibility is
// preserved). Query execution uses ScanColumn, which applies MVCC
// pruning; merge uses Column, which does not.
func (s *Segment) Column(field types.FieldID) (*ColumnReader, error) {
	return s.column(field)
}

// idIndex lazily loads the id.hash table ("加载可选", design 01 §4.4 —
// only point lookups pay for it).
func (s *Segment) idIndex() (*idHashTable, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idHash != nil {
		return s.idHash, nil
	}
	if s.closed {
		return nil, fmt.Errorf("segment: %s closed", s.dir)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, idHashFileName))
	if err != nil {
		return nil, fmt.Errorf("segment: read id.hash: %w", err)
	}
	t, err := readIDHash(data)
	if err != nil {
		return nil, err
	}
	s.idHash = t
	return t, nil
}

// LookupID resolves a document _id to its RowID (point-lookup path).
func (s *Segment) LookupID(ctx context.Context, id string) (types.RowID, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	t, err := s.idIndex()
	if err != nil {
		return 0, false, err
	}
	row, ok := t.lookup(id)
	return row, ok, nil
}

// DocID returns the _id of a row (row -> id direction; the scan/hits path).
// The row-ordered vector is materialized lazily from id.hash.
func (s *Segment) DocID(row types.RowID) (string, error) {
	if int(row) >= s.NumRows() {
		return "", fmt.Errorf("segment: row %d out of range (%d rows)", row, s.NumRows())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rowIDs == nil {
		if s.closed {
			return "", fmt.Errorf("segment: %s closed", s.dir)
		}
		data, err := os.ReadFile(filepath.Join(s.dir, idHashFileName))
		if err != nil {
			return "", fmt.Errorf("segment: read id.hash: %w", err)
		}
		t, err := readIDHash(data)
		if err != nil {
			return "", err
		}
		s.idHash = t
		ids, err := t.rowsToIDs(s.NumRows())
		if err != nil {
			return "", err
		}
		s.rowIDs = ids
	}
	return s.rowIDs[row], nil
}

// Source returns the _source bytes of one row. The src.zst stream is
// decompressed once and cached; the returned slice aliases the cached
// buffer and must not be modified. Segments written with _source disabled
// have no offsets and return an error.
func (s *Segment) Source(row types.RowID) ([]byte, error) {
	offs := s.footer.SourceOffsets
	if len(offs) == 0 {
		return nil, fmt.Errorf("segment: _source not stored in %s", s.dir)
	}
	if int(row) >= s.NumRows() {
		return nil, fmt.Errorf("segment: row %d out of range (%d rows)", row, s.NumRows())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.srcOpen {
		if s.closed {
			return nil, fmt.Errorf("segment: %s closed", s.dir)
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, sourceFileName))
		if err != nil {
			return nil, fmt.Errorf("segment: read src.zst: %w", err)
		}
		dec, err := zstdDecompress(raw)
		if err != nil {
			return nil, err
		}
		if uint64(len(dec)) != offs[len(offs)-1] {
			return nil, fmt.Errorf("segment: src.zst stream length %d != footer offset %d",
				len(dec), offs[len(offs)-1])
		}
		s.srcDec = dec
		s.srcOpen = true
	}
	start, end := offs[row], offs[row+1]
	return s.srcDec[start:end], nil
}

// VerifyChecksums recomputes the per-file checksum manifest and compares
// it with the footer (design 01 §4.5 checksum field). This is an explicit
// integrity check, not part of the open path.
func (s *Segment) VerifyChecksums() error {
	return verifyChecksums(s.dir, s.footer.Checksum)
}

// Close releases the cached in-memory state. Column readers are
// self-contained slices, so no file handles remain open.
func (s *Segment) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.cols = nil
	s.idHash = nil
	s.rowIDs = nil
	s.srcDec = nil
	return nil
}

// Ensure the concrete type satisfies the design-01 §10 interface.
var _ SegmentReader = (*Segment)(nil)
