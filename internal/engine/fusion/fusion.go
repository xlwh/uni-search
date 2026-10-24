// Package fusion implements the §5 hybrid retrieval fusion stage.
// Two M0 methods are supported:
//
//   - rrf     — Reciprocal Rank Fusion: score(d) = Σ_i 1/(k + rank_i(d))
//     with default rank_constant k = 60 (ES 8.x semantics).
//   - weighted — per-path min-max normalization then weighted sum
//     Σ_i w_i · (s_i − min_i) / (max_i − min_i).
//
// Each input path is truncated to WindowSize (default 500) before
// fusion so a single dominant path cannot starve the others. The
// fused output is sorted by descending score; ties are broken by
// the lexicographically smaller DocID for determinism.
//
// relative_score (Weaviate-hybrid) and rerank (external HTTP) are
// defined in the design but out of M0 scope; NewFusion returns
// ErrUnsupportedMethod for them.
package fusion

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	"github.com/unisearch-db/unisearch/internal/engine/exec"
)

// ScoredDoc is the fusion-stage scored document. DocID is the §3.3
// GlobalDocID (SegmentID<<32 | RowID); Score is the per-path raw
// score (BM25 / cosine / etc.) as produced by the upstream operator.
type ScoredDoc struct {
	DocID uint64
	Score float32
}

// ErrUnsupportedMethod is returned by NewFusion for rank methods
// not in M0 scope (relative_score, rerank).
var ErrUnsupportedMethod = errors.New("fusion: rank method not supported in M0")

// ErrWeightsMismatch is returned by WeightedFusion.Merge when the
// number of weights does not match the number of input paths.
var ErrWeightsMismatch = errors.New("fusion: weights count != paths count")

// ErrInvalidRankConstant is returned when the rank constant is ≤ 0
// (would cause division by zero).
var ErrInvalidRankConstant = errors.New("fusion: rank_constant must be > 0")

// ErrInvalidWindowSize is returned when WindowSize is ≤ 0 (cannot
// truncate paths to a non-positive budget).
var ErrInvalidWindowSize = errors.New("fusion: window_size must be > 0")

// Fusion is the §5 fusion interface. Merge takes the per-path
// ranked candidates (each path already sorted by descending score)
// and returns a single fused, descending-score list.
type Fusion interface {
	Merge(cands [][]ScoredDoc) ([]ScoredDoc, error)
}

// NewFusion constructs the fusion implementation for the given
// RankSpec. rrf → RRFFusion; weighted → WeightedFusion; others →
// ErrUnsupportedMethod. Defaults (rank_constant=60, window_size=500)
// are applied when the spec leaves them zero.
func NewFusion(spec dsl.RankSpec) (Fusion, error) {
	if spec.RankConst <= 0 {
		spec.RankConst = dsl.DefaultRankConstant
	}
	if spec.WindowSize <= 0 {
		spec.WindowSize = dsl.DefaultWindowSize
	}
	switch spec.Method {
	case dsl.RankMethodRRF:
		return &RRFFusion{K: spec.RankConst, Window: spec.WindowSize}, nil
	case dsl.RankMethodWeighted:
		if len(spec.Weights) == 0 {
			return &WeightedFusion{Weights: nil, Window: spec.WindowSize}, nil
		}
		return &WeightedFusion{Weights: append([]float64(nil), spec.Weights...), Window: spec.WindowSize}, nil
	case dsl.RankMethodRelativeScore:
		// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.4
		return &RelativeScoreFusion{Window: spec.WindowSize}, nil
	case dsl.RankMethodDBSF:
		// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.2
		return &DBSFFusion{Weights: append([]float64(nil), spec.Weights...), Window: spec.WindowSize, K: 3.0}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedMethod, spec.Method)
	}
}

// RRFFusion implements the §5 rrf fusion: score(d) = Σ_i 1/(k + rank_i(d))
// where rank_i(d) is the 0-based position of d in path i's truncated
// candidate list. K is the rank constant (default 60). Window caps
// each path's contribution.
type RRFFusion struct {
	K      int
	Window int
}

