package agg

import (
	"fmt"
	"hash/fnv"
	"math/bits"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// AggHashTable is the abstract interface for group-by hash tables used by
// GroupAggregator. Specialized implementations per key type avoid string
// conversion overhead and improve cache locality. @spec: 02_spec.md#4.3
type AggHashTable interface {
	// FindOrInsert looks up the key; if not present, creates a new bucket
	// with freshly initialized sub-aggregators. Returns the bucket and
	// whether it was newly created.
	FindOrInsert(key any) (*groupBucket, bool)

	// Iterate visits all buckets in arbitrary order.
	Iterate(fn func(key any, bucket *groupBucket) bool)

	// Len returns the number of groups.
	Len() int

	// Reset clears all entries for reuse.
	Reset()
}

// --- GoMapBackend (small data / backward compatible) -----------------------

// goMapBackend wraps Go's native map[string]*groupBucket. Used when the
// estimated group count is small (< 1000) where Go's built-in map has
// excellent memory allocation optimization. @spec: 02_spec.md#4.3
type goMapBackend struct {
	groups  map[string]*groupBucket
	aggDefs []AggDef
}

func newGoMapBackend(aggDefs []AggDef) *goMapBackend {
	return &goMapBackend{
		groups:  make(map[string]*groupBucket),
		aggDefs: aggDefs,
	}
}

func (g *goMapBackend) FindOrInsert(key any) (*groupBucket, bool) {
	k := key.(string)
	bucket, ok := g.groups[k]
	if !ok {
		bucket = newBucket(k, g.aggDefs)
		g.groups[k] = bucket
	}
	return bucket, !ok
}

func (g *goMapBackend) Iterate(fn func(key any, bucket *groupBucket) bool) {
	for k, b := range g.groups {
		if !fn(k, b) {
			return
		}
	}
}

func (g *goMapBackend) Len() int  { return len(g.groups) }
func (g *goMapBackend) Reset()    { g.groups = make(map[string]*groupBucket) }

// --- FixedHashTableU64 (long/date/ip keys) ----------------------------------

// fixedHashTableU64 uses open addressing with linear probing for uint64
// keys (long/date/ip). Avoids string conversion and hash computation
// overhead for numeric group-by columns. @spec: 02_spec.md#4.3
type fixedHashTableU64 struct {
	slots   []u64Slot
	buckets []*groupBucket
	aggDefs []AggDef
	count   int
}

type u64Slot struct {
	key   uint64 // 0 = empty; keys are offset by +1 to allow 0 as valid key
	bIdx   uint32 // index into buckets slice
}

func newFixedHashTableU64(capacity int, aggDefs []AggDef) *fixedHashTableU64 {
	// Power of 2 capacity with load factor 0.75.
	c := 1
	for c < capacity*4/3 {
		c <<= 1
	}
	if c < 16 {
		c = 16
	}
	return &fixedHashTableU64{
		slots:   make([]u64Slot, c),
		buckets: make([]*groupBucket, 0, capacity),
		aggDefs: aggDefs,
	}
}

func (h *fixedHashTableU64) FindOrInsert(key any) (*groupBucket, bool) {
	k := uint64(0)
	switch v := key.(type) {
	case int64:
		k = uint64(v)
	case uint64:
		k = v
	default:
		k = uint64(key.(int64))
	}
	// Offset by 1 so key 0 is distinguishable from empty slot.
	storedKey := k + 1
	mask := uint64(len(h.slots) - 1)
	idx := k & mask

	for {
		if h.slots[idx].key == 0 {
			// Empty slot — insert.
			bucket := newBucket(fmt.Sprintf("%d", key), h.aggDefs)
			bIdx := uint32(len(h.buckets))
			h.buckets = append(h.buckets, bucket)
			h.slots[idx] = u64Slot{key: storedKey, bIdx: bIdx}
			h.count++
			if h.count*4 > len(h.slots)*3 {
				h.grow()
			}
			return bucket, true
		}
		if h.slots[idx].key == storedKey {
			return h.buckets[h.slots[idx].bIdx], false
		}
		idx = (idx + 1) & mask
	}
}

func (h *fixedHashTableU64) grow() {
	newSlots := make([]u64Slot, len(h.slots)*2)
	mask := uint64(len(newSlots) - 1)
	for _, s := range h.slots {
		if s.key == 0 {
			continue
		}
		k := s.key - 1
		idx := k & mask
		for newSlots[idx].key != 0 {
			idx = (idx + 1) & mask
		}
		newSlots[idx] = s
	}
	h.slots = newSlots
}

func (h *fixedHashTableU64) Iterate(fn func(key any, bucket *groupBucket) bool) {
	for _, b := range h.buckets {
		if !fn(b.key, b) {
			return
		}
	}
}

func (h *fixedHashTableU64) Len() int { return h.count }
func (h *fixedHashTableU64) Reset() {
	for i := range h.slots {
		h.slots[i] = u64Slot{}
	}
	h.buckets = h.buckets[:0]
	h.count = 0
}

// --- StringHashTable (keyword/text keys) -----------------------------------

// stringHashTable uses open addressing with FNV-1a hashing for string keys.
// Short keys are stored inline in the slot; long keys reference the bucket.
// @spec: 02_spec.md#4.3
type stringHashTable struct {
	slots   []strSlot
	buckets []*groupBucket
	aggDefs []AggDef
	count   int
}

type strSlot struct {
	hash uint64 // 0 = empty
	bIdx uint32
}

func newStringHashTable(capacity int, aggDefs []AggDef) *stringHashTable {
	c := 1
	for c < capacity*4/3 {
		c <<= 1
	}
	if c < 16 {
		c = 16
	}
	return &stringHashTable{
		slots:   make([]strSlot, c),
		buckets: make([]*groupBucket, 0, capacity),
		aggDefs: aggDefs,
	}
}

func (h *stringHashTable) FindOrInsert(key any) (*groupBucket, bool) {
	k := key.(string)
	hash := fnv1aHash(k)
	if hash == 0 {
		hash = 1 // avoid 0 = empty sentinel
	}
	mask := uint64(len(h.slots) - 1)
	idx := hash & mask

	for {
		if h.slots[idx].hash == 0 {
			// Empty — insert.
			bucket := newBucket(k, h.aggDefs)
			bIdx := uint32(len(h.buckets))
			h.buckets = append(h.buckets, bucket)
			h.slots[idx] = strSlot{hash: hash, bIdx: bIdx}
			h.count++
			if h.count*4 > len(h.slots)*3 {
				h.grow()
			}
			return bucket, true
		}
		if h.slots[idx].hash == hash && h.buckets[h.slots[idx].bIdx].key == k {
			return h.buckets[h.slots[idx].bIdx], false
		}
		idx = (idx + 1) & mask
	}
}

func (h *stringHashTable) grow() {
	newSlots := make([]strSlot, len(h.slots)*2)
	mask := uint64(len(newSlots) - 1)
	for _, s := range h.slots {
		if s.hash == 0 {
			continue
		}
		idx := s.hash & mask
		for newSlots[idx].hash != 0 {
			idx = (idx + 1) & mask
		}
		newSlots[idx] = s
	}
	h.slots = newSlots
}

func (h *stringHashTable) Iterate(fn func(key any, bucket *groupBucket) bool) {
	for _, b := range h.buckets {
		if !fn(b.key, b) {
			return
		}
	}
}

func (h *stringHashTable) Len() int { return h.count }
func (h *stringHashTable) Reset() {
	for i := range h.slots {
		h.slots[i] = strSlot{}
	}
	h.buckets = h.buckets[:0]
	h.count = 0
}

// --- helpers ---------------------------------------------------------------

// newBucket creates a groupBucket with freshly initialized sub-aggregators.
func newBucket(key string, aggDefs []AggDef) *groupBucket {
	b := &groupBucket{key: key, aggs: make([]Aggregator, len(aggDefs))}
	for i, def := range aggDefs {
		agg, err := NewAggregator(def.Type, def.Params)
		if err != nil {
			continue
		}
		b.aggs[i] = agg
	}
	return b
}

// fnv1aHash computes FNV-1a 64-bit hash for string keys.
func fnv1aHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// trailingZeros is a helper for power-of-2 mask computation.
func trailingZeros64(v uint64) int { return bits.TrailingZeros64(v) }

// SelectHashTable chooses the optimal hash table implementation based on
// key type and estimated group count. For small datasets (< 1000 groups),
// Go's native map is used for its excellent allocation optimization.
// @spec: 02_spec.md#4.4
func SelectHashTable(keyType meta.FieldType, isComposite bool, estimatedGroups int64, aggDefs []AggDef) AggHashTable {
	// Small data: use Go map (string keys only for backward compat).
	if estimatedGroups < 1000 {
		return newGoMapBackend(aggDefs)
	}

	if isComposite {
		// Multi-column composite keys: use Go map with serialized key.
		// Future: implement CompositeKeyTable with packed keys.
		return newGoMapBackend(aggDefs)
	}

	switch keyType {
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		return newFixedHashTableU64(int(estimatedGroups), aggDefs)
	case meta.TypeBoolean:
		return newFixedHashTableU64(int(estimatedGroups), aggDefs)
	case meta.TypeKeyword, meta.TypeText:
		return newStringHashTable(int(estimatedGroups), aggDefs)
	default:
		return newGoMapBackend(aggDefs)
	}
}
