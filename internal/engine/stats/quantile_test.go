package stats

import (
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// P² (P-square) online quantile estimation tests.
// @spec: ./archive/2026-10-23-qdrant-features-import/02_spec.md#2.11
// ---------------------------------------------------------------------------

func TestPSquare_Median(t *testing.T) {
	// Estimate the median (p=0.5) of a uniform distribution [0, 100).
	// With enough samples, the estimate should be close to 50.
	e := NewPSquareEstimator(0.5)
	rng := rand.New(rand.NewSource(42))

	const n = 100000
	for i := 0; i < n; i++ {
		e.Add(rng.Float64() * 100)
	}

	est := e.Quantile()
	assert.True(t, math.Abs(est-50) < 5.0,
		"median estimate %v should be close to 50", est)
}

func TestPSquare_P90(t *testing.T) {
	// Estimate the 90th percentile of a uniform distribution [0, 100).
	// Expected: ~90
	e := NewPSquareEstimator(0.9)
	rng := rand.New(rand.NewSource(42))

	for i := 0; i < 100000; i++ {
		e.Add(rng.Float64() * 100)
	}

	est := e.Quantile()
	assert.True(t, math.Abs(est-90) < 5.0,
		"p90 estimate %v should be close to 90", est)
}

func TestPSquare_P1_OutlierRobust(t *testing.T) {
	// P1 of a distribution with outliers. Most data in [0, 1],
	// with a few extreme values. P1 should be near 0.
	e := NewPSquareEstimator(0.01)
	rng := rand.New(rand.NewSource(42))

	for i := 0; i < 100000; i++ {
		val := rng.Float64()
		if i%1000 == 0 {
			val = 10000.0 // outliers
		}
		e.Add(val)
	}

	est := e.Quantile()
	assert.True(t, est < 0.1,
		"p1 estimate %v should be near 0 (outlier-robust)", est)
}

func TestPSquare_P99_OutlierRobust(t *testing.T) {
	// P99 of the same distribution. P99 should be well below the
	// outlier value (10000), close to 1.0.
	e := NewPSquareEstimator(0.99)
	rng := rand.New(rand.NewSource(42))

	for i := 0; i < 100000; i++ {
		val := rng.Float64()
		if i%1000 == 0 {
			val = 10000.0
		}
		e.Add(val)
	}

	est := e.Quantile()
	assert.True(t, est < 100.0,
		"p99 estimate %v should be well below 10000 (outlier-robust)", est)
}

func TestPSquare_ConstantStream(t *testing.T) {
	// All identical values → quantile = that value.
	e := NewPSquareEstimator(0.5)
	for i := 0; i < 1000; i++ {
		e.Add(42.0)
	}

	est := e.Quantile()
	assert.True(t, math.Abs(est-42.0) < 1e-6,
		"constant stream quantile should be 42, got %v", est)
}

func TestPSquare_FewSamples(t *testing.T) {
	// With very few samples, the estimator should still return
	// a reasonable value (the last added or close to it).
	e := NewPSquareEstimator(0.5)
	e.Add(10)
	e.Add(20)
	e.Add(30)

	est := e.Quantile()
	// With 3 samples, the median should be 20.
	assert.True(t, math.Abs(est-20) < 5.0,
		"3-sample median should be near 20, got %v", est)
}

func TestPSquare_SortedInput(t *testing.T) {
	// Sorted input [0, 1, 2, ..., 999] → median ≈ 499.5
	e := NewPSquareEstimator(0.5)
	for i := 0; i < 1000; i++ {
		e.Add(float64(i))
	}

	est := e.Quantile()
	assert.True(t, math.Abs(est-499.5) < 20.0,
		"sorted input median should be near 499.5, got %v", est)
}

func TestPSquare_CompareWithSort(t *testing.T) {
	// Compare P² estimate with exact quantile from sorted data.
	rng := rand.New(rand.NewSource(123))
	data := make([]float64, 10000)
	e := NewPSquareEstimator(0.75)

	for i := range data {
		v := rng.NormFloat64()*10 + 50 // mean=50, stddev=10
		data[i] = v
		e.Add(v)
	}

	sort.Float64s(data)
	idx := int(0.75 * float64(len(data)-1))
	exact := data[idx]
	est := e.Quantile()

	// Allow 10% relative error for the P² algorithm.
	relErr := math.Abs(est-exact) / math.Abs(exact)
	assert.True(t, relErr < 0.1,
		"p75 estimate %v should be within 10%% of exact %v (rel err %v)", est, exact, relErr)
}

func TestPSquare_AddMultiple(t *testing.T) {
	// AddMultiple should produce the same result as individual Adds.
	vals := []float64{5, 3, 8, 1, 9, 2, 7, 4, 6, 0}

	e1 := NewPSquareEstimator(0.5)
	for _, v := range vals {
		e1.Add(v)
	}

	e2 := NewPSquareEstimator(0.5)
	e2.AddMultiple(vals)

	// Both should produce the same estimate.
	require.InDelta(t, e1.Quantile(), e2.Quantile(), 1e-10,
		"Add and AddMultiple should produce same result")
}

func TestPSquare_Count(t *testing.T) {
	e := NewPSquareEstimator(0.5)
	for i := 0; i < 500; i++ {
		e.Add(float64(i))
	}
	assert.Equal(t, 500, e.Count())
}

func TestPSquare_Reset(t *testing.T) {
	e := NewPSquareEstimator(0.5)
	for i := 0; i < 100; i++ {
		e.Add(float64(i))
	}
	require.Equal(t, 100, e.Count())

	e.Reset()
	assert.Equal(t, 0, e.Count())

	// Should work after reset.
	e.Add(42)
	assert.Equal(t, 1, e.Count())
	assert.True(t, math.Abs(e.Quantile()-42) < 1e-6)
}
