package inverted

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/index/analyzer"
)

func init() {
	// Register the M0 inverted index factory (§1.2). The default config
	// uses the standard analyzer; production wiring overrides it via
	// New with a field-specific Config.
	index.RegisterIndex(index.KindInverted, func() index.Index {
		return New(DefaultConfig())
	})
}

// TextColumn extends index.ColumnProvider with per-row text access for
// text-family columns (§3). The registry's ColumnProvider is in its
// M0 vector-only minimal form (internal/index/registry.go), so text
// columns reach Build through this narrow extension — the storage
// engine implements it on top of the col_*.bin block reader without
// either side importing the other's internals (00 §4 dependency rule).
// The interface is structurally identical to scalar.BytesColumn so a
// keyword column implementer satisfies both.
type TextColumn interface {
	index.ColumnProvider
	// ForEachText invokes fn once per row in row order with the row's
	// raw text. The bytes are only valid for the duration of the call.
	ForEachText(fn func(rowID int32, text []byte) error) error
}

// Config configures an inverted index build. The zero value is not
// usable: callers should use DefaultConfig and override fields.
type Config struct {
	// Field is the indexed field name; written to stats.bin.
	Field string
	// Analyzer tokenizes text into terms (§4). The same instance must
	// serve build and query so semantics stay identical.
	Analyzer analyzer.Analyzer
	// BM25 tunes scoring (§3.3). Zero value uses the ES 8.x defaults
	// (k1=1.2, b=0.75).
	BM25 BM25Params
	// StorePositions enables per-(term,doc) position recording so
	// match_phrase queries can verify token order (§3.2 pos.{field}).
	// False keeps the index smaller at the cost of phrase support.
	StorePositions bool
}

// DefaultConfig returns a Config for the standard analyzer on the
// "text" field with positions enabled and ES 8.x BM25 defaults.
func DefaultConfig() Config {
	std, _ := analyzer.Get(analyzer.NameStandard, nil)
	return Config{
		Field:          "text",
		Analyzer:       std,
		BM25:           DefaultBM25Params(),
		StorePositions: true,
	}
}

// New returns an inverted Index with the given build configuration.
func New(cfg Config) *Index {
	if cfg.Analyzer == nil {
		std, _ := analyzer.Get(analyzer.NameStandard, nil)
		cfg.Analyzer = std
	}
	if cfg.Field == "" {
		cfg.Field = "text"
	}
	if cfg.BM25.K1 == 0 && cfg.BM25.B == 0 {
		cfg.BM25 = DefaultBM25Params()
	}
	return &Index{cfg: cfg}
}

// Index is the index.Index implementation registered under
// index.KindInverted (§1.1). It carries the build configuration and,
// after Build/Merge, the built store; the loaded artifact handed to
// callers is loadedIndex.
type Index struct {
	cfg   Config
	store *invertedStore
}

// Kind implements index.Index.
func (ix *Index) Kind() index.IndexKind { return index.KindInverted }

// Config returns the build configuration. Tests use it to read the
// field name and analyzer the registry-wired factory picked.
func (ix *Index) Config() Config { return ix.cfg }

// invertedStore is the built or loaded in-memory index data. Immutable
// once built/loaded; safe for concurrent search.
type invertedStore struct {
	dict     *TermDict
	postings map[TermID]*PostingList
	stats    SegmentStats
	field    string
}

