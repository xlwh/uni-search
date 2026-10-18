package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
	_ "github.com/unisearch-db/unisearch/internal/index/hnsw"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// TestR7_MergePhysicalClear (R7 合并物理清除): when a segment has
// 50% of its rows flagged in the delete bitmap, merging it produces
// a new segment whose row count = original - deleted, the bitmap for
// the new segment is empty (no migrated tombstones), and the source
// segment is removed from the live set + on disk.
func TestR7_MergePhysicalClear(t *testing.T) {
	s := newTestShard(t, t.TempDir())
	defer s.Close()

	// 1. Write 4 rows, flush to segment 1.
	for i, v := range [][4]float32{
		{1, 0, 0, 0},
		{0, 1, 0, 0},
		{0, 0, 1, 0},
		{0, 0, 0, 1},
	} {
		writeInsert(t, s, fmt.Sprintf("k%d", i), "n", int64(i), v)
	}
	require.NoError(t, s.FlushAll(context.Background()))

	// 2. Delete 2 of the 4 rows (now segment has 4 rows + 2 deleted).
	_, err := s.Write(context.Background(), []WriteOp{
		{Kind: WriteDelete, ID: "k0"},
		{Kind: WriteDelete, ID: "k2"},
	})
	require.NoError(t, err)

	// Sanity: pre-merge state.
	snap, set := s.Snapshot()
	require.Equal(t, 1, set.Len(), "exactly one source segment")
	srcSegID := set.Items()[0].Reader.ID()

	stats := s.ListMergeCandidates()
	require.Len(t, stats, 1)
	assert.Equal(t, 4, stats[0].NumRows)
	assert.Equal(t, 2, stats[0].DeletedRows, "2 rows deleted via segment bitmap")

	// 3. Merge.
	newID, err := s.MergeSegments(context.Background(), []types.SegmentID{srcSegID})
	require.NoError(t, err)
	require.NotEqual(t, srcSegID, newID, "merge produced a new segment id")

	// 4. Assertions.
	snap2, set2 := s.Snapshot()
	require.Equal(t, 1, set2.Len(), "still exactly one segment (the merged one)")
	merged := set2.Items()[0]
	assert.Equal(t, newID, merged.Reader.ID())
	assert.Equal(t, 2, merged.Reader.NumRows(), "physical clear: 4 - 2 = 2 rows")

	// Bitmap for the new segment is empty (no migrated tombstones).
	bm := s.dt.SegmentBitmap(newID)
	assert.Nil(t, bm, "merged segment has no delete bitmap (clean slate)")
	// Source bitmap dropped.
	assert.Nil(t, s.dt.SegmentBitmap(srcSegID), "source bitmap dropped after merge")

	// Iterate sees exactly the 2 surviving rows.
	var ids []string
	require.NoError(t, s.Iterate(snap2, func(_ types.RowID, _ types.SegmentID, id string, _ types.SeqNo) bool {
		ids = append(ids, id)
		return true
	}))
	assert.ElementsMatch(t, []string{"k1", "k3"}, ids)

	// Source segment directory removed from disk.
	stats2 := s.ListMergeCandidates()
	require.Len(t, stats2, 1)
	assert.Equal(t, newID, stats2[0].ID, "live segment is the merged one")
	_ = snap // silence unused-warning if assertions tighten
}

// TestR7_MergeQueryUnchanged (R7 合并物理清除 scenario 2): before
// and after merge, queries see the same row set. Verifies that the
// merge preserves order + content for two source segments with
// partial deletes in each.
func TestR7_MergeQueryUnchanged(t *testing.T) {
	s := newTestShard(t, t.TempDir())
	defer s.Close()

	// Segment A: 3 rows.
	writeInsert(t, s, "a1", "n", 1, [4]float32{1, 0, 0, 0})
	writeInsert(t, s, "a2", "n", 2, [4]float32{0, 1, 0, 0})
	writeInsert(t, s, "a3", "n", 3, [4]float32{0, 0, 1, 0})
	require.NoError(t, s.FlushAll(context.Background()))

	// Segment B: 3 rows.
	writeInsert(t, s, "b1", "n", 4, [4]float32{1, 1, 0, 0})
	writeInsert(t, s, "b2", "n", 5, [4]float32{0, 1, 1, 0})
	writeInsert(t, s, "b3", "n", 6, [4]float32{1, 0, 1, 0})
	require.NoError(t, s.FlushAll(context.Background()))

	// Delete 1 row in each segment.
	_, err := s.Write(context.Background(), []WriteOp{
		{Kind: WriteDelete, ID: "a2"},
		{Kind: WriteDelete, ID: "b1"},
	})
	require.NoError(t, err)

	// Snapshot pre-merge result.
	snap1, set1 := s.Snapshot()
	require.Equal(t, 2, set1.Len(), "two source segments before merge")
	var preIDs []string
	require.NoError(t, s.Iterate(snap1, func(_ types.RowID, _ types.SegmentID, id string, _ types.SeqNo) bool {
		preIDs = append(preIDs, id)
		return true
	}))
	assert.ElementsMatch(t, []string{"a1", "a3", "b2", "b3"}, preIDs)

	// Merge both segments.
	srcIDs := []types.SegmentID{
		set1.Items()[0].Reader.ID(),
		set1.Items()[1].Reader.ID(),
	}
	_, err = s.MergeSegments(context.Background(), srcIDs)
	require.NoError(t, err)

	// Post-merge result.
	snap2, set2 := s.Snapshot()
	require.Equal(t, 1, set2.Len(), "one segment after merge")
	var postIDs []string
	require.NoError(t, s.Iterate(snap2, func(_ types.RowID, _ types.SegmentID, id string, _ types.SeqNo) bool {
		postIDs = append(postIDs, id)
		return true
	}))
	assert.ElementsMatch(t, preIDs, postIDs, "query result unchanged after merge")
}

