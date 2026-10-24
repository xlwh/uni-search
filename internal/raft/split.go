package raft

import (
	"fmt"
	"sync"

	"github.com/RoaringBitmap/roaring"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// SplitCoordinator manages the online split protocol (D4: Gen dual-
// version + bitmap inheritance). Split is a multi-phase operation:
// 1. Meta Raft issues SplitShard command (route table dual-version gen=N+1)
// 2. Parent shard enters dual-read frozen generation (segments shared, not rewritten)
// 3. Two child shard Raft groups created (inherit parent segment refs + DeleteBitmap)
// 4. Atomic gen switch (new writes go to children)
// 5. Children pull incremental WAL from parent to catch up
// 6. Parent shard decommissioned (segment ref count transferred)
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.6
type SplitCoordinator struct {
	mu           sync.Mutex
	activeSplits map[types.ShardID]*SplitState

	// Hooks for shard operations (injected by the cluster manager).
	freezeShard   func(types.ShardID) error              // freeze parent writes
	getBitmap     func(types.ShardID) (*roaring.Bitmap, error)  // get parent's DeleteBitmap
	createChild   func(types.ShardID, []types.SegmentID, *roaring.Bitmap) error  // create child with inherited segments
	replayWAL     func(types.ShardID, types.ShardID, int64) (int64, error)  // replay WAL from parent to child
	switchRoute   func(types.ShardID, uint64) error     // atomic route gen switch
	decommission  func(types.ShardID) error              // decommission parent
}

// SplitState tracks one in-progress split operation.
type SplitState struct {
	ParentID      types.ShardID
	ChildIDs      []types.ShardID
	OldGen        uint64
	NewGen        uint64
	SplitKey      uint32   // hash modulo split point
	Phase         SplitPhase
	// Tracking fields populated during split
	ParentSegs    []types.SegmentID  // parent segment IDs (for inheritance)
	ChildBitmaps  []*roaring.Bitmap   // per-child DeleteBitmap copies
	WALCursor     int64              // WAL replay cursor position
	Frozen        bool               // parent frozen flag
	ChildrenReady bool               // children created flag
}

// SplitPhase enumerates the split protocol phases.
type SplitPhase int

const (
	SplitPhaseInitiated SplitPhase = iota  // SplitShard command applied to meta
	SplitPhaseDualRead                     // Parent in dual-read, children created
	SplitPhaseCatching                     // Children pulling incremental WAL
	SplitPhaseComplete                     // Parent decommissioned
)

// NewSplitCoordinator creates a split coordinator.
func NewSplitCoordinator() *SplitCoordinator {
	return &SplitCoordinator{
		activeSplits: make(map[types.ShardID]*SplitState),
	}
}

// SetHooks injects shard operation callbacks for real data path execution.
func (sc *SplitCoordinator) SetHooks(
	freeze func(types.ShardID) error,
	getBitmap func(types.ShardID) (*roaring.Bitmap, error),
	create func(types.ShardID, []types.SegmentID, *roaring.Bitmap) error,
	replay func(types.ShardID, types.ShardID, int64) (int64, error),
	switchFn func(types.ShardID, uint64) error,
	decomm func(types.ShardID) error,
) {
	sc.freezeShard = freeze
	sc.getBitmap = getBitmap
	sc.createChild = create
	sc.replayWAL = replay
	sc.switchRoute = switchFn
	sc.decommission = decomm
}

// InitiateSplit starts a new split operation.
func (sc *SplitCoordinator) InitiateSplit(parentID types.ShardID, childIDs []types.ShardID, oldGen, newGen uint64, splitKey uint32) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if _, exists := sc.activeSplits[parentID]; exists {
		return fmt.Errorf("split: shard %d already has active split", parentID)
	}
	sc.activeSplits[parentID] = &SplitState{
		ParentID: parentID,
		ChildIDs: childIDs,
		OldGen:   oldGen,
		NewGen:   newGen,
		SplitKey: splitKey,
		Phase:    SplitPhaseInitiated,
	}
	return nil
}

