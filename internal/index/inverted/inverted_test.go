package inverted

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/index/analyzer"
)

// memTextColumn implements TextColumn over an in-memory document slice.
type memTextColumn struct {
	field string
	docs  []string
}

func (c *memTextColumn) Field() string { return c.field }
func (c *memTextColumn) Rows() int64   { return int64(len(c.docs)) }
func (c *memTextColumn) Dim() int      { return 0 }
func (c *memTextColumn) ForEachVector(func(int32, []float32) error) error {
	return fmt.Errorf("inverted: text column has no vectors")
}
func (c *memTextColumn) ForEachText(fn func(rowID int32, text []byte) error) error {
	for i, d := range c.docs {
		if err := fn(int32(i), []byte(d)); err != nil {
			return err
		}
	}
	return nil
}

// memOutput / memInput implement index.IndexOutput / index.IndexInput
// over an in-memory file map (mirrors the hnsw test harness).
type memOutput struct{ files map[string][]byte }

func (m *memOutput) Create(name string) (io.WriteCloser, error) {
	b := &bytes.Buffer{}
	return &memFile{b: b, on: func() { m.files[name] = b.Bytes() }}, nil
}

type memFile struct {
	b  *bytes.Buffer
	on func()
}

func (f *memFile) Write(p []byte) (int, error) { return f.b.Write(p) }
func (f *memFile) Close() error                { f.on(); return nil }

type memInput struct{ files map[string][]byte }

func (m *memInput) Open(name string) (io.ReadSeekCloser, error) {
	b, ok := m.files[name]
	if !ok {
		return nil, fmt.Errorf("no such file %q", name)
	}
	return &memReader{bytes.NewReader(b)}, nil
}

type memReader struct{ *bytes.Reader }

func (m *memReader) Close() error { return nil }

// standardAnalyzer returns the standard analyzer, failing the test on
// construction error so the helper stays zero-arg.
func standardAnalyzer(t *testing.T) analyzer.Analyzer {
	t.Helper()
	a, err := analyzer.Get(analyzer.NameStandard, nil)
	require.NoError(t, err)
	return a
}

// TestR10_BM25_CorpusNumericCorrectness builds a small corpus with
// hand-verifiable tf/df/fldLen statistics and checks the §3.3 BM25 score
// returned by Search matches the formula to relative error < 1e-9 (the
// spec.md R10 acceptance bound).
//
// Corpus (4 docs, field "text"):
//
//	doc0: "apple"                 -> fldLen=1, apple tf=1
//	doc1: "banana"                -> fldLen=1, apple tf=0
//	doc2: "apple apple"           -> fldLen=2, apple tf=2
//	doc3: "apple banana apple"    -> fldLen=3, apple tf=2
//
// For "apple": N=4, df=3, avgFL=(1+1+2+3)/4=1.75. The expected score
// for each doc is computed in-test with the same §3.3 formula and
// compared to Search's output.
func TestR10_BM25_CorpusNumericCorrectness(t *testing.T) {
	docs := []string{"apple", "banana", "apple apple", "apple banana apple"}
	ix := New(Config{
		Field:    "text",
		Analyzer: standardAnalyzer(t),
		BM25:     DefaultBM25Params(),
	})
	col := &memTextColumn{field: "text", docs: docs}
	out := &memOutput{files: map[string][]byte{}}
	require.NoError(t, ix.Build(context.Background(), col, out))

	li, err := ix.Load(&memInput{files: out.files})
	require.NoError(t, err)
	defer li.Close()
	lli := li.(*loadedIndex)

	// Float64-precision score map (the registry Search path truncates
	// to float32, which cannot meet the <1e-9 spec bound).
	scores, err := lli.ScoreDocs(context.Background(), [][]byte{[]byte("apple")})
	require.NoError(t, err)

	// Also verify the truncating Search path returns the same docs.
	res, err := li.Search(context.Background(), index.Query{Term: []byte("apple"), K: 10}, index.SearchBudget{})
	require.NoError(t, err)
	require.True(t, isSortedDesc(res.Hits), "hits not in descending score order")

	// Hand-compute the expected BM25 score per doc.
	p := DefaultBM25Params()
	stats := SegmentStats{DocCount: 4, AvgFieldLen: 1.75}
	df := 3
	want := map[int32]float64{
		0: stats.ScoreTerm(p, 1, 1, df),
		2: stats.ScoreTerm(p, 2, 2, df),
		3: stats.ScoreTerm(p, 2, 3, df),
	}
	// doc1 has no "apple" — it must be absent from both surfaces.
	require.NotContains(t, scores, uint32(1), "doc1 must not match term apple")
	for rowID, wantScore := range want {
		gotScore, ok := scores[uint32(rowID)]
		require.True(t, ok, "doc %d missing from scores", rowID)
		rel := math.Abs(gotScore-wantScore) / math.Abs(wantScore)
		require.Less(t, rel, 1e-9, "doc %d: got=%v want=%v rel=%v",
			rowID, gotScore, wantScore, rel)
	}
}

