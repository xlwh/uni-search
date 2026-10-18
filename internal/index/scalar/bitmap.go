package scalar

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/index"
)

func init() {
	index.RegisterIndex(index.KindBitmap, func() index.Index { return &Bitmap{} })
}

// BitmapCardinalityLimit is the §6.3 auto-enable rule: a keyword
// column whose global distinct cardinality stays below this limit gets
// a bitmap index at flush time; the storage layer probes the rule via
// Build (which enforces it) or via AutoEnable below.
const BitmapCardinalityLimit = 4096

// AutoEnable reports whether a column with the given distinct value
// count qualifies for the bitmap index (§6.3: < 4096).
func AutoEnable(distinct int) bool { return distinct >= 0 && distinct < BitmapCardinalityLimit }

// Segment index file set (little-endian), under the kind's index
// directory (seg_xxx/bitmap.idx/ in the full storage mapping):
//
//	bitmap.meta   magic, version, mode, cardinality, n
//	bitmap.dict   exact mode: u32 count + per value (u32 len + bytes), sorted
//	bitmap.posts  exact mode: per dict value (u32 len + roaring bytes)
//	bloom.bin     bloom mode: the §6.4 filter over all values
const (
	fileMetaB   = "bitmap.meta"
	fileDict    = "bitmap.dict"
	filePosts   = "bitmap.posts"
	fileBloomBn = "bloom.bin"
)

const (
	bitmapMagic   uint32 = 0x31504D42 // "BMP1"
	bitmapVersion uint8  = 1
	modeExact     uint8  = 0 // low-cardinality: value dict + roaring bitmaps
	modeBloom     uint8  = 1 // high-cardinality: bloom pre-filter (§6.4)
)

// BitmapConfig holds the bitmap index parameters. The zero value is
// valid.
type BitmapConfig struct {
	// BloomFPP is the false-positive probability of the bloom fallback
	// built when the auto-enable rule rejects a column; 0 -> 1% (§6.4).
	BloomFPP float64
}

func (c BitmapConfig) resolve() BitmapConfig {
	if c.BloomFPP <= 0 || c.BloomFPP >= 1 || isNaN(c.BloomFPP) {
		c.BloomFPP = DefaultBloomFPP
	}
	return c
}

// bitmapStore is the built or loaded in-memory index data. Immutable
// once built/loaded; safe for concurrent search.
type bitmapStore struct {
	mode        uint8
	n           int
	cardinality int               // exact in modeExact; lower bound (limit) in modeBloom
	dict        []string          // sorted distinct values (modeExact)
	posts       []*roaring.Bitmap // aligned with dict (modeExact)
	bloom       *Bloom            // modeBloom
}

// Bitmap is the index.Index implementation registered under
// index.KindBitmap (§6.3/§6.4). Build detects the column cardinality:
// below BitmapCardinalityLimit it writes the exact value-dictionary +
// per-value roaring bitmaps; at or above the limit the auto-enable
// rule rejects exact bitmaps and it writes the bloom pre-filter
// instead (the M0 segment-level stand-in for the design's per-block
// blooms, which live behind the storage column interface).
type Bitmap struct {
	cfg   BitmapConfig
	store *bitmapStore
}

// NewBitmap returns a Bitmap index with the given configuration.
func NewBitmap(cfg BitmapConfig) *Bitmap { return &Bitmap{cfg: cfg} }

// Kind implements index.Index.
func (ix *Bitmap) Kind() index.IndexKind { return index.KindBitmap }

// Build implements index.Index: the provider must expose BytesColumn.
// The scan aborts distinct tracking once the cardinality reaches the
// §6.3 limit, so memory stays bounded by the limit in either mode.
func (ix *Bitmap) Build(ctx context.Context, col index.ColumnProvider, w index.IndexOutput) error {
	bc, ok := col.(BytesColumn)
	if !ok {
		return fmt.Errorf("scalar: column %q does not expose a bytes row iterator "+
			"(need BytesColumn)", col.Field())
	}
	n := int(col.Rows())
	st, err := buildBitmapStore(ctx, n, bc.ForEachValue, ix.cfg.resolve())
	if err != nil {
		return err
	}
	if err := writeBitmap(w, st); err != nil {
		return err
	}
	ix.store = st
	return nil
}

