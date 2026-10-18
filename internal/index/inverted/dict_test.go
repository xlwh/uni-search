package inverted

import (
	"bytes"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR10_TermDict_Get exercises the §3.1 sorted-array dictionary: ids
// are assigned in sorted order starting at 0, Get resolves terms to ids,
// and absent terms return false.
func TestR10_TermDict_Get(t *testing.T) {
	terms := []string{"apple", "banana", "cherry", "date", "elderberry"}
	d, err := NewTermDict(terms)
	require.NoError(t, err)
	require.Equal(t, len(terms), d.Len())

	for i, want := range terms {
		gotID, ok := d.GetStr(want)
		require.True(t, ok, "term %q not found", want)
		require.Equal(t, TermID(i), gotID, "id of %q", want)
		require.Equal(t, want, d.Term(gotID))
	}

	id, ok := d.GetStr("missing")
	require.False(t, ok)
	require.Equal(t, TermID(0), id)

	id, ok = d.Get([]byte("banana"))
	require.True(t, ok)
	require.Equal(t, TermID(1), id)
}

// TestR10_TermDict_BinarySearchCorrectness cross-checks Get against
// sort.SearchStrings over a large random term set: every term present
// resolves to its index, every absent probe returns false.
func TestR10_TermDict_BinarySearchCorrectness(t *testing.T) {
	r := rand.New(rand.NewPCG(10, 20))
	const n = 5000
	set := make(map[string]struct{})
	for len(set) < n {
		set[randTerm(r, 4, 12)] = struct{}{}
	}
	terms := make([]string, 0, n)
	for k := range set {
		terms = append(terms, k)
	}
	sort.Strings(terms)
	d, err := NewTermDict(terms)
	require.NoError(t, err)

	for i, want := range terms {
		gotID, ok := d.GetStr(want)
		require.True(t, ok)
		require.Equal(t, TermID(i), gotID)
	}
	// Absent probes.
	for i := 0; i < 1000; i++ {
		probe := randTerm(r, 5, 11)
		idx := sort.SearchStrings(terms, probe)
		wantOK := idx < len(terms) && terms[idx] == probe
		gotID, gotOK := d.GetStr(probe)
		require.Equal(t, wantOK, gotOK, "probe %q", probe)
		if wantOK {
			require.Equal(t, TermID(idx), gotID, "probe %q", probe)
		}
	}
}

// TestR10_TermDict_Iterate walks the dictionary in sorted order and
// verifies prefix filtering.
func TestR10_TermDict_Iterate(t *testing.T) {
	terms := []string{"a-1", "a-2", "b-1", "b-2", "c"}
	d, err := NewTermDict(terms)
	require.NoError(t, err)

	it := d.Iterate(nil)
	var got []string
	for it.HasNext() {
		_, t := it.Next()
		got = append(got, t)
	}
	require.Equal(t, terms, got)

	// Prefix "b-".
	it = d.Iterate([]byte("b-"))
	got = got[:0]
	for it.HasNext() {
		_, t := it.Next()
		got = append(got, t)
	}
	require.Equal(t, []string{"b-1", "b-2"}, got)

	// Empty prefix iteration over empty dictionary.
	empty, err := NewTermDict(nil)
	require.NoError(t, err)
	require.False(t, empty.Iterate(nil).HasNext())
}

// TestR10_TermDict_RoundTrip serializes and reloads the dictionary and
// checks that the §3.1 file format preserves every term and id.
func TestR10_TermDict_RoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(99, 1))
	const n = 2000
	set := make(map[string]struct{})
	for len(set) < n {
		set[randTerm(r, 3, 16)] = struct{}{}
	}
	terms := make([]string, 0, n)
	for k := range set {
		terms = append(terms, k)
	}
	sort.Strings(terms)
	orig, err := NewTermDict(terms)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, writeTermDict(&buf, orig))
	reloaded, err := readTermDict(&buf)
	require.NoError(t, err)
	require.Equal(t, orig.Len(), reloaded.Len())
	for i := 0; i < orig.Len(); i++ {
		require.Equal(t, orig.terms[i], reloaded.terms[i], "term %d", i)
		id, ok := reloaded.GetStr(orig.terms[i])
		require.True(t, ok)
		require.Equal(t, TermID(i), id)
	}
}

// TestR10_TermDict_PrefixSharing verifies that zstd exploits the prefix
// redundancy in a sorted dictionary: the compressed blob is materially
// smaller than the raw concatenation when terms share prefixes.
func TestR10_TermDict_PrefixSharing(t *testing.T) {
	// 1000 distinct terms sharing a long common prefix.
	r := rand.New(rand.NewPCG(1, 2))
	seen := make(map[string]struct{})
	terms := make([]string, 0, 1000)
	for len(terms) < 1000 {
		s := "long-common-prefix-for-compression-testing-" + randTerm(r, 5, 8)
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		terms = append(terms, s)
	}
	sort.Strings(terms)
	d, err := NewTermDict(terms)
	require.NoError(t, err)

	raw := 0
	for _, t := range terms {
		raw += len(t)
	}
	var buf bytes.Buffer
	require.NoError(t, writeTermDict(&buf, d))
	// The serialized form (header + offsets + zstd blob) must be far
	// smaller than the raw term concatenation when prefixes dominate.
	require.Less(t, buf.Len(), raw/2, "compressed %d vs raw %d", buf.Len(), raw)
}

// TestR10_TermDict_BuilderDedup verifies the builder deduplicates terms
// and assigns sorted ids on Finish.
func TestR10_TermDict_BuilderDedup(t *testing.T) {
	b := NewTermDictBuilder()
	for _, t := range []string{"cherry", "apple", "banana", "apple", "cherry"} {
		b.Add(t)
	}
	d, err := b.Finish()
	require.NoError(t, err)
	require.Equal(t, []string{"apple", "banana", "cherry"}, d.terms)
}

// TestR10_TermDict_UnsortedRejected ensures the constructor rejects
// unsorted or duplicate input — a wiring bug at build time, not a
// runtime condition to paper over.
func TestR10_TermDict_UnsortedRejected(t *testing.T) {
	_, err := NewTermDict([]string{"a", "c", "b"})
	require.Error(t, err)
	_, err = NewTermDict([]string{"a", "a"})
	require.Error(t, err)
}

// randTerm returns a random lowercase alpha term of length in [minLen,
// maxLen].
func randTerm(r *rand.Rand, minLen, maxLen int) string {
	n := minLen + r.IntN(maxLen-minLen+1)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + r.IntN(26))
	}
	return string(b)
}
