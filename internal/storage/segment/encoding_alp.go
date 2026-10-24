package segment

import (
	"fmt"
	"math"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// ALP (Adaptive Lossless floating-Point) encoding converts double values
// to integers by finding the best power-of-10 multiplier, then applies
// delta + bitpack. Effective for float columns where Gorilla XOR encoding
// has poor compression. @spec: 02_spec.md#3.5
//
// Payload:
//
//	null header | u8 exponent | u8 bit_width | i64 first_value |
//	bitpacked zigzag deltas (n-1 values)
//
// Null rows carry delta 0 and do not update running values.

// alpCodec implements BlockCodec for ALP encoding.
type alpCodec struct{}

func (c *alpCodec) ID() uint8 { return CodecIDALP }

func (c *alpCodec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return encodeALPBlock(data)
}

func (c *alpCodec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return decodeALPBlock(payload, nrows)
}

// findALPExponent finds the smallest exponent e (0..18) such that all
// non-null values * 10^e are within int64 range and round-trip losslessly.
// Returns -1 if no suitable exponent is found (caller falls back to Gorilla).
func findALPExponent(vals []float64, nulls []uint64, nullCount int, n int) (int, bool) {
	for e := 0; e <= 18; e++ {
		factor := math.Pow(10, float64(e))
		ok := true
		for i := 0; i < n; i++ {
			if nullCount > 0 && nullBitIsSet(nulls, i) {
				continue
			}
			scaled := vals[i] * factor
			if math.IsInf(scaled, 0) || math.IsNaN(scaled) {
				ok = false
				break
			}
			rounded := math.Round(scaled)
			if math.Abs(scaled-rounded) > 1e-9 {
				ok = false
				break
			}
			iv := int64(rounded)
			if iv > math.MaxInt64 || iv < math.MinInt64 {
				ok = false
				break
			}
			// Verify exact round-trip: decode(encode(v)) == v.
			if float64(iv)/factor != vals[i] {
				ok = false
				break
			}
		}
		if ok {
			return e, true
		}
	}
	return -1, false
}

func encodeALPBlock(data *BlockData) ([]byte, error) {
	if data.doubles == nil {
		return nil, fmt.Errorf("segment: ALP encoding requires a double vector")
	}
	n := data.Rows
	out := appendNullPrefix(nil, data)

	// Find best exponent. If no lossless exponent exists, return an error
	// so the caller can fall back to a different encoding (e.g. Gorilla).
	e, ok := findALPExponent(data.doubles, data.nulls, data.nullCount, n)
	if !ok {
		return nil, fmt.Errorf("segment: ALP encoding: no lossless exponent found for %d values", n)
	}

	factor := math.Pow(10, float64(e))

	// Convert to int64 and compute deltas (like deltaCore but for floats).
	v0 := int64(0)
	started := false
	prev := int64(0)
	deltas := make([]int64, n)
	maxZig := uint64(0)

	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			continue
		}
		v := int64(math.Round(data.doubles[i] * factor))
		if !started {
			v0, prev, started = v, v, true
			continue
		}
		deltas[i] = v - prev
		if z := zigzag(deltas[i]); z > maxZig {
			maxZig = z
		}
		prev = v
	}

	w := bitWidth(maxZig)

	// Pack deltas for rows 1..n-1.
	zig := make([]uint64, n-1)
	for i := 1; i < n; i++ {
		zig[i-1] = zigzag(deltas[i])
	}

	out = append(out, byte(e))
	out = append(out, byte(w))
	out = appendU64LE(out, uint64(v0))
	out = append(out, bitpackEncode(zig, w)...)
	return out, nil
}

func decodeALPBlock(payload []byte, nrows int) (*BlockData, error) {
	nulls, nullCount, off, err := readNullPrefix(payload, 0)
	if err != nil {
		return nil, err
	}
	// Need: u8 exponent + u8 bit_width + i64 first_value = 10 bytes.
	if off+10 > len(payload) {
		return nil, fmt.Errorf("segment: truncated ALP header")
	}
	e := int(payload[off])
	off++
	w := int(payload[off])
	off++
	if w < 0 || w > 64 {
		return nil, fmt.Errorf("segment: invalid ALP bit width %d", w)
	}
	v0 := int64(readU64LE(payload, off))
	off += 8

	// Decode packed deltas for rows 1..nrows-1.
	numPacked := nrows - 1
	if numPacked < 0 {
		numPacked = 0
	}
	zig := bitpackDecode(payload[off:], numPacked, w)

	data := newBlockData(meta.TypeDouble, nrows)
	factor := math.Pow(10, float64(e))
	running := v0
	started := false

	for i := 0; i < nrows; i++ {
		if nullCount > 0 && nullBitIsSet(nulls, i) {
			data.appendNull()
			continue
		}
		if !started {
			started = true
			running = v0
		} else if i > 0 {
			running += unzigzag(zig[i-1])
		}
		data.doubles = append(data.doubles, float64(running)/factor)
		data.Rows++
	}
	return data, nil
}
