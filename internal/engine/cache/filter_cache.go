// Package cache implements a filter result cache that stores
// roaring.Bitmap results keyed by the normalized AST hash of the
// filter DSL, accelerating repeated filter queries.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.2
package cache

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// FilterCache caches filter→bitmap results per segment.
// Keyed by FNV-1a hash of the AST-normalized filter DSL JSON.
type FilterCache struct {
	maxEntries    int
	ttl           time.Duration
	mu            sync.RWMutex
	entries       map[uint64]*cacheEntry
	lru           *list.List // LRU eviction order (front = most recent)
	notifyCh      chan SegmentChangeEvent
	janitorCtx    context.Context
	janitorCancel context.CancelFunc
}

// SegmentChangeEvent is sent when segments are merged or flushed.
type SegmentChangeEvent struct {
	SegmentID types.SegmentID
}

type cacheEntry struct {
	key       uint64
	segmentID types.SegmentID
	bitmap    *roaring.Bitmap
	expireAt  time.Time
	lruElem   *list.Element
}

// NewFilterCache creates a cache with the given capacity and TTL.
func NewFilterCache(maxEntries int, ttl time.Duration) *FilterCache {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &FilterCache{
		maxEntries: maxEntries,
		ttl:        ttl,
		entries:    make(map[uint64]*cacheEntry),
		lru:        list.New(),
		notifyCh:   make(chan SegmentChangeEvent, 256),
	}
}

// Get returns the cached bitmap for (key, segmentID), or nil if
// the entry is missing or expired.
func (fc *FilterCache) Get(key uint64, segID types.SegmentID) *roaring.Bitmap {
	fc.mu.RLock()
	entry, ok := fc.entries[key]
	fc.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(entry.expireAt) {
		return nil
	}
	if entry.segmentID != segID {
		return nil
	}
	// Move to front of LRU.
	fc.mu.Lock()
	fc.lru.MoveToFront(entry.lruElem)
	fc.mu.Unlock()
	return entry.bitmap
}

// Put stores the bitmap for (key, segmentID). Evicts the least
// recently used entry when maxEntries is exceeded.
func (fc *FilterCache) Put(key uint64, segID types.SegmentID, bm *roaring.Bitmap) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if existing, ok := fc.entries[key]; ok {
		existing.bitmap = bm
		existing.expireAt = time.Now().Add(fc.ttl)
		fc.lru.MoveToFront(existing.lruElem)
		return
	}

	entry := &cacheEntry{
		key:       key,
		segmentID: segID,
		bitmap:    bm,
		expireAt:  time.Now().Add(fc.ttl),
	}
	entry.lruElem = fc.lru.PushFront(entry)
	fc.entries[key] = entry

	// Evict LRU if over capacity.
	for fc.lru.Len() > fc.maxEntries {
		oldest := fc.lru.Back()
		if oldest == nil {
			break
		}
		oe := oldest.Value.(*cacheEntry)
		fc.lru.Remove(oldest)
		delete(fc.entries, oe.key)
	}
}

// InvalidateSegment removes all entries for the given segmentID.
// Called when segments are merged or flushed.
func (fc *FilterCache) InvalidateSegment(segID types.SegmentID) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for key, entry := range fc.entries {
		if entry.segmentID == segID {
			fc.lru.Remove(entry.lruElem)
			delete(fc.entries, key)
		}
	}
}

// NotifySegmentChange enqueues a segment change event for
// asynchronous invalidation by the janitor.
func (fc *FilterCache) NotifySegmentChange(segID types.SegmentID) {
	select {
	case fc.notifyCh <- SegmentChangeEvent{SegmentID: segID}:
	default:
		// Channel full; invalidate synchronously as fallback.
		fc.InvalidateSegment(segID)
	}
}

// StartJanitor launches a background goroutine that evicts expired
// entries and processes segment change notifications.
func (fc *FilterCache) StartJanitor(ctx context.Context) {
	fc.janitorCtx, fc.janitorCancel = context.WithCancel(ctx)
	go fc.janitorLoop()
}

// StopJanitor stops the background janitor goroutine.
func (fc *FilterCache) StopJanitor() {
	if fc.janitorCancel != nil {
		fc.janitorCancel()
	}
}

func (fc *FilterCache) janitorLoop() {
	ticker := time.NewTicker(fc.ttl / 2)
	if ticker.C == nil {
		ticker = time.NewTicker(30 * time.Second)
	}
	defer ticker.Stop()
	for {
		select {
		case <-fc.janitorCtx.Done():
			return
		case ev := <-fc.notifyCh:
			fc.InvalidateSegment(ev.SegmentID)
		case <-ticker.C:
			fc.evictExpired()
		}
	}
}

func (fc *FilterCache) evictExpired() {
	now := time.Now()
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for key, entry := range fc.entries {
		if now.After(entry.expireAt) {
			fc.lru.Remove(entry.lruElem)
			delete(fc.entries, key)
		}
	}
}

// Len returns the current number of cached entries.
func (fc *FilterCache) Len() int {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return len(fc.entries)
}

// NormalizeFilter parses filter DSL JSON into a normalized form
// and returns its FNV-1a 64-bit hash. Normalization sorts object
// keys, removes whitespace, and deduplicates equivalent bool
// sub-clauses to maximize cache hit rate for semantically identical
// filters written differently.
func NormalizeFilter(filterJSON []byte) (uint64, error) {
	if len(filterJSON) == 0 {
		return 0, nil
	}

	var raw map[string]any
	if err := json.Unmarshal(filterJSON, &raw); err != nil {
		return 0, fmt.Errorf("cache: normalize filter: %w", err)
	}

	normalized := normalizeValue(raw)
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return 0, fmt.Errorf("cache: marshal normalized: %w", err)
	}

	h := fnv.New64a()
	h.Write(canonical)
	return h.Sum64(), nil
}

// normalizeValue recursively normalizes a parsed JSON value:
// - map keys are sorted alphabetically
// - nested bool clauses are canonicalized
// - arrays of objects are sorted by their canonical JSON
func normalizeValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		// Recursively normalize each value.
		for k, child := range val {
			val[k] = normalizeValue(child)
		}
		// Special handling for bool queries: sort must/should/filter arrays.
		if boolVal, ok := val["bool"].(map[string]any); ok {
			canonicalizeBool(boolVal)
		}
		return val
	case []any:
		normalized := make([]any, len(val))
		for i, item := range val {
			normalized[i] = normalizeValue(item)
		}
		return normalized
	default:
		return v
	}
}

// canonicalizeBool sorts the must/should/filter/not arrays in a
// bool query to produce a canonical form regardless of clause order.
func canonicalizeBool(boolVal map[string]any) {
	for _, key := range []string{"must", "should", "filter"} {
		if arr, ok := boolVal[key].([]any); ok && len(arr) > 1 {
			sort.SliceStable(arr, func(i, j int) bool {
				ci, _ := json.Marshal(arr[i])
				cj, _ := json.Marshal(arr[j])
				return string(ci) < string(cj)
			})
		}
	}
}
