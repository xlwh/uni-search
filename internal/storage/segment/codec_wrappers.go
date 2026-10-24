package segment

import (
	"fmt"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// Codec wrapper types that adapt existing encode/decode functions to the
// BlockCodec interface. @spec: 02_spec.md#3.3

// --- Dict --------------------------------------------------------------------

type dictCodec struct{}

func (c *dictCodec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return encodeDictBlock(ft, data)
}
func (c *dictCodec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return decodeDictBlock(ft, payload, nrows)
}
func (c *dictCodec) ID() uint8 { return CodecIDDict }

// --- DeltaBitpack ------------------------------------------------------------

type deltaBitpackCodec struct{}

func (c *deltaBitpackCodec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return encodeDeltaBitpackBlock(data)
}
func (c *deltaBitpackCodec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return decodeDeltaBitpackBlock(payload, nrows)
}
func (c *deltaBitpackCodec) ID() uint8 { return CodecIDDelta }

// --- Gorilla -----------------------------------------------------------------

type gorillaCodec struct{}

func (c *gorillaCodec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return encodeGorillaBlock(data)
}
func (c *gorillaCodec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return decodeGorillaBlock(payload, nrows)
}
func (c *gorillaCodec) ID() uint8 { return CodecIDGorilla }

// --- RLE ---------------------------------------------------------------------

type rleCodec struct{}

func (c *rleCodec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return encodeRLEBlock(data)
}
func (c *rleCodec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return decodeRLEBlock(payload, nrows)
}
func (c *rleCodec) ID() uint8 { return CodecIDRLE }

// --- ZstdRaw -----------------------------------------------------------------

type zstdRawCodec struct{}

func (c *zstdRawCodec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return encodeZstdRawBlock(data)
}
func (c *zstdRawCodec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return decodeZstdRawBlock(payload, nrows)
}
func (c *zstdRawCodec) ID() uint8 { return CodecIDZstd }

// --- ZstdTransform (byte-level ZSTD for CodecChain) --------------------------

// zstdTransformCodec is a byte-level ZSTD compressor used as the final stage
// in a CodecChain. Its Encode/Decode methods are never called directly;
// encodeByteTransform/decodeByteTransform intercept CodecIDZstd and call
// zstdCompress/zstdDecompress on the raw bytes. @spec: 02_spec.md#3.3
type zstdTransformCodec struct{}

func (c *zstdTransformCodec) ID() uint8 { return CodecIDZstd }
func (c *zstdTransformCodec) Encode(ft meta.FieldType, data *BlockData) ([]byte, error) {
	return nil, fmt.Errorf("segment: zstdTransformCodec is only for byte-level transforms in CodecChain")
}
func (c *zstdTransformCodec) Decode(ft meta.FieldType, payload []byte, nrows int) (*BlockData, error) {
	return nil, fmt.Errorf("segment: zstdTransformCodec is only for byte-level transforms in CodecChain")
}
