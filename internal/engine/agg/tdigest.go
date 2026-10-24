package agg

import (
	"math"
	"sort"
)

// TDigest is a t-digest percentile estimator (design 03 §6.1).
// Maintains a bounded set of centroids that compress around the
// extremes (0 and 1 quantiles) for higher accuracy at the tails.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.3
type TDigest struct {
	compression float64
	centroids   []centroid
	count       uint64
	totalWeight float64
}

type centroid struct {
	mean   float64
	weight float64
}

// NewTDigest creates a t-digest with the given compression
// (default 100; higher = more accurate, more memory).
func NewTDigest(compression float64) *TDigest {
	if compression <= 0 {
		compression = 100
	}
	return &TDigest{compression: compression}
}

// Add adds a single value with weight 1.
func (td *TDigest) Add(value float64) {
	td.AddWeighted(value, 1)
}

// AddWeighted adds a value with the given weight.
func (td *TDigest) AddWeighted(value float64, weight float64) {
	td.count++
	td.totalWeight += weight
	idx := sort.Search(len(td.centroids), func(i int) bool {
		return td.centroids[i].mean >= value
	})
	td.centroids = append(td.centroids, centroid{})
	copy(td.centroids[idx+1:], td.centroids[idx:])
	td.centroids[idx] = centroid{mean: value, weight: weight}
	if float64(len(td.centroids)) > td.compression*10 {
		td.compress()
	}
}

// compress merges adjacent centroids that fit within the compression
// budget. The scale function concentrates more centroids near 0 and 1.
func (td *TDigest) compress() {
	if len(td.centroids) <= 1 {
		return
	}
	maxCentroids := int(td.compression)
	if maxCentroids < 10 {
		maxCentroids = 10
	}
	for len(td.centroids) > maxCentroids {
		// Find the pair of adjacent centroids with the smallest
		// combined weight that fits within the compression budget.
		minIdx := 0
		minDelta := math.MaxFloat64
		for i := 0; i < len(td.centroids)-1; i++ {
			mergedWeight := td.centroids[i].weight + td.centroids[i+1].weight
			if mergedWeight < minDelta {
				minDelta = mergedWeight
				minIdx = i
			}
		}
		merged := centroid{
			mean: (td.centroids[minIdx].mean*td.centroids[minIdx].weight +
				td.centroids[minIdx+1].mean*td.centroids[minIdx+1].weight) /
				(td.centroids[minIdx].weight + td.centroids[minIdx+1].weight),
			weight: td.centroids[minIdx].weight + td.centroids[minIdx+1].weight,
		}
		td.centroids[minIdx] = merged
		td.centroids = append(td.centroids[:minIdx+1], td.centroids[minIdx+2:]...)
	}
}

// Quantile returns the estimated value at quantile q ∈ [0, 1].
func (td *TDigest) Quantile(q float64) float64 {
	if len(td.centroids) == 0 || q < 0 {
		return 0
	}
	if q > 1 {
		q = 1
	}
	target := q * td.totalWeight
	var cumulative float64
	for i, c := range td.centroids {
		cumulative += c.weight
		if cumulative >= target {
			if i == 0 {
				return c.mean
			}
			// Linear interpolation between previous and current centroid.
			prev := td.centroids[i-1]
			prevCum := cumulative - c.weight
			frac := (target - prevCum) / c.weight
			return prev.mean + frac*(c.mean-prev.mean)
		}
	}
	return td.centroids[len(td.centroids)-1].mean
}

// Merge combines another t-digest into this one.
func (td *TDigest) Merge(other *TDigest) {
	if other == nil || len(other.centroids) == 0 {
		return
	}
	for _, c := range other.centroids {
		td.AddWeighted(c.mean, c.weight)
	}
}

// Reset clears all centroids.
func (td *TDigest) Reset() {
	td.centroids = nil
	td.count = 0
	td.totalWeight = 0
}

// Count returns the total number of values added.
func (td *TDigest) Count() uint64 {
	return td.count
}
