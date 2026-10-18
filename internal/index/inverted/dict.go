// Package inverted implements the M0 inverted index (design
// 02-index-engine.md §3): a sorted-array term dictionary with a
// zstd-compressed blob (prefix sharing via zstd), roaring-bitmap posting
// lists with FOR-compressed tf and field-length buckets, position blocks
// for phrase queries, and BM25 scoring. The package registers
// index.KindInverted in init.
//
// The M0 dictionary is the "sorted array + binary search" form (§3.1).
// The M1 FST will swap in behind the same TermDict surface. TermIDs are
// assigned in sorted term order starting at 0, so the dictionary doubles
// as a sorted term table that binary search walks directly.
package inverted

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// TermID is the numeric id assigned to a distinct term in the dictionary
// (§3.1). IDs are assigned in sorted term order starting at 0.
type TermID uint32

// Segment index file names (§3.1/§3.2 layout under seg_xxx/inv/).
const (
	fileTerms     = "terms.sorted" // §3.1 sorted-array dictionary
	filePostings  = "post.bin"     // §3.2 posting lists (one field per index)
	filePositions = "pos.bin"      // §3.2 position blocks (phrase-capable fields)
	fileStats     = "stats.bin"    // §3.2/§3.3 segment-level BM25 statistics
)

// dictMagic / dictVersion tag terms.sorted ("TDR1" little-endian).
const (
	dictMagic   uint32 = 0x31524454 // "TDR1"
	dictVersion uint8  = 1
)

// zstd codecs shared across dictionary (de)compression. klauspost
// EncodeAll/DecodeAll are safe for concurrent use.
var (
	zstdOnce sync.Once
	zstdEnc  *zstd.Encoder
	zstdDec  *zstd.Decoder
	zstdErr  error
)

func zstdCodecs() (*zstd.Encoder, *zstd.Decoder, error) {
	zstdOnce.Do(func() {
		zstdEnc, zstdErr = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if zstdErr != nil {
			return
		}
		zstdDec, zstdErr = zstd.NewReader(nil)
	})
	return zstdEnc, zstdDec, zstdErr
}

func zstdCompress(src []byte) ([]byte, error) {
	enc, _, err := zstdCodecs()
	if err != nil {
		return nil, err
	}
	return enc.EncodeAll(src, nil), nil
}

func zstdDecompress(src []byte) ([]byte, error) {
	_, dec, err := zstdCodecs()
	if err != nil {
		return nil, err
	}
	out, err := dec.DecodeAll(src, nil)
	if err != nil {
		return nil, fmt.Errorf("inverted: zstd decode: %w", err)
	}
	return out, nil
}

// TermDict is the M0 sorted-array term dictionary (§3.1). Lookup is a
// binary search over the in-memory sorted term slice; serialization is a
// zstd-compressed length-prefixed blob (prefix sharing via zstd). The
// file format is mmap-friendly — a fixed header followed by the offsets
// array and the compressed blob — so M2 can swap to mmap-backed loading
// without a format migration.
type TermDict struct {
	// terms are the distinct terms in sorted order. terms[i] has TermID i.
	terms []string
	// blob is the decompressed concatenation of every term in sorted
	// order; offsets[i:i+1] index into it. Kept for iteration; nil when
	// the dict was assembled directly from a terms slice.
	blob    []byte
	offsets []uint32
}

// NewTermDict builds a TermDict from a sorted, deduplicated term slice.
// The slice is retained; callers must not mutate it. Sort order and
// uniqueness are verified.
func NewTermDict(sorted []string) (*TermDict, error) {
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return nil, fmt.Errorf("inverted: duplicate term %q at index %d", sorted[i], i)
		}
		if sorted[i] < sorted[i-1] {
			return nil, fmt.Errorf("inverted: term %q at index %d precedes %q", sorted[i], i, sorted[i-1])
		}
	}
	return &TermDict{terms: sorted}, nil
}

// Len returns the number of distinct terms.
func (d *TermDict) Len() int { return len(d.terms) }

// Term returns the term at the given id; behavior undefined when id is
// out of range.
func (d *TermDict) Term(id TermID) string { return d.terms[id] }

// Get resolves a term to its TermID. The boolean is false when the term
// is not in the dictionary.
func (d *TermDict) Get(term []byte) (TermID, bool) {
	if d == nil || len(d.terms) == 0 {
		return 0, false
	}
	i := sort.Search(len(d.terms), func(i int) bool {
		return d.terms[i] >= string(term)
	})
	if i < len(d.terms) && d.terms[i] == string(term) {
		return TermID(i), true
	}
	return 0, false
}

// GetStr is the string-argument form of Get.
func (d *TermDict) GetStr(term string) (TermID, bool) {
	if d == nil || len(d.terms) == 0 {
		return 0, false
	}
	i := sort.Search(len(d.terms), func(i int) bool {
		return d.terms[i] >= term
	})
	if i < len(d.terms) && d.terms[i] == term {
		return TermID(i), true
	}
	return 0, false
}

// Iterate returns a TermIterator over the dictionary in sorted order. A
// non-empty prefix restricts iteration to terms sharing that prefix.
func (d *TermDict) Iterate(prefix []byte) *TermIterator {
	start := 0
	if len(prefix) > 0 {
		start = sort.Search(len(d.terms), func(i int) bool {
			return d.terms[i] >= string(prefix)
		})
	}
	return &TermIterator{d: d, i: start, prefix: prefix}
}

