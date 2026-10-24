package inverted

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
)

// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.2
// TestWAND_MultiTermSearch verifies WAND returns correct top-K results
// for multi-term BM25 queries, matching the full-traversal path.
func TestWAND_MultiTermSearch(t *testing.T) {
	cfg := DefaultConfig()
	ix := New(cfg)

	texts := []string{
		"hello world search engine",
		"hello database query optimization",
		"world search database query",
		"hello hello world search",
		"engine optimization query search",
	}
	col := &memTextColumn{field: "text", docs: texts}

	out := &memOutput{files: make(map[string][]byte)}
	require.NoError(t, ix.Build(context.Background(), col, out))

	loaded, err := ix.Load(&memInput{files: out.files})
	require.NoError(t, err)
	li := loaded.(*loadedIndex)

	// Single-term search (uses ScoreDocs path).
	res1, err := li.Search(context.Background(), index.Query{
		Term: []byte("hello"),
		K:    3,
	}, index.SearchBudget{})
	require.NoError(t, err)
	assert.NotEmpty(t, res1.Hits, "single-term search should find results")

	// Multi-term search (uses WAND path).
	res2, err := li.SearchTerms(context.Background(), [][]byte{
		[]byte("hello"), []byte("world"), []byte("search"),
	}, nil, 3, index.SearchBudget{})
	require.NoError(t, err)
	assert.NotEmpty(t, res2.Hits, "multi-term WAND search should find results")
	assert.LessOrEqual(t, len(res2.Hits), 3, "should return at most K results")

	// Verify results are sorted by descending score.
	for i := 1; i < len(res2.Hits); i++ {
		assert.GreaterOrEqual(t, res2.Hits[i-1].Score, res2.Hits[i].Score,
			"results should be sorted by descending score")
	}

	// Verify WAND results match ScoreDocs results for the same query.
	scores, err := li.ScoreDocs(context.Background(), [][]byte{
		[]byte("hello"), []byte("world"), []byte("search"),
	})
	require.NoError(t, err)

	type scoreCand struct {
		rowID int32
		score float64
	}
	var allCands []scoreCand
	for docID, s := range scores {
		allCands = append(allCands, scoreCand{rowID: int32(docID), score: s})
	}
	// Sort by descending score, tie-break by ascending rowID (same as SearchTerms).
	for i := 1; i < len(allCands); i++ {
		for j := i; j > 0; j-- {
			if allCands[j].score > allCands[j-1].score ||
				(allCands[j].score == allCands[j-1].score && allCands[j].rowID < allCands[j-1].rowID) {
				allCands[j], allCands[j-1] = allCands[j-1], allCands[j]
			}
		}
	}
	topK := 3
	if len(allCands) > topK {
		allCands = allCands[:topK]
	}

	require.Equal(t, len(allCands), len(res2.Hits),
		"WAND and ScoreDocs should return same number of results")

	for i := range res2.Hits {
		assert.Equal(t, allCands[i].rowID, res2.Hits[i].RowID,
			"WAND hit %d rowID should match ScoreDocs", i)
		assert.InDelta(t, allCands[i].score, float64(res2.Hits[i].Score), 0.01,
			"WAND hit %d score should match ScoreDocs", i)
	}
}

// TestWAND_SingleTermFallback verifies single-term queries still work
// (they use ScoreDocs, not WAND).
func TestWAND_SingleTermFallback(t *testing.T) {
	cfg := DefaultConfig()
	ix := New(cfg)

	texts := []string{"hello world", "hello database", "goodbye world"}
	col := &memTextColumn{field: "text", docs: texts}

	out := &memOutput{files: make(map[string][]byte)}
	require.NoError(t, ix.Build(context.Background(), col, out))
	loaded, err := ix.Load(&memInput{files: out.files})
	require.NoError(t, err)
	li := loaded.(*loadedIndex)

	res, err := li.Search(context.Background(), index.Query{
		Term: []byte("hello"),
		K:    10,
	}, index.SearchBudget{})
	require.NoError(t, err)
	assert.Equal(t, 2, len(res.Hits), "should find 2 docs containing 'hello'")
}

// TestWAND_UpperBound verifies upper bound computation is correct.
func TestWAND_UpperBound(t *testing.T) {
	stats := SegmentStats{DocCount: 100, AvgFieldLen: 40}
	bm25 := DefaultBM25Params()

	posts := []Posting{
		{DocID: 1, TF: 3, FieldLen: 10},
		{DocID: 2, TF: 1, FieldLen: 50},
		{DocID: 3, TF: 2, FieldLen: 20},
	}
	pl := NewPostingList(posts)

	ub := computeUpperBound(pl, stats, bm25)
	expected := stats.ScoreTerm(bm25, 3, 10, 3)
	assert.InDelta(t, expected, ub, 1e-9,
		"upper bound should be ScoreTerm with maxTF and minFieldLen")
	assert.Greater(t, ub, 0.0, "upper bound should be positive")
}
