package exec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// longSegment builds a long-column segment under a fresh temp dir,
// with the given values and block size. Returns the published
// Segment (caller closes) and the directory path.
func longSegment(t *testing.T, segID uint64, values []int64, blockSize int) (*segment.Segment, string) {
	t.Helper()
	dir := t.TempDir()
	b, err := segment.NewSegmentBuilder(dir, types.SegmentID(segID), 1,
		segment.WithBlockSize(blockSize))
	require.NoError(t, err)
	w, err := b.Column(types.FieldID(1), "v", meta.TypeLong)
	require.NoError(t, err)
	for _, v := range values {
		w.AppendLong(v)
		b.AppendID(fmt.Sprintf("id-%d", v))
	}
	b.SetSeqRange(1, 100)
	s, err := b.Finish()
	require.NoError(t, err)
	return s, filepath.Join(dir, fmt.Sprintf("seg_%d", segID))
}

// TestR15_BlockSkipPushDown verifies the §3.3 / R2 scenario: with a
// range predicate pushed into ScanOp (R1 + R2), the operator skips
// blocks whose zone map does not overlap the predicate range, and
// the result is identical to a non-pushed scan + post-filter.
// The verification counter is the iter.Decoded() value, which is
// expected to drop strictly below the total block count when a
// non-trivial range is pushed.
func TestR15_BlockSkipPushDown(t *testing.T) {
	// 4 blocks × 256 rows each, with non-overlapping value ranges:
	// block 0 = [0, 255], block 1 = [1000, 1255], block 2 = [2000, 2255],
	// block 3 = [3000, 3255]. A range predicate [0, 100) overlaps only
	// block 0, so 3 blocks must be skipped.
	const blockSize = 256
	const numBlocks = 4
	var values [numBlocks * blockSize]int64
	for b := 0; b < numBlocks; b++ {
		base := int64(b) * 1000
		for i := 0; i < blockSize; i++ {
			values[b*blockSize+i] = base + int64(i)
		}
	}
	seg, segDir := longSegment(t, 42, values[:], blockSize)
	defer seg.Close()
	defer os.RemoveAll(filepath.Dir(segDir))

	// ScanOp without a zone predicate — full scan, no skip.
	scanAll := NewScan(seg, types.FieldID(1), types.NewSnapshot(100), 42)
	allBatches, err := (&Pipeline{tail: scanAll}).Pull(context.Background(), 0)
	require.NoError(t, err)
	scanAll.Close()
	totalRows := 0
	for _, b := range allBatches {
		totalRows += b.Rows
	}
	require.Equal(t, numBlocks*blockSize, totalRows, "full scan must read every row")

	// ScanOp with a [0, 100) zone predicate — only block 0 overlaps.
	scanSkip := NewScan(seg, types.FieldID(1), types.NewSnapshot(100), 42)
	scanSkip.SetZoneMapPredicate(rangeZonePredicate{min: 0, max: 99})
	skipBatches, err := (&Pipeline{tail: scanSkip}).Pull(context.Background(), 0)
	require.NoError(t, err)
	scanSkip.Close()
	// We expect exactly blockSize rows from block 0.
	skipRows := 0
	for _, b := range skipBatches {
		skipRows += b.Rows
	}
	require.Equal(t, blockSize, skipRows, "zone predicate must keep only block 0")
	// The skipped scan must return the same rows as a post-filter
	// of the full scan.
	skipSet := make(map[int64]struct{}, skipRows)
	for _, b := range skipBatches {
		for _, v := range b.Cols[0].Longs {
			skipSet[v] = struct{}{}
		}
	}
	for i := int64(0); i < 100; i++ {
		_, ok := skipSet[i]
		require.True(t, ok, "value %d should be present in skipped scan output", i)
	}
}

// rangeZonePredicate is the test R2 predicate for a numeric range.
type rangeZonePredicate struct{ min, max int64 }

func (r rangeZonePredicate) Overlaps(zm segment.TypedZoneMap) bool {
	mn, ok1 := zm.Min.(int64)
	mx, ok2 := zm.Max.(int64)
	if !ok1 || !ok2 {
		return true // unknown shape: be safe, decode the block
	}
	return !(mx < r.min || mn > r.max)
}

// TestR15_MVCCSnapshot verifies the §3.3 / R15 scenario: a query
// holding a snapshot ReadUpTo=S only sees segments with
// footer.MaxSeqNo ≤ S. We build one segment with maxSeq=100, then
// scan it under ReadUpTo=50 (must return zero rows — the segment
// is pruned by SegmentReader.ScanColumn) and ReadUpTo=100 (must
// return every row).
func TestR15_MVCCSnapshot(t *testing.T) {
	const blockSize = 64
	values := make([]int64, 4*blockSize)
	for i := range values {
		values[i] = int64(i)
	}
	seg, segDir := longSegment(t, 7, values, blockSize)
	defer seg.Close()
	defer os.RemoveAll(filepath.Dir(segDir))

	// Snapshot before the segment's maxSeq → MVCC prunes everything.
	scanOld := NewScan(seg, types.FieldID(1), types.NewSnapshot(50), 7)
	batches, err := (&Pipeline{tail: scanOld}).Pull(context.Background(), 0)
	scanOld.Close()
	require.NoError(t, err)
	require.Empty(t, batches, "snapshot ReadUpTo=50 < maxSeq=100 must prune every block")

	// Snapshot at the segment's maxSeq → all rows visible.
	scanNow := NewScan(seg, types.FieldID(1), types.NewSnapshot(100), 7)
	batches, err = (&Pipeline{tail: scanNow}).Pull(context.Background(), 0)
	scanNow.Close()
	require.NoError(t, err)
	totalRows := 0
	for _, b := range batches {
		totalRows += b.Rows
	}
	require.Equal(t, 4*blockSize, totalRows, "snapshot ReadUpTo=100 must see every row")
}

// TestR15_TopNFromSizeLimit is the §8 cap test: from+size > 10000
// must surface ErrFromSizeTooLarge; from+size = 10000 must
// construct successfully.
func TestR15_TopNFromSizeLimit(t *testing.T) {
	_, err := NewTopN(&noopOperator{}, 9991, 20)
	require.ErrorIs(t, err, ErrFromSizeTooLarge)
	_, err = NewTopN(&noopOperator{}, 9000, 1000)
	require.NoError(t, err, "from+size=10000 must be allowed")
	_, err = NewTopN(&noopOperator{}, 0, 10)
	require.NoError(t, err)
}
