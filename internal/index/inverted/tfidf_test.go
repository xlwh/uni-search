package inverted

import (
	"math"
	"testing"
)

// TestTFIDFScoreTerm verifies basic TF-IDF score computation.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.13
func TestTFIDFScoreTerm(t *testing.T) {
	s := NewTFIDFScorer(DefaultTFIDFParams())

	// tf=2, df=10, N=100: score = 2 * log(100/10) = 2 * log(10)
	score := s.ScoreTerm(2, 10, 100)
	expected := 2.0 * math.Log(10.0)
	if math.Abs(score-expected) > 0.001 {
		t.Fatalf("expected %.4f, got %.4f", expected, score)
	}
}

// TestTFIDFScoreTermDFZero verifies df=0 returns 0.
func TestTFIDFScoreTermDFZero(t *testing.T) {
	s := NewTFIDFScorer(DefaultTFIDFParams())
	if score := s.ScoreTerm(5, 0, 100); score != 0 {
		t.Fatalf("expected 0 for df=0, got %f", score)
	}
}

// TestTFIDFScoreTermDFAllDocs verifies df>=N returns 0 (idf=0).
func TestTFIDFScoreTermDFAllDocs(t *testing.T) {
	s := NewTFIDFScorer(DefaultTFIDFParams())
	if score := s.ScoreTerm(5, 100, 100); score != 0 {
		t.Fatalf("expected 0 for df>=N, got %f", score)
	}
}

// TestTFIDFIDF verifies IDF computation.
func TestTFIDFIDF(t *testing.T) {
	s := NewTFIDFScorer(DefaultTFIDFParams())
	idf := s.IDF(10, 100)
	expected := math.Log(10.0)
	if math.Abs(idf-expected) > 0.001 {
		t.Fatalf("expected IDF %.4f, got %.4f", expected, idf)
	}
}

// TestTFIDFUpperBound verifies WAND upper bound computation.
func TestTFIDFUpperBound(t *testing.T) {
	s := NewTFIDFScorer(DefaultTFIDFParams())
	ub := s.UpperBound(5, 10, 100)
	expected := 5.0 * math.Log(10.0)
	if math.Abs(ub-expected) > 0.001 {
		t.Fatalf("expected upper bound %.4f, got %.4f", expected, ub)
	}
}

// TestTFIDFBoost verifies boost factor is applied.
func TestTFIDFBoost(t *testing.T) {
	s := NewTFIDFScorer(TFIDFParams{Boost: 2.0})
	score := s.ScoreTerm(1, 10, 100)
	expected := 1.0 * math.Log(10.0) * 2.0
	if math.Abs(score-expected) > 0.001 {
		t.Fatalf("expected %.4f with boost, got %.4f", expected, score)
	}
}

// TestTFIDFDefaultBoost verifies default boost is 1.0.
func TestTFIDFDefaultBoost(t *testing.T) {
	s := NewTFIDFScorer(TFIDFParams{})
	score := s.ScoreTerm(1, 10, 100)
	expected := math.Log(10.0)
	if math.Abs(score-expected) > 0.001 {
		t.Fatalf("expected %.4f with default boost, got %.4f", expected, score)
	}
}

// TestTFIDFCompareBM25 verifies TF-IDF gives different scores than BM25.
func TestTFIDFCompareBM25(t *testing.T) {
	tfidf := NewTFIDFScorer(DefaultTFIDFParams())
	bm25Params := DefaultBM25Params()
	stats := NewSegmentStats([]int{50})

	// tf=3, df=5, N=1000, docLen=50
	tfidfScore := tfidf.ScoreTerm(3, 5, 1000)
	bm25Score := stats.ScoreTerm(bm25Params, 3, 50, 5)

	if math.Abs(tfidfScore-bm25Score) < 0.001 {
		t.Fatalf("TF-IDF and BM25 should differ: tfidf=%.4f, bm25=%.4f", tfidfScore, bm25Score)
	}
}
