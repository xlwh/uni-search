package segment

import (
	"encoding/binary"
	"fmt"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// T64 encoding transposes 64-bit integers by byte position (8 columns),
// then bitpacks each column independently. Effective when some byte
// positions have low variance (e.g., high bytes are constant for small
// positive numbers). @spec: 02_spec.md#3.5
//
// Payload:
//
//	null header | u8 min_bytes[8] | u8 bit_widths[8] |
//	concatenated bitpacked columns (each column has nrows values)

// t64Codec implements BlockCodec for T64 encoding.
type t64Codec struct{}

func (c *t64Codec) ID() uint8 { return CodecIDT64 }

func (c *t64Codec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return encodeT64Block(data)
}

func (c *t64Codec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return decodeT64Block(payload, nrows)
}

func encodeT64Block(data *BlockData) ([]byte, error) {
	if data.longs == nil {
		return nil, fmt.Errorf("segment: T64 encoding requires a long vector")
	}
	n := data.Rows
	out := appendNullPrefix(nil, data)

	// Extract bytes per column, skipping null rows (null rows get min value).
	var minBytes [8]byte
	var maxBytes [8]byte
	for b := 0; b < 8; b++ {
		minBytes[b] = 0xFF
	}
	// Collect non-null values' bytes.
	cols := make([][8]byte, n)
	hasNonNull := false
	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			continue
		}
		hasNonNull = true
		var tmp [8]byte
		binary.LittleEndian.PutUint64(tmp[:], uint64(data.longs[i]))
		for b := 0; b < 8; b++ {
			cols[i][b] = tmp[b]
			if tmp[b] < minBytes[b] {
				minBytes[b] = tmp[b]
			}
			if tmp[b] > maxBytes[b] {
				maxBytes[b] = tmp[b]
			}
		}
	}
	if !hasNonNull {
		// All nulls: write zeros.
		out = append(out, minBytes[:]...)
		out = append(out, make([]byte, 8)...) // bit_widths all 0
		return out, nil
	}

	// Compute bit widths per column (after subtracting min).
	var bitWidths [8]int
	for b := 0; b < 8; b++ {
		rng := uint64(maxBytes[b]) - uint64(minBytes[b])
		bitWidths[b] = bitWidth(rng)
	}

	// Write header: min_bytes[8] | bit_widths[8].
	out = append(out, minBytes[:]...)
	for b := 0; b < 8; b++ {
		out = append(out, byte(bitWidths[b]))
	}

	// Pack each column: for each byte position, collect (byte - min) values
	// for all rows (null rows contribute 0), then bitpack.
	for b := 0; b < 8; b++ {
		if bitWidths[b] == 0 {
			continue // All bytes in this column are the same (== min).
		}
		vals := make([]uint64, n)
		for i := 0; i < n; i++ {
			if data.IsNull(i) {
				vals[i] = 0
			} else {
				vals[i] = uint64(cols[i][b]) - uint64(minBytes[b])
			}
		}
		out = append(out, bitpackEncode(vals, bitWidths[b])...)
	}
	return out, nil
}

func decodeT64Block(payload []byte, nrows int) (*BlockData, error) {
	nulls, nullCount, off, err := readNullPrefix(payload, 0)
	if err != nil {
		return nil, err
	}
	// Need 16 bytes: 8 min_bytes + 8 bit_widths.
	if off+16 > len(payload) {
		return nil, fmt.Errorf("segment: truncated T64 header")
	}
	var minBytes [8]byte
	copy(minBytes[:], payload[off:off+8])
	off += 8
	var bitWidths [8]int
	for b := 0; b < 8; b++ {
		bitWidths[b] = int(payload[off])
		off++
	}

	// Decode each column.
	cols := make([][8]byte, nrows)
	for b := 0; b < 8; b++ {
		if bitWidths[b] == 0 {
			// All values are min_bytes[b].
			for i := 0; i < nrows; i++ {
				cols[i][b] = minBytes[b]
			}
			continue
		}
		numBytes := (nrows*bitWidths[b] + 63) / 64 * 8
		if off+numBytes > len(payload) {
			return nil, fmt.Errorf("segment: truncated T64 column %d", b)
		}
		vals := bitpackDecode(payload[off:], nrows, bitWidths[b])
		off += numBytes
		for i := 0; i < nrows; i++ {
			cols[i][b] = byte(vals[i]) + minBytes[b]
		}
	}

	// Reconstruct long values.
	data := newBlockData(meta.TypeLong, nrows)
	for i := 0; i < nrows; i++ {
		if nullCount > 0 && nullBitIsSet(nulls, i) {
			data.appendNull()
			continue
		}
		v := binary.LittleEndian.Uint64(cols[i][:])
		data.longs = append(data.longs, int64(v))
		data.Rows++
	}
	return data, nil
}
