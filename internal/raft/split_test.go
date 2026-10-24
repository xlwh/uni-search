// Package raft split_test.go tests the online split protocol with
// real data path hooks.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.6
package raft

import (
	"testing"

	"github.com/RoaringBitmap/roaring"
	"github.com/stretchr/testify/assert"

	"github.com/unisearch-db/unisearch/pkg/types"
)

func TestInitiateSplit(t *testing.T) {
	sc := NewSplitCoordinator()
	parentID := types.ShardID(1)
	childIDs := []types.ShardID{2, 3}

	err := sc.InitiateSplit(parentID, childIDs, 1, 2, 100)
	assert.NoError(t, err)

	state, ok := sc.GetState(parentID)
	assert.True(t, ok)
	assert.Equal(t, SplitPhaseInitiated, state.Phase)
	assert.Equal(t, uint64(1), state.OldGen)
	assert.Equal(t, uint64(2), state.NewGen)
	assert.True(t, state.IsDualRead())
}

func TestInitiateSplitDuplicate(t *testing.T) {
	sc := NewSplitCoordinator()
	parentID := types.ShardID(1)

	_ = sc.InitiateSplit(parentID, []types.ShardID{2, 3}, 1, 2, 100)
	err := sc.InitiateSplit(parentID, []types.ShardID{4, 5}, 1, 3, 200)
	assert.Error(t, err)
}

func TestAdvancePhaseWithHooks(t *testing.T) {
	sc := NewSplitCoordinator()
	parentID := types.ShardID(1)
	childIDs := []types.ShardID{2, 3}

	freezeCalled := false
	createChildCalled := 0
	replayWALCalled := 0
	switchCalled := false
	decommissionCalled := false

	sc.SetHooks(
		func(id types.ShardID) error { freezeCalled = true; return nil },
		func(id types.ShardID) (*roaring.Bitmap, error) { return roaring.New(), nil }, // getBitmap
		func(id types.ShardID, segs []types.SegmentID, bm *roaring.Bitmap) error { createChildCalled++; return nil },
		func(parent, child types.ShardID, cursor int64) (int64, error) { replayWALCalled++; return cursor + 100, nil },
		func(id types.ShardID, gen uint64) error { switchCalled = true; return nil },
		func(id types.ShardID) error { decommissionCalled = true; return nil },
	)

	_ = sc.InitiateSplit(parentID, childIDs, 1, 2, 100)

	// Phase 1 → 2: freeze + create children
	err := sc.AdvancePhase(parentID)
	assert.NoError(t, err)
	assert.True(t, freezeCalled)
	assert.Equal(t, 2, createChildCalled)

	// Phase 2 → 3: replay WAL
	err = sc.AdvancePhase(parentID)
	assert.NoError(t, err)
	assert.Equal(t, 2, replayWALCalled)

	// Phase 3 → 4: switch route + decommission
	err = sc.AdvancePhase(parentID)
	assert.NoError(t, err)
	assert.True(t, switchCalled)
	assert.True(t, decommissionCalled)

	state, _ := sc.GetState(parentID)
	assert.Equal(t, SplitPhaseComplete, state.Phase)
}

func TestAdvancePhaseAlreadyComplete(t *testing.T) {
	sc := NewSplitCoordinator()
	parentID := types.ShardID(1)

	_ = sc.InitiateSplit(parentID, []types.ShardID{2}, 1, 2, 100)
	_ = sc.AdvancePhase(parentID)
	_ = sc.AdvancePhase(parentID)
	_ = sc.AdvancePhase(parentID)

	err := sc.AdvancePhase(parentID)
	assert.Error(t, err)
}

func TestRouteDoc(t *testing.T) {
	state := &SplitState{
		ParentID: types.ShardID(1),
		ChildIDs: []types.ShardID{2, 3},
	}

	// Same doc ID should always route to same child (deterministic hash)
	child1 := state.RouteDoc("doc-001")
	child2 := state.RouteDoc("doc-001")
	assert.Equal(t, child1, child2)

	// Different doc IDs may route to different children
	child3 := state.RouteDoc("doc-002")
	assert.True(t, child3 == types.ShardID(2) || child3 == types.ShardID(3))
}

func TestCompleteSplit(t *testing.T) {
	sc := NewSplitCoordinator()
	parentID := types.ShardID(1)

	_ = sc.InitiateSplit(parentID, []types.ShardID{2}, 1, 2, 100)
	_ = sc.AdvancePhase(parentID)
	_ = sc.AdvancePhase(parentID)

	// Should be in SplitPhaseCatching now
	err := sc.CompleteSplit(parentID)
	assert.NoError(t, err)

	// Should be removed from active splits
	_, ok := sc.GetState(parentID)
	assert.False(t, ok)
}
