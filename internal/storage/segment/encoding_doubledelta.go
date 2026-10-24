package segment

import (
	"fmt"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// DoubleDelta encoding for long/date/ip blocks with near-constant step
// size (e.g., evenly-spaced timestamps). Computes second-order deltas:
// dd[i] = (v[i]-v[i-1]) - (v[i-1]-v[i-2]) = v[i] - 2*v[i-1] + v[i-2].
// @spec: 02_spec.md#3.5
//
// Payload:
//
//	null header | i64 first_value | i64 first_delta | u8 bit_width |
//	bitpacked zigzag double-deltas (n-2 values)
//
// Null rows carry double-delta 0 and do not update running values.

// doubleDeltaCodec implements BlockCodec for DoubleDelta encoding.
type doubleDeltaCodec struct{}

func (c *doubleDeltaCodec) ID() uint8 { return CodecIDDoubleDelta }

func (c *doubleDeltaCodec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return encodeDoubleDeltaBlock(data)
}

func (c *doubleDeltaCodec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return decodeDoubleDeltaBlock(payload, nrows)
}

func encodeDoubleDeltaBlock(data *BlockData) ([]byte, error) {
	if data.longs == nil {
		return nil, fmt.Errorf("segment: double-delta encoding requires a long vector")
	}
	n := data.Rows
	out := appendNullPrefix(nil, data)

	// Handle small blocks (0 or 1 rows): no double-deltas to pack.
	if n < 2 {
		// Store first_value (0 if no rows), no first_delta, no packed data.
		v0 := int64(0)
		if n == 1 && !data.IsNull(0) {
			v0 = data.longs[0]
		}
		out = appendU64LE(out, uint64(v0))
		out = appendU64LE(out, 0) // first_delta = 0
		out = append(out, 0)      // bit_width = 0
		return out, nil
	}

	// Compute first non-null value and first delta.
	v0 := int64(0)
	firstDelta := int64(0)
	started := 0 // count of non-null values seen
	prev1 := int64(0)
	prevDelta := int64(0)

	doubleDeltas := make([]int64, n) // doubleDeltas[0] and [1] are implicit
	maxZig := uint64(0)

	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			continue // double-delta stays 0
		}
		v := data.longs[i]
		switch started {
		case 0:
			v0 = v
			started = 1
		case 1:
			firstDelta = v - v0
			prev1 = v
			prevDelta = firstDelta
			started = 2
		default:
			delta := v - prev1
			dd := delta - prevDelta
			doubleDeltas[i] = dd
			if z := zigzag(dd); z > maxZig {
				maxZig = z
			}
			prev1 = v
			prevDelta = delta
		}
	}

	w := bitWidth(maxZig)

	// Pack double-deltas for rows 2..n-1.
	zig := make([]uint64, n-2)
	for i := 2; i < n; i++ {
		zig[i-2] = zigzag(doubleDeltas[i])
	}

	out = appendU64LE(out, uint64(v0))
	out = appendU64LE(out, uint64(firstDelta))
	out = append(out, byte(w))
	out = append(out, bitpackEncode(zig, w)...)
	return out, nil
}

func decodeDoubleDeltaBlock(payload []byte, nrows int) (*BlockData, error) {
	nulls, nullCount, off, err := readNullPrefix(payload, 0)
	if err != nil {
		return nil, err
	}
	// Need at least: i64 first_value + i64 first_delta + u8 bit_width = 17 bytes.
	if off+17 > len(payload) {
		return nil, fmt.Errorf("segment: truncated double-delta header")
	}
	v0 := int64(readU64LE(payload, off))
	off += 8
	firstDelta := int64(readU64LE(payload, off))
	off += 8
	w := int(payload[off])
	off += 1
	if w < 0 || w > 64 {
		return nil, fmt.Errorf("segment: invalid double-delta bit width %d", w)
	}

	// Decode packed double-deltas for rows 2..nrows-1.
	numPacked := nrows - 2
	if numPacked < 0 {
		numPacked = 0
	}
	zig := bitpackDecode(payload[off:], numPacked, w)

	data := newBlockData(meta.TypeLong, nrows)
	running := v0
	prevDelta := firstDelta
	started := 0 // count of non-null values reconstructed

	for i := 0; i < nrows; i++ {
		if nullCount > 0 && nullBitIsSet(nulls, i) {
			data.appendNull()
			continue
		}
		switch started {
		case 0:
			running = v0
			started = 1
		case 1:
			running = v0 + firstDelta
			prevDelta = firstDelta
			started = 2
		default:
			dd := unzigzag(zig[i-2])
			delta := prevDelta + dd
			running = running + delta
			prevDelta = delta
		}
		data.longs = append(data.longs, running)
		data.Rows++
	}
	return data, nil
}
