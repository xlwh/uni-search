package agg

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.3

func TestSumAgg(t *testing.T) {
	a, _ := NewAggregator("sum", nil)
	a.Collect(10.0)
	a.Collect(20.5)
	a.Collect(nil) // ignored
	a.Collect(5)
	assert.Equal(t, 35.5, a.Result())
}

func TestCountAgg(t *testing.T) {
	a, _ := NewAggregator("count", nil)
	a.Collect(10)
	a.Collect("hello")
	a.Collect(nil) // not counted
	a.Collect(3.14)
	assert.Equal(t, float64(3), a.Result())
}

func TestMinAgg(t *testing.T) {
	a, _ := NewAggregator("min", nil)
	a.Collect(10.0)
	a.Collect(3.0)
	a.Collect(7.0)
	a.Collect(nil)
	assert.Equal(t, 3.0, a.Result())
}

func TestMaxAgg(t *testing.T) {
	a, _ := NewAggregator("max", nil)
	a.Collect(10.0)
	a.Collect(30.0)
	a.Collect(7.0)
	a.Collect(nil)
	assert.Equal(t, 30.0, a.Result())
}

func TestAvgAgg(t *testing.T) {
	a, _ := NewAggregator("avg", nil)
	a.Collect(10.0)
	a.Collect(20.0)
	a.Collect(30.0)
	a.Collect(nil) // ignored
	assert.Equal(t, 20.0, a.Result())
}

func TestStatsAgg(t *testing.T) {
	a, _ := NewAggregator("stats", nil)
	a.Collect(10.0)
	a.Collect(20.0)
	a.Collect(30.0)
	result := a.Result().(map[string]any)
	assert.Equal(t, float64(3), result["count"])
	assert.Equal(t, 60.0, result["sum"])
	assert.Equal(t, 10.0, result["min"])
	assert.Equal(t, 30.0, result["max"])
	assert.Equal(t, 20.0, result["avg"])
}

func TestValueCountAgg(t *testing.T) {
	a, _ := NewAggregator("value_count", nil)
	a.Collect(10)
	a.Collect(nil) // counted (unlike count)
	a.Collect(30)
	assert.Equal(t, float64(3), a.Result())
}

func TestCardinalityAgg(t *testing.T) {
	a, _ := NewAggregator("cardinality", nil)
	a.Collect("hello")
	a.Collect("world")
	a.Collect("hello") // duplicate
	a.Collect("foo")
	result := a.Result().(float64)
	assert.Equal(t, float64(3), result, "cardinality should be 3 distinct values")
}

func TestGroupAggregator(t *testing.T) {
	defs := []AggDef{
		{Name: "total", Type: "sum"},
		{Name: "cnt", Type: "count"},
	}
	g := NewGroupAggregator("category", defs)

	// Group "a": values 10, 20
	g.Collect("a", map[string]any{"total": 10.0, "cnt": 10.0})
	g.Collect("a", map[string]any{"total": 20.0, "cnt": 20.0})

	// Group "b": values 5, 15, 25
	g.Collect("b", map[string]any{"total": 5.0, "cnt": 5.0})
	g.Collect("b", map[string]any{"total": 15.0, "cnt": 15.0})
	g.Collect("b", map[string]any{"total": 25.0, "cnt": 25.0})

	buckets := g.Buckets()
	require.Len(t, buckets, 2, "should have 2 groups")

	// Group "b" has 3 docs, group "a" has 2 docs → "b" comes first.
	assert.Equal(t, "b", buckets[0].Key)
	assert.Equal(t, "a", buckets[1].Key)

	// Check sub-aggregations for group "b".
	bSubAggs := buckets[0].SubAggs
	assert.Equal(t, 45.0, bSubAggs["total"], "sum for group b")
}

func TestAggReset(t *testing.T) {
	a, _ := NewAggregator("sum", nil)
	a.Collect(10.0)
	a.Collect(20.0)
	assert.Equal(t, 30.0, a.Result())
	a.Reset()
	assert.Equal(t, 0.0, a.Result())
}

func TestAggUnknownType(t *testing.T) {
	_, err := NewAggregator("nonexistent", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown aggregation type")
}
