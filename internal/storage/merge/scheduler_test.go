package merge

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/unisearch-db/unisearch/internal/storage"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// fakeShard is an in-memory ShardForMerge used to drive the scheduler
// without touching disk. It records every MergeSegments call so the
// tests can assert which segment set the scheduler picked.
type fakeShard struct {
	mu      sync.Mutex
	segs    []storage.SegmentStats
	merges  [][]types.SegmentID // each call's input segIDs
	merged  int                // number of MergeSegments calls
	failErr error              // when set, MergeSegments returns this
}

func (f *fakeShard) ListMergeCandidates() []storage.SegmentStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]storage.SegmentStats, len(f.segs))
	copy(cp, f.segs)
	return cp
}

func (f *fakeShard) MergeSegments(ctx context.Context, segIDs []types.SegmentID) (types.SegmentID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]types.SegmentID, len(segIDs))
	copy(cp, segIDs)
	f.merges = append(f.merges, cp)
	f.merged++
	if f.failErr != nil {
		return 0, f.failErr
	}
	// Simulate the post-merge segment set: drop the merged IDs and
	// append one consolidated segment whose min_seq is the minimum
	// among the merged set.
	merged := make(map[types.SegmentID]bool, len(segIDs))
	var minSeq types.SeqNo
	for _, id := range segIDs {
		merged[id] = true
	}
	first := true
	out := make([]storage.SegmentStats, 0, len(f.segs)-len(segIDs)+1)
	for _, st := range f.segs {
		if merged[st.ID] {
			if first || st.MinSeq < minSeq {
				minSeq = st.MinSeq
				first = false
			}
			continue
		}
		out = append(out, st)
	}
	if !first {
		out = append(out, storage.SegmentStats{
			ID:      9999,
			NumRows: 100,
			MinSeq:  minSeq,
		})
	}
	f.segs = out
	return 9999, nil
}

// makeSeg is a small helper for building SegmentStats.
func makeSeg(id types.SegmentID, rows, deleted int, bytes int64, minSeq types.SeqNo) storage.SegmentStats {
	return storage.SegmentStats{
		ID:          id,
		NumRows:     rows,
		DeletedRows: deleted,
		Bytes:       bytes,
		MinSeq:      minSeq,
	}
}

// TestR7_Scheduler_NewConfig_Defaults verifies that zero fields fall
// back to the design §7.1 defaults (5s interval, ≤2 concurrent,
// 0.30 delete rate, 10 segment count).
func TestR7_Scheduler_NewConfig_Defaults(t *testing.T) {
	cfg := NewConfig(Config{})
	if cfg.Interval != 5*time.Second {
		t.Fatalf("default interval = %v, want 5s", cfg.Interval)
	}
	if cfg.MaxConcurrent != 2 {
		t.Fatalf("default MaxConcurrent = %d, want 2", cfg.MaxConcurrent)
	}
	if cfg.DeleteRateThreshold != 0.30 {
		t.Fatalf("default DeleteRateThreshold = %v, want 0.30", cfg.DeleteRateThreshold)
	}
	if cfg.SegmentCountThreshold != 10 {
		t.Fatalf("default SegmentCountThreshold = %d, want 10", cfg.SegmentCountThreshold)
	}
}

// TestR7_Scheduler_PickCandidates_TooFewSegments: when fewer than 2
// segments exist, pickCandidates returns nil (no merge is due).
func TestR7_Scheduler_PickCandidates_TooFewSegments(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(1, 100, 0, 1024, 1),
	}}
	s := NewScheduler(shard, Config{})
	if got := s.PickCandidates(); got != nil {
		t.Fatalf("PickCandidates with 1 seg = %v, want nil", got)
	}

	shard.segs = nil
	if got := s.PickCandidates(); got != nil {
		t.Fatalf("PickCandidates with 0 seg = %v, want nil", got)
	}
}

