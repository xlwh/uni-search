package inverted

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/RoaringBitmap/roaring"
)

// Posting is one (doc, term) occurrence bundle: the doc id, the term
// frequency (saturated at 255 per §3.2), the document's field-length
// bucket (the BM25 B-term source, capped at 65535), and the term's
// token positions in that document (empty when the field is not
// phrase-capable).
type Posting struct {
	DocID     uint32
	TF        uint8
	FieldLen  uint16
	Positions []uint16
}

// maxTF caps the stored term frequency at 255 so it fits in a u8 (§3.2:
// "饱和上限 255"). The BM25 saturation means tf values above ~10
// contribute almost indistinguishably to the score, so capping at 255
// preserves scoring fidelity for any realistic document.
const maxTF = 255

// PostingList is the M0 in-memory posting list for one term (§3.2).
// The docid set is a roaring bitmap (array/bitmap-adaptive); tf and
// field-length buckets are parallel slices indexed by bitmap iteration
// order; positions are a parallel slice of position lists. Once built
// the list is immutable and safe for concurrent read.
type PostingList struct {
	docIDs    *roaring.Bitmap
	tfs       []uint8
	fldLens   []uint16
	positions [][]uint16
}

// NewPostingList builds a PostingList from a slice of Postings. The
// slice is sorted ascending by DocID and duplicate docids are merged
// (the first occurrence wins; callers using the builder never produce
// duplicates, but the constructor is defensive so a malformed input
// cannot desynchronize the roaring cardinality from the parallel
// slices). The positions slice is retained per posting (may be nil).
func NewPostingList(posts []Posting) *PostingList {
	sort.Slice(posts, func(i, j int) bool { return posts[i].DocID < posts[j].DocID })
	bm := roaring.New()
	tfs := make([]uint8, 0, len(posts))
	fls := make([]uint16, 0, len(posts))
	pos := make([][]uint16, 0, len(posts))
	var prev uint32
	first := true
	for _, p := range posts {
		if !first && p.DocID == prev {
			continue
		}
		bm.Add(p.DocID)
		tfs = append(tfs, p.TF)
		fls = append(fls, p.FieldLen)
		pos = append(pos, p.Positions)
		prev = p.DocID
		first = false
	}
	return &PostingList{docIDs: bm, tfs: tfs, fldLens: fls, positions: pos}
}

// DocFreq returns the number of documents the term occurs in.
func (pl *PostingList) DocFreq() int {
	if pl == nil || pl.docIDs == nil {
		return 0
	}
	return int(pl.docIDs.GetCardinality())
}

// DocIDs returns the roaring bitmap of matching doc ids. Callers must
// not mutate it.
func (pl *PostingList) DocIDs() *roaring.Bitmap {
	if pl == nil || pl.docIDs == nil {
		return roaring.New()
	}
	return pl.docIDs
}

// Get resolves the per-doc data for a docid. found is false when the
// term does not occur in the document.
func (pl *PostingList) Get(docID uint32) (tf uint8, fldLen uint16, positions []uint16, found bool) {
	if pl == nil || pl.docIDs == nil {
		return 0, 0, nil, false
	}
	// roaring.Iterator would be O(rank); for the M0 in-memory path a
	// binary-rank lookup via Contains + iteration is acceptable, but
	// the typical caller iterates, so we expose Iterator instead. The
	// random-access path below uses roaring's rank-based index which
	// is O(log n) on the high-water mark containers.
	if !pl.docIDs.Contains(docID) {
		return 0, 0, nil, false
	}
	// linear scan is acceptable for M0 in-memory tests; replace with
	// rank-based index when the storage layer drives real workloads.
	it := pl.docIDs.Iterator()
	idx := 0
	for it.HasNext() {
		d := it.Next()
		if d == docID {
			return pl.tfs[idx], pl.fldLens[idx], pl.positions[idx], true
		}
		idx++
	}
	return 0, 0, nil, false
}

// PostingListIterator walks a PostingList in ascending docid order.
type PostingListIterator struct {
	pl    *PostingList
	it    roaring.IntIterable
	idx   int
}

// Iterate returns an iterator over the posting list.
func (pl *PostingList) Iterate() *PostingListIterator {
	if pl == nil || pl.docIDs == nil {
		return &PostingListIterator{}
	}
	return &PostingListIterator{pl: pl, it: pl.docIDs.Iterator()}
}

