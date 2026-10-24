// TFIDFScorer implements classic TF-IDF scoring as an alternative
// to the default BM25 scorer.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.13
package inverted

import "math"

// TFIDFParams holds TF-IDF scoring parameters. Unlike BM25, TF-IDF
// has no saturation (k1) or document length normalization (b).
type TFIDFParams struct {
	Boost float64
}

// DefaultTFIDFParams returns sensible defaults.
func DefaultTFIDFParams() TFIDFParams {
	return TFIDFParams{Boost: 1.0}
}

// TFIDFScorer computes TF-IDF relevance scores.
type TFIDFScorer struct {
	params TFIDFParams
}

// NewTFIDFScorer creates a scorer with the given parameters.
func NewTFIDFScorer(params TFIDFParams) *TFIDFScorer {
	if params.Boost == 0 {
		params.Boost = 1.0
	}
	return &TFIDFScorer{params: params}
}

// ScoreTerm computes: tf * log(N / df) * boost
// where N = total docs, df = docs containing term, tf = term frequency.
// Returns 0 when df == 0 (avoid log(0)) or df >= N (term appears in
// all documents, idf = 0).
func (s *TFIDFScorer) ScoreTerm(tf, df, N uint64) float64 {
	if df == 0 || N == 0 {
		return 0
	}
	if df >= N {
		// log(N/N) = log(1) = 0; term in all docs contributes nothing.
		return 0
	}
	idf := math.Log(float64(N) / float64(df))
	return float64(tf) * idf * s.params.Boost
}

// IDF computes log(N / df) — the inverse document frequency.
// Used for WAND upper bound calculation.
func (s *TFIDFScorer) IDF(df, N uint64) float64 {
	if df == 0 || N == 0 || df >= N {
		return 0
	}
	return math.Log(float64(N)/float64(df)) * s.params.Boost
}

// UpperBound computes the maximum possible score for a block with
// the given block-max term frequency. For TF-IDF this is:
// blockMaxTF * idf * boost.
func (s *TFIDFScorer) UpperBound(blockMaxTF, df, N uint64) float64 {
	return s.IDF(df, N) * float64(blockMaxTF)
}