// Build implements index.Index: the provider must expose TextColumn.
// Each document is tokenized with the configured analyzer, terms are
// accumulated into per-term PostingListBuilders (with positions when
// StorePositions is set), the dictionary is finalized in sorted order,
// posting lists are finalized, and the §3.1/§3.2/§3.3 file set is
// written through w.
func (ix *Index) Build(ctx context.Context, col index.ColumnProvider, w index.IndexOutput) error {
	tc, ok := col.(TextColumn)
	if !ok {
		return fmt.Errorf("inverted: column %q does not expose a text row iterator "+
			"(need TextColumn)", col.Field())
	}
	n := int(col.Rows())
	builders := make(map[string]*PostingListBuilder)
	fieldLens := make([]int, n)
	dictB := NewTermDictBuilder()

	err := tc.ForEachText(func(rowID int32, text []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		tokens := ix.cfg.Analyzer.Analyze(text)
		fieldLens[rowID] = len(tokens)
		for _, tok := range tokens {
			b, ok := builders[tok.Term]
			if !ok {
				b = NewPostingListBuilder()
				builders[tok.Term] = b
			}
			b.Add(uint32(rowID), tok.Pos, len(tokens), ix.cfg.StorePositions)
			dictB.Add(tok.Term)
		}
		return nil
	})
	if err != nil {
		return err
	}
	dict, err := dictB.Finish()
	if err != nil {
		return err
	}
	postings := make(map[TermID]*PostingList, len(builders))
	for term, b := range builders {
		id, ok := dict.GetStr(term)
		if !ok {
			continue
		}
		postings[id] = b.Finish()
	}
	stats := NewSegmentStats(fieldLens)
	st := &invertedStore{dict: dict, postings: postings, stats: stats, field: ix.cfg.Field}
	if err := writeInvertedFiles(w, st, ix.cfg.StorePositions); err != nil {
		return err
	}
	ix.store = st
	return nil
}

// Load implements index.Index: it reads the §3.1/§3.2/§3.3 file set from
// r and returns an immutable, concurrency-safe loadedIndex.
func (ix *Index) Load(r index.IndexInput) (index.LoadedIndex, error) {
	st, err := readInvertedFiles(r)
	if err != nil {
		return nil, err
	}
	return &loadedIndex{st: st, cfg: ix.cfg}, nil
}