// TestR7_Scheduler_PickCandidates_DeleteRate: rule 1 fires first —
// segments with delete rate >30% are picked. When two such segments
// exist, they are returned (the §7.1 "delete rate >30% priority").
func TestR7_Scheduler_PickCandidates_DeleteRate(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(1, 100, 50, 1024, 1), // 50% deleted → hot
		makeSeg(2, 100, 40, 2048, 2), // 40% deleted → hot
		makeSeg(3, 100, 0, 4096, 3),  // 0% deleted → cold
	}}
	s := NewScheduler(shard, Config{})
	got := s.PickCandidates()
	if len(got) != 2 {
		t.Fatalf("PickCandidates returned %d ids, want 2 (hot segments)", len(got))
	}
	// Sorted by byte size: seg 1 (1024) before seg 2 (2048).
	want := []types.SegmentID{1, 2}
	for i, id := range got {
		if id != want[i] {
			t.Fatalf("PickCandidates[%d] = %d, want %d (sorted by bytes asc)", i, id, want[i])
		}
	}
}

// TestR7_Scheduler_PickCandidates_DeleteRate_NoHot: when delete rates
// are at or below the threshold, rule 1 does not fire.
func TestR7_Scheduler_PickCandidates_DeleteRate_AtThreshold(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(1, 100, 30, 1024, 1), // exactly 30% → NOT hot (strict >)
		makeSeg(2, 100, 29, 2048, 2), // 29% → NOT hot
	}}
	s := NewScheduler(shard, Config{})
	if got := s.PickCandidates(); got != nil {
		t.Fatalf("PickCandidates at-threshold = %v, want nil", got)
	}
}

// TestR7_Scheduler_PickCandidates_CountThreshold: rule 2 fires when
// segment count >10; pickCandidates returns the top-4 smallest by
// bytes (the M0 "adjacent tier" approximation).
func TestR7_Scheduler_PickCandidates_CountThreshold(t *testing.T) {
	// 11 segments, all below the delete-rate threshold.
	segs := make([]storage.SegmentStats, 11)
	for i := range segs {
		// Bytes increase with id so top-4 smallest = ids 1..4 after sort.
		segs[i] = makeSeg(types.SegmentID(i+1), 100, 0, int64(1024*(i+1)), types.SeqNo(i+1))
	}
	shard := &fakeShard{segs: segs}
	s := NewScheduler(shard, Config{})
	got := s.PickCandidates()
	if len(got) != 4 {
		t.Fatalf("PickCandidates returned %d ids, want 4 (top-4 smallest)", len(got))
	}
	// Expect ids 1..4 in byte-ascending order.
	want := []types.SegmentID{1, 2, 3, 4}
	for i, id := range got {
		if id != want[i] {
			t.Fatalf("PickCandidates[%d] = %d, want %d (sorted by bytes asc)", i, id, want[i])
		}
	}
}

// TestR7_Scheduler_PickCandidates_BelowCountThreshold: with 10 or
// fewer segments (no hot), no merge fires.
func TestR7_Scheduler_PickCandidates_BelowCountThreshold(t *testing.T) {
	segs := make([]storage.SegmentStats, 10)
	for i := range segs {
		segs[i] = makeSeg(types.SegmentID(i+1), 100, 0, 1024, types.SeqNo(i+1))
	}
	shard := &fakeShard{segs: segs}
	s := NewScheduler(shard, Config{})
	if got := s.PickCandidates(); got != nil {
		t.Fatalf("PickCandidates with 10 segs (no hot) = %v, want nil", got)
	}
}

