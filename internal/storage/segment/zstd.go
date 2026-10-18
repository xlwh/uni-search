package segment

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Shared zstd codecs. klauspost EncodeAll/DecodeAll are safe for
// concurrent use (they draw from internal encoder/decoder pools), so one
// package-level pair serves all block (de)compressions without per-block
// setup cost. src.zst additionally requests the zstd-19-equivalent level.
var (
	zstdOnce   sync.Once
	zstdEnc    *zstd.Encoder
	zstdDec    *zstd.Decoder
	zstdSrcEnc *zstd.Encoder // level 19 mapping, for src.zst
)

func zstdCodecs() (enc *zstd.Encoder, dec *zstd.Decoder, err error) {
	zstdOnce.Do(func() {
		zstdEnc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			return
		}
		zstdSrcEnc, err = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(19)),
			zstd.WithZeroFrames(false))
		if err != nil {
			return
		}
		zstdDec, err = zstd.NewReader(nil)
	})
	return zstdEnc, zstdDec, err
}

// zstdCompress compresses src as one frame at the block level (default).
func zstdCompress(src []byte) ([]byte, error) {
	enc, _, err := zstdCodecs()
	if err != nil {
		return nil, err
	}
	return enc.EncodeAll(src, nil), nil
}

// zstdCompressSource compresses the whole _source column as one stream at
// the zstd-19-equivalent level (design 01 §4.4).
func zstdCompressSource(src []byte) ([]byte, error) {
	_, _, err := zstdCodecs()
	if err != nil {
		return nil, err
	}
	return zstdSrcEnc.EncodeAll(src, nil), nil
}

// zstdDecompress decodes one complete frame.
func zstdDecompress(src []byte) ([]byte, error) {
	_, dec, err := zstdCodecs()
	if err != nil {
		return nil, err
	}
	out, err := dec.DecodeAll(src, nil)
	if err != nil {
		return nil, fmt.Errorf("segment: zstd decode: %w", err)
	}
	return out, nil
}

// Raw zstd block encoding (EncodingZstd): the fallback for high-cardinality
// keyword/text blocks.
//
// Payload (see api/proto/segment.proto):
//
//	null header | u32 frame_len | zstd(blob = per row (u32 len | bytes))

func encodeZstdRawBlock(data *BlockData) ([]byte, error) {
	if data.strs == nil {
		return nil, fmt.Errorf("segment: raw zstd encoding requires a string vector")
	}
	blob := make([]byte, 0, data.Rows*12)
	for i := 0; i < data.Rows; i++ {
		blob = appendU32LE(blob, uint32(len(data.strs[i])))
		blob = append(blob, data.strs[i]...)
	}
	frame, err := zstdCompress(blob)
	if err != nil {
		return nil, err
	}
	out := appendNullPrefix(nil, data)
	out = appendU32LE(out, uint32(len(frame)))
	return append(out, frame...), nil
}

func decodeZstdRawBlock(payload []byte, nrows int) (*BlockData, error) {
	nulls, nullCount, off, err := readNullPrefix(payload, 0)
	if err != nil {
		return nil, err
	}
	if off+4 > len(payload) {
		return nil, fmt.Errorf("segment: truncated raw zstd frame length")
	}
	frameLen := int(readU32LE(payload, off))
	off += 4
	if frameLen < 0 || off+frameLen > len(payload) {
		return nil, fmt.Errorf("segment: truncated raw zstd frame")
	}
	blob, err := zstdDecompress(payload[off : off+frameLen])
	if err != nil {
		return nil, err
	}

	data := &BlockData{strs: make([]string, 0, nrows)}
	p := 0
	for i := range nrows {
		if nullCount > 0 && nullBitIsSet(nulls, i) {
			// Null rows occupy an empty (u32 0) slot in the blob.
			if p+4 > len(blob) {
				return nil, fmt.Errorf("segment: truncated raw zstd value at row %d", i)
			}
			p += 4
			data.appendNull()
			continue
		}
		if p+4 > len(blob) {
			return nil, fmt.Errorf("segment: truncated raw zstd value at row %d", i)
		}
		l := int(readU32LE(blob, p))
		p += 4
		if l < 0 || p+l > len(blob) {
			return nil, fmt.Errorf("segment: truncated raw zstd value at row %d", i)
		}
		data.strs = append(data.strs, string(blob[p:p+l]))
		p += l
		data.Rows++
	}
	return data, nil
}