// Merge implements index.Index. M0 semantics: rebuild by concatenating
// the per-term posting lists of every source. New row ids are assigned
// by position; the storage layer owns the old-to-new row mapping (same
// contract as HNSW/Bitmap). The file format stays the same so the M1
// WAND merge path can swap in without migration.
func (ix *Index) Merge(ctx context.Context, others []index.LoadedIndex, w index.IndexOutput) error {
	var src []*invertedStore
	if ix.store != nil {
		src = append(src, ix.store)
	}
	for _, o := range others {
		li, ok := o.(*loadedIndex)
		if !ok {
			return fmt.Errorf("inverted: cannot merge foreign loaded index %T", o)
		}
		src = append(src, li.st)
	}
	if len(src) == 0 {
		return fmt.Errorf("inverted: merge requires at least one source index")
	}
	// Collect the union of terms across all sources; assign new sorted
	// TermIDs; merge each term's postings in source order.
	termSet := make(map[string]struct{})
	for _, s := range src {
		it := s.dict.Iterate(nil)
		for it.HasNext() {
			_, t := it.Next()
			termSet[t] = struct{}{}
		}
	}
	terms := make([]string, 0, len(termSet))
	for t := range termSet {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	dict, err := NewTermDict(terms)
	if err != nil {
		return err
	}
	postings := make(map[TermID]*PostingList, len(terms))
	// Per-source row offset so docids are reassigned by concatenation.
	rowOff := make([]uint32, len(src))
	totalRows := uint32(0)
	for i, s := range src {
		rowOff[i] = totalRows
		totalRows += s.stats.DocCount
	}
	// Per-source field lengths, concatenated for the merged stats.
	mergedFieldLens := make([]int, 0, int(totalRows))
	for _, s := range src {
		// Reconstruct per-doc field lengths from each term's posting
		// fldLen bucket: a doc's field length is the max fldLen seen
		// across its terms (terms only see their own tf, but fldLen is
		// the full document field length stored redundantly per term).
		docLen := make([]int, s.stats.DocCount)
		for _, pl := range s.postings {
			it := pl.Iterate()
			for it.HasNext() {
				p := it.Next()
				if int(p.FieldLen) > docLen[p.DocID] {
					docLen[p.DocID] = int(p.FieldLen)
				}
			}
		}
		for _, l := range docLen {
			mergedFieldLens = append(mergedFieldLens, l)
		}
	}
	stats := NewSegmentStats(mergedFieldLens)
	for _, term := range terms {
		id, _ := dict.GetStr(term)
		var merged []Posting
		for i, s := range src {
			oldID, ok := s.dict.GetStr(term)
			if !ok {
				continue
			}
			pl := s.postings[oldID]
			if pl == nil {
				continue
			}
			it := pl.Iterate()
			for it.HasNext() {
				p := it.Next()
				p.DocID = p.DocID + rowOff[i]
				merged = append(merged, p)
			}
		}
		postings[id] = NewPostingList(merged)
	}
	st := &invertedStore{dict: dict, postings: postings, stats: stats, field: ix.cfg.Field}
	if err := writeInvertedFiles(w, st, ix.cfg.StorePositions); err != nil {
		return err
	}
	ix.store = st
	return nil
}

// Quantize implements index.Index: inverted indexes have no quantization.
func (ix *Index) Quantize(ctx context.Context, to index.QuantLevel) error {
	return fmt.Errorf("%w: inverted index has no quantization (level %d)",
		index.ErrUnsupported, int(to))
}

// MemEstimate implements index.Index.
func (ix *Index) MemEstimate() int64 {
	if ix.store == nil {
		return 0
	}
	var sz int64 = 64
	for _, t := range ix.store.dict.terms {
		sz += int64(len(t)) + 16
	}
	for _, pl := range ix.store.postings {
		sz += int64(pl.docIDs.GetSerializedSizeInBytes()) + int64(len(pl.tfs)) +
			int64(len(pl.fldLens))*2
		for _, pos := range pl.positions {
			sz += int64(len(pos)) * 2
		}
	}
	return sz
}

// loadedIndex is the searchable handle over one built inverted index.
// Immutable and safe for concurrent use.
type loadedIndex struct {
	st  *invertedStore
	cfg Config
}

var _ index.LoadedIndex = (*loadedIndex)(nil)

// Close implements index.LoadedIndex. M0 stores are in-memory.
func (l *loadedIndex) Close() error { return nil }

// Search implements index.LoadedIndex: index.Query.Term is the single
// term to match (M0 single-term path; multi-term Boolean and phrase
// queries are served by SearchTerms / SearchPhrase for the query
// engine, §3.3 BM25 sum). The returned hits are ordered by descending
// BM25 score.
func (l *loadedIndex) Search(ctx context.Context, q index.Query, budget index.SearchBudget) (*index.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(q.Term) == 0 {
		return nil, fmt.Errorf("inverted: empty query term")
	}
	return l.SearchTerms(ctx, [][]byte{q.Term}, nil, q.K, budget)
}

// ScoreDocs computes the §3.3 BM25 score contribution of every term
// for every matching document and returns the full float64 score map.
// It is the precision-preserving surface the query engine (03 §3.4
// BM25Score operator) and numeric-correctness tests consume; the
// registry Search path truncates to float32 for the SearchHit wire
// format but the underlying computation stays float64 throughout.
func (l *loadedIndex) ScoreDocs(ctx context.Context, terms [][]byte) (map[uint32]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scores := make(map[uint32]float64, 64)
	for _, term := range terms {
		id, ok := l.st.dict.Get(term)
		if !ok {
			continue
		}
		pl := l.st.postings[id]
		if pl == nil {
			continue
		}
		df := pl.DocFreq()
		it := pl.Iterate()
		for it.HasNext() {
			p := it.Next()
			s := l.st.stats.ScoreTerm(l.cfg.BM25, int(p.TF), int(p.FieldLen), df)
			scores[p.DocID] += s
		}
	}
	return scores, nil
}

// SearchTerms runs a multi-term BM25 OR query: documents containing
// any of the terms are scored by the §3.3 sum over the term
// contributions, and the top-k by descending score are returned. The
// optional filter is applied before scoring (pre-filter, §2.6). When
// the term list is empty the result is empty.
func (l *loadedIndex) SearchTerms(ctx context.Context, terms [][]byte, phraseFilter *roaring.Bitmap, k int, budget index.SearchBudget) (*index.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k <= 0 {
		k = 10
	}
	if len(terms) == 0 {
		return &index.SearchResult{}, nil
	}
	scores, err := l.ScoreDocs(ctx, terms)
	if err != nil {
		return nil, err
	}
	type cand struct {
		rowID int32
		score float64
	}
	out := make([]cand, 0, len(scores))
	for docID, s := range scores {
		if phraseFilter != nil && !phraseFilter.Contains(docID) {
			continue
		}
		out = append(out, cand{rowID: int32(docID), score: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score == out[j].score {
			return out[i].rowID < out[j].rowID
		}
		return out[i].score > out[j].score
	})
	if len(out) > k {
		out = out[:k]
	}
	hits := make([]index.SearchHit, len(out))
	for i, c := range out {
		hits[i] = index.SearchHit{RowID: c.rowID, Score: float32(c.score)}
	}
	return &index.SearchResult{Hits: hits}, nil
}

// SearchPhrase runs a match_phrase query (§3.2 position blocks). The
// query terms must occur in the document in the same order and within
// `slop` positions of each other (slop=0 means consecutive). Documents
// that satisfy the phrase constraint are scored with the §3.3 BM25 sum
// over the phrase terms, then top-k by descending score.
func (l *loadedIndex) SearchPhrase(ctx context.Context, terms [][]byte, slop int, k int, budget index.SearchBudget) (*index.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k <= 0 {
		k = 10
	}
	if len(terms) == 0 {
		return &index.SearchResult{}, nil
	}
	// Intersect docids across all terms (a phrase requires every term
	// present).
	var common *roaring.Bitmap
	termPLs := make([]*PostingList, 0, len(terms))
	for _, term := range terms {
		id, ok := l.st.dict.Get(term)
		if !ok {
			return &index.SearchResult{}, nil // a term is absent: no phrase matches.
		}
		pl := l.st.postings[id]
		if pl == nil {
			return &index.SearchResult{}, nil
		}
		termPLs = append(termPLs, pl)
		if common == nil {
			common = pl.DocIDs()
		} else {
			common = roaring.And(common, pl.DocIDs())
		}
	}
	if common == nil || common.IsEmpty() {
		return &index.SearchResult{}, nil
	}
	// For each candidate doc, fetch each term's positions and check
	// the phrase constraint: term[0] at pos p, term[1] at pos p+1+δ
	// (δ ≤ slop), ..., allowing slop extra positions across the whole
	// phrase. The check uses the "positions form an ascending run
	// within slop slack" interpretation ES uses for slop>0.
	candidates := make([]uint32, 0, common.GetCardinality())
	it := common.Iterator()
	for it.HasNext() {
		candidates = append(candidates, it.Next())
	}
	var matched []uint32
	for _, docID := range candidates {
		if phraseMatches(termPLs, docID, slop) {
			matched = append(matched, docID)
		}
	}
	// Score matched docs by the BM25 sum over the phrase terms.
	filter := roaring.New()
	for _, d := range matched {
		filter.Add(d)
	}
	// Build term byte slice list for SearchTerms scoring.
	termBytes := make([][]byte, len(terms))
	for i, t := range terms {
		termBytes[i] = t
	}
	return l.SearchTerms(ctx, termBytes, filter, k, budget)
}

// phraseMatches checks whether the terms occur in the document in order
// within slop positions. termPLs[i] is the posting list for terms[i].
// The M0 algorithm: for each starting position p of term[0], check if
// term[1] occurs at p+1 (or within slop), term[2] at the next, etc.
// slop allows cumulative extra positions across the whole phrase (ES
// semantics: total edits allowed = slop).
func phraseMatches(termPLs []*PostingList, docID uint32, slop int) bool {
	positions := make([][]uint16, len(termPLs))
	for i, pl := range termPLs {
		_, _, pos, ok := pl.Get(docID)
		if !ok || len(pos) == 0 {
			return false
		}
		positions[i] = pos
	}
	// Try every starting position of term[0]; for each, greedily walk
	// forward to term[1], term[2], ... allowing positions to advance by
	// 1 + slack where cumulative slack ≤ slop.
	for _, start := range positions[0] {
		usedSlop := 0
		prev := int(start)
		ok := true
		for i := 1; i < len(positions); i++ {
			// Find the smallest position in term[i] > prev.
			nextPos := -1
			for _, p := range positions[i] {
				if int(p) > prev {
					nextPos = int(p)
					break
				}
			}
			if nextPos == -1 {
				ok = false
				break
			}
			gap := nextPos - prev - 1 // extra positions between consecutive terms
			if gap < 0 {
				gap = 0
			}
			usedSlop += gap
			if usedSlop > slop {
				ok = false
				break
			}
			prev = nextPos
		}
		if ok {
			return true
		}
	}
	return false
}

// writeInvertedFiles serializes the §3.1/§3.2/§3.3 file set into w.
func writeInvertedFiles(w index.IndexOutput, st *invertedStore, storePositions bool) error {
	// stats.bin first (carries field name + SegmentStats).
	fs, err := w.Create(fileStats)
	if err != nil {
		return fmt.Errorf("inverted: create %s: %w", fileStats, err)
	}
	if err := writeStats(fs, st.field, st.stats); err != nil {
		fs.Close()
		return err
	}
	if err := fs.Close(); err != nil {
		return fmt.Errorf("inverted: close %s: %w", fileStats, err)
	}
	// terms.sorted.
	ft, err := w.Create(fileTerms)
	if err != nil {
		return fmt.Errorf("inverted: create %s: %w", fileTerms, err)
	}
	if err := writeTermDict(ft, st.dict); err != nil {
		ft.Close()
		return err
	}
	if err := ft.Close(); err != nil {
		return fmt.Errorf("inverted: close %s: %w", fileTerms, err)
	}
	// post.bin: count + per-term posting block.
	fp, err := w.Create(filePostings)
	if err != nil {
		return fmt.Errorf("inverted: create %s: %w", filePostings, err)
	}
	if err := binary.Write(fp, binary.LittleEndian, postingsMagic); err != nil {
		fp.Close()
		return fmt.Errorf("inverted: write %s magic: %w", filePostings, err)
	}
	if err := binary.Write(fp, binary.LittleEndian, postingsVersion); err != nil {
		fp.Close()
		return fmt.Errorf("inverted: write %s version: %w", filePostings, err)
	}
	if err := binary.Write(fp, binary.LittleEndian, uint32(len(st.postings))); err != nil {
		fp.Close()
		return err
	}
	// Iterate TermIDs in ascending order so the file is deterministic.
	ids := make([]TermID, 0, len(st.postings))
	for id := range st.postings {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if err := writePostingList(fp, id, st.postings[id]); err != nil {
			fp.Close()
			return err
		}
	}
	if err := fp.Close(); err != nil {
		return fmt.Errorf("inverted: close %s: %w", filePostings, err)
	}
	// pos.bin: per-term position frames (only when StorePositions).
	if storePositions {
		fpp, err := w.Create(filePositions)
		if err != nil {
			return fmt.Errorf("inverted: create %s: %w", filePositions, err)
		}
		if err := binary.Write(fpp, binary.LittleEndian, postingsMagic); err != nil {
			fpp.Close()
			return err
		}
		if err := binary.Write(fpp, binary.LittleEndian, postingsVersion); err != nil {
			fpp.Close()
			return err
		}
		if err := binary.Write(fpp, binary.LittleEndian, uint32(len(st.postings))); err != nil {
			fpp.Close()
			return err
		}
		for _, id := range ids {
			if err := writePositions(fpp, id, st.postings[id]); err != nil {
				fpp.Close()
				return err
			}
		}
		if err := fpp.Close(); err != nil {
			return fmt.Errorf("inverted: close %s: %w", filePositions, err)
		}
	}
	return nil
}

// readInvertedFiles is the inverse of writeInvertedFiles.
func readInvertedFiles(r index.IndexInput) (*invertedStore, error) {
	fs, err := r.Open(fileStats)
	if err != nil {
		return nil, fmt.Errorf("inverted: open %s: %w", fileStats, err)
	}
	statsBytes, err := io.ReadAll(fs)
	fs.Close()
	if err != nil {
		return nil, fmt.Errorf("inverted: read %s: %w", fileStats, err)
	}
	field, stats, err := readStats(&byteReader{data: statsBytes})
	if err != nil {
		return nil, err
	}
	ft, err := r.Open(fileTerms)
	if err != nil {
		return nil, fmt.Errorf("inverted: open %s: %w", fileTerms, err)
	}
	dict, err := readTermDict(ft)
	ft.Close()
	if err != nil {
		return nil, err
	}
	fp, err := r.Open(filePostings)
	if err != nil {
		return nil, fmt.Errorf("inverted: open %s: %w", filePostings, err)
	}
	postBytes, err := io.ReadAll(fp)
	fp.Close()
	if err != nil {
		return nil, fmt.Errorf("inverted: read %s: %w", filePostings, err)
	}
	pr := &byteReader{data: postBytes}
	var magic uint32
	if magic, err = pr.readU32(); err != nil {
		return nil, fmt.Errorf("inverted: read %s magic: %w", filePostings, err)
	}
	if magic != postingsMagic {
		return nil, fmt.Errorf("inverted: bad %s magic %#x", filePostings, magic)
	}
	if pr.off >= len(pr.data) {
		return nil, fmt.Errorf("inverted: truncated %s version", filePostings)
	}
	version := pr.data[pr.off]
	pr.off++
	if version != postingsVersion {
		return nil, fmt.Errorf("inverted: unsupported %s version %d", filePostings, version)
	}
	count, err := pr.readU32()
	if err != nil {
		return nil, fmt.Errorf("inverted: read %s count: %w", filePostings, err)
	}
	postings := make(map[TermID]*PostingList, count)
	for i := uint32(0); i < count; i++ {
		id, pl, err := readPostingList(pr)
		if err != nil {
			return nil, err
		}
		postings[id] = pl
	}
	st := &invertedStore{dict: dict, postings: postings, stats: stats, field: field}
	// pos.bin is optional (only built when StorePositions). Try to
	// open it; on absence, leave positions as the empty lists the
	// post.bin reader initialized.
	fpp, err := r.Open(filePositions)
	if err == nil {
		posBytes, _ := io.ReadAll(fpp)
		fpp.Close()
		posReader := &byteReader{data: posBytes}
		// skip magic/version/count
		_, _ = posReader.readU32()
		if posReader.off < len(posReader.data) {
			posReader.off++
		}
		_, _ = posReader.readU32()
		for i := uint32(0); i < count; i++ {
			id, positions, off, err := readPositions(posBytes, posReader.off)
			if err != nil {
				break
			}
			posReader.off = off
			pl := postings[id]
			if pl != nil {
				pl.positions = positions
			}
		}
	}
	return st, nil
}

// Stringer aid for debugging.
func (l *loadedIndex) String() string {
	if l == nil || l.st == nil {
		return "inverted:<empty>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "inverted<field=%s docs=%d terms=%d avgFL=%.2f>",
		l.st.field, l.st.stats.DocCount, l.st.dict.Len(), l.st.stats.AvgFieldLen)
	return b.String()
}
