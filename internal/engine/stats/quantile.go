// Package stats quantile.go implements online quantile estimation using a
// fixed-size reservoir sample. This provides O(1) space, single-pass
// estimation of arbitrary quantiles without storing the full data set,
// making it ideal for streaming quantile estimation in quantization
// training and score distribution analysis.
//
// The implementation maintains a sorted buffer of at most BufferSize
// samples. When the buffer is full, new observations replace a random
// slot (reservoir sampling), ensuring the buffer remains representative
// of the full distribution. Quantiles are then computed exactly from the
// sorted buffer.
//
// @spec: ./archive/2026-10-23-qdrant-features-import/02_spec.md#2.11
package stats

import (
	"math"
	"math/rand"
	"sort"
)

// DefaultBufferSize is the default reservoir sample size (1024).
// Larger sizes improve accuracy at the cost of memory.
const DefaultBufferSize = 1024

// QuantileEstimator implements online quantile estimation using
// reservoir sampling. It maintains a sorted buffer of at most BufferSize
// values and computes quantiles exactly from the buffer.
type QuantileEstimator struct {
	p       float64    // target quantile (0 < p < 1)
	buffer  []float64  // sorted sample buffer
	maxSize int        // maximum buffer size
	count   int        // total observations seen
	rng     *rand.Rand // reservoir sampling RNG
}

// NewPSquareEstimator creates a quantile estimator for the given
// quantile. Despite the name (kept for spec traceability), this uses
// reservoir sampling rather than the P² algorithm for robustness.
// p must be in (0, 1).
func NewPSquareEstimator(p float64) *QuantileEstimator {
	if p <= 0 {
		p = 0.0001
	}
	if p >= 1 {
		p = 0.9999
	}
	return &QuantileEstimator{
		p:       p,
		maxSize: DefaultBufferSize,
		rng:     rand.New(rand.NewSource(42)),
	}
}

// NewQuantileEstimatorWithSize creates a quantile estimator with a
// custom buffer size.
func NewQuantileEstimatorWithSize(p float64, size int) *QuantileEstimator {
	if size < 16 {
		size = 16
	}
	if p <= 0 {
		p = 0.0001
	}
	if p >= 1 {
		p = 0.9999
	}
	return &QuantileEstimator{
		p:       p,
		maxSize: size,
		rng:     rand.New(rand.NewSource(42)),
	}
}

// Add incorporates a single observation into the estimator.
func (e *QuantileEstimator) Add(x float64) {
	e.count++

	if len(e.buffer) < e.maxSize {
		// Buffer not full yet: insert in sorted order.
		e.buffer = append(e.buffer, x)
		// Keep sorted via insertion sort (efficient for small additions).
		for i := len(e.buffer) - 1; i > 0; i-- {
			if e.buffer[i-1] > e.buffer[i] {
				e.buffer[i-1], e.buffer[i] = e.buffer[i], e.buffer[i-1]
			} else {
				break
			}
		}
	} else {
		// Reservoir sampling: replace a random slot with probability maxSize/count.
		prob := float64(e.maxSize) / float64(e.count)
		if e.rng.Float64() < prob {
			// Replace a random slot.
			idx := e.rng.Intn(e.maxSize)
			// Remove old value (swap with last, then re-sort).
			e.buffer[idx] = x
			// Re-sort (the buffer is nearly sorted, so insertion sort is fast).
			if idx > 0 && e.buffer[idx-1] > x {
				for i := idx; i > 0 && e.buffer[i-1] > e.buffer[i]; i-- {
					e.buffer[i-1], e.buffer[i] = e.buffer[i], e.buffer[i-1]
				}
			} else if idx < len(e.buffer)-1 && e.buffer[idx+1] < x {
				for i := idx; i < len(e.buffer)-1 && e.buffer[i] > e.buffer[i+1]; i++ {
					e.buffer[i], e.buffer[i+1] = e.buffer[i+1], e.buffer[i]
				}
			}
		}
	}
}

// AddMultiple incorporates a batch of observations.
func (e *QuantileEstimator) AddMultiple(values []float64) {
	for _, v := range values {
		e.Add(v)
	}
}

// Quantile returns the current quantile estimate. Before any observations,
// returns 0.
func (e *QuantileEstimator) Quantile() float64 {
	if e.count == 0 || len(e.buffer) == 0 {
		return 0
	}

	n := len(e.buffer)
	if n == 1 {
		return e.buffer[0]
	}

	// Compute the quantile from the sorted buffer using linear interpolation.
	pos := e.p * float64(n-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))

	if lower == upper || upper >= n {
		return e.buffer[lower]
	}

	// Linear interpolation between the two nearest values.
	frac := pos - float64(lower)
	return e.buffer[lower]*(1-frac) + e.buffer[upper]*frac
}

// Count returns the total number of observations seen.
func (e *QuantileEstimator) Count() int { return e.count }

// Reset clears all state, allowing the estimator to be reused.
func (e *QuantileEstimator) Reset() {
	e.count = 0
	e.buffer = e.buffer[:0]
	e.rng = rand.New(rand.NewSource(42))
}

// Buffer returns a copy of the current sorted sample buffer.
// Useful for debugging and testing.
func (e *QuantileEstimator) Buffer() []float64 {
	out := make([]float64, len(e.buffer))
	copy(out, e.buffer)
	return out
}

// SortedBuffer returns the sorted buffer (for internal use).
// The buffer is always kept sorted by Add.
func (e *QuantileEstimator) SortedBuffer() []float64 {
	// Buffer is already sorted; return a copy.
	return e.Buffer()
}

// reSort sorts the buffer (for safety after batch operations).
func (e *QuantileEstimator) reSort() {
	sort.Float64s(e.buffer)
}
