package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/index"
	_ "github.com/unisearch-db/unisearch/internal/index/hnsw"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/memtable"
	"github.com/unisearch-db/unisearch/internal/storage/wal"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// testMappingFn returns a fixed mapping for the test schema:
//
//	fid=1 _id (keyword, virtual)
//	fid=2 name (keyword)
//	fid=3 ts (long)
//	fid=10 vec (dense_vector, dim=4)
func testMappingFn() map[types.FieldID]*meta.FieldDef {
	return map[types.FieldID]*meta.FieldDef{
		1:  {Name: "_id", Type: meta.TypeKeyword},
		2:  {Name: "name", Type: meta.TypeKeyword},
		3:  {Name: "ts", Type: meta.TypeLong},
		10: {Name: "vec", Type: meta.TypeDenseVector, Dimensions: 4, Element: meta.ElementFloat32, Similarity: "cosine"},
	}
}

// testApplyInsert parses a "id|name|ts|v0,v1,v2,v3" payload into a
// memtable.Row. Avoids pulling encoding/json into the storage test.
func testApplyInsert(payload []byte, seq types.SeqNo) (memtable.Row, error) {
	s := string(payload)
	// Split into 4 fields: id|name|ts|vec.
	parts := []string{}
	start := 0
	for i := 0; i < len(s) && len(parts) < 3; i++ {
		if s[i] == '|' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	if len(parts) != 3 || start >= len(s) {
		return memtable.Row{}, fmt.Errorf("test: bad payload %q", s)
	}
	parts = append(parts, s[start:])
	id, name, tsStr, vecStr := parts[0], parts[1], parts[2], parts[3]
	var ts int64
	if _, err := fmt.Sscanf(tsStr, "%d", &ts); err != nil {
		return memtable.Row{}, fmt.Errorf("test: bad ts %q: %w", tsStr, err)
	}
	vecParts := []string{}
	start = 0
	for i := 0; i < len(vecStr); i++ {
		if vecStr[i] == ',' {
			vecParts = append(vecParts, vecStr[start:i])
			start = i + 1
		}
	}
	vecParts = append(vecParts, vecStr[start:])
	if len(vecParts) != 4 {
		return memtable.Row{}, fmt.Errorf("test: bad vec %q", vecStr)
	}
	vec := make([]float32, 4)
	for i, p := range vecParts {
		var f float64
		if _, err := fmt.Sscanf(p, "%f", &f); err != nil {
			return memtable.Row{}, fmt.Errorf("test: bad vec part %q: %w", p, err)
		}
		vec[i] = float32(f)
	}
	return memtable.Row{
		ID:     id,
		SeqNo:  seq,
		Source: payload,
		Values: map[types.FieldID]any{
			2:  name,
			3:  ts,
			10: vec,
		},
	}, nil
}

func newTestShard(t *testing.T, dir string) *Shard {
	t.Helper()
	cfg := ShardConfig{
		ShardID:     1,
		DataDir:     dir,
		ApplyInsert: testApplyInsert,
		MappingFn:   testMappingFn,
		WALOpts:     []wal.Option{wal.WithCommitWindow(time.Millisecond)},
	}
	s, err := OpenShard(context.Background(), cfg)
	require.NoError(t, err)
	return s
}

func writeInsert(t *testing.T, s *Shard, id, name string, ts int64, vec [4]float32) types.SeqNo {
	t.Helper()
	doc := []byte(fmt.Sprintf("%s|%s|%d|%v,%v,%v,%v", id, name, ts, vec[0], vec[1], vec[2], vec[3]))
	_, err := s.Write(context.Background(), []WriteOp{{Kind: WriteInsert, Doc: doc}})
	require.NoError(t, err)
	return s.VisibleSeqNo()
}

// TestR6_DeleteImmediateEffect (R6 scenario 1): a DELETE on a memtable
// row must hide it from queries immediately (no async window).
func TestR6_DeleteImmediateEffect(t *testing.T) {
	s := newTestShard(t, t.TempDir())
	defer s.Close()

	writeInsert(t, s, "a", "alpha", 10, [4]float32{1, 0, 0, 0})
	writeInsert(t, s, "b", "beta", 11, [4]float32{0, 1, 0, 0})

	_, err := s.Write(context.Background(), []WriteOp{{Kind: WriteDelete, ID: "a"}})
	require.NoError(t, err)

	snap, _ := s.Snapshot()

	if _, ok := s.LookupID(context.Background(), "a", snap); ok {
		t.Fatalf("deleted id a still visible")
	}
	if _, ok := s.LookupID(context.Background(), "b", snap); !ok {
		t.Fatalf("live id b missing")
	}
	var ids []string
	require.NoError(t, s.Iterate(snap, func(_ types.RowID, _ types.SegmentID, id string, _ types.SeqNo) bool {
		ids = append(ids, id)
		return true
	}))
	assert.ElementsMatch(t, []string{"b"}, ids)
}

// TestR6_UpdateAtomicity (R6 scenario 2): UPDATE = DELETE + INSERT at
// the same seq_no. At any snapshot a reader sees either the old value
// or the new value, never both, never neither.
func TestR6_UpdateAtomicity(t *testing.T) {
	s := newTestShard(t, t.TempDir())
	defer s.Close()

	oldSeq := writeInsert(t, s, "u", "old", 1, [4]float32{1, 1, 1, 1})
	snapBefore, _ := s.Snapshot()

	doc := []byte("u|new|2|2,2,2,2")
	newSeq, err := s.Write(context.Background(), []WriteOp{{Kind: WriteUpdate, ID: "u", Doc: doc}})
	require.NoError(t, err)
	snapAfter := types.NewSnapshot(newSeq)

	if v, ok := s.LookupID(context.Background(), "u", snapBefore); !ok {
		t.Fatalf("snapBefore: u missing")
	} else if v.SeqNo != oldSeq {
		t.Fatalf("snapBefore: expected old seq %d, got %d", oldSeq, v.SeqNo)
	}
	if v, ok := s.LookupID(context.Background(), "u", snapAfter); !ok {
		t.Fatalf("snapAfter: u missing")
	} else if v.SeqNo != newSeq {
		t.Fatalf("snapAfter: expected new seq %d, got %d", newSeq, v.SeqNo)
	}
	var seen []types.SeqNo
	require.NoError(t, s.Iterate(snapAfter, func(_ types.RowID, _ types.SegmentID, id string, seq types.SeqNo) bool {
		if id == "u" {
			seen = append(seen, seq)
		}
		return true
	}))
	assert.Len(t, seen, 1, "exactly one version visible after update")
	assert.Equal(t, newSeq, seen[0])
}

// TestR7_FlushAtomicity (R7 scenario 1): during flush, concurrent
// queries must observe zero errors and no duplicate or missing rows.
func TestR7_FlushAtomicity(t *testing.T) {
	s := newTestShard(t, t.TempDir())
	defer s.Close()

	const N = 50
	for i := 0; i < N; i++ {
		writeInsert(t, s, fmt.Sprintf("k%02d", i), "n", int64(i), [4]float32{float32(i), 0, 0, 0})
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
				if count != N {
					readerErr <- fmt.Errorf("expected %d rows, got %d", N, count)
					return
				}
			}
		}()
	}

	require.NoError(t, s.FlushAll(context.Background()))

	close(stop)
	wg.Wait()
	close(readerErr)
	for err := range readerErr {
		t.Fatalf("reader error during flush: %v", err)
	}

	snap, _ := s.Snapshot()
	count := 0
	require.NoError(t, s.Iterate(snap, func(_ types.RowID, _ types.SegmentID, _ string, _ types.SeqNo) bool {
		count++
		return true
	}))
	assert.Equal(t, N, count, "post-flush count must match")
}

