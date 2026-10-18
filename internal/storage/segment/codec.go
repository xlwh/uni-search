package segment

import (
	"fmt"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// BlockData is the decoded in-memory representation of one block: a row
// count, an optional null bitmap (bit i set = row i null) and exactly one
// typed value vector matching the column FieldType. Null rows carry a
// zero-value entry in the vector; use IsNull to mask them.
type BlockData struct {
	Rows      int
	nulls     []uint64 // nil when the block has no nulls
	nullCount int

	longs   []int64   // long/date/ip
	doubles []float64 // double
	strs    []string  // keyword/text
	bools   []bool    // boolean
}

// Longs returns the int64 vector (valid for long/date/ip columns).
func (b *BlockData) Longs() []int64 { return b.longs }

// Doubles returns the float64 vector (valid for double columns).
func (b *BlockData) Doubles() []float64 { return b.doubles }

// Strings returns the string vector (valid for keyword/text columns).
func (b *BlockData) Strings() []string { return b.strs }

// Bools returns the bool vector (valid for boolean columns).
func (b *BlockData) Bools() []bool { return b.bools }

// IsNull reports whether row i is null.
func (b *BlockData) IsNull(i int) bool {
	return b.nullCount > 0 && nullBitIsSet(b.nulls, i)
}

// NullCount returns the number of null rows in the block.
func (b *BlockData) NullCount() int { return b.nullCount }

// newBlockData allocates the typed vector matching ft.
func newBlockData(ft meta.FieldType, n int) *BlockData {
	b := &BlockData{Rows: 0}
	switch ft {
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		b.longs = make([]int64, 0, n)
	case meta.TypeDouble:
		b.doubles = make([]float64, 0, n)
	case meta.TypeKeyword, meta.TypeText:
		b.strs = make([]string, 0, n)
	case meta.TypeBoolean:
		b.bools = make([]bool, 0, n)
	}
	return b
}

// appendNull appends a null row (placeholder typed zero + null bit).
func (b *BlockData) appendNull() {
	b.nulls = nullBitmapSet(b.nulls, b.Rows)
	b.nullCount++
	switch {
	case b.longs != nil:
		b.longs = append(b.longs, 0)
	case b.doubles != nil:
		b.doubles = append(b.doubles, 0)
	case b.strs != nil:
		b.strs = append(b.strs, "")
	case b.bools != nil:
		b.bools = append(b.bools, false)
	default:
		b.longs = append(b.longs, 0) // typed vector decided at construction
	}
	b.Rows++
}

// appendNullPrefix writes the shared null header: u32 null_count, followed
// by u32 word_count + words when null_count > 0.
func appendNullPrefix(out []byte, data *BlockData) []byte {
	out = appendU32LE(out, uint32(data.nullCount))
	if data.nullCount > 0 {
		out = appendU32LE(out, uint32(len(data.nulls)))
		for _, w := range data.nulls {
			out = appendU64LE(out, w)
		}
	}
	return out
}

// readNullPrefix parses the shared null header written by appendNullPrefix.
func readNullPrefix(payload []byte, off int) (nulls []uint64, nullCount uint32, n int, err error) {
	if off+4 > len(payload) {
		return nil, 0, 0, fmt.Errorf("segment: truncated null header at %d", off)
	}
	nullCount = readU32LE(payload, off)
	off += 4
	if nullCount > 0 {
		if off+4 > len(payload) {
			return nil, 0, 0, fmt.Errorf("segment: truncated null bitmap length at %d", off)
		}
		wc := int(readU32LE(payload, off))
		off += 4
		if wc < 0 || off+wc*8 > len(payload) {
			return nil, 0, 0, fmt.Errorf("segment: truncated null bitmap at %d", off)
		}
		nulls = make([]uint64, wc)
		for i := range nulls {
			nulls[i] = readU64LE(payload, off+i*8)
		}
		off += wc * 8
	}
	return nulls, nullCount, off, nil
}

// encodeBlock serializes one block with the given encoding. The payload
// layout per encoding is documented in api/proto/segment.proto.
func encodeBlock(ft meta.FieldType, enc Encoding, data *BlockData) ([]byte, error) {
	switch enc {
	case EncodingDict:
		return encodeDictBlock(ft, data)
	case EncodingDeltaBitpack:
		return encodeDeltaBitpackBlock(data)
	case EncodingDeltaZstd:
		return encodeDeltaZstdBlock(data)
	case EncodingGorilla:
		return encodeGorillaBlock(data)
	case EncodingRLE:
		return encodeRLEBlock(data)
	case EncodingZstd:
		return encodeZstdRawBlock(data)
	default:
		return nil, fmt.Errorf("segment: unsupported block encoding %d", enc)
	}
}

// decodeBlock parses a block payload back into its typed vector.
func decodeBlock(ft meta.FieldType, enc Encoding, payload []byte, nrows int) (*BlockData, error) {
	switch enc {
	case EncodingDict:
		return decodeDictBlock(ft, payload, nrows)
	case EncodingDeltaBitpack:
		return decodeDeltaBitpackBlock(payload, nrows)
	case EncodingDeltaZstd:
		return decodeDeltaZstdBlock(payload, nrows)
	case EncodingGorilla:
		return decodeGorillaBlock(payload, nrows)
	case EncodingRLE:
		return decodeRLEBlock(payload, nrows)
	case EncodingZstd:
		return decodeZstdRawBlock(payload, nrows)
	default:
		return nil, fmt.Errorf("segment: unsupported block encoding %d", enc)
	}
}
