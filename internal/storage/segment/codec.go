package segment

import (
	"fmt"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// BlockCodec is the interface for a single block encoder/decoder. @spec: 02_spec.md#3.3
type BlockCodec interface {
	// Encode serializes one block into a payload (including null prefix).
	Encode(ft meta.FieldType, data *BlockData) ([]byte, error)
	// Decode deserializes a payload back into a typed block.
	Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error)
	// ID returns the codec identifier recorded in composite chains.
	ID() uint8
}

// CodecChain applies multiple codecs in sequence. On encode, codecs are
// applied in order (e.g. Delta → ZSTD); on decode, in reverse order
// (ZSTD → Delta). @spec: 02_spec.md#3.3
type CodecChain struct {
	codecs []BlockCodec
}

// NewCodecChain creates a chain from the given codecs (applied left-to-right on encode).
func NewCodecChain(codecs ...BlockCodec) *CodecChain {
	return &CodecChain{codecs: codecs}
}

// IDs returns the ordered list of codec IDs in the chain.
func (c *CodecChain) IDs() []uint8 {
	ids := make([]uint8, len(c.codecs))
	for i, codec := range c.codecs {
		ids[i] = codec.ID()
	}
	return ids
}

// Encode applies codecs in order: the first codec encodes the BlockData to
// bytes, and each subsequent codec wraps the previous output. The final
// output includes a chain descriptor prefix (u8 length + codec IDs).
func (c *CodecChain) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	if len(c.codecs) == 0 {
		return nil, fmt.Errorf("segment: empty codec chain")
	}
	// First codec encodes BlockData → []byte.
	payload, err := c.codecs[0].Encode(ft, data)
	if err != nil {
		return nil, fmt.Errorf("segment: codec %d encode: %w", c.codecs[0].ID(), err)
	}
	// Subsequent codecs operate on raw bytes (byte→byte transform).
	for i := 1; i < len(c.codecs); i++ {
		transformed, err := encodeByteTransform(c.codecs[i], ft, payload)
		if err != nil {
			return nil, fmt.Errorf("segment: codec %d transform: %w", c.codecs[i].ID(), err)
		}
		payload = transformed
	}
	// Prepend chain descriptor: u8 chain_len + codec IDs.
	desc := make([]byte, 1+len(c.codecs))
	desc[0] = byte(len(c.codecs))
	for i, codec := range c.codecs {
		desc[1+i] = codec.ID()
	}
	return append(desc, payload...), nil
}

// Decode reverses the chain: reads the descriptor prefix, then applies
// codecs in reverse order. The last codec produces the final BlockData.
func (c *CodecChain) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	if len(payload) < 1 {
		return nil, fmt.Errorf("segment: empty composite payload")
	}
	chainLen := int(payload[0])
	if chainLen < 1 || len(payload) < 1+chainLen {
		return nil, fmt.Errorf("segment: invalid chain descriptor length %d", chainLen)
	}
	// Read codec IDs from descriptor (informational; the chain is already constructed).
	_ = payload[1 : 1+chainLen] // codec IDs
	body := payload[1+chainLen:]

	// Apply codecs in reverse order for byte transforms, last codec decodes to BlockData.
	if len(c.codecs) == 0 {
		return nil, fmt.Errorf("segment: empty codec chain for decode")
	}
	// Reverse byte transforms: from last codec down to the second.
	for i := len(c.codecs) - 1; i >= 1; i-- {
		decoded, err := decodeByteTransform(c.codecs[i], ft, body)
		if err != nil {
			return nil, fmt.Errorf("segment: codec %d inverse transform: %w", c.codecs[i].ID(), err)
		}
		body = decoded
	}
	// First codec decodes []byte → BlockData.
	return c.codecs[0].Decode(ft, body, nrows)
}

// NewCodecByID constructs a codec instance by its ID. @spec: 02_spec.md#3.3
func NewCodecByID(id uint8) (BlockCodec, error) {
	switch id {
	case CodecIDDelta:
		return &deltaBitpackCodec{}, nil
	case CodecIDDoubleDelta:
		return &doubleDeltaCodec{}, nil
	case CodecIDGorilla:
		return &gorillaCodec{}, nil
	case CodecIDT64:
		return &t64Codec{}, nil
	case CodecIDALP:
		return &alpCodec{}, nil
	case CodecIDZstd:
		return &zstdRawCodec{}, nil
	case CodecIDDict:
		return &dictCodec{}, nil
	case CodecIDRLE:
		return &rleCodec{}, nil
	default:
		return nil, fmt.Errorf("segment: unknown codec ID %d", id)
	}
}

// NewCodecChainFromIDs builds a CodecChain from an ordered list of codec IDs.
func NewCodecChainFromIDs(ids []uint8) (*CodecChain, error) {
	codecs := make([]BlockCodec, len(ids))
	for i, id := range ids {
		c, err := NewCodecByID(id)
		if err != nil {
			return nil, err
		}
		codecs[i] = c
	}
	return NewCodecChain(codecs...), nil
}

// encodeByteTransform applies a byte-to-byte codec transform (e.g. ZSTD
// compression) on an already-encoded payload. Only certain codecs
// support byte transforms (ZSTD); others (Delta, Gorilla, etc.) are
// data-structure codecs that only work on BlockData.
func encodeByteTransform(codec BlockCodec, ft meta.FieldType, payload []byte) ([]byte, error) {
	// Only ZSTD supports byte-level transform in a chain.
	if codec.ID() != CodecIDZstd {
		// For non-transform codecs, we wrap the payload as a single-block
		// BlockData and re-encode. This is used when a data-structure codec
		// follows another in the chain (rare case).
		return nil, fmt.Errorf("segment: codec %d does not support byte transform", codec.ID())
	}
	return zstdCompress(payload)
}

// decodeByteTransform reverses a byte-to-byte codec transform.
func decodeByteTransform(codec BlockCodec, ft meta.FieldType, payload []byte) ([]byte, error) {
	if codec.ID() != CodecIDZstd {
		return nil, fmt.Errorf("segment: codec %d does not support byte transform", codec.ID())
	}
	return zstdDecompress(payload)
}

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
	case meta.TypeDouble, meta.TypeFloat:
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
// @spec: 02_spec.md#3.3
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
	case EncodingComposite:
		return nil, fmt.Errorf("segment: EncodingComposite requires a CodecChain, use encodeBlockComposite instead")
	default:
		return nil, fmt.Errorf("segment: unsupported block encoding %d", enc)
	}
}

// decodeBlock parses a block payload back into its typed vector.
// @spec: 02_spec.md#3.3
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
	case EncodingComposite:
		return decodeBlockComposite(ft, payload, nrows)
	default:
		return nil, fmt.Errorf("segment: unsupported block encoding %d", enc)
	}
}

// decodeBlockComposite decodes a composite-encoded block by reading the
// chain descriptor prefix and applying codecs in reverse order.
// @spec: 02_spec.md#3.3
func decodeBlockComposite(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	if len(payload) < 1 {
		return nil, fmt.Errorf("segment: empty composite payload")
	}
	chainLen := int(payload[0])
	if chainLen < 1 || len(payload) < 1+chainLen {
		return nil, fmt.Errorf("segment: invalid chain descriptor length %d", chainLen)
	}
	ids := make([]uint8, chainLen)
	copy(ids, payload[1:1+chainLen])
	chain, err := NewCodecChainFromIDs(ids)
	if err != nil {
		return nil, err
	}
	return chain.Decode(ft, payload, nrows)
}