// TestR6_CrashRecovery (R5/R6): after Close + reopen, every write and
// every tombstone must be replayed from the WAL.
func TestR6_CrashRecovery(t *testing.T) {
	dir := t.TempDir()

	func() {
		s := newTestShard(t, dir)
		writeInsert(t, s, "x1", "a", 1, [4]float32{1, 0, 0, 0})
		writeInsert(t, s, "x2", "b", 2, [4]float32{0, 1, 0, 0})
		writeInsert(t, s, "x3", "c", 3, [4]float32{0, 0, 1, 0})
		_, err := s.Write(context.Background(), []WriteOp{{Kind: WriteDelete, ID: "x2"}})
		require.NoError(t, err)
		require.NoError(t, s.Close())
	}()

	s := newTestShard(t, dir)
	defer s.Close()
	snap, _ := s.Snapshot()
	var ids []string
	require.NoError(t, s.Iterate(snap, func(_ types.RowID, _ types.SegmentID, id string, _ types.SeqNo) bool {
		ids = append(ids, id)
		return true
	}))
	assert.ElementsMatch(t, []string{"x1", "x3"}, ids, "deleted row must stay hidden after restart")
}

// TestFlushWorker_DenseVector_HNSWBuild verifies the flush path
// produces a queryable HNSW index in the published segment.
func TestFlushWorker_DenseVector_HNSWBuild(t *testing.T) {
	s := newTestShard(t, t.TempDir())
	defer s.Close()

	vecs := [][4]float32{
		{1, 0, 0, 0},
		{0, 1, 0, 0},
		{0, 0, 1, 0},
		{0, 0, 0, 1},
		{0.9, 0.1, 0, 0},
		{0.1, 0.9, 0, 0},
		{0, 0.9, 0.1, 0},
		{0, 0.1, 0.9, 0},
	}
	for i, v := range vecs {
		writeInsert(t, s, fmt.Sprintf("d%d", i), "n", int64(i), v)
	}

	require.NoError(t, s.FlushAll(context.Background()))
	_, set := s.Snapshot()
	require.Equal(t, 1, set.Len(), "exactly one segment published")

	entry := set.Items()[0]
	require.Contains(t, entry.Indexes, types.FieldID(10), "HNSW index for fid=10 must be loaded")

	res, err := entry.Indexes[types.FieldID(10)].Search(context.Background(),
		index.Query{Vector: []float32{1, 0, 0, 0}, K: 1}, index.SearchBudget{})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, int32(0), res.Hits[0].RowID)
}
