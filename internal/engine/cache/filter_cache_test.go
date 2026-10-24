package cache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// TestFilterCachePutGet verifies basic put/get operations.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.2
func TestFilterCachePutGet(t *testing.T) {
	fc := NewFilterCache(100, 60*time.Second)
	bm := roaring.New()
	bm.AddInt(1)
	bm.AddInt(2)
	bm.AddInt(3)

	segID := types.SegmentID(42)
	fc.Put(123, segID, bm)

	got := fc.Get(123, segID)
	if got == nil {
		t.Fatal("expected cache hit, got nil")
	}
	if got.GetCardinality() != 3 {
		t.Fatalf("expected 3 elements, got %d", got.GetCardinality())
	}
}

// TestFilterCacheMiss verifies that missing keys return nil.
func TestFilterCacheMiss(t *testing.T) {
	fc := NewFilterCache(100, 60*time.Second)
	if got := fc.Get(999, types.SegmentID(1)); got != nil {
		t.Fatal("expected nil for missing key")
	}
}

// TestFilterCacheSegmentMismatch verifies that a cached entry
// for segment A is not returned for segment B.
func TestFilterCacheSegmentMismatch(t *testing.T) {
	fc := NewFilterCache(100, 60*time.Second)
	bm := roaring.New()
	bm.AddInt(1)

	fc.Put(1, types.SegmentID(10), bm)
	if got := fc.Get(1, types.SegmentID(20)); got != nil {
		t.Fatal("expected nil for different segment")
	}
}

// TestFilterCacheTTLExpiry verifies that entries expire after TTL.
func TestFilterCacheTTLExpiry(t *testing.T) {
	fc := NewFilterCache(100, 50*time.Millisecond)
	bm := roaring.New()
	fc.Put(1, types.SegmentID(1), bm)

	// Should be present immediately.
	if got := fc.Get(1, types.SegmentID(1)); got == nil {
		t.Fatal("expected cache hit before TTL")
	}

	time.Sleep(60 * time.Millisecond)

	// Should be expired now.
	if got := fc.Get(1, types.SegmentID(1)); got != nil {
		t.Fatal("expected nil after TTL expiry")
	}
}

// TestFilterCacheLRUEviction verifies that the least recently
// used entry is evicted when maxEntries is exceeded.
func TestFilterCacheLRUEviction(t *testing.T) {
	fc := NewFilterCache(2, 60*time.Second)
	bm := roaring.New()

	fc.Put(1, types.SegmentID(1), bm)
	fc.Put(2, types.SegmentID(1), bm)

	// Access key 1 to make key 2 the LRU.
	fc.Get(1, types.SegmentID(1))

	// Insert key 3 — should evict key 2.
	fc.Put(3, types.SegmentID(1), bm)

	if got := fc.Get(2, types.SegmentID(1)); got != nil {
		t.Fatal("expected key 2 evicted as LRU")
	}
	if got := fc.Get(1, types.SegmentID(1)); got == nil {
		t.Fatal("expected key 1 still cached")
	}
	if got := fc.Get(3, types.SegmentID(1)); got == nil {
		t.Fatal("expected key 3 cached")
	}
}

// TestFilterCacheInvalidateSegment verifies that all entries
// for a segment are removed on invalidation.
func TestFilterCacheInvalidateSegment(t *testing.T) {
	fc := NewFilterCache(100, 60*time.Second)
	bm := roaring.New()
	segA := types.SegmentID(10)
	segB := types.SegmentID(20)

	fc.Put(1, segA, bm)
	fc.Put(2, segA, bm)
	fc.Put(3, segB, bm)

	fc.InvalidateSegment(segA)

	if got := fc.Get(1, segA); got != nil {
		t.Fatal("expected key 1 invalidated for segA")
	}
	if got := fc.Get(2, segA); got != nil {
		t.Fatal("expected key 2 invalidated for segA")
	}
	if got := fc.Get(3, segB); got == nil {
		t.Fatal("expected key 3 still cached for segB")
	}
}

