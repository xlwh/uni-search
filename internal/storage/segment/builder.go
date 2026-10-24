package segment

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// footerFileName is the segment metadata file (design 01 §4.1).
const footerFileName = "meta.footer"

// sourceFileName is the zstd-19 _source column file (design 01 §4.4).
const sourceFileName = "src.zst"

// idDictFileName / idHashFileName are the _id dictionary and sparse hash
// (design 01 §4.4).
const (
	idDictFileName = "id.dict"
	idHashFileName = "id.hash"
)

// SegmentBuilder assembles one immutable segment directory (design
// 01 §4.1): col_{field_id}.bin files produced by ColumnWriter, the src.zst
// _source stream, id.dict/id.hash, and meta.footer. Finish writes every
// file with fsync, then publishes the directory with an atomic rename
// (design 01 §5: rename(2) is the segment's point of atomic visibility).
//
// The builder itself is single-goroutine (per-shard flush concurrency is 1,
// design 01 §5); the per-column ColumnWriters it hands out may however be
// filled in parallel by the flush worker, since they share no state.
type SegmentBuilder struct {
	parentDir string
	tmpDir    string
	finalDir  string
	segID     types.SegmentID
	shardID   types.ShardID
	blockSize int

	columns  map[types.FieldID]*ColumnWriter
	colOrder []types.FieldID
	ids      []string
	sources  [][]byte
	minSeq   types.SeqNo
	maxSeq   types.SeqNo
	finished bool
}

// NewSegmentBuilder creates the builder working set at
// parentDir/seg_{segment_id}.tmp.
func NewSegmentBuilder(parentDir string, segID types.SegmentID, shardID types.ShardID, opts ...Option) (*SegmentBuilder, error) {
	o := options{blockSize: DefaultBlockSize}
	for _, opt := range opts {
		opt(&o)
	}
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return nil, fmt.Errorf("segment: create parent dir: %w", err)
	}
	b := &SegmentBuilder{
		parentDir: parentDir,
		tmpDir:    filepath.Join(parentDir, fmt.Sprintf("seg_%d.tmp", segID)),
		finalDir:  filepath.Join(parentDir, fmt.Sprintf("seg_%d", segID)),
		segID:     segID,
		shardID:   shardID,
		blockSize: o.blockSize,
		columns:   map[types.FieldID]*ColumnWriter{},
	}
	if err := os.Mkdir(b.tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("segment: create tmp dir: %w", err)
	}
	return b, nil
}

// Column returns (creating on first use) the ColumnWriter for one field.
// Every column must receive exactly one value (or AppendNull) per row.
func (b *SegmentBuilder) Column(fieldID types.FieldID, name string, ft meta.FieldType) (*ColumnWriter, error) {
	if w, ok := b.columns[fieldID]; ok {
		if w.ft != ft || w.name != name {
			return nil, fmt.Errorf("segment: field %d redefined", fieldID)
		}
		return w, nil
	}
	w := NewColumnWriter(fieldID, name, ft, WithBlockSize(b.blockSize))
	if w.err != nil {
		return nil, w.err
	}
	b.columns[fieldID] = w
	b.colOrder = append(b.colOrder, fieldID)
	return w, nil
}

// AppendID records the _id of the next row (in row order, exactly one per
// row).
func (b *SegmentBuilder) AppendID(id string) {
	b.ids = append(b.ids, id)
}

// AppendSource records the _source bytes of the next row (JSON document).
// Rows without a call carry an empty source. When no source is appended at
// all, the segment is written without src.zst (_source disabled).
func (b *SegmentBuilder) AppendSource(doc []byte) {
	b.sources = append(b.sources, append([]byte(nil), doc...))
}

// SetSeqRange records the MVCC seq_no range of the rows (design 01 §8).
func (b *SegmentBuilder) SetSeqRange(min, max types.SeqNo) {
	b.minSeq, b.maxSeq = min, max
}

// NumRows returns the current row count (derived from the _id column).
func (b *SegmentBuilder) NumRows() int { return len(b.ids) }

// Dir returns the final segment directory path.
func (b *SegmentBuilder) Dir() string { return b.finalDir }