// TestR7_Scheduler_ForceMerge_All: with maxNumSegments=1 and >1
// segments, ForceMerge merges every segment into one. The input
// slice must be sorted by min_seq (§7.1 order preservation).
func TestR7_Scheduler_ForceMerge_All(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(5, 100, 0, 1024, 5),
		makeSeg(2, 100, 0, 2048, 2),
		makeSeg(9, 100, 0, 4096, 9),
	}}
	s := NewScheduler(shard, Config{})
	if err := s.ForceMerge(context.Background(), 1); err != nil {
		t.Fatalf("ForceMerge(1) failed: %v", err)
	}
	if shard.merged != 1 {
		t.Fatalf("MergeSegments call count = %d, want 1", shard.merged)
	}
	// Input must be sorted by min_seq → ids 2, 5, 9.
	got := shard.merges[0]
	want := []types.SegmentID{2, 5, 9}
	if len(got) != len(want) {
		t.Fatalf("ForceMerge input len = %d, want %d", len(got), len(want))
	}
	for i, id := range got {
		if id != want[i] {
			t.Fatalf("ForceMerge input[%d] = %d, want %d (sorted by min_seq)", i, id, want[i])
		}
	}
}

// TestR7_Scheduler_ForceMerge_NoOp: when the shard already has ≤
// maxNumSegments, ForceMerge returns nil without invoking
// MergeSegments.
func TestR7_Scheduler_ForceMerge_NoOp(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(1, 100, 0, 1024, 1),
	}}
	s := NewScheduler(shard, Config{})
	if err := s.ForceMerge(context.Background(), 1); err != nil {
		t.Fatalf("ForceMerge(1) on 1 seg failed: %v", err)
	}
	if shard.merged != 0 {
		t.Fatalf("MergeSegments call count = %d, want 0 (no-op)", shard.merged)
	}
}

// TestR7_Scheduler_ForceMerge_InvalidMax: maxNumSegments < 1 is an
// invalid request and returns an error.
func TestR7_Scheduler_ForceMerge_InvalidMax(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(1, 100, 0, 1024, 1),
	}}
	s := NewScheduler(shard, Config{})
	if err := s.ForceMerge(context.Background(), 0); err == nil {
		t.Fatalf("ForceMerge(0) returned nil, want error")
	}
}

// TestR7_Scheduler_ForceMerge_PropagatesError: when the shard's
// MergeSegments returns an error, ForceMerge surfaces it.
func TestR7_Scheduler_ForceMerge_PropagatesError(t *testing.T) {
	shard := &fakeShard{
		segs: []storage.SegmentStats{
			makeSeg(1, 100, 0, 1024, 1),
			makeSeg(2, 100, 0, 2048, 2),
		},
		failErr: errors.New("simulated merge failure"),
	}
	s := NewScheduler(shard, Config{})
	err := s.ForceMerge(context.Background(), 1)
	if err == nil || err.Error() != "simulated merge failure" {
		t.Fatalf("ForceMerge error = %v, want \"simulated merge failure\"", err)
	}
}

// TestR7_Scheduler_Tick_PicksHotAndMerges: tick() with hot segments
// invokes MergeSegments and decrements inflight after completion.
func TestR7_Scheduler_Tick_PicksHotAndMerges(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(1, 100, 50, 1024, 1), // 50% deleted → hot
		makeSeg(2, 100, 40, 2048, 2), // 40% deleted → hot
	}}
	s := NewScheduler(shard, Config{})
	s.tick(context.Background())
	if s.inflight != 0 {
		t.Fatalf("inflight after tick = %d, want 0 (decremented)", s.inflight)
	}
	if shard.merged != 1 {
		t.Fatalf("MergeSegments call count = %d, want 1", shard.merged)
	}
}

// TestR7_Scheduler_Tick_NoOpWhenFewCandidates: tick() with no merge
// due does not call MergeSegments.
func TestR7_Scheduler_Tick_NoOpWhenFewCandidates(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(1, 100, 0, 1024, 1),
		makeSeg(2, 100, 0, 2048, 2),
	}}
	s := NewScheduler(shard, Config{})
	s.tick(context.Background())
	if shard.merged != 0 {
		t.Fatalf("MergeSegments call count = %d, want 0", shard.merged)
	}
	if s.inflight != 0 {
		t.Fatalf("inflight after no-op tick = %d, want 0", s.inflight)
	}
}

