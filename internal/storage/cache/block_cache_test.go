package cache

import (
	"testing"

	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// TestBlockCachePutGet verifies basic put/get operations.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.6
func TestBlockCachePutGet(t *testing.T) {
	bc := NewBlockCache(1024 * 1024)
	data := &segment.BlockData{Rows: 100}

	bc.Put(types.SegmentID(1), types.FieldID(10), 0, data)

	got := bc.Get(types.SegmentID(1), types.FieldID(10), 0)
	if got == nil {
		t.Fatal("expected cache hit, got nil")
	}
	if got.Rows != 100 {
		t.Fatalf("expected 100 rows, got %d", got.Rows)
	}
}

// TestBlockCacheMiss verifies missing entries return nil.
func TestBlockCacheMiss(t *testing.T) {
	bc := NewBlockCache(1024 * 1024)
	if got := bc.Get(types.SegmentID(1), types.FieldID(10), 0); got != nil {
		t.Fatal("expected nil for missing entry")
	}
}

// TestBlockCacheSegmentMismatch verifies entries are segment-scoped.
func TestBlockCacheSegmentMismatch(t *testing.T) {
	bc := NewBlockCache(1024 * 1024)
	data := &segment.BlockData{Rows: 10}

	bc.Put(types.SegmentID(1), types.FieldID(10), 0, data)
	if got := bc.Get(types.SegmentID(2), types.FieldID(10), 0); got != nil {
		t.Fatal("expected nil for different segment")
	}
}

// TestBlockCacheInvalidateSegment verifies segment invalidation.
func TestBlockCacheInvalidateSegment(t *testing.T) {
	bc := NewBlockCache(1024 * 1024)
	data := &segment.BlockData{Rows: 10}

	bc.Put(types.SegmentID(1), types.FieldID(10), 0, data)
	bc.Put(types.SegmentID(1), types.FieldID(20), 0, data)
	bc.Put(types.SegmentID(2), types.FieldID(10), 0, data)

	bc.InvalidateSegment(types.SegmentID(1))

	if got := bc.Get(types.SegmentID(1), types.FieldID(10), 0); got != nil {
		t.Fatal("expected nil after invalidation for seg 1")
	}
	if got := bc.Get(types.SegmentID(2), types.FieldID(10), 0); got == nil {
		t.Fatal("expected seg 2 still cached")
	}
}

// TestBlockCacheLRUEviction verifies LRU eviction when capacity exceeded.
func TestBlockCacheLRUEviction(t *testing.T) {
	bc := NewBlockCache(8000) // ~5 blocks worth (100 rows * 16 bytes = 1600 each)
	data := &segment.BlockData{Rows: 100}

	for i := 0; i < 20; i++ {
		bc.Put(types.SegmentID(1), types.FieldID(10), i, data)
	}

	// Early blocks should have been evicted (cache holds ~5 of 20).
	if got := bc.Get(types.SegmentID(1), types.FieldID(10), 0); got != nil {
		t.Fatal("expected block 0 evicted")
	}
	// Most recent blocks should still be cached.
	if got := bc.Get(types.SegmentID(1), types.FieldID(10), 19); got == nil {
		t.Fatal("expected block 19 cached")
	}
}

// TestBlockCacheLen verifies entry count.
func TestBlockCacheLen(t *testing.T) {
	bc := NewBlockCache(1024 * 1024)
	data := &segment.BlockData{Rows: 10}

	if bc.Len() != 0 {
		t.Fatalf("expected 0 entries, got %d", bc.Len())
	}
	bc.Put(types.SegmentID(1), types.FieldID(10), 0, data)
	bc.Put(types.SegmentID(1), types.FieldID(10), 1, data)
	if bc.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", bc.Len())
	}
}

// TestBlockCacheOverwrite verifies putting the same key overwrites.
func TestBlockCacheOverwrite(t *testing.T) {
	bc := NewBlockCache(1024 * 1024)

	data1 := &segment.BlockData{Rows: 50}
	bc.Put(types.SegmentID(1), types.FieldID(10), 0, data1)

	data2 := &segment.BlockData{Rows: 200}
	bc.Put(types.SegmentID(1), types.FieldID(10), 0, data2)

	got := bc.Get(types.SegmentID(1), types.FieldID(10), 0)
	if got == nil {
		t.Fatal("expected cache hit")
	}
	if got.Rows != 200 {
		t.Fatalf("expected overwritten 200 rows, got %d", got.Rows)
	}
}

// TestBlockCacheSize verifies size tracking.
func TestBlockCacheSize(t *testing.T) {
	bc := NewBlockCache(1024 * 1024)
	if bc.Size() != 0 {
		t.Fatalf("expected size 0, got %d", bc.Size())
	}
	data := &segment.BlockData{Rows: 100}
	bc.Put(types.SegmentID(1), types.FieldID(10), 0, data)
	if bc.Size() <= 0 {
		t.Fatalf("expected positive size after put, got %d", bc.Size())
	}
}