// Finish serializes every file, fsyncs, computes the checksum manifest,
// writes meta.footer and publishes the segment atomically via rename. The
// returned Segment is an open reader of the published directory.
func (b *SegmentBuilder) Finish() (*Segment, error) {
	if b.finished {
		return nil, fmt.Errorf("segment: builder already finished")
	}
	b.finished = true

	numRows := len(b.ids)
	if numRows == 0 {
		os.RemoveAll(b.tmpDir)
		return nil, fmt.Errorf("segment: refusing to write an empty segment")
	}
	if len(b.sources) > numRows {
		os.RemoveAll(b.tmpDir)
		return nil, fmt.Errorf("segment: %d source rows exceed %d id rows", len(b.sources), numRows)
	}

	// Column files.
	var stats []ColumnStats
	for _, fid := range b.colOrder {
		w := b.columns[fid]
		if w.NumRows() != numRows {
			os.RemoveAll(b.tmpDir)
			return nil, fmt.Errorf("segment: column %q has %d rows, want %d", w.name, w.NumRows(), numRows)
		}
		cf, err := w.Finish()
		if err != nil {
			os.RemoveAll(b.tmpDir)
			return nil, err
		}
		path := filepath.Join(b.tmpDir, fmt.Sprintf("col_%d.bin", fid))
		if err := writeFileSync(path, cf.Data); err != nil {
			os.RemoveAll(b.tmpDir)
			return nil, err
		}
		stats = append(stats, cf.Stats)
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].FieldID < stats[j].FieldID })

	// src.zst + row offsets (decompressed) for the footer.
	var sourceOffsets []uint64
	if len(b.sources) > 0 {
		var stream []byte
		for _, s := range b.sources {
			sourceOffsets = append(sourceOffsets, uint64(len(stream)))
			stream = append(stream, s...)
		}
		sourceOffsets = append(sourceOffsets, uint64(len(stream)))
		for len(sourceOffsets) < numRows+1 {
			// Rows without an appended source own an empty span.
			sourceOffsets = append(sourceOffsets, uint64(len(stream)))
		}
		frame, err := zstdCompressSource(stream)
		if err != nil {
			os.RemoveAll(b.tmpDir)
			return nil, err
		}
		if err := writeFileSync(filepath.Join(b.tmpDir, sourceFileName), frame); err != nil {
			os.RemoveAll(b.tmpDir)
			return nil, err
		}
	}

	// id.dict + id.hash.
	dict, err := buildIDDict(b.ids)
	if err != nil {
		os.RemoveAll(b.tmpDir)
		return nil, err
	}
	if err := writeFileSync(filepath.Join(b.tmpDir, idDictFileName), dict); err != nil {
		os.RemoveAll(b.tmpDir)
		return nil, err
	}
	hashTable, err := buildIDHash(b.ids)
	if err != nil {
		os.RemoveAll(b.tmpDir)
		return nil, err
	}
	if err := writeFileSync(filepath.Join(b.tmpDir, idHashFileName), hashTable); err != nil {
		os.RemoveAll(b.tmpDir)
		return nil, err
	}

	// Checksum manifest over the directory so far (meta.footer excluded).
	manifest, err := checksumManifest(b.tmpDir)
	if err != nil {
		os.RemoveAll(b.tmpDir)
		return nil, err
	}

	footer := &SegmentFooter{
		SegmentID:     b.segID,
		ShardID:       b.shardID,
		NumRows:       uint64(numRows),
		MinSeqNo:      b.minSeq,
		MaxSeqNo:      b.maxSeq,
		ColumnStats:   stats,
		CreatedAt:     time.Now().UnixMilli(),
		FormatVersion: FormatVersion,
		Checksum:      manifest,
		SourceOffsets: sourceOffsets,
	}
	fw := MarshalSegmentFooter(footer)
	footerFile := appendU64LE(nil, footerMagic)
	footerFile = appendU32LE(footerFile, uint32(len(fw)))
	footerFile = append(footerFile, fw...)
	if err := writeFileSync(filepath.Join(b.tmpDir, footerFileName), footerFile); err != nil {
		os.RemoveAll(b.tmpDir)
		return nil, err
	}

	// fsync tmp dir, atomic publish, fsync parent dir (design 01 §5).
	if err := syncDir(b.tmpDir); err != nil {
		os.RemoveAll(b.tmpDir)
		return nil, err
	}
	if err := os.Rename(b.tmpDir, b.finalDir); err != nil {
		os.RemoveAll(b.tmpDir)
		return nil, fmt.Errorf("segment: publish rename: %w", err)
	}
	if err := syncDir(b.parentDir); err != nil {
		return nil, err
	}
	return OpenSegment(b.finalDir)
}

// Abort discards the temporary directory without publishing anything.
func (b *SegmentBuilder) Abort() error {
	if b.tmpDir == "" {
		return nil
	}
	return os.RemoveAll(b.tmpDir)
}

// writeFileSync writes data and fsyncs before closing.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("segment: create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("segment: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("segment: fsync %s: %w", path, err)
	}
	return f.Close()
}

// syncDir fsyncs a directory entry (after rename, the parent dir entry).
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("segment: open dir %s: %w", path, err)
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return fmt.Errorf("segment: fsync dir %s: %w", path, err)
	}
	return d.Close()
}

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// checksumManifest serializes the per-file manifest: for every file in
// dir (recursively, sorted by relative path, meta.footer excluded):
// u32 name_len | name | u64 size | u32 crc32c (design 01 §4.5:
// "全目录文件清单 + 各文件 crc"). Subdirectories (vec.idx/, inv.idx/,
// scalar.idx/) are traversed recursively so index files are covered.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.5
func checksumManifest(dir string) ([]byte, error) {
	var names []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil // recurse into subdirectories
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == footerFileName {
			return nil
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	out := appendU32LE(nil, uint32(len(names)))
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		out = appendU32LE(out, uint32(len(name)))
		out = append(out, name...)
		out = appendU64LE(out, uint64(info.Size()))
		out = appendU32LE(out, crc32.Checksum(data, crc32cTable))
	}
	return out, nil
}

// verifyChecksums recomputes the manifest of dir and compares it byte for
// byte with the footer's checksum field. It reports the first mismatch or
// missing file.
func verifyChecksums(dir string, manifest []byte) error {
	cur, err := checksumManifest(dir)
	if err != nil {
		return err
	}
	if string(cur) != string(manifest) {
		return fmt.Errorf("segment: checksum manifest mismatch (directory corrupted)")
	}
	return nil
}

// colFileName returns the on-disk name of a column file (design 01 §4.1).
func colFileName(fieldID types.FieldID) string {
	return fmt.Sprintf("col_%d.bin", fieldID)
}

// segmentDirName returns the on-disk directory name of a segment.
func segmentDirName(segID types.SegmentID) string {
	return fmt.Sprintf("seg_%d", segID)
}
