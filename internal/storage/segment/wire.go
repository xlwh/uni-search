// Package segment implements the immutable on-disk segment format of
// design 01-storage-engine.md §4: the column file layout (header + 8192-row
// independently encoded blocks with per-block zone maps), the encoding
// selection matrix, src.zst / id.dict / id.hash and the protobuf footer.
// The protobuf messages are hand-rolled with protowire (no protoc in the
// build environment); api/proto/segment.proto is the normative format
// document and field numbers map 1:1 to the tags below.
package segment

import (
	"fmt"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
	"google.golang.org/protobuf/encoding/protowire"
)

// Container magics, documented in api/proto/segment.proto.
const (
	colMagic    uint64 = 0x0031304C4F435355 // "USCOL01\0" little-endian
	footerMagic uint64 = 0x3154464745535555 // "USSEGFT1" little-endian
)

// FormatVersion is the current segment format version (bumped on any
// incompatible change, design 01 §4.5).
const FormatVersion uint32 = 1

// FieldType wire codes, mirroring enum FieldType in segment.proto.
const (
	ftUnspecified uint64 = 0
	ftKeyword     uint64 = 1
	ftText        uint64 = 2
	ftLong        uint64 = 3
	ftDouble      uint64 = 4
	ftDate        uint64 = 5
	ftBoolean     uint64 = 6
	ftIP          uint64 = 7
	ftGeoPoint    uint64 = 8
	ftDenseVector uint64 = 9
	ftSparseVec   uint64 = 10
	ftObject      uint64 = 11
	ftNested      uint64 = 12
)

// fieldTypeCode maps a meta.FieldType to its proto wire code.
func fieldTypeCode(ft meta.FieldType) uint64 {
	switch ft {
	case meta.TypeKeyword:
		return ftKeyword
	case meta.TypeText:
		return ftText
	case meta.TypeLong:
		return ftLong
	case meta.TypeDouble:
		return ftDouble
	case meta.TypeDate:
		return ftDate
	case meta.TypeBoolean:
		return ftBoolean
	case meta.TypeIP:
		return ftIP
	case meta.TypeGeoPoint:
		return ftGeoPoint
	case meta.TypeDenseVector:
		return ftDenseVector
	case meta.TypeSparseVector:
		return ftSparseVec
	case meta.TypeObject:
		return ftObject
	case meta.TypeNested:
		return ftNested
	default:
		return ftUnspecified
	}
}

// fieldTypeFromCode maps a proto wire code back to meta.FieldType. It
// returns false for unknown codes so callers can reject forward-incompatible
// columns explicitly instead of silently mis-typing them.
func fieldTypeFromCode(code uint64) (meta.FieldType, bool) {
	switch code {
	case ftKeyword:
		return meta.TypeKeyword, true
	case ftText:
		return meta.TypeText, true
	case ftLong:
		return meta.TypeLong, true
	case ftDouble:
		return meta.TypeDouble, true
	case ftDate:
		return meta.TypeDate, true
	case ftBoolean:
		return meta.TypeBoolean, true
	case ftIP:
		return meta.TypeIP, true
	case ftGeoPoint:
		return meta.TypeGeoPoint, true
	case ftDenseVector:
		return meta.TypeDenseVector, true
	case ftSparseVec:
		return meta.TypeSparseVector, true
	case ftObject:
		return meta.TypeObject, true
	case ftNested:
		return meta.TypeNested, true
	default:
		return "", false
	}
}

// ColumnHeader is the parsed header of a col_{field_id}.bin file
// (segment.proto message ColumnHeader).
type ColumnHeader struct {
	FieldID   types.FieldID
	FieldName string
	Type      meta.FieldType
	NumRows   uint64
	Blocks    []BlockIndex
}

// BlockIndex is one entry of the header's block offset table
// (segment.proto message BlockIndex). Offset is relative to the block data
// region of the file container.
type BlockIndex struct {
	Offset    uint64
	Encoding  Encoding
	Zone      ZoneMap
	NullCount uint32
	NumRows   uint32
}

// ZoneMap is the raw per-block min/max pair (segment.proto message
// ZoneMap); interpretation of Min/Max depends on the column FieldType.
type ZoneMap struct {
	Min     []byte
	Max     []byte
	HasNull bool
}