// HasNext reports whether another posting remains.
func (it *PostingListIterator) HasNext() bool {
	return it != nil && it.it != nil && it.it.HasNext()
}

// Next returns the next Posting and advances.
func (it *PostingListIterator) Next() Posting {
	docID := it.it.Next()
	p := Posting{DocID: docID, TF: it.pl.tfs[it.idx], FieldLen: it.pl.fldLens[it.idx], Positions: it.pl.positions[it.idx]}
	it.idx++
	return p
}

// PostingListBuilder accumulates postings for one term and finalizes
// them into a sorted PostingList.
type PostingListBuilder struct {
	posts map[uint32]*Posting // keyed by docid
}

// NewPostingListBuilder returns an empty builder.
func NewPostingListBuilder() *PostingListBuilder {
	return &PostingListBuilder{posts: make(map[uint32]*Posting)}
}

// Add records one occurrence of the term in docID at token position
// pos. fieldLen is the document's full field length (token count),
// used for the BM25 B-term; it is updated to the latest non-zero value
// seen. Positions are accumulated only when storePositions is true.
func (b *PostingListBuilder) Add(docID uint32, pos int, fieldLen int, storePositions bool) {
	p, ok := b.posts[docID]
	if !ok {
		p = &Posting{DocID: docID}
		b.posts[docID] = p
	}
	if p.TF < maxTF {
		p.TF++
	}
	if fieldLen > 0 {
		if fieldLen > 0xFFFF {
			fieldLen = 0xFFFF
		}
		p.FieldLen = uint16(fieldLen)
	}
	if storePositions {
		p.Positions = append(p.Positions, uint16(pos))
	}
}

// Finish returns a PostingList sorted by docid. The builder is consumed.
func (b *PostingListBuilder) Finish() *PostingList {
	posts := make([]Posting, 0, len(b.posts))
	for _, p := range b.posts {
		// Positions are appended in token-position order by the
		// analyzer (which emits them ascending), but sort defensively
		// so the delta encoder can assume sorted input.
		sort.Slice(p.Positions, func(i, j int) bool { return p.Positions[i] < p.Positions[j] })
		posts = append(posts, *p)
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].DocID < posts[j].DocID })
	return NewPostingList(posts)
}

// postingsMagic / postingsVersion tag post.bin ("POST" little-endian).
const (
	postingsMagic   uint32 = 0x544F4F50 // "POST" (LE)
	postingsVersion uint8  = 1
)

// writePostingList serializes one term's posting list (the §3.2
// docids+tf+fldLen block; positions are written separately to pos.bin
// via writePositions). Format:
//
//	TermID u32 | docFreq u32 | roaringLen u32 | roaringBytes |
//	FOR(tf[docFreq]) | FOR(fldLen[docFreq])
func writePostingList(w io.Writer, termID TermID, pl *PostingList) error {
	df := uint32(pl.DocFreq())
	roarBytes, err := pl.docIDs.ToBytes()
	if err != nil {
		return fmt.Errorf("inverted: serialize docids: %w", err)
	}
	for _, v := range []any{uint32(termID), df, uint32(len(roarBytes))} {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return fmt.Errorf("inverted: write posting header: %w", err)
		}
	}
	if _, err := w.Write(roarBytes); err != nil {
		return fmt.Errorf("inverted: write docids: %w", err)
	}
	if _, err := w.Write(forEncodeU8(pl.tfs)); err != nil {
		return fmt.Errorf("inverted: write tf: %w", err)
	}
	if _, err := w.Write(forEncodeU16(pl.fldLens)); err != nil {
		return fmt.Errorf("inverted: write fldLen: %w", err)
	}
	return nil
}