// TestFilterCacheJanitor verifies that the background janitor
// evicts expired entries and processes segment change events.
func TestFilterCacheJanitor(t *testing.T) {
	fc := NewFilterCache(100, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc.StartJanitor(ctx)
	defer fc.StopJanitor()

	bm := roaring.New()
	segID := types.SegmentID(5)
	fc.Put(1, segID, bm)

	// Wait for TTL + janitor interval.
	time.Sleep(200 * time.Millisecond)

	if got := fc.Get(1, segID); got != nil {
		t.Fatal("expected janitor to evict expired entry")
	}
}

// TestFilterCacheNotifySegmentChange verifies that segment change
// notifications invalidate entries.
func TestFilterCacheNotifySegmentChange(t *testing.T) {
	fc := NewFilterCache(100, 60*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc.StartJanitor(ctx)
	defer fc.StopJanitor()

	bm := roaring.New()
	segID := types.SegmentID(7)
	fc.Put(1, segID, bm)

	// Notify segment change.
	fc.NotifySegmentChange(segID)

	// Wait for janitor to process.
	time.Sleep(100 * time.Millisecond)

	if got := fc.Get(1, segID); got != nil {
		t.Fatal("expected entry invalidated after segment change")
	}
}

// TestNormalizeFilterBasic verifies that basic filter JSON
// produces a consistent hash.
func TestNormalizeFilterBasic(t *testing.T) {
	filter := []byte(`{"term":{"field":"value"}}`)
	h1, err := NormalizeFilter(filter)
	if err != nil {
		t.Fatalf("NormalizeFilter failed: %v", err)
	}
	if h1 == 0 {
		t.Fatal("expected non-zero hash")
	}
}

// TestNormalizeFilterKeyOrder verifies that JSON with different
// key order produces the same hash.
func TestNormalizeFilterKeyOrder(t *testing.T) {
	filter1 := []byte(`{"term":{"field":"value","boost":1.0}}`)
	filter2 := []byte(`{"term":{"boost":1.0,"field":"value"}}`)

	h1, _ := NormalizeFilter(filter1)
	h2, _ := NormalizeFilter(filter2)

	if h1 != h2 {
		t.Fatalf("expected same hash for reordered keys: %d vs %d", h1, h2)
	}
}

// TestNormalizeFilterWhitespace verifies that whitespace
// differences produce the same hash.
func TestNormalizeFilterWhitespace(t *testing.T) {
	filter1 := []byte(`{"term": {"field": "value"}}`)
	filter2 := []byte(`{"term":{"field":"value"}}`)

	h1, _ := NormalizeFilter(filter1)
	h2, _ := NormalizeFilter(filter2)

	if h1 != h2 {
		t.Fatalf("expected same hash for whitespace differences: %d vs %d", h1, h2)
	}
}

// TestNormalizeFilterBoolClauseOrder verifies that bool query
// clauses in different order produce the same hash.
func TestNormalizeFilterBoolClauseOrder(t *testing.T) {
	filter1 := []byte(`{"bool":{"must":[{"term":{"a":"1"}},{"term":{"b":"2"}}]}}`)
	filter2 := []byte(`{"bool":{"must":[{"term":{"b":"2"}},{"term":{"a":"1"}}]}}`)

	h1, _ := NormalizeFilter(filter1)
	h2, _ := NormalizeFilter(filter2)

	if h1 != h2 {
		t.Fatalf("expected same hash for reordered bool clauses: %d vs %d", h1, h2)
	}
}

// TestNormalizeFilterEmpty verifies empty input returns zero hash.
func TestNormalizeFilterEmpty(t *testing.T) {
	h, err := NormalizeFilter([]byte{})
	if err != nil {
		t.Fatalf("NormalizeFilter empty failed: %v", err)
	}
	if h != 0 {
		t.Fatalf("expected zero hash for empty input, got %d", h)
	}
}

// TestNormalizeFilterInvalid verifies invalid JSON returns error.
func TestNormalizeFilterInvalid(t *testing.T) {
	_, err := NormalizeFilter([]byte(`{invalid json}`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// TestFilterCachePutOverwrite verifies that putting with an
// existing key overwrites the value.
func TestFilterCachePutOverwrite(t *testing.T) {
	fc := NewFilterCache(100, 60*time.Second)
	segID := types.SegmentID(1)

	bm1 := roaring.New()
	bm1.AddInt(1)
	bm1.AddInt(2)
	fc.Put(1, segID, bm1)

	bm2 := roaring.New()
	bm2.AddInt(10)
	bm2.AddInt(20)
	bm2.AddInt(30)
	fc.Put(1, segID, bm2)

	got := fc.Get(1, segID)
	if got == nil {
		t.Fatal("expected cache hit")
	}
	if got.GetCardinality() != 3 {
		t.Fatalf("expected overwritten value with 3 elements, got %d", got.GetCardinality())
	}
}

// TestFilterCacheLen verifies the Len method.
func TestFilterCacheLen(t *testing.T) {
	fc := NewFilterCache(100, 60*time.Second)
	bm := roaring.New()
	segID := types.SegmentID(1)

	if fc.Len() != 0 {
		t.Fatalf("expected 0 entries, got %d", fc.Len())
	}

	fc.Put(1, segID, bm)
	fc.Put(2, segID, bm)
	if fc.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", fc.Len())
	}
}

// TestFilterCacheDefaults verifies default values when
// zero/negative params are passed.
func TestFilterCacheDefaults(t *testing.T) {
	fc := NewFilterCache(0, 0)
	bm := roaring.New()
	fc.Put(1, types.SegmentID(1), bm)

	// Should work with defaults.
	if got := fc.Get(1, types.SegmentID(1)); got == nil {
		t.Fatal("expected cache hit with default params")
	}
}

// Suppress unused import for json in test (used indirectly via NormalizeFilter).
var _ = json.Marshal
