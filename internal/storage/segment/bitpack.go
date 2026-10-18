package segment

import (
	"encoding/binary"
	"math/bits"
)

// bitMask returns the low-w-bits mask; w must be in [0, 64].
func bitMask(w int) uint64 {
	if w >= 64 {
		return ^uint64(0)
	}
	return (uint64(1) << uint(w)) - 1
}

// bitWidth returns the number of bits needed to represent v (0 for v == 0).
func bitWidth(v uint64) int {
	return 64 - bits.LeadingZeros64(v)
}

// bitpackEncode packs n values of width w bits into a byte stream,
// LSB-first, with no padding between values (except the trailing word).
// The output length is 8 * ceil(n*w/64) bytes.
func bitpackEncode(vals []uint64, w int) []byte {
	if w == 0 || len(vals) == 0 {
		return nil
	}
	words := make([]uint64, (len(vals)*w+63)/64)
	bit := 0
	for _, v := range vals {
		lo := bit & 63
		wi := bit >> 6
		words[wi] |= (v & bitMask(w)) << uint(lo)
		if lo+w > 64 {
			words[wi+1] |= (v & bitMask(w)) >> uint(64-lo)
		}
		bit += w
	}
	out := make([]byte, len(words)*8)
	for i, word := range words {
		binary.LittleEndian.PutUint64(out[i*8:], word)
	}
	return out
}

// bitpackDecode is the inverse of bitpackEncode; n values of width w bits.
func bitpackDecode(b []byte, n int, w int) []uint64 {
	vals := make([]uint64, n)
	if w == 0 || n == 0 {
		return vals
	}
	nw := (n*w + 63) / 64
	if len(b) < nw*8 {
		nw = len(b) / 8 // truncated input decodes garbage zeros; caller validates length upstream
	}
	words := make([]uint64, nw)
	for i := range words {
		words[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	bit := 0
	mask := bitMask(w)
	for i := 0; i < n; i++ {
		lo := bit & 63
		wi := bit >> 6
		v := words[wi] >> uint(lo)
		if lo+w > 64 && wi+1 < len(words) {
			v |= words[wi+1] << uint(64-lo)
		}
		vals[i] = v & mask
		bit += w
	}
	return vals
}

// zigzag / unzigzag for signed deltas.
func zigzag(v int64) uint64 {
	return uint64(v<<1) ^ uint64(v>>63)
}

func unzigzag(u uint64) int64 {
	return int64(u>>1) ^ -int64(u&1)
}

// null bitmap helpers: []uint64 words, bit i set = row i is null.

func nullBitmapSet(bm []uint64, i int) []uint64 {
	need := i/64 + 1
	for len(bm) < need {
		bm = append(bm, 0)
	}
	bm[i/64] |= 1 << uint(i%64)
	return bm
}

func nullBitIsSet(bm []uint64, i int) bool {
	w := i / 64
	if w >= len(bm) {
		return false // bitmap only covers rows up to the highest null
	}
	return bm[w]&(1<<uint(i%64)) != 0
}

// appendU32LE appends v little-endian.
func appendU32LE(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

// appendU64LE appends v little-endian.
func appendU64LE(b []byte, v uint64) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(b, tmp[:]...)
}

// readU32LE reads a little-endian u32 at offset off.
func readU32LE(b []byte, off int) uint32 {
	return binary.LittleEndian.Uint32(b[off:])
}

// readU64LE reads a little-endian u64 at offset off.
func readU64LE(b []byte, off int) uint64 {
	return binary.LittleEndian.Uint64(b[off:])
}
