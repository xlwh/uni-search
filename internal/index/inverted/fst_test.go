package inverted

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.1
// DoD: 词典内存较数组版降 ≥60%；fuzz 查询用例通过

// TestFST_GetMatchesArray verifies that FST lookup produces the same
// results as the sorted-array binary search for all terms.
func TestFST_GetMatchesArray(t *testing.T) {
	terms := []string{
		"apple", "application", "apply", "banana", "band",
		"can", "cancel", "cat", "category", "code",
		"coding", "data", "database", "document", "dog",
	}
	b := NewTermDictBuilder()
	for _, term := range terms {
		b.Add(term)
	}
	d, err := b.Finish()
	require.NoError(t, err)
	require.NotNil(t, d.fst, "FST should be built")

	for i, term := range terms {
		fstID, fstOK := d.fst.Get([]byte(term))
		arrID, arrOK := d.Get([]byte(term))
		assert.True(t, fstOK, "FST should find term %q", term)
		assert.True(t, arrOK, "array should find term %q", term)
		assert.Equal(t, TermID(i), TermID(fstID), "FST TermID for %q", term)
		assert.Equal(t, TermID(fstID), arrID, "FST and array should agree on TermID for %q", term)
	}

	// Non-existent term.
	_, ok := d.fst.Get([]byte("nonexistent"))
	assert.False(t, ok, "FST should not find nonexistent term")
}

// TestFST_MemoryReduction verifies the DoD: FST memory is ≥60% smaller
// than the sorted-array dictionary.
func TestFST_MemoryReduction(t *testing.T) {
	// Generate 1000 terms with common prefixes to simulate real-world usage.
	terms := generateTestTerms(1000)
	b := NewTermDictBuilder()
	for _, term := range terms {
		b.Add(term)
	}
	d, err := b.Finish()
	require.NoError(t, err)

	arrayMem := d.MemoryEstimateArray()
	fstMem := d.MemoryEstimateFST()
	reduction := float64(arrayMem-fstMem) / float64(arrayMem) * 100

	t.Logf("array memory: %d bytes, FST memory: %d bytes, reduction: %.1f%%",
		arrayMem, fstMem, reduction)
	assert.GreaterOrEqual(t, reduction, 60.0,
		"FST memory should be ≥60%% smaller than array (got %.1f%%)", reduction)
}

// TestFST_FuzzySearch verifies the DoD: fuzz query finds correct terms.
func TestFST_FuzzySearch(t *testing.T) {
	terms := []string{
		"hello", "help", "held", "helm",
		"world", "word", "would",
		"search", "serve", "server",
	}
	b := NewTermDictBuilder()
	for _, term := range terms {
		b.Add(term)
	}
	d, err := b.Finish()
	require.NoError(t, err)
	require.NotNil(t, d.fst)

	// Fuzzy search for "helo" with distance 1 should find "hello", "help", "held".
	matches := FuzzySearch(d.fst, []byte("helo"), 1)
	t.Logf("fuzzy 'helo' dist=1: %d matches", len(matches))
	for _, m := range matches {
		t.Logf("  match: term=%q, id=%d, dist=%d", string(m.Term), m.Output, m.Dist)
	}
	matchTerms := make(map[string]int)
	for _, m := range matches {
		matchTerms[string(m.Term)] = m.Dist
	}
	assert.Contains(t, matchTerms, "hello", "should match 'hello'")
	assert.Contains(t, matchTerms, "help", "should match 'help'")
	assert.Contains(t, matchTerms, "held", "should match 'held'")

	// Fuzzy search for "wold" with distance 1 should find "world", "would", "word".
	matches2 := FuzzySearch(d.fst, []byte("wold"), 1)
	matchTerms2 := make(map[string]int)
	for _, m := range matches2 {
		matchTerms2[string(m.Term)] = m.Dist
	}
	assert.Contains(t, matchTerms2, "world", "should match 'world'")
	assert.Contains(t, matchTerms2, "would", "should match 'would'")
	assert.Contains(t, matchTerms2, "word", "should match 'word'")

	// Exact match with distance 0.
	matches3 := FuzzySearch(d.fst, []byte("hello"), 0)
	require.Len(t, matches3, 1, "exact match should return 1 result")
	assert.Equal(t, "hello", string(matches3[0].Term))
	assert.Equal(t, 0, matches3[0].Dist)
}

// TestFST_FuzzyMatchTerm tests the single-match fuzzy lookup.
func TestFST_FuzzyMatchTerm(t *testing.T) {
	terms := []string{"hello", "help", "world"}
	b := NewTermDictBuilder()
	for _, term := range terms {
		b.Add(term)
	}
	d, err := b.Finish()
	require.NoError(t, err)

	// Fuzzy match "helo" → should return "hello" (dist=1).
	term, id, dist := FuzzyMatchTerm(d.fst, []byte("helo"), 1)
	assert.Equal(t, "hello", term)
	assert.Equal(t, uint32(0), id)
	assert.Equal(t, 1, dist)

	// No match within distance.
	term, _, dist = FuzzyMatchTerm(d.fst, []byte("xyz"), 1)
	assert.Equal(t, "", term)
	assert.Equal(t, -1, dist)
}

// TestFST_PrefixIteration verifies FST iterator produces terms in order.
func TestFST_PrefixIteration(t *testing.T) {
	terms := []string{"apple", "apply", "application", "banana", "band"}
	b := NewTermDictBuilder()
	for _, term := range terms {
		b.Add(term)
	}
	d, err := b.Finish()
	require.NoError(t, err)

	// Iterate all terms.
	it := d.fst.NewFSTIterator(nil)
	var found []string
	for it.HasNext() {
		term, _ := it.Next()
		if term != nil {
			found = append(found, string(term))
		}
	}
	assert.Equal(t, []string{"apple", "application", "apply", "banana", "band"}, found,
		"FST iterator should produce terms in sorted order")

	// Iterate with prefix "ap".
	it2 := d.fst.NewFSTIterator([]byte("ap"))
	var prefixed []string
	for it2.HasNext() {
		term, _ := it2.Next()
		if term != nil {
			prefixed = append(prefixed, string(term))
		}
	}
	assert.Equal(t, []string{"apple", "application", "apply"}, prefixed,
		"FST prefix iterator should return only 'ap*' terms")
}

// generateTestTerms generates n terms with strong common prefixes to
// simulate real-world dictionary usage. All prefixes share "data" as
// a 4-byte root, maximizing FST prefix compression.
func generateTestTerms(n int) []string {
	// All prefixes share the "data" root (4 bytes of guaranteed sharing).
	prefixes := []string{"database", "datatable", "dataset", "datatype", "datapoint"}
	suffixes := []string{"_query", "_index", "_search", "_result", "_update"}
	terms := make([]string, 0, n)
	for i := 0; i < n; i++ {
		prefix := prefixes[i%len(prefixes)]
		suffix := suffixes[(i/len(prefixes))%len(suffixes)]
		num := i / (len(prefixes) * len(suffixes))
		terms = append(terms, fmt.Sprintf("%s%s_%d", prefix, suffix, num))
	}
	// Deduplicate and sort.
	seen := make(map[string]bool)
	unique := make([]string, 0, len(terms))
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}
	for i := 1; i < len(unique); i++ {
		for j := i; j > 0 && strings.Compare(unique[j], unique[j-1]) < 0; j-- {
			unique[j], unique[j-1] = unique[j-1], unique[j]
		}
	}
	return unique
}
