package segment

import "fmt"

// Run-length encoding (EncodingRLE) for boolean blocks.
//
// Payload (see api/proto/segment.proto):
//
//	null header | u32 run_count | per run (u32 run_len | u8 value)
//
// Null rows participate in runs with their placeholder value (false); the
// null bitmap restores nullness on decode.

func encodeRLEBlock(data *BlockData) ([]byte, error) {
	if data.bools == nil {
		return nil, fmt.Errorf("segment: rle encoding requires a bool vector")
	}
	out := appendNullPrefix(nil, data)

	runs := 0
	for i := 0; i < data.Rows; {
		j := i
		for j < data.Rows && data.bools[j] == data.bools[i] {
			j++
		}
		runs++
		i = j
	}
	out = appendU32LE(out, uint32(runs))

	for i := 0; i < data.Rows; {
		j := i
		for j < data.Rows && data.bools[j] == data.bools[i] {
			j++
		}
		out = appendU32LE(out, uint32(j-i))
		if data.bools[i] {
			out = append(out, 1)
		} else {
			out = append(out, 0)
		}
		i = j
	}
	return out, nil
}

func decodeRLEBlock(payload []byte, nrows int) (*BlockData, error) {
	nulls, nullCount, off, err := readNullPrefix(payload, 0)
	if err != nil {
		return nil, err
	}
	if off+4 > len(payload) {
		return nil, fmt.Errorf("segment: truncated rle run count")
	}
	runs := int(readU32LE(payload, off))
	off += 4

	data := &BlockData{bools: make([]bool, 0, nrows)}
	for range runs {
		if off+5 > len(payload) {
			return nil, fmt.Errorf("segment: truncated rle run")
		}
		length := int(readU32LE(payload, off))
		val := payload[off+4] != 0
		off += 5
		if length < 0 || data.Rows+length > nrows {
			return nil, fmt.Errorf("segment: rle run overflows block rows")
		}
		for range length {
			if nullCount > 0 && nullBitIsSet(nulls, data.Rows) {
				data.appendNull() // placeholder false + null bit
				continue
			}
			data.bools = append(data.bools, val)
			data.Rows++
		}
	}
	if data.Rows != nrows {
		return nil, fmt.Errorf("segment: rle runs decode to %d rows, expected %d", data.Rows, nrows)
	}
	return data, nil
}
