package inverted

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR10_BM25_IDF hand-computes the §3.3 IDF for a tiny corpus and
// checks the implementation matches to floating-point precision.
//
//	IDF(t) = ln(1 + (N - df + 0.5)/(df + 0.5))
func TestR10_BM25_IDF(t *testing.T) {
	st := SegmentStats{DocCount: 10, AvgFieldLen: 50}
	cases := []struct {
		df   int
		want float64
	}{
		{1, math.Log(1 + (10-1+0.5)/(1+0.5))}, // = ln(1 + 9.5/1.5) = ln(7.333..)
		{5, math.Log(1 + (10-5+0.5)/(5+0.5))}, // = ln(1 + 5.5/5.5) = ln(2)
		{10, math.Log(1 + (10-10+0.5)/(10+0.5))}, // = ln(1 + 0.5/10.5) ≈ 0.0465
		{0, 0}, // zero df is a guard: returns 0
	}
	for i, c := range cases {
		got := st.IDF(c.df)
		require.Less(t, math.Abs(got-c.want), 1e-12, "case %d df=%d", i, c.df)
	}
}

// TestR10_BM25_ScoreTerm hand-computes one (term, doc) score using the
// full §3.3 formula and checks the implementation to relative error
// < 1e-9 (the spec.md R10 acceptance bound).
//
//	score = IDF · (tf · (k1+1)) / (tf + k1·(1 - b + b·fldLen/avgFL))
func TestR10_BM25_ScoreTerm(t *testing.T) {
	p := DefaultBM25Params()
	st := SegmentStats{DocCount: 100, AvgFieldLen: 40}
	// tf=2, fldLen=20, df=5
	tf, fldLen, df := 2, 20, 5
	idf := math.Log(1 + (100-5+0.5)/(5+0.5))
	norm := 1 - p.B + p.B*float64(fldLen)/st.AvgFieldLen
	want := idf * (float64(tf) * (p.K1 + 1)) / (float64(tf) + p.K1*norm)
	got := st.ScoreTerm(p, tf, fldLen, df)
	require.Less(t, math.Abs(got-want)/math.Abs(want), 1e-9, "got=%v want=%v", got, want)
}

// TestR10_BM25_ScoreTerm_NonNegativity checks that IDF stays non-
// negative even when a term appears in every document (df → N), the
// ES 8.x behavior the design pins to.
func TestR10_BM25_ScoreTerm_NonNegativity(t *testing.T) {
	p := DefaultBM25Params()
	st := SegmentStats{DocCount: 50, AvgFieldLen: 30}
	got := st.ScoreTerm(p, 3, 30, 50) // df == N
	require.GreaterOrEqual(t, got, 0.0)
}

// TestR10_BM25_SegmentStats verifies the segment statistics are
// computed from per-doc field lengths.
func TestR10_BM25_SegmentStats(t *testing.T) {
	st := NewSegmentStats([]int{10, 20, 30, 0, 40})
	require.Equal(t, uint32(5), st.DocCount)
	require.Equal(t, uint64(100), st.TotalTokens)
	require.Equal(t, 20.0, st.AvgFieldLen)
}