// TestR7_Scheduler_Tick_RespectsInflightCap: when inflight is already
// at MaxConcurrent, tick() returns without dispatching a new merge.
func TestR7_Scheduler_Tick_RespectsInflightCap(t *testing.T) {
	shard := &fakeShard{segs: []storage.SegmentStats{
		makeSeg(1, 100, 50, 1024, 1),
		makeSeg(2, 100, 40, 2048, 2),
	}}
	s := NewScheduler(shard, Config{MaxConcurrent: 1})
	// Pre-saturate the inflight slot without dispatching a merge.
	s.mu.Lock()
	s.inflight = 1
	s.mu.Unlock()
	s.tick(context.Background())
	if shard.merged != 0 {
		t.Fatalf("MergeSegments call count = %d, want 0 (inflight capped)", shard.merged)
	}
	// The pre-set inflight slot should not be touched by the early-return path.
	s.mu.Lock()
	got := s.inflight
	s.mu.Unlock()
	if got != 1 {
		t.Fatalf("inflight after capped tick = %d, want 1 (untouched)", got)
	}
}

// TestR7_Scheduler_TopNByBytes_SortsCopy: topNByBytes must sort a copy
// and leave the input slice order unchanged. It also caps n at len(segs).
func TestR7_Scheduler_TopNByBytes_SortsCopy(t *testing.T) {
	in := []storage.SegmentStats{
		makeSeg(3, 100, 0, 3072, 3),
		makeSeg(1, 100, 0, 1024, 1),
		makeSeg(2, 100, 0, 2048, 2),
	}
	got := topNByBytes(in, 2)
	want := []types.SegmentID{1, 2}
	for i, id := range got {
		if id != want[i] {
			t.Fatalf("topNByBytes[%d] = %d, want %d", i, id, want[i])
		}
	}
	// Input order must be unchanged.
	for i, wantID := range []types.SegmentID{3, 1, 2} {
		if in[i].ID != wantID {
			t.Fatalf("input[%d].ID = %d, want %d (sort must not mutate caller)", i, in[i].ID, wantID)
		}
	}
	// n > len(segs) is capped.
	if got := topNByBytes(in, 10); len(got) != 3 {
		t.Fatalf("topNByBytes(n=10) returned %d, want 3 (capped to len)", len(got))
	}
}

// TestR7_Scheduler_Run_StopsOnContextCancel: Run() blocks until the
// context is canceled; this test ensures it actually returns.
func TestR7_Scheduler_Run_StopsOnContextCancel(t *testing.T) {
	shard := &fakeShard{segs: nil}
	s := NewScheduler(shard, Config{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after context cancel")
	}
}

// TestR7_Scheduler_PickCandidates_Rule1WinsOverRule2: when both rule
// 1 and rule 2 would fire (>=2 hot segments AND segment count >10),
// rule 1 takes precedence — only the hot segments are returned.
func TestR7_Scheduler_PickCandidates_Rule1WinsOverRule2(t *testing.T) {
	segs := make([]storage.SegmentStats, 12)
	for i := range segs {
		segs[i] = makeSeg(types.SegmentID(i+1), 100, 0, int64(1024*(i+1)), types.SeqNo(i+1))
	}
	// Make two of them hot.
	segs[0].DeletedRows = 50 // seg 1: 50% deleted
	segs[1].DeletedRows = 60 // seg 2: 60% deleted
	shard := &fakeShard{segs: segs}
	s := NewScheduler(shard, Config{})
	got := s.PickCandidates()
	// Rule 1 wins: only the two hot segments, not top-4 by bytes.
	if len(got) != 2 {
		t.Fatalf("PickCandidates returned %d ids, want 2 (rule 1 wins over rule 2)", len(got))
	}
	want := []types.SegmentID{1, 2}
	for i, id := range got {
		if id != want[i] {
			t.Fatalf("PickCandidates[%d] = %d, want %d", i, id, want[i])
		}
	}
}

// ensure sort is referenced even when no tests above use it directly
// (NewConfig/ForceMerge use sort indirectly via sort.SliceStable).
var _ = sort.SliceStable
