package inverted

import (
	"testing"
)

// TestNGramScorerBasic verifies basic n-gram similarity computation.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.12
func TestNGramScorerBasic(t *testing.T) {
	s := NewNGramScorer("hello", 2, 0.0)

	if s.QuerySize() != 4 { // "he","el","ll","lo"
		t.Fatalf("expected 4 query n-grams, got %d", s.QuerySize())
	}

	matched := s.MatchNGrams("hello")
	if matched != 4 {
		t.Fatalf("expected 4 matched n-grams for identical text, got %d", matched)
	}

	score := s.Score(matched, 5)
	if score < 0.99 || score > 1.01 {
		t.Fatalf("expected score ~1.0 for identical text, got %.4f", score)
	}
}

// TestNGramScorerPartial verifies partial match scoring.
func TestNGramScorerPartial(t *testing.T) {
	s := NewNGramScorer("hello", 2, 0.0)

	matched := s.MatchNGrams("help") // "he" matches, "el" matches, "lp" doesn't
	if matched != 2 {
		t.Fatalf("expected 2 matched n-grams, got %d", matched)
	}

	score := s.Score(matched, 4)
	if score <= 0 || score >= 1.0 {
		t.Fatalf("expected partial score 0<x<1, got %.4f", score)
	}
}

// TestNGramScorerNoMatch verifies zero score for no matches.
func TestNGramScorerNoMatch(t *testing.T) {
	s := NewNGramScorer("abc", 2, 0.0)

	matched := s.MatchNGrams("xyz")
	if matched != 0 {
		t.Fatalf("expected 0 matched, got %d", matched)
	}

	score := s.Score(matched, 3)
	if score != 0 {
		t.Fatalf("expected 0 score for no match, got %.4f", score)
	}
}

// TestNGramScorerThreshold verifies threshold checking.
func TestNGramScorerThreshold(t *testing.T) {
	s := NewNGramScorer("hello", 2, 0.5)

	matched := s.MatchNGrams("hello")
	score := s.Score(matched, 5)
	if !s.MeetsThreshold(score) {
		t.Fatalf("expected score %.4f to meet threshold 0.5", score)
	}

	matched2 := s.MatchNGrams("xyz")
	score2 := s.Score(matched2, 3)
	if s.MeetsThreshold(score2) {
		t.Fatalf("expected score %.4f to NOT meet threshold 0.5", score2)
	}
}

// TestNGramScorerEmptyQuery verifies empty query produces zero scores.
func TestNGramScorerEmptyQuery(t *testing.T) {
	s := NewNGramScorer("", 2, 0.0)

	if s.QuerySize() != 0 {
		t.Fatalf("expected 0 query n-grams, got %d", s.QuerySize())
	}

	score := s.Score(0, 5)
	if score != 0 {
		t.Fatalf("expected 0 score for empty query, got %.4f", score)
	}
}

// TestNGramScorerNGramSizeClamp verifies n-gram size is clamped.
func TestNGramScorerNGramSizeClamp(t *testing.T) {
	s := NewNGramScorer("test", 0, 0.0)
	if s.NGramSize() != 2 {
		t.Fatalf("expected n-gram size clamped to 2, got %d", s.NGramSize())
	}

	s2 := NewNGramScorer("test", 20, 0.0)
	if s2.NGramSize() != 10 {
		t.Fatalf("expected n-gram size clamped to 10, got %d", s2.NGramSize())
	}
}

// TestNGramScorerShortText verifies text shorter than n-gram size.
func TestNGramScorerShortText(t *testing.T) {
	s := NewNGramScorer("a", 2, 0.0)

	if s.QuerySize() != 1 {
		t.Fatalf("expected 1 n-gram for short text, got %d", s.QuerySize())
	}

	matched := s.MatchNGrams("a")
	if matched != 1 {
		t.Fatalf("expected 1 matched, got %d", matched)
	}
}