// Merge implements Fusion. Each path is truncated to Window entries
// (top-Window by input order; callers must hand in already-sorted
// paths from the upstream TopN/ANN/BM25 operators).
func (f *RRFFusion) Merge(cands [][]ScoredDoc) ([]ScoredDoc, error) {
	if f.K <= 0 {
		return nil, ErrInvalidRankConstant
	}
	if f.Window <= 0 {
		return nil, ErrInvalidWindowSize
	}
	scores := map[uint64]float32{}
	for _, path := range cands {
		w := path
		if len(w) > f.Window {
			w = w[:f.Window]
		}
		for rank, d := range w {
			scores[d.DocID] += 1.0 / float32(f.K+rank)
		}
	}
	out := make([]ScoredDoc, 0, len(scores))
	for doc, s := range scores {
		out = append(out, ScoredDoc{DocID: doc, Score: s})
	}
	sortDesc(out)
	return out, nil
}

// WeightedFusion implements the §5 weighted fusion: each path is
// min-max normalized within the path (so cross-path scores become
// comparable on a [0, 1] scale), then Σ_i w_i · ŝ_i(d). Weights is
// either nil (equal weights 1/n) or must match the path count.
type WeightedFusion struct {
	Weights []float64
	Window  int
}

// Merge implements Fusion.
func (f *WeightedFusion) Merge(cands [][]ScoredDoc) ([]ScoredDoc, error) {
	if f.Window <= 0 {
		return nil, ErrInvalidWindowSize
	}
	if f.Weights != nil && len(f.Weights) != len(cands) {
		return nil, ErrWeightsMismatch
	}
	scores := map[uint64]float32{}
	for i, path := range cands {
		w := path
		if len(w) > f.Window {
			w = w[:f.Window]
		}
		if len(w) == 0 {
			continue
		}
		min, max := w[0].Score, w[0].Score
		for _, d := range w {
			if d.Score < min {
				min = d.Score
			}
			if d.Score > max {
				max = d.Score
			}
		}
		var weight float64
		if f.Weights != nil {
			weight = f.Weights[i]
		} else {
			weight = 1.0 / float64(len(cands))
		}
		span := max - min
		for _, d := range w {
			var norm float32
			if span > 0 {
				norm = (d.Score - min) / span
			} else {
				// All scores identical: normalize to 1.0 so the path
				// still contributes its full weight.
				norm = 1.0
			}
			scores[d.DocID] += float32(weight) * norm
		}
	}
	out := make([]ScoredDoc, 0, len(scores))
	for doc, s := range scores {
		out = append(out, ScoredDoc{DocID: doc, Score: s})
	}
	sortDesc(out)
	return out, nil
}

// RelativeScoreFusion implements the §5 relative_score fusion: the
// first path (BM25) is the baseline; other paths' scores are mapped
// to the baseline's [min, max] range via min-max normalization, then
// summed: score(d) = baseline(d) + Σ_mapped ŝ_i(d).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.4
type RelativeScoreFusion struct {
	Window int
}

func (f *RelativeScoreFusion) Merge(cands [][]ScoredDoc) ([]ScoredDoc, error) {
	if len(cands) == 0 {
		return nil, nil
	}
	if f.Window <= 0 {
		f.Window = dsl.DefaultWindowSize
	}
	// Truncate each path to Window.
	for i := range cands {
		if len(cands[i]) > f.Window {
			cands[i] = cands[i][:f.Window]
		}
	}
	// Path 0 = baseline (BM25). Compute its min/max.
	baseline := cands[0]
	if len(baseline) == 0 {
		// No baseline: fall back to simple sum of normalized scores.
		return (&WeightedFusion{Window: f.Window}).Merge(cands)
	}
	bMin, bMax := baseline[0].Score, baseline[0].Score
	for _, d := range baseline {
		if d.Score < bMin {
			bMin = d.Score
		}
		if d.Score > bMax {
			bMax = d.Score
		}
	}
	bRange := bMax - bMin
	if bRange <= 0 {
		bRange = 1 // avoid div-by-zero when all baseline scores equal
	}
	// Build score map: docID → combined score.
	scores := make(map[uint64]float32)
	for _, d := range baseline {
		scores[d.DocID] = d.Score
	}
	// Map each non-baseline path to baseline range and add.
	for i := 1; i < len(cands); i++ {
		path := cands[i]
		if len(path) == 0 {
			continue
		}
		// Min-max normalize this path, then scale to baseline range.
		pMin, pMax := path[0].Score, path[0].Score
		for _, d := range path {
			if d.Score < pMin {
				pMin = d.Score
			}
			if d.Score > pMax {
				pMax = d.Score
			}
		}
		pRange := pMax - pMin
		if pRange <= 0 {
			pRange = 1
		}
		for _, d := range path {
			// Normalize to [0,1] then scale to baseline range.
			mapped := (d.Score - pMin) / pRange * bRange
			scores[d.DocID] += mapped
		}
	}
	out := make([]ScoredDoc, 0, len(scores))
	for doc, s := range scores {
		out = append(out, ScoredDoc{DocID: doc, Score: s})
	}
	sortDesc(out)
	return out, nil
}