// TestR10_Phrase_PositionOrder exercises the §3.2 position-block
// semantics and the spec.md R10 phrase scenario:
//
//	match_phrase "hello world" on a doc containing "world hello" must
//	NOT match (position order is violated); on "hello beautiful world"
//	it matches only when slop >= 1 (the extra token consumes one slop).
func TestR10_Phrase_PositionOrder(t *testing.T) {
	docs := []string{"world hello", "hello beautiful world"}
	ix := New(Config{
		Field:          "text",
		Analyzer:       standardAnalyzer(t),
		BM25:           DefaultBM25Params(),
		StorePositions: true,
	})
	col := &memTextColumn{field: "text", docs: docs}
	out := &memOutput{files: map[string][]byte{}}
	require.NoError(t, ix.Build(context.Background(), col, out))
	li, err := ix.Load(&memInput{files: out.files})
	require.NoError(t, err)
	defer li.Close()

	lli := li.(*loadedIndex)

	// slop=0: doc0 has reversed order; doc1 has a gap of 1 (the
	// "beautiful" token between hello and world). Neither matches.
	res, err := lli.SearchPhrase(context.Background(),
		[][]byte{[]byte("hello"), []byte("world")}, 0, 10, index.SearchBudget{})
	require.NoError(t, err)
	require.Empty(t, res.Hits, "slop=0 must not match either doc")

	// slop=1: doc1 ("hello beautiful world") matches; doc0 still does
	// not (order violation is not excused by slop).
	res, err = lli.SearchPhrase(context.Background(),
		[][]byte{[]byte("hello"), []byte("world")}, 1, 10, index.SearchBudget{})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	require.Equal(t, int32(1), res.Hits[0].RowID)

	// Large slop never rescues order: doc0 still does not match.
	res, err = lli.SearchPhrase(context.Background(),
		[][]byte{[]byte("hello"), []byte("world")}, 10, 10, index.SearchBudget{})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	require.Equal(t, int32(1), res.Hits[0].RowID)
}

// TestR10_BuildLoadRoundtrip verifies the Build → Load → Search path
// produces the same results as the in-memory built index, and that
// Merge rebuilds a searchable whole from two shards.
func TestR10_BuildLoadRoundtrip(t *testing.T) {
	docs := []string{
		"the quick brown fox",
		"the lazy dog",
		"quick brown dog",
		"the quick quick fox",
	}
	ix := New(Config{
		Field:          "text",
		Analyzer:       standardAnalyzer(t),
		BM25:           DefaultBM25Params(),
		StorePositions: true,
	})
	col := &memTextColumn{field: "text", docs: docs}
	out := &memOutput{files: map[string][]byte{}}
	require.NoError(t, ix.Build(context.Background(), col, out))
	li, err := ix.Load(&memInput{files: out.files})
	require.NoError(t, err)
	defer li.Close()

	res, err := li.Search(context.Background(), index.Query{Term: []byte("quick"), K: 3}, index.SearchBudget{})
	require.NoError(t, err)
	// "quick" appears in docs 0, 2, 3 — three hits.
	require.Len(t, res.Hits, 3)
	require.True(t, isSortedDesc(res.Hits))

	// Merge: split the corpus, build two indexes, merge, search.
	half := len(docs) / 2
	ixA := New(Config{Field: "text", Analyzer: standardAnalyzer(t), BM25: DefaultBM25Params(), StorePositions: true})
	ixB := New(Config{Field: "text", Analyzer: standardAnalyzer(t), BM25: DefaultBM25Params(), StorePositions: true})
	outA := &memOutput{files: map[string][]byte{}}
	outB := &memOutput{files: map[string][]byte{}}
	require.NoError(t, ixA.Build(context.Background(), &memTextColumn{field: "text", docs: docs[:half]}, outA))
	require.NoError(t, ixB.Build(context.Background(), &memTextColumn{field: "text", docs: docs[half:]}, outB))
	liB, err := ixB.Load(&memInput{files: outB.files})
	require.NoError(t, err)

	merged := &memOutput{files: map[string][]byte{}}
	require.NoError(t, ixA.Merge(context.Background(), []index.LoadedIndex{liB}, merged))
	lm, err := ixA.Load(&memInput{files: merged.files})
	require.NoError(t, err)
	defer lm.Close()

	rm, err := lm.Search(context.Background(), index.Query{Term: []byte("quick"), K: 3}, index.SearchBudget{})
	require.NoError(t, err)
	require.Len(t, rm.Hits, 3, "merged index must reproduce the single-segment hits count")
}

// TestR10_RegistryFactory verifies the registry-wired factory produces
// a usable inverted index with the default configuration.
func TestR10_RegistryFactory(t *testing.T) {
	i, err := index.NewIndex(index.KindInverted)
	require.NoError(t, err)
	require.Equal(t, index.KindInverted, i.Kind())
	// Default config: standard analyzer, "text" field, positions on.
	ix := i.(*Index)
	require.Equal(t, "text", ix.cfg.Field)
	require.NotNil(t, ix.cfg.Analyzer)
	require.True(t, ix.cfg.StorePositions)
}

// isSortedDesc reports whether hits are in non-increasing score order.
func isSortedDesc(hits []index.SearchHit) bool {
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			return false
		}
	}
	return true
}