// ColumnStats is the whole-column aggregate stored in the segment footer
// (segment.proto message ColumnStats).
type ColumnStats struct {
	FieldID     types.FieldID
	Min         []byte
	Max         []byte
	NullCount   uint64
	Cardinality uint64
}

// SegmentFooter is the parsed meta.footer (segment.proto message
// SegmentFooter). Kept in memory after load (<1KB/segment, design 01 §4.5).
type SegmentFooter struct {
	SegmentID     types.SegmentID
	ShardID       types.ShardID
	NumRows       uint64
	MinSeqNo      types.SeqNo
	MaxSeqNo      types.SeqNo
	ColumnStats   []ColumnStats
	CreatedAt     int64 // unix millis
	TTLExpireAt   int64 // unix millis, 0 = no TTL
	FormatVersion uint32
	Checksum      []byte
	SourceOffsets []uint64 // decompressed src.zst offsets, len = NumRows+1
}

// ---------------------------------------------------------------------------
// Marshal (protowire; AppendBytes/AppendString append a length prefix,
// nested messages therefore use tag + length-prefixed payload)
// ---------------------------------------------------------------------------

func marshalZoneMap(z ZoneMap) []byte {
	var b []byte
	if len(z.Min) > 0 {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, z.Min)
	}
	if len(z.Max) > 0 {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, z.Max)
	}
	if z.HasNull {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	return b
}

func marshalBlockIndex(bi BlockIndex) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, bi.Offset)
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(bi.Encoding))
	if z := marshalZoneMap(bi.Zone); len(z) > 0 {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, z)
	}
	if bi.NullCount > 0 {
		b = protowire.AppendTag(b, 4, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(bi.NullCount))
	}
	if bi.NumRows > 0 {
		b = protowire.AppendTag(b, 5, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(bi.NumRows))
	}
	return b
}

// MarshalColumnHeader encodes the ColumnHeader message.
func MarshalColumnHeader(h *ColumnHeader) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(h.FieldID))
	if h.FieldName != "" {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, h.FieldName)
	}
	if c := fieldTypeCode(h.Type); c != ftUnspecified {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, c)
	}
	if h.NumRows > 0 {
		b = protowire.AppendTag(b, 4, protowire.VarintType)
		b = protowire.AppendVarint(b, h.NumRows)
	}
	for i := range h.Blocks {
		sub := marshalBlockIndex(h.Blocks[i])
		b = protowire.AppendTag(b, 5, protowire.BytesType)
		b = protowire.AppendBytes(b, sub)
	}
	return b
}

func marshalColumnStats(cs ColumnStats) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(cs.FieldID))
	if len(cs.Min) > 0 {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, cs.Min)
	}
	if len(cs.Max) > 0 {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, cs.Max)
	}
	if cs.NullCount > 0 {
		b = protowire.AppendTag(b, 4, protowire.VarintType)
		b = protowire.AppendVarint(b, cs.NullCount)
	}
	if cs.Cardinality > 0 {
		b = protowire.AppendTag(b, 5, protowire.VarintType)
		b = protowire.AppendVarint(b, cs.Cardinality)
	}
	return b
}

// MarshalSegmentFooter encodes the SegmentFooter message.
func MarshalSegmentFooter(f *SegmentFooter) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(f.SegmentID))
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(f.ShardID))
	b = protowire.AppendTag(b, 3, protowire.VarintType)
	b = protowire.AppendVarint(b, f.NumRows)
	b = protowire.AppendTag(b, 4, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(f.MinSeqNo))
	b = protowire.AppendTag(b, 5, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(f.MaxSeqNo))
	for i := range f.ColumnStats {
		sub := marshalColumnStats(f.ColumnStats[i])
		b = protowire.AppendTag(b, 6, protowire.BytesType)
		b = protowire.AppendBytes(b, sub)
	}
	if f.CreatedAt > 0 {
		b = protowire.AppendTag(b, 7, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(f.CreatedAt))
	}
	if f.TTLExpireAt != 0 {
		b = protowire.AppendTag(b, 8, protowire.VarintType)
		b = protowire.AppendVarint(b, protowire.EncodeZigZag(f.TTLExpireAt))
	}
	if f.FormatVersion > 0 {
		b = protowire.AppendTag(b, 9, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(f.FormatVersion))
	}
	if len(f.Checksum) > 0 {
		b = protowire.AppendTag(b, 10, protowire.BytesType)
		b = protowire.AppendBytes(b, f.Checksum)
	}
	for _, off := range f.SourceOffsets {
		b = protowire.AppendTag(b, 11, protowire.VarintType)
		b = protowire.AppendVarint(b, off)
	}
	return b
}

