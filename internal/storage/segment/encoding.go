package segment

import (
	"encoding/binary"
	"math"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// Encoding identifies the block codec recorded in BlockIndex.encoding and
// selected at flush time by the type x cardinality matrix (design
// 01 §4.2). Values mirror enum Encoding in api/proto/segment.proto.
type Encoding uint32

const (
	EncodingUnspecified Encoding = 0
	// EncodingDict: sorted distinct values (zstd-compressed dictionary)
	// plus bitpacked row indices; nulls use the sentinel index == dict
	// size. keyword/text low cardinality, long/date low cardinality.
	EncodingDict Encoding = 1
	// EncodingDeltaBitpack: zigzag deltas of consecutive values, bitpacked
	// LSB-first. Monotonic long/date blocks (timestamps, auto-increment).
	EncodingDeltaBitpack Encoding = 2
	// EncodingDeltaZstd: DELTA_BITPACK delta section wrapped in a zstd
	// frame. General high-cardinality non-monotonic long/date blocks.
	EncodingDeltaZstd Encoding = 3
	// EncodingGorilla: XOR float encoding. double blocks.
	EncodingGorilla Encoding = 4
	// EncodingRLE: run-length encoding with a per-block null bitmap.
	// boolean blocks.
	EncodingRLE Encoding = 5
	// EncodingZstd: length-prefixed raw values in one zstd frame.
	// High-cardinality keyword/text fallback.
	EncodingZstd Encoding = 6
	// EncodingComposite: multi-codec chain. The payload prefix contains
	// a u8 chain length followed by codec IDs; each codec is applied in
	// order on encode and reverse order on decode. @spec: 02_spec.md#3.3
	EncodingComposite Encoding = 7
)

// Codec ID constants for composite codec chains. @spec: 02_spec.md#3.3
const (
	CodecIDDelta       uint8 = 1
	CodecIDDoubleDelta uint8 = 2 // F2: new
	CodecIDGorilla     uint8 = 3
	CodecIDT64         uint8 = 4 // F2: new
	CodecIDALP         uint8 = 5 // F2: new
	CodecIDZstd        uint8 = 7
	CodecIDDict        uint8 = 9
	CodecIDRLE         uint8 = 10
	CodecIDBitpack     uint8 = 11
)

// String implements fmt.Stringer for diagnostics.
func (e Encoding) String() string {
	switch e {
	case EncodingDict:
		return "dict"
	case EncodingDeltaBitpack:
		return "delta_bitpack"
	case EncodingDeltaZstd:
		return "delta_zstd"
	case EncodingGorilla:
		return "gorilla"
	case EncodingRLE:
		return "rle"
	case EncodingZstd:
		return "zstd"
	case EncodingComposite:
		return "composite"
	default:
		return "unspecified"
	}
}

// dictCardinalityLimit is the cardinality threshold below which the
// selection matrix prefers dictionary encoding (design 01 §4.2).
const dictCardinalityLimit = 4096

// blockStats summarizes the per-block statistics the selection matrix
// decides on. Nulls are excluded from every statistic.
type blockStats struct {
	cardinality int  // distinct non-null count
	monotonic   bool // non-decreasing or non-increasing over non-null rows
}

// chooseEncoding applies the selection matrix of design 01 §4.2:
//
//	keyword/text : cardinality < 4096 -> DICT, else ZSTD
//	long/date/ip : cardinality < 4096 -> DICT; monotonic -> DELTA_BITPACK;
//	               otherwise -> DELTA_ZSTD
//	double       : GORILLA
//	boolean      : RLE
func chooseEncoding(ft meta.FieldType, st blockStats) Encoding {
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
		if st.cardinality < dictCardinalityLimit {
			return EncodingDict
		}
		return EncodingZstd
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		if st.cardinality < dictCardinalityLimit {
			return EncodingDict
		}
		if st.monotonic {
			return EncodingDeltaBitpack
		}
		return EncodingDeltaZstd
	case meta.TypeDouble, meta.TypeFloat:
		return EncodingGorilla
	case meta.TypeBoolean:
		return EncodingRLE
	default:
		return EncodingZstd
	}
}

// chooseCompositeCodec optionally returns a CodecChain for the block when a
// composite encoding is expected to outperform the single encoding selected
// by chooseEncoding. Returns nil when the single encoding should be used
// (backward compatible). @spec: 02_spec.md#3.6
//
// TODO(F2): The composite codec selection is currently disabled to maintain
// backward compatibility with existing encoding selection tests. Enable
// after benchmark validation confirms compression improvements. The F2
// infrastructure (BlockCodec, CodecChain, DoubleDelta/T64/ALP encodings)
// is in place and tested via dedicated F2 tests.
func chooseCompositeCodec(ft meta.FieldType, st blockStats, data *BlockData) *CodecChain {
	_ = ft
	_ = st
	_ = data
	return nil
}

// floatOrderedBits converts IEEE-754 bits to an order-preserving u64 so
// byte-wise little-endian comparison matches the numeric order of the
// floats (including negatives); the inverse is floatUnorderedBits.
func floatOrderedBits(f float64) uint64 {
	b := math.Float64bits(f)
	if b&(1<<63) != 0 {
		return ^b
	}
	return b | (1 << 63)
}

func floatUnorderedBits(u uint64) float64 {
	if u&(1<<63) != 0 {
		return math.Float64frombits(u &^ (1 << 63))
	}
	return math.Float64frombits(^u)
}

// serializeZoneValue encodes one typed value with the per-type fixed
// serialization documented in api/proto/segment.proto (ZoneMap comment).
func serializeZoneValue(ft meta.FieldType, v any) []byte {
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
		return []byte(v.(string))
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(v.(int64)))
		return b[:]
	case meta.TypeDouble, meta.TypeFloat:
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], floatOrderedBits(v.(float64)))
		return b[:]
	case meta.TypeBoolean:
		if v.(bool) {
			return []byte{1}
		}
		return []byte{0}
	default:
		return nil
	}
}

// deserializeZoneValue decodes one typed value from its fixed
// serialization; returns nil for empty input (block with no non-null rows).
func deserializeZoneValue(ft meta.FieldType, b []byte) any {
	if len(b) == 0 {
		return nil
	}
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
		return string(b)
	case meta.TypeLong, meta.TypeDate, meta.TypeIP:
		if len(b) != 8 {
			return nil
		}
		return int64(binary.LittleEndian.Uint64(b))
	case meta.TypeDouble, meta.TypeFloat:
		if len(b) != 8 {
			return nil
		}
		return floatUnorderedBits(binary.LittleEndian.Uint64(b))
	case meta.TypeBoolean:
		return b[0] != 0
	default:
		return nil
	}
}