// AdvancePhase moves the split to the next phase, executing real data
// migration logic at each transition.
func (sc *SplitCoordinator) AdvancePhase(parentID types.ShardID) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	state, ok := sc.activeSplits[parentID]
	if !ok {
		return fmt.Errorf("split: no active split for shard %d", parentID)
	}
	if state.Phase >= SplitPhaseComplete {
		return fmt.Errorf("split: already complete for shard %d", parentID)
	}

	switch state.Phase {
	case SplitPhaseInitiated:
		// Phase 1 → 2: Enter dual-read, freeze parent writes
		if sc.freezeShard != nil {
			if err := sc.freezeShard(state.ParentID); err != nil {
				return fmt.Errorf("split: freeze parent %d: %w", state.ParentID, err)
			}
		}
		state.Frozen = true

		// Create child shards with inherited segment references + DeleteBitmap copies
		if sc.createChild != nil {
			// Get parent's actual DeleteBitmap for inheritance.
			var parentBitmap *roaring.Bitmap
			if sc.getBitmap != nil {
				bm, err := sc.getBitmap(state.ParentID)
				if err != nil {
					return fmt.Errorf("split: get parent bitmap: %w", err)
				}
				parentBitmap = bm
			} else {
				parentBitmap = roaring.New() // fallback: empty bitmap
			}
			for _, childID := range state.ChildIDs {
				// Each child gets an independent copy of the parent's DeleteBitmap
				// for future divergence (design 05 §3.2).
				if err := sc.createChild(childID, state.ParentSegs, parentBitmap.Clone()); err != nil {
					return fmt.Errorf("split: create child %d: %w", childID, err)
				}
			}
		}
		state.ChildrenReady = true

	case SplitPhaseDualRead:
		// Phase 2 → 3: Replay incremental WAL from parent to children
		if sc.replayWAL != nil {
			for _, childID := range state.ChildIDs {
				newCursor, err := sc.replayWAL(state.ParentID, childID, state.WALCursor)
				if err != nil {
					return fmt.Errorf("split: replay WAL to child %d: %w", childID, err)
				}
				state.WALCursor = newCursor
			}
		}

	case SplitPhaseCatching:
		// Phase 3 → 4: Atomic gen switch + decommission parent
		if sc.switchRoute != nil {
			if err := sc.switchRoute(state.ParentID, state.NewGen); err != nil {
				return fmt.Errorf("split: switch route gen: %w", err)
			}
		}
		if sc.decommission != nil {
			if err := sc.decommission(state.ParentID); err != nil {
				return fmt.Errorf("split: decommission parent: %w", err)
			}
		}
	}

	state.Phase++
	return nil
}

// GetState returns the split state for a shard.
func (sc *SplitCoordinator) GetState(parentID types.ShardID) (*SplitState, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	state, ok := sc.activeSplits[parentID]
	return state, ok
}

// CompleteSplit finalizes the split and removes it from active tracking.
func (sc *SplitCoordinator) CompleteSplit(parentID types.ShardID) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	state, ok := sc.activeSplits[parentID]
	if !ok {
		return fmt.Errorf("split: no active split for shard %d", parentID)
	}
	if state.Phase != SplitPhaseCatching {
		return fmt.Errorf("split: cannot complete from phase %d", state.Phase)
	}
	state.Phase = SplitPhaseComplete
	delete(sc.activeSplits, parentID)
	return nil
}

// RouteDoc determines which child shard a document belongs to after split.
func (s *SplitState) RouteDoc(docID string) types.ShardID {
	if len(s.ChildIDs) == 0 {
		return s.ParentID
	}
	h := uint32(0)
	for _, b := range []byte(docID) {
		h = h*31 + uint32(b)
	}
	childIdx := int(h % uint32(len(s.ChildIDs)))
	return s.ChildIDs[childIdx]
}

// IsDualRead returns true during the dual-read window (phases 1-2).
func (s *SplitState) IsDualRead() bool {
	return s.Phase == SplitPhaseInitiated || s.Phase == SplitPhaseDualRead
}
