package agg

import (
	"fmt"
	"testing"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/stretchr/testify/require"
)

// @spec: 02_spec.md#4.8 F3-T01..T07

func TestF3_GoMapBackend(t *testing.T) {
	ht := newGoMapBackend([]AggDef{{Name: "sum", Type: "sum", Params: map[string]any{"field": "v"}}})

	// Insert and find.
	b1, created := ht.FindOrInsert("cat")
	require.True(t, created)
	require.NotNil(t, b1)
	require.Equal(t, "cat", b1.key)

	// Find existing.
	b2, created := ht.FindOrInsert("cat")
	require.False(t, created)
	require.Equal(t, b1, b2)

	// Insert another.
	ht.FindOrInsert("dog")
	require.Equal(t, 2, ht.Len())

	// Iterate.
	keys := map[string]bool{}
	ht.Iterate(func(key any, bucket *groupBucket) bool {
		keys[bucket.key] = true
		return true
	})
	require.True(t, keys["cat"])
	require.True(t, keys["dog"])

	// Reset.
	ht.Reset()
	require.Equal(t, 0, ht.Len())
}

func TestF3_FixedHashTableU64(t *testing.T) {
	ht := newFixedHashTableU64(100, []AggDef{{Name: "sum", Type: "sum", Params: map[string]any{"field": "v"}}})

	// Insert int64 keys.
	for i := int64(0); i < 50; i++ {
		b, created := ht.FindOrInsert(i)
		require.True(t, created, "insert %d should be new", i)
		require.NotNil(t, b)
		require.Equal(t, fmt.Sprintf("%d", i), b.key)
	}
	require.Equal(t, 50, ht.Len())

	// Find existing.
	b, created := ht.FindOrInsert(int64(25))
	require.False(t, created)
	require.NotNil(t, b)
	require.Equal(t, "25", b.key)

	// Iterate and count.
	count := 0
	ht.Iterate(func(key any, bucket *groupBucket) bool {
		count++
		return true
	})
	require.Equal(t, 50, count)

	// Reset.
	ht.Reset()
	require.Equal(t, 0, ht.Len())
}

func TestF3_FixedHashTableU64_HashCollision(t *testing.T) {
	ht := newFixedHashTableU64(16, nil)
	// Keys that collide in the same bucket (power-of-2 mask).
	keys := []int64{0, 16, 32, 48}
	for _, k := range keys {
		ht.FindOrInsert(k)
	}
	require.Equal(t, len(keys), ht.Len())

	// All keys should be findable.
	for _, k := range keys {
		b, created := ht.FindOrInsert(k)
		require.False(t, created, "key %d should exist", k)
		require.NotNil(t, b)
	}
}

func TestF3_StringHashTable(t *testing.T) {
	ht := newStringHashTable(100, []AggDef{{Name: "count", Type: "count", Params: map[string]any{}}})

	// Insert string keys.
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("key_%d", i)
		b, created := ht.FindOrInsert(key)
		require.True(t, created, "insert %s should be new", key)
		require.NotNil(t, b)
		require.Equal(t, key, b.key)
	}
	require.Equal(t, 50, ht.Len())

	// Find existing.
	b, created := ht.FindOrInsert("key_25")
	require.False(t, created)
	require.NotNil(t, b)
	require.Equal(t, "key_25", b.key)

	// Iterate.
	count := 0
	ht.Iterate(func(key any, bucket *groupBucket) bool {
		count++
		return true
	})
	require.Equal(t, 50, count)

	// Reset.
	ht.Reset()
	require.Equal(t, 0, ht.Len())
}

func TestF3_StringHashTable_HashCollision(t *testing.T) {
	ht := newStringHashTable(16, nil)
	// Insert many keys to force collisions and growth.
	for i := 0; i < 100; i++ {
		ht.FindOrInsert(fmt.Sprintf("collision_key_%d", i))
	}
	require.Equal(t, 100, ht.Len())

	// All keys should be findable.
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("collision_key_%d", i)
		b, created := ht.FindOrInsert(key)
		require.False(t, created, "key %s should exist", key)
		require.NotNil(t, b)
	}
}

func TestF3_SelectHashTable(t *testing.T) {
	defs := []AggDef{{Name: "count", Type: "count", Params: map[string]any{}}}

	// Small data → Go map.
	ht := SelectHashTable(meta.TypeKeyword, false, 500, defs)
	require.NotNil(t, ht)
	require.IsType(t, &goMapBackend{}, ht)

	// Large data, long → FixedHashTableU64.
	ht = SelectHashTable(meta.TypeLong, false, 10000, defs)
	require.NotNil(t, ht)
	require.IsType(t, &fixedHashTableU64{}, ht)

	// Large data, keyword → StringHashTable.
	ht = SelectHashTable(meta.TypeKeyword, false, 10000, defs)
	require.NotNil(t, ht)
	require.IsType(t, &stringHashTable{}, ht)

	// Composite → Go map.
	ht = SelectHashTable(meta.TypeLong, true, 10000, defs)
	require.NotNil(t, ht)
	require.IsType(t, &goMapBackend{}, ht)
}

func TestF3_GroupAggregatorWithHashTable(t *testing.T) {
	defs := []AggDef{{Name: "count", Type: "count", Params: map[string]any{}}}
	agg := NewGroupAggregatorWithKeyType("category", meta.TypeKeyword, defs, 1000)

	// Collect documents into groups.
	agg.Collect("electronics", map[string]any{"count": 1})
	agg.Collect("books", map[string]any{"count": 1})
	agg.Collect("electronics", map[string]any{"count": 1})
	agg.Collect("electronics", map[string]any{"count": 1})
	agg.Collect("books", map[string]any{"count": 1})

	buckets := agg.Buckets()
	require.Len(t, buckets, 2)

	// Electronics should have higher doc count.
	require.Equal(t, "electronics", buckets[0].Key)
	require.Equal(t, float64(3), buckets[0].DocCount)

	require.Equal(t, "books", buckets[1].Key)
	require.Equal(t, float64(2), buckets[1].DocCount)
}