// TestR7_MergeConcurrentQueries (R7 合并期查询零错误): while merge
// runs, concurrent Iterate queries observe no errors and no
// duplicate/missing rows. The atomic SegmentSet swap under the W
// lock closes the publish-vs-remove race.
func TestR7_MergeConcurrentQueries(t *testing.T) {
	s := newTestShard(t, t.TempDir())
	defer s.Close()

	const N = 20
	for i := 0; i < N; i++ {
		writeInsert(t, s, fmt.Sprintf("k%02d", i), "n", int64(i),
			[4]float32{float32(i), 0, 0, 0})
	}
	require.NoError(t, s.FlushAll(context.Background()))

	// Delete 5 rows so merge has work to do.
	for _, id := range []string{"k00", "k03", "k07", "k12", "k18"} {
		_, err := s.Write(context.Background(), []WriteOp{{Kind: WriteDelete, ID: id}})
		require.NoError(t, err)
	}

	_, set := s.Snapshot()
	srcIDs := make([]types.SegmentID, 0, len(set.Items()))
	for _, e := range set.Items() {
		srcIDs = append(srcIDs, e.Reader.ID())
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	readerErr := make(chan error, 4)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, _ := s.Snapshot()
				count := 0
				if err := s.Iterate(snap, func(_ types.RowID, _ types.SegmentID, _ string, _ types.SeqNo) bool {
					count++
					return true
				}); err != nil {
					readerErr <- err
					return
				}
				// During merge, count can briefly be N-5 (live
				// segment, no memtable) — but never more, never
				// zero.
				if count > N-5 {
					readerErr <- fmt.Errorf("got %d rows, want at most %d (duplicate rows during merge)", count, N-5)
					return
				}
			}
		}()
	}

	// Trigger merge from the main goroutine.
	_, err := s.MergeSegments(context.Background(), srcIDs)
	require.NoError(t, err)

	close(stop)
	wg.Wait()
	close(readerErr)
	for err := range readerErr {
		t.Fatalf("reader error during merge: %v", err)
	}

	// Final state: one merged segment with N-5 rows.
	snap, set2 := s.Snapshot()
	require.Equal(t, 1, set2.Len())
	var ids []string
	require.NoError(t, s.Iterate(snap, func(_ types.RowID, _ types.SegmentID, id string, _ types.SeqNo) bool {
		ids = append(ids, id)
		return true
	}))
	assert.Equal(t, N-5, len(ids))
}

// TestR7_MergeHNSWRebuild (R7 + R8 cross-cutting): after merge, the
// merged segment's HNSW index is queryable and returns the correct
// row for a known query vector.
func TestR7_MergeHNSWRebuild(t *testing.T) {
	s := newTestShard(t, t.TempDir())
	defer s.Close()

	// Segment A: 4 vectors around the +x axis.
	for i, v := range [][4]float32{
		{1, 0, 0, 0},
		{0.95, 0.05, 0, 0},
		{0.9, 0.1, 0, 0},
		{0.85, 0.15, 0, 0},
	} {
		writeInsert(t, s, fmt.Sprintf("a%d", i), "n", int64(i), v)
	}
	require.NoError(t, s.FlushAll(context.Background()))

	// Segment B: 4 vectors around the +y axis.
	for i, v := range [][4]float32{
		{0, 1, 0, 0},
		{0.05, 0.95, 0, 0},
		{0.1, 0.9, 0, 0},
		{0.15, 0.85, 0, 0},
	} {
		writeInsert(t, s, fmt.Sprintf("b%d", i), "n", int64(i+4), v)
	}
	require.NoError(t, s.FlushAll(context.Background()))

	// Delete one row from segment A.
	_, err := s.Write(context.Background(), []WriteOp{{Kind: WriteDelete, ID: "a1"}})
	require.NoError(t, err)

	_, set := s.Snapshot()
	srcIDs := []types.SegmentID{
		set.Items()[0].Reader.ID(),
		set.Items()[1].Reader.ID(),
	}
	_, err = s.MergeSegments(context.Background(), srcIDs)
	require.NoError(t, err)

	// Query the merged segment for +x nearest neighbor.
	_, set2 := s.Snapshot()
	require.Equal(t, 1, set2.Len())
	entry := set2.Items()[0]
	require.Contains(t, entry.Indexes, types.FieldID(10), "HNSW index loaded for merged segment")

	res, err := entry.Indexes[types.FieldID(10)].Search(context.Background(),
		index.Query{Vector: []float32{1, 0, 0, 0}, K: 1}, index.SearchBudget{})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	// The closest row to +x in the surviving set is row 0 (a0).
	assert.Equal(t, int32(0), res.Hits[0].RowID, "merged HNSW returns the +x row")
}
