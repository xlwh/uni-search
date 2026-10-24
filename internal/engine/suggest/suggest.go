// Package suggest implements the M1 suggester types (design
// 02-index-engine §3.1/§7, 03-query-engine §10): term suggester
// (Levenshtein edit distance), phrase suggester, and completion
// suggester (FST prefix lookup).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.5
package suggest

import (
	"sort"
	"strings"
)

// Suggestion is one suggested term/phrase.
type Suggestion struct {
	Text      string
	Score     float64
	Frequency int
}

// TermSuggester finds terms within a given edit distance of the query
// term, ranked by frequency (term suggester, design 02 §3.1).
type TermSuggester struct {
	terms map[string]int // term → document frequency
}

// NewTermSuggester creates a term suggester from a term-frequency map.
func NewTermSuggester(terms map[string]int) *TermSuggester {
	return &TermSuggester{terms: terms}
}

// Suggest returns terms within maxDist edits of query, ranked by
// frequency (descending) then by edit distance (ascending).
func (s *TermSuggester) Suggest(query string, maxDist int) []Suggestion {
	if maxDist < 0 {
		maxDist = 2
	}
	if maxDist > 2 {
		maxDist = 2
	}

	var results []Suggestion
	for term, freq := range s.terms {
		dist := levenshtein(query, term)
		if dist > maxDist {
			continue
		}
		results = append(results, Suggestion{
			Text:      term,
			Score:     float64(freq) / float64(dist+1),
			Frequency: freq,
		})
	}

	// Sort by score descending, then by text ascending.
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Text < results[j].Text
	})

	return results
}

// PhraseSuggester suggests corrected phrases by replacing terms
// with their best term suggestions (phrase suggester, design 03 §10).
type PhraseSuggester struct {
	termSuggester *TermSuggester
}

// NewPhraseSuggester creates a phrase suggester backed by a term suggester.
func NewPhraseSuggester(ts *TermSuggester) *PhraseSuggester {
	return &PhraseSuggester{termSuggester: ts}
}

// Suggest takes a phrase (space-separated words), corrects each word
// if it's not in the dictionary, and returns the corrected phrase.
func (s *PhraseSuggester) Suggest(phrase string, maxDist int) []Suggestion {
	words := strings.Fields(phrase)
	if len(words) == 0 {
		return nil
	}

	corrected := make([]string, len(words))
	totalScore := 0.0
	for i, word := range words {
		// If the word is in the dictionary, keep it.
		if _, ok := s.termSuggester.terms[word]; ok {
			corrected[i] = word
			continue
		}
		// Otherwise, find the best suggestion.
		suggestions := s.termSuggester.Suggest(word, maxDist)
		if len(suggestions) > 0 {
			corrected[i] = suggestions[0].Text
			totalScore += suggestions[0].Score
		} else {
			corrected[i] = word
		}
	}

	result := Suggestion{
		Text:  strings.Join(corrected, " "),
		Score: totalScore / float64(len(words)),
	}
	return []Suggestion{result}
}

// CompletionEntry is one autocomplete candidate for the completion
// suggester.
type CompletionEntry struct {
	Text  string
	Score float64
}

// CompletionSuggester provides prefix-based autocomplete suggestions
// (completion suggester, design 02 §7). Uses a sorted prefix index for
// M1; M2 can swap to FST-backed lookup.
type CompletionSuggester struct {
	entries []CompletionEntry
}

// NewCompletionSuggester creates a completion suggester from a set of
// (text, score) entries.
func NewCompletionSuggester(entries []CompletionEntry) *CompletionSuggester {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Text < entries[j].Text })
	return &CompletionSuggester{entries: entries}
}

// Suggest returns entries that start with the given prefix, sorted
// by score descending, limited to size.
func (s *CompletionSuggester) Suggest(prefix string, size int) []Suggestion {
	if size <= 0 {
		size = 5
	}

	// Binary search for the prefix range.
	start := sort.Search(len(s.entries), func(i int) bool {
		return s.entries[i].Text >= prefix
	})

	var results []Suggestion
	for i := start; i < len(s.entries); i++ {
		if !strings.HasPrefix(s.entries[i].Text, prefix) {
			break
		}
		results = append(results, Suggestion{
			Text:  s.entries[i].Text,
			Score: s.entries[i].Score,
		})
		if len(results) >= size {
			break
		}
	}

	// Sort by score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	// Use two rows for O(min(m,n)) space.
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
