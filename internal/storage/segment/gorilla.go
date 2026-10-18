package segment

import (
	"fmt"
	"math"
	"math/bits"
)

// Gorilla XOR float encoding (EncodingGorilla) for double blocks
// (design 01 §4.2; Facebook gorilla paper).
//
// Payload (see api/proto/segment.proto):
//
//	null header | u32 num_rows | f64 first_value | xor bitstream
//
// Bitstream per row: '0' = same as previous (also used for null rows);
// '1' + '1' = reuse the previous (leading,trailing) window, then the
// (64 - leading - trailing) meaningful xor bits; '1' + '0' = new window:
// u6 leading, u6 (meaningful_bits - 1), then the meaningful bits.

func encodeGorillaBlock(data *BlockData) ([]byte, error) {
	if data.doubles == nil {
		return nil, fmt.Errorf("segment: gorilla encoding requires a double vector")
	}
	n := data.Rows

	// first_value = first non-null value (0 when the block has none).
	first := float64(0)
	for i := 0; i < n; i++ {
		if !data.IsNull(i) {
			first = data.doubles[i]
			break
		}
	}

	out := appendNullPrefix(nil, data)
	out = appendU32LE(out, uint32(n))
	out = appendU64LE(out, math.Float64bits(first))

	var bw msbBitWriter
	prev := math.Float64bits(first)
	prevLead, prevTrail := -1, -1 // no window yet
	for i := 0; i < n; i++ {
		if data.IsNull(i) {
			bw.writeBit(0)
			continue
		}
		cur := math.Float64bits(data.doubles[i])
		xor := cur ^ prev
		if xor == 0 {
			bw.writeBit(0)
			continue
		}
		bw.writeBit(1)
		lead := bits.LeadingZeros64(xor)
		trail := bits.TrailingZeros64(xor)
		meaningful := 64 - lead - trail
		if prevLead >= 0 && lead >= prevLead && trail >= prevTrail {
			// Reuse window: the xor bits inside the previous window.
			bw.writeBit(1)
			bw.write(xor>>uint(prevTrail), 64-prevLead-prevTrail)
		} else {
			bw.writeBit(0)
			bw.write(uint64(lead), 6)
			bw.write(uint64(meaningful-1), 6)
			bw.write(xor>>uint(trail), meaningful)
			prevLead, prevTrail = lead, trail
		}
		prev = cur
	}
	return append(out, bw.bytes()...), nil
}

func decodeGorillaBlock(payload []byte, nrows int) (*BlockData, error) {
	nulls, nullCount, off, err := readNullPrefix(payload, 0)
	if err != nil {
		return nil, err
	}
	if off+12 > len(payload) { // num_rows(4) + first_value(8)
		return nil, fmt.Errorf("segment: truncated gorilla header")
	}
	n := int(readU32LE(payload, off))
	if n != nrows {
		return nil, fmt.Errorf("segment: gorilla block row count %d != expected %d", n, nrows)
	}
	prev := readU64LE(payload, off+4)
	off += 12

	br := &msbBitReader{buf: payload[off:]}
	prevLead, prevTrail := -1, -1

	data := newBlockDataForGorilla(nrows)
	for i := 0; i < n; i++ {
		v := prev
		if br.readBit() == 1 {
			if br.readBit() == 1 { // reuse window
				v = prev ^ (br.read(64-prevLead-prevTrail) << uint(prevTrail))
			} else { // new window
				lead := int(br.read(6))
				meaningful := int(br.read(6)) + 1
				trail := 64 - lead - meaningful
				v = prev ^ (br.read(meaningful) << uint(trail))
				prevLead, prevTrail = lead, trail
			}
		}
		if nullCount > 0 && nullBitIsSet(nulls, i) {
			data.appendNull()
		} else {
			data.doubles = append(data.doubles, math.Float64frombits(v))
			data.Rows++
		}
		prev = v
	}
	return data, nil
}

func newBlockDataForGorilla(n int) *BlockData {
	return &BlockData{doubles: make([]float64, 0, n)}
}