// buildBitmapStore produces a store from a row-order value iterator,
// enforcing the auto-enable rule: it tracks distinct values up to
// BitmapCardinalityLimit, then either finalizes the exact bitmaps
// (below the limit) or falls back to a second scan filling a bloom.
func buildBitmapStore(ctx context.Context, n int, iter func(fn func(rowID int32, v []byte) error) error, cfg BitmapConfig) (*bitmapStore, error) {
	m := make(map[string]*roaring.Bitmap)
	aborted := false
	err := iter(func(rowID int32, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if aborted {
			return nil // distinct cap hit; drain the pass cheaply
		}
		bm, ok := m[string(v)]
		if !ok {
			if len(m) >= BitmapCardinalityLimit {
				// Next distinct value would reach the limit: the
				// auto-enable rule (strictly below) can no longer fire.
				aborted = true
				return nil
			}
			bm = roaring.New()
			m[string(v)] = bm
		}
		bm.Add(uint32(rowID))
		return nil
	})
	if err != nil {
		return nil, err
	}
	// §6.3: exact bitmaps only when the full distinct count stays
	// strictly below the limit (aborted implies it was reached).
	if !aborted && AutoEnable(len(m)) {
		st := &bitmapStore{mode: modeExact, n: n, cardinality: len(m)}
		st.dict = make([]string, 0, len(m))
		for k := range m {
			st.dict = append(st.dict, k)
		}
		sort.Strings(st.dict)
		st.posts = make([]*roaring.Bitmap, len(st.dict))
		for i, k := range st.dict {
			st.posts[i] = m[k]
		}
		return st, nil
	}
	// Auto-enable rejected the column: bloom pre-filter over a second
	// pass (§6.4). Cardinality is only known as a lower bound.
	bloom := newBloom(n, cfg.BloomFPP)
	err = iter(func(_ int32, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bloom.Add(v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &bitmapStore{mode: modeBloom, n: n, cardinality: BitmapCardinalityLimit, bloom: bloom}, nil
}

// Load implements index.Index.
func (ix *Bitmap) Load(r index.IndexInput) (index.LoadedIndex, error) {
	st, err := readBitmap(r)
	if err != nil {
		return nil, err
	}
	return &bitmapLoaded{st: st}, nil
}

// Merge implements index.Index: dictionaries and bitmaps are merged by
// rebuilding over the concatenated row-ordered value streams; new row
// ids are assigned by position (the storage layer owns the old-to-new
// row mapping, same contract as HNSW/BKD). Bloom-mode sources do not
// retain their values and cannot merge.
func (ix *Bitmap) Merge(ctx context.Context, others []index.LoadedIndex, w index.IndexOutput) error {
	var streams []func(fn func(rowID int32, v []byte) error) error
	total := 0
	appendStore := func(s *bitmapStore) error {
		if s.mode != modeExact {
			return fmt.Errorf("%w: bloom-mode bitmap index retains no values",
				index.ErrUnsupported)
		}
		// Reconstruct the row-ordered values from dict+bitmaps.
		vals := make([]string, s.n)
		for di, term := range s.dict {
			it := s.posts[di].Iterator()
			for it.HasNext() {
				vals[it.Next()] = term
			}
		}
		streams = append(streams, func(fn func(rowID int32, v []byte) error) error {
			for row, term := range vals {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := fn(int32(row), []byte(term)); err != nil {
					return err
				}
			}
			return nil
		})
		total += s.n
		return nil
	}
	if ix.store != nil {
		if err := appendStore(ix.store); err != nil {
			return err
		}
	}
	for _, o := range others {
		li, ok := o.(*bitmapLoaded)
		if !ok {
			return fmt.Errorf("scalar: cannot merge foreign loaded index %T", o)
		}
		if err := appendStore(li.st); err != nil {
			return err
		}
	}
	if len(streams) == 0 {
		return fmt.Errorf("scalar: merge requires at least one source index")
	}
	// Concatenate with positional row reassignment. The closure must be
	// re-iterable: the bloom fallback path consumes it a second time.
	concat := func(fn func(rowID int32, v []byte) error) error {
		pos := 0
		for _, stream := range streams {
			err := stream(func(_ int32, v []byte) error {
				p := pos
				pos++
				return fn(int32(p), v)
			})
			if err != nil {
				return err
			}
		}
		return nil
	}
	st, err := buildBitmapStore(ctx, total, concat, ix.cfg.resolve())
	if err != nil {
		return err
	}
	if err := writeBitmap(w, st); err != nil {
		return err
	}
	ix.store = st
	return nil
}

// Quantize implements index.Index: scalar indexes have no quantization.
func (ix *Bitmap) Quantize(ctx context.Context, to index.QuantLevel) error {
	return fmt.Errorf("%w: bitmap has no quantization (level %d)", index.ErrUnsupported, int(to))
}

// MemEstimate implements index.Index.
func (ix *Bitmap) MemEstimate() int64 {
	if ix.store == nil {
		return 0
	}
	return ix.store.memBytes()
}

func (s *bitmapStore) memBytes() int64 {
	if s == nil {
		return 0
	}
	sz := int64(64)
	if s.mode == modeExact {
		for _, t := range s.dict {
			sz += int64(len(t)) + 16
		}
		for _, p := range s.posts {
			sz += int64(p.GetSerializedSizeInBytes()) + 16
		}
		return sz
	}
	return sz + s.bloom.MemBytes()
}

// bitmapLoaded is the searchable handle over one built bitmap index.
// Immutable and safe for concurrent use.
type bitmapLoaded struct {
	st *bitmapStore
}

var (
	_ index.LoadedIndex = (*bitmapLoaded)(nil)
	_ TermSearcher      = (*bitmapLoaded)(nil)
)

// SearchTerm implements TermSearcher: equality against the value
// dictionary. The returned bitmap is shared with the index and must not
// be mutated; absent terms yield an empty bitmap.
func (l *bitmapLoaded) SearchTerm(ctx context.Context, term []byte, budget index.SearchBudget) (*roaring.Bitmap, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.st.mode != modeExact {
		return nil, fmt.Errorf("%w: bloom-mode index cannot enumerate rows; "+
			"use MightContain + column scan", index.ErrUnsupported)
	}
	i := sort.SearchStrings(l.st.dict, string(term))
	if i < len(l.st.dict) && l.st.dict[i] == string(term) {
		return l.st.posts[i], nil
	}
	return roaring.New(), nil
}

// MightContain answers equality pre-checks: exact mode is definitive;
// bloom mode is probabilistic with the configured fpp (§6.4). This is
// the scan pre-filter surface for high-cardinality columns.
func (l *bitmapLoaded) MightContain(term []byte) bool {
	if l.st.mode == modeExact {
		i := sort.SearchStrings(l.st.dict, string(term))
		return i < len(l.st.dict) && l.st.dict[i] == string(term)
	}
	return l.st.bloom.MightContain(term)
}

// Cardinality returns the distinct-value count detected at build. For
// bloom-mode indexes it reports the lower bound (>= limit).
func (l *bitmapLoaded) Cardinality() int { return l.st.cardinality }

// AutoEnabled reports whether the §6.3 auto-enable rule fired (exact
// bitmaps built because the cardinality stayed below the limit).
func (l *bitmapLoaded) AutoEnabled() bool { return l.st.mode == modeExact }

// Search implements index.LoadedIndex: index.Query.Term is the value
// to match (no tag prefix); bloom-mode indexes report ErrUnsupported
// because they cannot enumerate rows.
func (l *bitmapLoaded) Search(ctx context.Context, q index.Query, budget index.SearchBudget) (*index.SearchResult, error) {
	rows, err := l.SearchTerm(ctx, q.Term, budget)
	if err != nil {
		return nil, err
	}
	return hitsFromBitmap(rows, q.K, q.Filter), nil
}

// Close implements index.LoadedIndex. M0 stores are in-memory.
func (l *bitmapLoaded) Close() error { return nil }

// writeBitmap serializes a store into the file set.
func writeBitmap(w index.IndexOutput, st *bitmapStore) error {
	f, err := w.Create(fileMetaB)
	if err != nil {
		return fmt.Errorf("scalar: create %s: %w", fileMetaB, err)
	}
	hdr := []any{bitmapMagic, bitmapVersion, st.mode,
		int32(st.cardinality), int32(st.n)}
	for _, v := range hdr {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			f.Close()
			return fmt.Errorf("scalar: write %s header: %w", fileMetaB, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("scalar: close %s: %w", fileMetaB, err)
	}

	if st.mode == modeBloom {
		fb, err := w.Create(fileBloomBn)
		if err != nil {
			return fmt.Errorf("scalar: create %s: %w", fileBloomBn, err)
		}
		if _, err := st.bloom.WriteTo(fb); err != nil {
			fb.Close()
			return err
		}
		if err := fb.Close(); err != nil {
			return fmt.Errorf("scalar: close %s: %w", fileBloomBn, err)
		}
		return nil
	}

	fd, err := w.Create(fileDict)
	if err != nil {
		return fmt.Errorf("scalar: create %s: %w", fileDict, err)
	}
	if err := binary.Write(fd, binary.LittleEndian, uint32(len(st.dict))); err != nil {
		fd.Close()
		return fmt.Errorf("scalar: write %s count: %w", fileDict, err)
	}
	for _, t := range st.dict {
		if err := binary.Write(fd, binary.LittleEndian, uint32(len(t))); err != nil {
			fd.Close()
			return fmt.Errorf("scalar: write %s len: %w", fileDict, err)
		}
		if _, err := fd.Write([]byte(t)); err != nil {
			fd.Close()
			return fmt.Errorf("scalar: write %s value: %w", fileDict, err)
		}
	}
	if err := fd.Close(); err != nil {
		return fmt.Errorf("scalar: close %s: %w", fileDict, err)
	}

	fp, err := w.Create(filePosts)
	if err != nil {
		return fmt.Errorf("scalar: create %s: %w", filePosts, err)
	}
	for _, p := range st.posts {
		b, err := p.ToBytes()
		if err != nil {
			fp.Close()
			return fmt.Errorf("scalar: serialize posting bitmap: %w", err)
		}
		if err := binary.Write(fp, binary.LittleEndian, uint32(len(b))); err != nil {
			fp.Close()
			return fmt.Errorf("scalar: write %s len: %w", filePosts, err)
		}
		if _, err := fp.Write(b); err != nil {
			fp.Close()
			return fmt.Errorf("scalar: write %s data: %w", filePosts, err)
		}
	}
	if err := fp.Close(); err != nil {
		return fmt.Errorf("scalar: close %s: %w", filePosts, err)
	}
	return nil
}

// readBitmap is the inverse of writeBitmap.
func readBitmap(r index.IndexInput) (*bitmapStore, error) {
	f, err := r.Open(fileMetaB)
	if err != nil {
		return nil, fmt.Errorf("scalar: open %s: %w", fileMetaB, err)
	}
	var magic uint32
	var version, mode uint8
	var cardinality, n int32
	if err := binary.Read(f, binary.LittleEndian, &magic); err != nil {
		f.Close()
		return nil, fmt.Errorf("scalar: read %s magic: %w", fileMetaB, err)
	}
	for _, v := range []any{&version, &mode, &cardinality, &n} {
		if err := binary.Read(f, binary.LittleEndian, v); err != nil {
			f.Close()
			return nil, fmt.Errorf("scalar: read %s header: %w", fileMetaB, err)
		}
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("scalar: close %s: %w", fileMetaB, err)
	}
	if magic != bitmapMagic || version != bitmapVersion {
		return nil, fmt.Errorf("scalar: bad bitmap header (magic %#x version %d)", magic, version)
	}
	if mode != modeExact && mode != modeBloom || cardinality < 0 || n < 0 {
		return nil, fmt.Errorf("scalar: corrupt bitmap header (mode=%d card=%d n=%d)", mode, cardinality, n)
	}
	st := &bitmapStore{mode: mode, cardinality: int(cardinality), n: int(n)}
	if mode == modeBloom {
		fb, err := r.Open(fileBloomBn)
		if err != nil {
			return nil, fmt.Errorf("scalar: open %s: %w", fileBloomBn, err)
		}
		defer fb.Close()
		st.bloom, err = ReadBloom(fb)
		if err != nil {
			return nil, err
		}
		return st, nil
	}
	fd, err := r.Open(fileDict)
	if err != nil {
		return nil, fmt.Errorf("scalar: open %s: %w", fileDict, err)
	}
	defer fd.Close()
	var count uint32
	if err := binary.Read(fd, binary.LittleEndian, &count); err != nil {
		return nil, fmt.Errorf("scalar: read %s count: %w", fileDict, err)
	}
	if int(count) != int(cardinality) {
		return nil, fmt.Errorf("scalar: dict count %d != header cardinality %d", count, cardinality)
	}
	st.dict = make([]string, count)
	for i := range st.dict {
		var l uint32
		if err := binary.Read(fd, binary.LittleEndian, &l); err != nil {
			return nil, fmt.Errorf("scalar: read %s entry %d: %w", fileDict, i, err)
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(fd, b); err != nil {
			return nil, fmt.Errorf("scalar: read %s entry %d bytes: %w", fileDict, i, err)
		}
		st.dict[i] = string(b)
	}
	if !sort.StringsAreSorted(st.dict) {
		return nil, fmt.Errorf("scalar: bitmap.dict not sorted")
	}
	fp, err := r.Open(filePosts)
	if err != nil {
		return nil, fmt.Errorf("scalar: open %s: %w", filePosts, err)
	}
	defer fp.Close()
	st.posts = make([]*roaring.Bitmap, count)
	for i := range st.posts {
		var l uint32
		if err := binary.Read(fp, binary.LittleEndian, &l); err != nil {
			return nil, fmt.Errorf("scalar: read %s len %d: %w", filePosts, i, err)
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(fp, b); err != nil {
			return nil, fmt.Errorf("scalar: read %s data %d: %w", filePosts, i, err)
		}
		bm := roaring.New()
		if _, err := bm.FromBuffer(b); err != nil {
			return nil, fmt.Errorf("scalar: parse %s bitmap %d: %w", filePosts, i, err)
		}
		st.posts[i] = bm
	}
	return st, nil
}