// TermIterator walks a TermDict in sorted order, optionally filtered by
// a prefix (M1 wildcard/prefix queries consume this; M0 uses it for
// debug/printing).
type TermIterator struct {
	d      *TermDict
	i      int
	prefix []byte
}

// HasNext reports whether another term remains.
func (it *TermIterator) HasNext() bool {
	if it == nil || it.d == nil {
		return false
	}
	if it.i >= len(it.d.terms) {
		return false
	}
	if len(it.prefix) == 0 {
		return true
	}
	return bytes.HasPrefix([]byte(it.d.terms[it.i]), it.prefix)
}

// Next returns the next (TermID, term) pair and advances.
func (it *TermIterator) Next() (TermID, string) {
	if !it.HasNext() {
		return 0, ""
	}
	id := TermID(it.i)
	t := it.d.terms[it.i]
	it.i++
	return id, t
}

// writeTermDict serializes a TermDict into the §3.1 file format:
//
//	magic u32 | version u8 | count u32 |
//	offsets[count+1] u32 (offsets[0]=0, offsets[count]=blobLen) |
//	u32 raw blob length | zstd(blob)
//
// All integers are little-endian.
func writeTermDict(w io.Writer, d *TermDict) error {
	count := len(d.terms)
	offsets := make([]uint32, count+1)
	blob := make([]byte, 0, count*8)
	for i, t := range d.terms {
		offsets[i] = uint32(len(blob))
		blob = append(blob, t...)
	}
	offsets[count] = uint32(len(blob))

	compressed, err := zstdCompress(blob)
	if err != nil {
		return fmt.Errorf("inverted: compress terms blob: %w", err)
	}
	hdr := []any{dictMagic, dictVersion, uint32(count)}
	for _, v := range hdr {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return fmt.Errorf("inverted: write terms header: %w", err)
		}
	}
	if err := binary.Write(w, binary.LittleEndian, offsets); err != nil {
		return fmt.Errorf("inverted: write terms offsets: %w", err)
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(blob))); err != nil {
		return fmt.Errorf("inverted: write terms blob length: %w", err)
	}
	if _, err := w.Write(compressed); err != nil {
		return fmt.Errorf("inverted: write terms blob: %w", err)
	}
	return nil
}

// readTermDict is the inverse of writeTermDict.
func readTermDict(r io.Reader) (*TermDict, error) {
	var magic uint32
	var version uint8
	var count uint32
	for _, v := range []any{&magic, &version, &count} {
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			return nil, fmt.Errorf("inverted: read terms header: %w", err)
		}
	}
	if magic != dictMagic {
		return nil, fmt.Errorf("inverted: bad terms magic %#x", magic)
	}
	if version != dictVersion {
		return nil, fmt.Errorf("inverted: unsupported terms version %d", version)
	}
	if count > 1<<28 {
		return nil, fmt.Errorf("inverted: implausible term count %d", count)
	}
	offsets := make([]uint32, count+1)
	if err := binary.Read(r, binary.LittleEndian, offsets); err != nil {
		return nil, fmt.Errorf("inverted: read terms offsets: %w", err)
	}
	var blobLen uint32
	if err := binary.Read(r, binary.LittleEndian, &blobLen); err != nil {
		return nil, fmt.Errorf("inverted: read terms blob length: %w", err)
	}
	if offsets[count] != blobLen {
		return nil, fmt.Errorf("inverted: terms blob length %d mismatches offsets %d", blobLen, offsets[count])
	}
	compressed := make([]byte, 0)
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			compressed = append(compressed, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("inverted: read terms blob: %w", err)
		}
	}
	blob, err := zstdDecompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("inverted: decompress terms blob: %w", err)
	}
	if uint32(len(blob)) != blobLen {
		return nil, fmt.Errorf("inverted: decompressed terms blob %d != %d", len(blob), blobLen)
	}
	terms := make([]string, count)
	for i := range terms {
		start := offsets[i]
		end := offsets[i+1]
		terms[i] = string(blob[start:end])
	}
	return &TermDict{terms: terms, blob: blob, offsets: offsets}, nil
}

// TermDictBuilder accumulates distinct terms and finalizes them into a
// sorted TermDict. It is the build-side companion used by the index
// builder (§3.1: terms are harvested from analyzed documents, then
// sorted and assigned ids).
type TermDictBuilder struct {
	seen  map[string]struct{}
	order []string
}

// NewTermDictBuilder returns an empty builder.
func NewTermDictBuilder() *TermDictBuilder {
	return &TermDictBuilder{seen: make(map[string]struct{})}
}

// Add records a term if it has not been seen before. Returns true when
// the term was new.
func (b *TermDictBuilder) Add(term string) bool {
	if _, ok := b.seen[term]; ok {
		return false
	}
	b.seen[term] = struct{}{}
	b.order = append(b.order, term)
	return true
}

// Finish sorts the accumulated terms and returns a TermDict. The
// builder is consumed and must not be reused.
func (b *TermDictBuilder) Finish() (*TermDict, error) {
	sort.Strings(b.order)
	return NewTermDict(b.order)
}