// readPostingList is the inverse of writePostingList. It reads one
// term's posting block from r and returns the TermID and PostingList.
func readPostingList(r *byteReader) (TermID, *PostingList, error) {
	termID, err := r.readU32()
	if err != nil {
		return 0, nil, fmt.Errorf("inverted: read TermID: %w", err)
	}
	df, err := r.readU32()
	if err != nil {
		return 0, nil, fmt.Errorf("inverted: read docFreq: %w", err)
	}
	roarLen, err := r.readU32()
	if err != nil {
		return 0, nil, fmt.Errorf("inverted: read roaring len: %w", err)
	}
	roarBytes, err := r.readN(int(roarLen))
	if err != nil {
		return 0, nil, fmt.Errorf("inverted: read roaring bytes: %w", err)
	}
	bm := roaring.New()
	if _, err := bm.FromBuffer(roarBytes); err != nil {
		return 0, nil, fmt.Errorf("inverted: parse roaring: %w", err)
	}
	tfs, err := forDecodeU8(r.readRestOfBlock(int(df)), int(df))
	if err != nil {
		return 0, nil, fmt.Errorf("inverted: decode tf: %w", err)
	}
	fldLens, err := forDecodeU16(r.readRestOfBlock(int(df)), int(df))
	if err != nil {
		return 0, nil, fmt.Errorf("inverted: decode fldLen: %w", err)
	}
	// post.bin carries no positions (they live in pos.bin); initialize
	// the parallel slice so iterator indexing does not panic. A later
	// readPositions pass fills the entries; until then they stay nil
	// (empty position list), which phrase queries treat as no-match.
	positions := make([][]uint16, df)
	return TermID(termID), &PostingList{docIDs: bm, tfs: tfs, fldLens: fldLens, positions: positions}, nil
}

// writePositions serializes one term's per-doc position frames to the
// pos.bin stream. Format:
//
//	TermID u32 | docFreq u32 | per-doc (encodePositions frame)
//
// Documents are written in the same ascending docid order the posting
// list uses, so the i-th position frame corresponds to the i-th posting.
func writePositions(w io.Writer, termID TermID, pl *PostingList) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(termID)); err != nil {
		return fmt.Errorf("inverted: write positions TermID: %w", err)
	}
	df := uint32(pl.DocFreq())
	if err := binary.Write(w, binary.LittleEndian, df); err != nil {
		return fmt.Errorf("inverted: write positions docFreq: %w", err)
	}
	it := pl.Iterate()
	for it.HasNext() {
		p := it.Next()
		if err := encodePositions(w, p.Positions); err != nil {
			return err
		}
	}
	return nil
}

// readPositions is the inverse of writePositions. It reads one term's
// per-doc position frames and returns them aligned with the posting
// list's iteration order.
func readPositions(data []byte, off int) (termID TermID, positions [][]uint16, nextOff int, err error) {
	if off+8 > len(data) {
		return 0, nil, off, fmt.Errorf("inverted: truncated positions header at %d", off)
	}
	termID = TermID(binary.LittleEndian.Uint32(data[off:]))
	df := binary.LittleEndian.Uint32(data[off+4:])
	off += 8
	positions = make([][]uint16, 0, df)
	for i := uint32(0); i < df; i++ {
		var pos []uint16
		pos, off, err = decodePositions(data, off)
		if err != nil {
			return 0, nil, off, err
		}
		positions = append(positions, pos)
	}
	return termID, positions, off, nil
}

// byteReader is a minimal cursor over a byte slice for sequential
// posting-list decoding. It exists because the FOR blocks are self-
// describing length-wise, so callers need "read everything until the
// end of this term" semantics; rather than track lengths explicitly,
// we decode the FOR block from the current cursor and let the bit
// width tell us how far to advance.
type byteReader struct {
	data []byte
	off  int
}

func (r *byteReader) readU32() (uint32, error) {
	if r.off+4 > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint32(r.data[r.off:])
	r.off += 4
	return v, nil
}

func (r *byteReader) readN(n int) ([]byte, error) {
	if n < 0 || r.off+n > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	b := r.data[r.off : r.off+n]
	r.off += n
	return b, nil
}

// readRestOfBlock returns a slice starting at the cursor from which a
// FOR block of `count` values can be decoded. It advances the cursor
// past the block. The FOR block is self-describing: 1 byte bit width
// then ceil(count*width/8) packed bytes.
func (r *byteReader) readRestOfBlock(count int) []byte {
	if r.off >= len(r.data) {
		return nil
	}
	bitW := int(r.data[r.off])
	packedLen := (count*bitW + 7) / 8
	end := r.off + 1 + packedLen
	if end > len(r.data) {
		end = len(r.data)
	}
	b := r.data[r.off:end]
	r.off = end
	return b
}
