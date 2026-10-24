// Package storage crosslink_merge_test.go tests the Cross-Link merge.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.7
package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPlanMerge(t *testing.T) {
	merger := NewCrossLinkMerger()

	// Need at least 2 segments
	_, err := merger.PlanMerge([]*SegmentEntry{})
	assert.Error(t, err)

	_, err = merger.PlanMerge([]*SegmentEntry{{}})
	assert.Error(t, err)
}

func TestExecuteMergeEmpty(t *testing.T) {
	merger := NewCrossLinkMerger()
	plan := &MergePlan{}

	_, err := merger.ExecuteMerge(context.Background(), plan, []*SegmentEntry{})
	assert.Error(t, err)
}

func TestRebuildCrossLinks(t *testing.T) {
	// After merge, all internal edges are intra-segment.
	// CrossLinks should be empty when all sources share the target SegmentID.
	refs := RebuildCrossLinks(1, nil)
	assert.NotNil(t, refs)
	assert.Empty(t, refs)
}
