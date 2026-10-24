// Package cache implements a block-level LRU cache for cold segment
// data. Hot segments rely on mmap page cache and are NOT cached here.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.6
package cache

import (
	"container/list"
	"sync"

	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// BlockCache is a block-level LRU cache for cold segment data.
// Hot segments rely on mmap page cache and are NOT cached here.
type BlockCache struct {
	maxSize int64
	curSize int64
	mu      sync.Mutex
	entries map[blockCacheKey]*blockCacheEntry
	lru     *list.List
}

type blockCacheKey struct {
	segmentID  types.SegmentID
	fieldID    types.FieldID
	blockIndex int
}

type blockCacheEntry struct {
	key     blockCacheKey
	data    *segment.BlockData
	size    int64
	lruElem *list.Element
}

// NewBlockCache creates a block cache with the given max size in bytes.
func NewBlockCache(maxSizeBytes int64) *BlockCache {
	if maxSizeBytes <= 0 {
		maxSizeBytes = 1024 * 1024 * 1024 // 1GB default
	}
	return &BlockCache{
		maxSize: maxSizeBytes,
		entries: make(map[blockCacheKey]*blockCacheEntry),
		lru:     list.New(),
	}
}

// Get returns the cached BlockData for (segID, field, blockIdx), or nil.
func (bc *BlockCache) Get(segID types.SegmentID, field types.FieldID, blockIdx int) *segment.BlockData {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	key := blockCacheKey{segID, field, blockIdx}
	entry, ok := bc.entries[key]
	if !ok {
		return nil
	}
	bc.lru.MoveToFront(entry.lruElem)
	return entry.data
}

// Put stores a decoded BlockData in the cache. Evicts LRU entries
// when maxSize is exceeded.
func (bc *BlockCache) Put(segID types.SegmentID, field types.FieldID, blockIdx int, data *segment.BlockData) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	key := blockCacheKey{segID, field, blockIdx}
	if existing, ok := bc.entries[key]; ok {
		bc.curSize -= existing.size
		existing.data = data
		existing.size = estimateBlockSize(data)
		bc.curSize += existing.size
		bc.lru.MoveToFront(existing.lruElem)
		bc.evictIfNeeded()
		return
	}
	entry := &blockCacheEntry{
		key:  key,
		data: data,
		size: estimateBlockSize(data),
	}
	entry.lruElem = bc.lru.PushFront(entry)
	bc.entries[key] = entry
	bc.curSize += entry.size
	bc.evictIfNeeded()
}

// InvalidateSegment removes all entries for the given segment.
func (bc *BlockCache) InvalidateSegment(segID types.SegmentID) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	for key, entry := range bc.entries {
		if key.segmentID == segID {
			bc.lru.Remove(entry.lruElem)
			bc.curSize -= entry.size
			delete(bc.entries, key)
		}
	}
}

// Len returns the number of cached entries.
func (bc *BlockCache) Len() int {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return len(bc.entries)
}

// Size returns the current cache size in bytes.
func (bc *BlockCache) Size() int64 {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.curSize
}

func (bc *BlockCache) evictIfNeeded() {
	for bc.curSize > bc.maxSize && bc.lru.Len() > 0 {
		oldest := bc.lru.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*blockCacheEntry)
		bc.lru.Remove(oldest)
		bc.curSize -= entry.size
		delete(bc.entries, entry.key)
	}
}

// estimateBlockSize approximates the memory footprint of a BlockData.
func estimateBlockSize(data *segment.BlockData) int64 {
	if data == nil {
		return 0
	}
	// Rough estimate: each row takes ~16 bytes on average across types.
	return int64(data.Rows) * 16
}