// ---------------------------------------------------------------------------
// Unmarshal (unknown fields are skipped for forward compatibility;
// readers must consult FormatVersion for incompatible-format detection)
// ---------------------------------------------------------------------------

func unmarshalZoneMap(b []byte) (ZoneMap, error) {
	var z ZoneMap
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return z, protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return z, protowire.ParseError(n)
			}
			z.Min = append([]byte(nil), v...)
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return z, protowire.ParseError(n)
			}
			z.Max = append([]byte(nil), v...)
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return z, protowire.ParseError(n)
			}
			z.HasNull = v != 0
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return z, protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return z, nil
}

func unmarshalBlockIndex(b []byte) (BlockIndex, error) {
	var bi BlockIndex
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return bi, protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return bi, protowire.ParseError(n)
			}
			bi.Offset = v
			b = b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return bi, protowire.ParseError(n)
			}
			bi.Encoding = Encoding(v)
			b = b[n:]
		case num == 3 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return bi, protowire.ParseError(n)
			}
			z, err := unmarshalZoneMap(v)
			if err != nil {
				return bi, err
			}
			bi.Zone = z
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return bi, protowire.ParseError(n)
			}
			bi.NullCount = uint32(v)
			b = b[n:]
		case num == 5 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return bi, protowire.ParseError(n)
			}
			bi.NumRows = uint32(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return bi, protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return bi, nil
}

// UnmarshalColumnHeader decodes the ColumnHeader message.
func UnmarshalColumnHeader(b []byte) (*ColumnHeader, error) {
	h := &ColumnHeader{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			h.FieldID = types.FieldID(v)
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			h.FieldName = string(v)
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			ft, ok := fieldTypeFromCode(v)
			if !ok {
				return nil, fmt.Errorf("segment: unknown field type code %d", v)
			}
			h.Type = ft
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			h.NumRows = v
			b = b[n:]
		case num == 5 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			bi, err := unmarshalBlockIndex(v)
			if err != nil {
				return nil, err
			}
			h.Blocks = append(h.Blocks, bi)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return h, nil
}

func unmarshalColumnStats(b []byte) (ColumnStats, error) {
	var cs ColumnStats
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return cs, protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return cs, protowire.ParseError(n)
			}
			cs.FieldID = types.FieldID(v)
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return cs, protowire.ParseError(n)
			}
			cs.Min = append([]byte(nil), v...)
			b = b[n:]
		case num == 3 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return cs, protowire.ParseError(n)
			}
			cs.Max = append([]byte(nil), v...)
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return cs, protowire.ParseError(n)
			}
			cs.NullCount = v
			b = b[n:]
		case num == 5 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return cs, protowire.ParseError(n)
			}
			cs.Cardinality = v
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return cs, protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return cs, nil
}

// UnmarshalSegmentFooter decodes the SegmentFooter message.
func UnmarshalSegmentFooter(b []byte) (*SegmentFooter, error) {
	f := &SegmentFooter{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.SegmentID = types.SegmentID(v)
			b = b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.ShardID = types.ShardID(v)
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.NumRows = v
			b = b[n:]
		case num == 4 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.MinSeqNo = types.SeqNo(v)
			b = b[n:]
		case num == 5 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.MaxSeqNo = types.SeqNo(v)
			b = b[n:]
		case num == 6 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			cs, err := unmarshalColumnStats(v)
			if err != nil {
				return nil, err
			}
			f.ColumnStats = append(f.ColumnStats, cs)
			b = b[n:]
		case num == 7 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.CreatedAt = int64(v)
			b = b[n:]
		case num == 8 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.TTLExpireAt = protowire.DecodeZigZag(v)
			b = b[n:]
		case num == 9 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.FormatVersion = uint32(v)
			b = b[n:]
		case num == 10 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.Checksum = append([]byte(nil), v...)
			b = b[n:]
		case num == 11 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			f.SourceOffsets = append(f.SourceOffsets, v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return f, nil
}
