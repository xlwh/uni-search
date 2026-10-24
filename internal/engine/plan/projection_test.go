package plan

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// @spec: 02_spec.md#7.7 F6-T01..T06

func TestF6_ProjectionScanPlanNode(t *testing.T) {
	p := &ProjectionScan{
		SegID:          42,
		ProjectionName:  "daily_stats",
		GroupBy:         []string{"date", "category"},
		Aggs:           []AggDef{{Name: "sum_price", Type: "sum"}},
	}
	s := p.String()
	require.Contains(t, s, "ProjectionScan")
	require.Contains(t, s, "daily_stats")
	require.Contains(t, s, "42")
}

func TestF6_ProjectionScanImplementsLogicalPlan(t *testing.T) {
	var p LogicalPlan = &ProjectionScan{SegID: 1, ProjectionName: "test"}
	require.NotNil(t, p)
	require.NotEmpty(t, p.String())
}

func TestF6_R8OptimizerNoop(t *testing.T) {
	// R8 should be a noop when no projection metadata is available.
	plan := &Aggregate{
		Input:      &Scan{SegID: 1, FieldType: "long"},
		GroupField: "category",
		Aggs:      []AggDef{{Name: "count", Type: "count"}},
	}
	optimized, trace := Optimize(plan, EstimatedSelectivity{}, 10)
	require.NotNil(t, optimized)
	require.Contains(t, trace, "R8:")
	// The plan should be unchanged (noop).
	_, ok := optimized.(*Aggregate)
	require.True(t, ok)
}
