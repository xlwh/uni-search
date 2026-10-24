// NGramScorer computes n-gram similarity scores for fuzzy text matching.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.12
package inverted

import "math"

// NGramScorer computes Dice similarity: 2|Q∩D| / (|Q| + |D|)
// where Q is the query n-gram set and D is the document n-gram set.
type NGramScorer struct {
	queryNGrams map[string]struct{}
	querySize   int
	ngramSize   int
	threshold   float64
}

// NewNGramScorer creates a scorer for the given query text.
func NewNGramScorer(queryText string, ngramSize int, threshold float64) *NGramScorer {
	if ngramSize < 1 {
		ngramSize = 2
	}
	if ngramSize > 10 {
		ngramSize = 10
	}
	qn := extractNGrams(queryText, ngramSize)
	return &NGramScorer{
		queryNGrams: qn,
		querySize:   len(qn),
		ngramSize:   ngramSize,
		threshold:   threshold,
	}
}

// Score computes the Dice similarity given the number of matched
// n-grams and the document field length (used to estimate |D|).
// score = 2 * matched / (|Q| + |D|)
func (s *NGramScorer) Score(matchedNGrams int, docFieldLen int) float64 {
	if s.querySize == 0 || docFieldLen == 0 {
		return 0
	}
	// Estimate |D| from field length: roughly (fieldLen - n + 1) n-grams.
	docSize := docFieldLen - s.ngramSize + 1
	if docSize <= 0 {
		docSize = 1
	}
	return 2.0 * float64(matchedNGrams) / float64(s.querySize+docSize)
}

// MeetsThreshold returns true if the score meets the minimum threshold.
func (s *NGramScorer) MeetsThreshold(score float64) bool {
	return score >= s.threshold
}

// QuerySize returns the number of n-grams in the query.
func (s *NGramScorer) QuerySize() int { return s.querySize }

// NGramSize returns the n-gram size.
func (s *NGramScorer) NGramSize() int { return s.ngramSize }

// extractNGrams returns the set of n-grams from the input text.
func extractNGrams(text string, n int) map[string]struct{} {
	runes := []rune(text)
	if len(runes) < n {
		if len(runes) == 0 {
			return map[string]struct{}{}
		}
		return map[string]struct{}{text: {}}
	}
	set := make(map[string]struct{}, len(runes)-n+1)
	for i := 0; i <= len(runes)-n; i++ {
		gram := string(runes[i : i+n])
		set[gram] = struct{}{}
	}
	return set
}

// MatchNGrams counts how many query n-grams appear in the document
// text. This is used by the execution operator to compute the
// matched count for each document.
func (s *NGramScorer) MatchNGrams(docText string) int {
	if len(s.queryNGrams) == 0 {
		return 0
	}
	docNGrams := extractNGrams(docText, s.ngramSize)
	count := 0
	for gram := range s.queryNGrams {
		if _, ok := docNGrams[gram]; ok {
			count++
		}
	}
	return count
}

// Ensure math is used.
var _ = math.Log
