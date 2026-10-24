// Package highlight implements the M1 unified highlighter (design
// 02-index-engine §3.5, 03-query-engine §10). It marks query term
// occurrences in document text using <em> tags (ES unified highlighter
// style). The highlighter works on raw text — it re-analyzes the text
// to find term positions, then wraps matches in <em></em>.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.5
package highlight

import (
	"fmt"
	"sort"
	"strings"

	"github.com/unisearch-db/unisearch/internal/index/analyzer"
)

// Fragment is one highlighted text snippet.
type Fragment struct {
	Text string
}

// HighlightResult is the output for one field's highlighting.
type HighlightResult struct {
	Field     string
	Fragments []Fragment
}

// Highlighter highlights query terms in document text.
type Highlighter struct {
	analyzer analyzer.Analyzer
}

// New creates a highlighter using the given analyzer for tokenization.
func New(a analyzer.Analyzer) *Highlighter {
	return &Highlighter{analyzer: a}
}

// Highlight marks query term occurrences in text. Returns highlighted
// fragments separated by ellipsis. fragmentSize controls the max
// fragment length (default 100). If the text is shorter than
// fragmentSize, the entire text is returned with highlights.
func (h *Highlighter) Highlight(text string, queryTerms []string, fragmentSize int) HighlightResult {
	if fragmentSize <= 0 {
		fragmentSize = 100
	}
	if h.analyzer == nil {
		return HighlightResult{Field: "", Fragments: []Fragment{{Text: text}}}
	}

	// Analyze the text to get token positions.
	tokens := h.analyzer.Analyze([]byte(text))
	if len(tokens) == 0 {
		return HighlightResult{Field: "", Fragments: []Fragment{{Text: text}}}
	}

	// Build a set of query terms for matching.
	qSet := make(map[string]bool)
	for _, q := range queryTerms {
		qSet[strings.ToLower(q)] = true
	}

	// Find matching token positions in the text.
	type match struct {
		start int
		end   int
	}
	var matches []match
	for _, tok := range tokens {
		if qSet[strings.ToLower(tok.Term)] {
			// Map token position to byte offset in the original text.
			start, end := tokenByteRange(text, tok)
			matches = append(matches, match{start: start, end: end})
		}
	}

	if len(matches) == 0 {
		return HighlightResult{Field: "", Fragments: []Fragment{{Text: text}}}
	}

	// Build highlighted text by inserting <em> tags around matches.
	var sb strings.Builder
	lastEnd := 0
	for _, m := range matches {
		sb.WriteString(text[lastEnd:m.start])
		sb.WriteString("<em>")
		sb.WriteString(text[m.start:m.end])
		sb.WriteString("</em>")
		lastEnd = m.end
	}
	sb.WriteString(text[lastEnd:])

	highlighted := sb.String()

	// If the highlighted text is short enough, return as one fragment.
	if len(highlighted) <= fragmentSize {
		return HighlightResult{Field: "", Fragments: []Fragment{{Text: highlighted}}}
	}

	// Otherwise, extract fragments around matches.
	return HighlightResult{Field: "", Fragments: extractFragments(highlighted, fragmentSize)}
}

// tokenByteRange maps a token's position to byte offsets in the
// original text. Since the analyzer reports byte offsets, use them
// directly; if offsets are not available, fall back to string search.
func tokenByteRange(text string, tok analyzer.Token) (int, int) {
	if tok.Start >= 0 && tok.End > tok.Start && tok.End <= len(text) {
		return tok.Start, tok.End
	}
	// Fallback: search for the token in the text.
	idx := strings.Index(strings.ToLower(text), strings.ToLower(tok.Term))
	if idx < 0 {
		return 0, 0
	}
	return idx, idx + len(tok.Term)
}

// extractFragments splits highlighted text into fragments of at most
// fragmentSize bytes, centered around <em> tags.
func extractFragments(text string, fragmentSize int) []Fragment {
	var fragments []Fragment
	emStart := "<em>"
	emEnd := "</em>"

	// Find all <em> positions and create fragments around them.
	positions := findAllOccurrences(text, emStart)
	if len(positions) == 0 {
		return []Fragment{{Text: text}}
	}

	// Deduplicate overlapping fragments.
	used := make(map[int]bool)
	for _, pos := range positions {
		if used[pos] {
			continue
		}
		start := pos - fragmentSize/3
		if start < 0 {
			start = 0
		}
		end := start + fragmentSize
		if end > len(text) {
			end = len(text)
			start = end - fragmentSize
			if start < 0 {
				start = 0
			}
		}

		frag := text[start:end]
		fragments = append(fragments, Fragment{Text: frag})

		// Mark positions within this fragment as used.
		for _, p := range positions {
			if p >= start && p < end {
				used[p] = true
			}
		}
	}

	sort.Slice(fragments, func(i, j int) bool {
		return strings.Index(fragments[i].Text, emEnd) < strings.Index(fragments[j].Text, emEnd)
	})

	return fragments
}

func findAllOccurrences(text, sub string) []int {
	var positions []int
	idx := 0
	for {
		pos := strings.Index(text[idx:], sub)
		if pos < 0 {
			break
		}
		positions = append(positions, idx+pos)
		idx += pos + 1
	}
	return positions
}

// HighlightFields highlights multiple fields from a document.
func (h *Highlighter) HighlightFields(doc map[string]any, fieldQueries map[string][]string, fragmentSize int) map[string]HighlightResult {
	results := make(map[string]HighlightResult)
	for field, terms := range fieldQueries {
		val, ok := doc[field]
		if !ok {
			continue
		}
		text, ok := val.(string)
		if !ok {
			text = fmt.Sprintf("%v", val)
		}
		r := h.Highlight(text, terms, fragmentSize)
		r.Field = field
		results[field] = r
	}
	return results
}
