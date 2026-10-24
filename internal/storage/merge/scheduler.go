// Package merge implements the size-tiered segment merge scheduler
// (design 01-storage-engine.md §7.1). It evaluates candidate segments
// per shard every 5 seconds, picks a set to merge (delete rate >30%
// priority, segment count >10 threshold, or _forcemerge target), and
// drives the merge through the storage.Shard contract.
//
// The scheduler lives in a sub-package so storage internals stay
// private; the storage.Shard type implements the ShardForMerge
// interface declared here.
package merge

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/unisearch-db/unisearch/internal/storage"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// ShardForMerge is the narrow contract storage.Shard exposes to the
// merge scheduler. Storage implements it; merge consumes it.
type ShardForMerge interface {
	// ListMergeCandidates returns the per-segment stats the scheduler
	// needs to evaluate the §7.1 rules. The returned slice is in
	// segment-id order; the scheduler may re-sort it by min_seq.
	ListMergeCandidates() []storage.SegmentStats
	// MergeSegments merges the given segments into one and atomically
	// swaps the SegmentSet. Returns the new segment ID.
	MergeSegments(ctx context.Context, segIDs []types.SegmentID) (types.SegmentID, error)
}

// Config carries the §7.1 thresholds. Zero fields fall back to the
// design defaults via NewConfig.
type Config struct {
	// Interval is the scheduler's evaluation period (5s default).
	Interval time.Duration
	// MaxConcurrent caps in-flight merges per shard (≤2, design §7.1).
	MaxConcurrent int
	// DeleteRateThreshold is the deletion fraction above which a
	// segment is prioritized for merge (0.30 default).
	DeleteRateThreshold float64
	// SegmentCountThreshold triggers an adjacent-tier merge when the
	// shard exceeds this many segments (10 default).
	SegmentCountThreshold int
	// F4: TTL interval (parsed from IndexSettings.TTL). When non-zero,
	// segments whose MaxTimestamp is older than now - TTLInterval are
	// prioritized for merge. @spec: 02_spec.md#5.4
	TTLInterval time.Duration
	// F4: TTL column name (e.g. "@timestamp"). Empty = no TTL.
	TTLColumn string
}

// NewConfig returns cfg with zero fields replaced by the design §7.1
// defaults.
func NewConfig(cfg Config) Config {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2
	}
	if cfg.DeleteRateThreshold <= 0 {
		cfg.DeleteRateThreshold = 0.30
	}
	if cfg.SegmentCountThreshold <= 0 {
		cfg.SegmentCountThreshold = 10
	}
	return cfg
}

// Scheduler evaluates a shard's segment set every Interval and merges
// per the §7.1 rules. It runs at most MaxConcurrent merges in flight
// per shard; the design's "concurrency ≤ 2/shard" cap.
type Scheduler struct {
	shard ShardForMerge
	cfg   Config

	inflight int
	mu       sync.Mutex
}

// NewScheduler constructs a scheduler over the given shard. cfg is
// normalized through NewConfig (zero fields → defaults).
func NewScheduler(shard ShardForMerge, cfg Config) *Scheduler {
	return &Scheduler{shard: shard, cfg: NewConfig(cfg)}
}

// Run polls the shard every Interval and triggers a merge when the
// §7.1 rules pick candidates. Blocks until ctx is canceled. A
// failure to merge is logged-and-continue: the next tick re-
// evaluates with the current segment set.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick runs one evaluation pass. Exposed via unexported so tests
// can drive it synchronously.
func (s *Scheduler) tick(ctx context.Context) {
	s.mu.Lock()
	if s.inflight >= s.cfg.MaxConcurrent {
		s.mu.Unlock()
		return
	}
	s.inflight++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()

	candidates := s.pickCandidates()
	if len(candidates) < 2 {
		return
	}
	_, _ = s.shard.MergeSegments(ctx, candidates)
}

// PickCandidates evaluates the §7.1 rules and returns the segment
// IDs to merge in this pass (or nil/empty if no merge is due).
// Exported for tests; the public Run path calls the unexported
// tick.
func (s *Scheduler) PickCandidates() []types.SegmentID {
	return s.pickCandidates()
}

// pickCandidates implements the §7.1 rules:
//  1. Delete rate > DeleteRateThreshold segments merge first
//     (reclaim space).
//  2. Segment count > SegmentCountThreshold → merge adjacent tier.
//  3. ForceMerge mode: merge down to maxNumSegments.
//
// M0 simplification: when more than one rule fires, rule 1 wins. The
// "adjacent tier" concept (size-bucket grouping) is reduced to "the
// N smallest segments by byte size" for the count-threshold rule.
func (s *Scheduler) pickCandidates() []types.SegmentID {
	segs := s.shard.ListMergeCandidates()
	if len(segs) < 2 {
		return nil
	}

	// F4 Rule 0: TTL-expired segments merge first. @spec: 02_spec.md#5.4
	if s.cfg.TTLInterval > 0 && s.cfg.TTLColumn != "" {
		now := time.Now().UnixMilli()
		threshold := now - s.cfg.TTLInterval.Milliseconds()
		var ttlHot []storage.SegmentStats
		for _, st := range segs {
			if st.MaxTimestamp > 0 && st.MaxTimestamp < threshold {
				ttlHot = append(ttlHot, st)
			}
		}
		if len(ttlHot) >= 2 {
			return topNByBytes(ttlHot, len(ttlHot))
		}
	}

	// Rule 1: collect segments with delete rate > threshold.
	hot := make([]storage.SegmentStats, 0, len(segs))
	for _, st := range segs {
		if st.NumRows == 0 {
			continue
		}
		rate := float64(st.DeletedRows) / float64(st.NumRows)
		if rate > s.cfg.DeleteRateThreshold {
			hot = append(hot, st)
		}
	}
	if len(hot) >= 2 {
		return topNByBytes(hot, len(hot))
	}

	// Rule 2: segment count > threshold → merge the smallest tier.
	if len(segs) > s.cfg.SegmentCountThreshold {
		return topNByBytes(segs, min(len(segs), 4))
	}

	return nil
}

// ForceMerge is the _forcemerge API path (04 §6.3): merge the shard
// down to at most maxNumSegments. It blocks until the merge
// completes or ctx is canceled. With maxNumSegments=1 it merges
// every segment into one. Returns nil if the shard already has ≤
// maxNumSegments.
func (s *Scheduler) ForceMerge(ctx context.Context, maxNumSegments int) error {
	if maxNumSegments < 1 {
		return fmt.Errorf("merge: max_num_segments must be >= 1")
	}
	segs := s.shard.ListMergeCandidates()
	if len(segs) <= maxNumSegments {
		return nil
	}
	// Sort by min_seq so the merge preserves global row order.
	sort.SliceStable(segs, func(i, j int) bool {
		return segs[i].MinSeq < segs[j].MinSeq
	})
	ids := make([]types.SegmentID, len(segs))
	for i, st := range segs {
		ids[i] = st.ID
	}
	_, err := s.shard.MergeSegments(ctx, ids)
	return err
}

// topNByBytes returns the IDs of the N segments with the smallest
// byte size (the "adjacent tier" approximation for M0). It sorts a
// copy so the caller's slice stays unchanged.
func topNByBytes(segs []storage.SegmentStats, n int) []types.SegmentID {
	if n > len(segs) {
		n = len(segs)
	}
	cp := make([]storage.SegmentStats, len(segs))
	copy(cp, segs)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].Bytes < cp[j].Bytes })
	out := make([]types.SegmentID, n)
	for i := 0; i < n; i++ {
		out[i] = cp[i].ID
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
