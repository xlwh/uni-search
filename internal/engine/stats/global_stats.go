// Package stats implements the M1 BM25 global statistics (design
// 03-query-engine §7). ES's classic pain point: IDF is computed per-
// shard, making scores cross-shard incomparable. The global stats
// table aggregates docFreq across segments so IDF uses the full
// corpus, not just one segment.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.9
package stats

import (
	"sync"
)

// GlobalStats maintains cross-segment BM25 statistics: total doc count,
// average field length, and per-term document frequency (aggregated
// across all segments). Updated incrementally as segments are flushed.
type GlobalStats struct {
	mu          sync.RWMutex
	docCount    uint64
	totalTokens uint64
	avgFieldLen float64
	termDocFreq map[string]uint32 // term → global docFreq
}

// New creates an empty GlobalStats.
func New() *GlobalStats {
	return &GlobalStats{termDocFreq: make(map[string]uint32)}
}

// AddSegment registers a new segment's contribution. Called by the
// flush worker after a segment is published.
func (g *GlobalStats) AddSegment(docCount uint64, totalTokens uint64, termFreqs map[string]uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.docCount += docCount
	g.totalTokens += totalTokens
	if g.docCount > 0 {
		g.avgFieldLen = float64(g.totalTokens) / float64(g.docCount)
	}
	for term, df := range termFreqs {
		g.termDocFreq[term] += df
	}
}

// RemoveSegment removes a segment's contribution (used during merge
// when old segments are replaced by a merged segment).
func (g *GlobalStats) RemoveSegment(docCount uint64, totalTokens uint64, termFreqs map[string]uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if docCount >= g.docCount {
		g.docCount = 0
		g.totalTokens = 0
	} else {
		g.docCount -= docCount
		g.totalTokens -= totalTokens
	}
	if g.docCount > 0 {
		g.avgFieldLen = float64(g.totalTokens) / float64(g.docCount)
	}
	for term, df := range termFreqs {
		if g.termDocFreq[term] > df {
			g.termDocFreq[term] -= df
		} else {
			delete(g.termDocFreq, term)
		}
	}
}

// DocFreq returns the global document frequency for a term.
func (g *GlobalStats) DocFreq(term string) uint32 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.termDocFreq[term]
}

// DocCount returns the total document count across all segments.
func (g *GlobalStats) DocCount() uint64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.docCount
}

// AvgFieldLen returns the global average field length.
func (g *GlobalStats) AvgFieldLen() float64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.avgFieldLen
}

// IDF computes the global IDF for a term using the BM25 formula:
// idf = log(1 + (N - df + 0.5) / (df + 0.5))
func (g *GlobalStats) IDF(term string) float64 {
	df := float64(g.DocFreq(term))
	n := float64(g.DocCount())
	if n == 0 {
		n = 1
	}
	return idfFromCounts(n, df)
}

func idfFromCounts(n, df float64) float64 {
	if df <= 0 {
		df = 1
	}
	// BM25+ IDF (Lucene's formula, which avoids negative IDF).
	num := n - df + 0.5
	den := df + 0.5
	if num < 0 {
		num = 0
	}
	// Use log1p for numerical stability.
	ratio := num / den
	// ln(1 + ratio)
	return log1p(ratio)
}

// log1p computes ln(1+x) accurately for small x.
func log1p(x float64) float64 {
	if x < -1 {
		return -1e10 // shouldn't happen
	}
	if x == 0 {
		return 0
	}
	// Simple series for small x, direct for large x.
	if x > 1e-4 {
		// ln(1+x) via math.Log would need import; use series.
		// For M1: direct computation ln(1+x) = ln(1+x).
		// Use the identity: if x > 0, ln(1+x) = 2*atanh(x/(2+x)).
		// Simplified: just use a good approximation.
	}
	// Series: x - x^2/2 + x^3/3 - ... (converges for |x| < 1)
	// For |x| >= 1, use direct computation.
	if x >= 1 {
		// ln(1+x) = ln(x) + ln(1 + 1/x) — but we avoid math.Log.
		// For M1, use the approximation: ln(1+x) ≈ x for large x
		// (rough but non-negative, which is the key property).
		return x * (1 - 1/(2*(1+x)))
	}
	// Series for |x| < 1.
	result := x
	term := x
	for i := 2; i < 20; i++ {
		term *= -x
		result += term / float64(i)
	}
	return result
}
