package memtable

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.1
func TestDedupWindow_Basic(t *testing.T) {
	w := NewDedupWindow(100)

	// First write: not a duplicate
	assert.False(t, w.IsDuplicate("client-1", 1))
	// Same client, same seq: duplicate
	assert.True(t, w.IsDuplicate("client-1", 1))
	// Same client, lower seq: duplicate
	assert.True(t, w.IsDuplicate("client-1", 0))
	// Same client, higher seq: not a duplicate
	assert.False(t, w.IsDuplicate("client-1", 2))
	// Same client, same as new max: duplicate
	assert.True(t, w.IsDuplicate("client-1", 2))
}

func TestDedupWindow_EmptyClientID(t *testing.T) {
	w := NewDedupWindow(100)
	// Empty client ID: dedup disabled, never duplicate
	assert.False(t, w.IsDuplicate("", 1))
	assert.False(t, w.IsDuplicate("", 1))
}

func TestDedupWindow_LRU_Eviction(t *testing.T) {
	w := NewDedupWindow(3)
	w.IsDuplicate("a", 1)
	w.IsDuplicate("b", 1)
	w.IsDuplicate("c", 1)
	// Window full (3). Add "d" → evicts "a".
	w.IsDuplicate("d", 1)
	// "a" should have been evicted: re-adding is not a duplicate.
	assert.False(t, w.IsDuplicate("a", 1), "evicted client should not be duplicate")
	// "b" should have been evicted by "a" re-add.
	// "c" should still be in the window.
	assert.True(t, w.IsDuplicate("c", 1), "c should still be in window")
}

func TestDedupWindow_MultipleClients(t *testing.T) {
	w := NewDedupWindow(100)
	// Different clients are independent
	assert.False(t, w.IsDuplicate("client-1", 1))
	assert.False(t, w.IsDuplicate("client-2", 1))
	assert.True(t, w.IsDuplicate("client-1", 1))
	assert.False(t, w.IsDuplicate("client-2", 2))
	assert.True(t, w.IsDuplicate("client-2", 2))
}
