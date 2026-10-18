package inverted

import (
	"fmt"
)

// FOR (Frame-of-Reference) bitpacking helpers (§3.2: tf and fldLen are
// stored as FOR-compressed integer arrays). A block of N integers is
// encoded by selecting the minimum bit width that fits every value, then
// packing the values bit-by-bit into a byte slice. The bit width is
// stored as a single leading byte, so decoding is self-describing.
//
// M0 keeps the scheme simple: u64 values, no per-block frame offset
// beyond the bit width. The M1 WAND path can extend this with block-max
// upper bounds (§3.4) without changing the encoding of the values
// themselves.

// bitWidth returns the minimum number of bits needed to represent every
// value in vals. 0 when vals is empty; 0 when every value is 0.
func bitWidth(vals []uint64) uint8 {
	var max uint64
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	w := 0
	for max > 0 {
		w++
		max >>= 1
	}
	return uint8(w)
}

// forEncode bitpacks vals using the minimum bit width. The output is
//
//	[1 byte bitWidth][ceil(N*width/8) bytes packed values]
//
// little-endian bit order within each byte (bit 0 = LSB).
func forEncode(vals []uint64) []byte {
	w := bitWidth(vals)
	if w == 0 {
		return []byte{0}
	}
	packed := make([]byte, (len(vals)*int(w)+7)/8)
	for i, v := range vals {
		bit := uint(i) * uint(w)
		for b := uint(0); b < uint(w); b++ {
			if v&(1<<b) != 0 {
				packed[bit/8] |= 1 << (bit % 8)
			}
			bit++
		}
	}
	return append([]byte{w}, packed...)
}

// forDecode is the inverse of forEncode.
func forDecode(data []byte, count int) ([]uint64, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("inverted: empty FOR block")
	}
	w := uint(data[0])
	if w == 0 {
		if count > 0 {
			out := make([]uint64, count)
			return out, nil
		}
		return nil, nil
	}
	need := (count*int(w) + 7) / 8
	if len(data)-1 < need {
		return nil, fmt.Errorf("inverted: FOR block truncated: have %d bytes, need %d", len(data)-1, need)
	}
	packed := data[1:]
	out := make([]uint64, count)
	for i := 0; i < count; i++ {
		bit := uint(i) * w
		var v uint64
		for b := uint(0); b < w; b++ {
			if packed[bit/8]&(1<<(bit%8)) != 0 {
				v |= 1 << b
			}
			bit++
		}
		out[i] = v
	}
	return out, nil
}

// forEncodeU8 wraps forEncode for []uint8 inputs (tf values, §3.2).
func forEncodeU8(vals []uint8) []byte {
	tmp := make([]uint64, len(vals))
	for i, v := range vals {
		tmp[i] = uint64(v)
	}
	return forEncode(tmp)
}

// forDecodeU8 decodes a []uint8 view of a FOR block of length count.
func forDecodeU8(data []byte, count int) ([]uint8, error) {
	out, err := forDecode(data, count)
	if err != nil {
		return nil, err
	}
	res := make([]uint8, len(out))
	for i, v := range out {
		res[i] = uint8(v)
	}
	return res, nil
}

// forEncodeU16 wraps forEncode for []uint16 inputs (fldLen buckets and
// positions, §3.2).
func forEncodeU16(vals []uint16) []byte {
	tmp := make([]uint64, len(vals))
	for i, v := range vals {
		tmp[i] = uint64(v)
	}
	return forEncode(tmp)
}

// forDecodeU16 decodes a []uint16 view of a FOR block of length count.
func forDecodeU16(data []byte, count int) ([]uint16, error) {
	out, err := forDecode(data, count)
	if err != nil {
		return nil, err
	}
	res := make([]uint16, len(out))
	for i, v := range out {
		res[i] = uint16(v)
	}
	return res, nil
}

// appendU32LE appends a little-endian uint32 to dst and returns the
// extended slice.
func appendU32LE(dst []byte, v uint32) []byte {
	return append(dst,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// appendU64LE appends a little-endian uint64 to dst.
func appendU64LE(dst []byte, v uint64) []byte {
	return append(dst,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}
