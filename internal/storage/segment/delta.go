package segment

import (
	"fmt"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// Delta encodings (EncodingDeltaBitpack / EncodingDeltaZstd) for
// long/date/ip blocks.
//
// Core section (shared by both):
//
//	i64 first_value | u8 bit_width | bitpacked zigzag deltas (n-1 values)
//
// The first non-null row carries an implicit delta of 0 relative to
// first_value; null rows carry delta 0 and do not update the running value.
//
// DELTA_BITPACK payload: null header | core section
// DELTA_ZSTD payload:    null header | u32 frame_len | zstd(core section)
//
// (see api/proto/segment.proto)

// deltaCore computes the deltas and packs the core section.
func deltaCore(data *BlockData) []byte {
	n := data.Rows
	v0 := int64(0)
	started := false
	prev := int64(0)
	deltas := make([]int64, n) // deltas[0] is implicit 0, never packed
	maxZig := uint64(0)
	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			continue // delta stays 0
		}
		v := data.longs[i]
		if !started {
			v0, prev, started = v, v, true
			continue // delta[0] = 0
		}
		deltas[i] = v - prev
		if z := zigzag(deltas[i]); z > maxZig {
			maxZig = z
		}
		prev = v
	}
	w := bitWidth(maxZig)

	zig := make([]uint64, n-1)
	for i := 1; i < n; i++ {
		zig[i-1] = zigzag(deltas[i])
	}
	out := appendU64LE(nil, uint64(v0))
	out = append(out, byte(w))
	out = append(out, bitpackEncode(zig, w)...)
	return out
}

func encodeDeltaBitpackBlock(data *BlockData) ([]byte, error) {
	if data.longs == nil {
		return nil, fmt.Errorf("segment: delta encoding requires a long vector")
	}
	out := appendNullPrefix(nil, data)
	return append(out, deltaCore(data)...), nil
}

func encodeDeltaZstdBlock(data *BlockData) ([]byte, error) {
	if data.longs == nil {
		return nil, fmt.Errorf("segment: delta encoding requires a long vector")
	}
	core := deltaCore(data)
	frame, err := zstdCompress(core)
	if err != nil {
		return nil, err
	}
	out := appendNullPrefix(nil, data)
	out = appendU32LE(out, uint32(len(frame)))
	out = append(out, frame...)
	return out, nil
}

// deltaExpand reconstructs the long vector from the core section.
func deltaExpand(core []byte, nulls []uint64, nullCount uint32, nrows int) (*BlockData, error) {
	if len(core) < 9 { // v0(8) + bit_width(1)
		return nil, fmt.Errorf("segment: truncated delta core section")
	}
	v0 := int64(readU64LE(core, 0))
	w := int(core[8])
	if w < 0 || w > 64 {
		return nil, fmt.Errorf("segment: invalid delta bit width %d", w)
	}
	if len(core)-9 < ((nrows-1)*w+63)/64*8 {
		return nil, fmt.Errorf("segment: truncated delta bitpack section")
	}
	zig := bitpackDecode(core[9:], nrows-1, w)

	data := newBlockData(meta.TypeLong, nrows)
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
		data.longs = append(data.longs, running)
		data.Rows++
	}
	return data, nil
}

func decodeDeltaBitpackBlock(payload []byte, nrows int) (*BlockData, error) {
	nulls, nullCount, off, err := readNullPrefix(payload, 0)
	if err != nil {
		return nil, err
	}
	return deltaExpand(payload[off:], nulls, nullCount, nrows)
}

func decodeDeltaZstdBlock(payload []byte, nrows int) (*BlockData, error) {
	nulls, nullCount, off, err := readNullPrefix(payload, 0)
	if err != nil {
		return nil, err
	}
	if off+4 > len(payload) {
		return nil, fmt.Errorf("segment: truncated delta zstd frame length")
	}
	frameLen := int(readU32LE(payload, off))
	off += 4
	if frameLen < 0 || off+frameLen > len(payload) {
		return nil, fmt.Errorf("segment: truncated delta zstd frame")
	}
	core, err := zstdDecompress(payload[off : off+frameLen])
	if err != nil {
		return nil, err
	}
	return deltaExpand(core, nulls, nullCount, nrows)
}