// DBSFFusion implements Distribution-Based Score Fusion: per-path scores
// are z-score normalized using Welford's online variance algorithm, then
// combined as a weighted sum. Unlike WeightedFusion's min-max normalization,
// z-score normalization is robust against extreme outliers.
//
// z-score(d, path_i) = (score_i(d) - mean_i) / (k * stddev_i)
// final_score(d) = Σ_i weight_i * z-score(d, path_i)
//
// Documents not appearing in a path contribute 0 (the mean) for that path.
// The normalization factor k (default 3.0) maps ±3σ to approximately [-1, 1].
//
// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.2
type DBSFFusion struct {
	Weights []float64 // per-path weights; nil → equal 1/n
	Window  int       // per-path truncation before statistics
	K       float64   // normalization factor; default 3.0
}

// Merge implements Fusion.
func (f *DBSFFusion) Merge(cands [][]ScoredDoc) ([]ScoredDoc, error) {
	if len(cands) == 0 {
		return nil, nil
	}
	if f.Window <= 0 {
		f.Window = 500
	}
	if f.K <= 0 {
		f.K = 3.0
	}

	n := len(cands)
	weights := f.Weights
	if len(weights) == 0 {
		weights = make([]float64, n)
		for i := range weights {
			weights[i] = 1.0 / float64(n)
		}
	} else if len(weights) != n {
		return nil, ErrWeightsMismatch
	}

	// 1. Truncate each path to Window entries.
	truncated := make([][]ScoredDoc, n)
	for i, path := range cands {
		if len(path) > f.Window {
			truncated[i] = path[:f.Window]
		} else {
			truncated[i] = path
		}
	}

	// 2. Per-path mean and variance (Welford single-pass).
	type stats struct{ mean, m2 float64; count int }
	pathStats := make([]stats, n)
	for i, path := range truncated {
		s := stats{}
		for _, d := range path {
			s.count++
			delta := float64(d.Score) - s.mean
			s.mean += delta / float64(s.count)
			s.m2 += delta * (float64(d.Score) - s.mean)
		}
		pathStats[i] = s
	}

	// 3. Z-score normalize and weighted sum per document.
	scoreMap := make(map[uint64]float64)
	for i, path := range truncated {
		s := pathStats[i]
		var stddev float64
		if s.count > 1 && s.m2 > 0 {
			stddev = math.Sqrt(s.m2 / float64(s.count))
		}
		for _, d := range path {
			var z float64
			if stddev > 0 {
				z = (float64(d.Score) - s.mean) / (f.K * stddev)
			}
			scoreMap[d.DocID] += weights[i] * z
		}
	}

	// 4. Collect and sort descending; ties broken by smaller DocID.
	out := make([]ScoredDoc, 0, len(scoreMap))
	for doc, s := range scoreMap {
		out = append(out, ScoredDoc{DocID: doc, Score: float32(s)})
	}
	sortDesc(out)
	return out, nil
}

// sortDesc sorts by descending Score with a stable tie-break on
// the smaller DocID so fusion output is deterministic across runs.
func sortDesc(s []ScoredDoc) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Score != s[j].Score {
			return s[i].Score > s[j].Score
		}
		return s[i].DocID < s[j].DocID
	})
}

// ValidateFromSize enforces the §8 from+size ≤ 10000 cap. The REST
// layer calls this to fail-fast before constructing a TopN op. It
// returns ErrFromSizeTooLarge when from+size exceeds the cap (with
// the ES-flavored message) and nil otherwise. Negative from/size
// is also rejected.
func ValidateFromSize(from, size int) error {
	if from < 0 || size < 0 {
		return fmt.Errorf("fusion: from/size must be non-negative (from=%d size=%d)", from, size)
	}
	if from+size > exec.MaxFromPlusSize {
		return exec.ErrFromSizeTooLarge
	}
	return nil
}

// Compile-time assertions.
var (
	_ Fusion = (*RRFFusion)(nil)
	_ Fusion = (*WeightedFusion)(nil)
	_ Fusion = (*DBSFFusion)(nil)
)
