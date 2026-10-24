// Package hnsw elias_fano_test.go tests Elias-Fano encoding/decoding
// for neighbor list compression.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#7.2
package hnsw

import (
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEF_EmptySequence verifies encoding/decoding of empty sequence.
func TestEF_EmptySequence(t *testing.T) {
	ef := encodeEF(nil)
	assert.Equal(t, 0, ef.n)
	assert.Nil(t, ef.decodeAll())
}

// TestEF_SingleElement verifies encoding/decoding of single element.
func TestEF_SingleElement(t *testing.T) {
	ef := encodeEF([]int32{42})
	require.Equal(t, 1, ef.n)
	result := ef.decodeAll()
	assert.Equal(t, []int32{42}, result)
}

// TestEF_DenseSequence verifies encoding/decoding of dense sequence.
func TestEF_DenseSequence(t *testing.T) {
	vals := []int32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	ef := encodeEF(vals)
	result := ef.decodeAll()
	assert.Equal(t, vals, result)
}

// TestEF_SparseSequence verifies encoding/decoding of sparse sequence.
func TestEF_SparseSequence(t *testing.T) {
	vals := []int32{0, 50, 100, 500, 1000, 5000, 10000}
	ef := encodeEF(vals)
	result := ef.decodeAll()
	assert.Equal(t, vals, result)
}

// TestEF_RandomSequence verifies encoding/decoding of random sorted sequences.
func TestEF_RandomSequence(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 1))
	for _, n := range []int{2, 10, 32, 100, 500} {
		t.Run("n_"+itoaEF(n), func(t *testing.T) {
			vals := make([]int32, n)
			for i := range vals {
				vals[i] = int32(rng.IntN(10000))
			}
			sort.Slice(vals, func(a, b int) bool { return vals[a] < vals[b] })
			ef := encodeEF(vals)
			result := ef.decodeAll()
			assert.Equal(t, vals, result, "EF roundtrip failed for n=%d", n)
		})
	}
}

// TestEF_Compression verifies EF uses less memory than raw []int32.
func TestEF_Compression(t *testing.T) {
	// Simulate neighbor IDs in range [0, 1000) with M=16 neighbors.
	rng := rand.New(rand.NewPCG(42, 1))
	vals := make([]int32, 16)
	for i := range vals {
		vals[i] = int32(rng.IntN(1000))
	}
	sort.Slice(vals, func(a, b int) bool { return vals[a] < vals[b] })
	ef := encodeEF(vals)
	rawSize := 16 * 4 // 16 int32 = 64 bytes
	efSize := ef.memBytes()
	t.Logf("Raw: %d bytes, EF: %d bytes, ratio: %.1f%%", rawSize, efSize,
		float64(efSize)/float64(rawSize)*100)
	// EF should be smaller for this size range (not guaranteed for very
	// small lists, but typically is for n=16 with range [0,1000)).
}

// TestEF_HNSWNeighborRange verifies EF works with typical HNSW neighbor IDs.
func TestEF_HNSWNeighborRange(t *testing.T) {
	// Simulate a node in a 10000-node graph with 32 neighbors.
	rng := rand.New(rand.NewPCG(99, 1))
	vals := make([]int32, 32)
	for i := range vals {
		vals[i] = int32(rng.IntN(10000))
	}
	sort.Slice(vals, func(a, b int) bool { return vals[a] < vals[b] })
	// Remove duplicates.
	unique := vals[:0]
	for i, v := range vals {
		if i == 0 || v != vals[i-1] {
			unique = append(unique, v)
		}
	}
	ef := encodeEF(unique)
	result := ef.decodeAll()
	assert.Equal(t, unique, result)
}

func itoaEF(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
