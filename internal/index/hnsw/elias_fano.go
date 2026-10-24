// Package hnsw elias_fano.go implements Elias-Fano encoding for
// compressing HNSW level-0 neighbor lists (inspired by vsag's
// compressed_graph_datacell.h). Upper levels use raw []int32; level 0
// (the densest layer) uses Elias-Fano for ~30-50% memory reduction.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.5
package hnsw

import (
	"encoding/binary"
	"math/bits"
)

// efLevelData stores per-node Elias-Fano encoded neighbor lists for
// level 0. Upper levels use the existing CSR []int32 layout.
type efLevelData struct {
	data []*eliasFano // one per node (level 0 only)
}

// eliasFano encodes a monotonically increasing int32 sequence using
// Elias-Fano coding: low l bits packed via bitpack, high bits via
// unary (bitmap).
type eliasFano struct {
	lowBits    int    // l = floor(log2(upperBound/n))
	lowData    []byte // bitpacked low parts
	highData   []byte // unary-encoded high parts (bitmap)
	n          int    // number of elements
	upperBound int32  // max value
}

// encodeEF builds an Elias-Fano encoding for a sorted int32 sequence.
func encodeEF(vals []int32) *eliasFano {
	if len(vals) == 0 {
		return &eliasFano{n: 0}
	}
	n := len(vals)
	upper := vals[n-1]
	u := uint64(upper)
	l := 0
	if u > 0 && n > 0 {
		l = bits.Len64(u / uint64(n))
		if l > 31 {
			l = 31
		}
	}
	// Low parts: n values of l bits each, packed LSB-first.
	lowVals := make([]uint64, n)
	for i, v := range vals {
		if l > 0 {
			lowVals[i] = uint64(v) & ((1 << uint(l)) - 1)
		}
	}
	var lowData []byte
	if l > 0 {
		lowData = efBitpackEncode(lowVals, l)
	}
	// High parts: unary encoding as bitmap.
	// high[i] = vals[i] >> l; position = high[i] + i in the bitmap.
	highBitLen := int(u>>uint(l)) + n + 1
	if highBitLen < 0 {
		highBitLen = 0
	}
	highWords := make([]uint64, (highBitLen+63)/64)
	for i, v := range vals {
		hi := uint64(v) >> uint(l)
		pos := hi + uint64(i)
		if pos>>6 < uint64(len(highWords)) {
			highWords[pos>>6] |= 1 << (pos & 63)
		}
	}
	highData := make([]byte, len(highWords)*8)
	for i, w := range highWords {
		binary.LittleEndian.PutUint64(highData[i*8:], w)
	}
	return &eliasFano{
		lowBits:    l,
		lowData:    lowData,
		highData:   highData,
		n:          n,
		upperBound: upper,
	}
}

// decodeAll decodes the entire sequence.
func (ef *eliasFano) decodeAll() []int32 {
	if ef.n == 0 {
		return nil
	}
	result := make([]int32, ef.n)
	var lowVals []uint64
	if ef.lowBits > 0 {
		lowVals = efBitpackDecode(ef.lowData, ef.n, ef.lowBits)
	} else {
		lowVals = make([]uint64, ef.n)
	}
	// Walk high bitmap: each set bit at position p gives
	// high = p - index, value = (high << l) | low[index]
	idx := 0
	for pos := 0; pos < len(ef.highData)*8 && idx < ef.n; pos++ {
		byteIdx := pos >> 3
		bitIdx := pos & 7
		if ef.highData[byteIdx]&(1<<uint(bitIdx)) != 0 {
			hi := int32(pos - idx)
			result[idx] = (hi << uint(ef.lowBits)) | int32(lowVals[idx])
			idx++
		}
	}
	return result
}

// memBytes reports the encoded size.
func (ef *eliasFano) memBytes() int {
	return len(ef.lowData) + len(ef.highData)
}

// efBitpackEncode packs n values of width w bits into a byte stream,
// LSB-first. (Same algorithm as segment.bitpackEncode but local to
// avoid cross-package dependency on unexported functions.)
func efBitpackEncode(vals []uint64, w int) []byte {
	if w == 0 || len(vals) == 0 {
		return nil
	}
	words := make([]uint64, (len(vals)*w+63)/64)
	bit := 0
	for _, v := range vals {
		mask := uint64(0)
		if w >= 64 {
			mask = ^uint64(0)
		} else {
			mask = (1 << uint(w)) - 1
		}
		val := v & mask
		lo := bit & 63
		wi := bit >> 6
		words[wi] |= val << uint(lo)
		if lo+w > 64 && wi+1 < len(words) {
			words[wi+1] |= val >> uint(64-lo)
		}
		bit += w
	}
	out := make([]byte, len(words)*8)
	for i, word := range words {
		binary.LittleEndian.PutUint64(out[i*8:], word)
	}
	return out
}

// efBitpackDecode is the inverse of efBitpackEncode.
func efBitpackDecode(b []byte, n, w int) []uint64 {
	vals := make([]uint64, n)
	if w == 0 || n == 0 {
		return vals
	}
	nw := (n*w + 63) / 64
	if len(b) < nw*8 {
		nw = len(b) / 8
	}
	words := make([]uint64, nw)
	for i := range words {
		words[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	mask := uint64(0)
	if w >= 64 {
		mask = ^uint64(0)
	} else {
		mask = (1 << uint(w)) - 1
	}
	bit := 0
	for i := 0; i < n; i++ {
		lo := bit & 63
		wi := bit >> 6
		if wi >= len(words) {
			break
		}
		val := words[wi] >> uint(lo)
		if lo+w > 64 && wi+1 < len(words) {
			val |= words[wi+1] << uint(64-lo)
		}
		vals[i] = val & mask
		bit += w
	}
	return vals
}
