package segment

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestR4_WireColumnHeaderRoundtrip(t *testing.T) {
	h := &ColumnHeader{
		FieldID:   types.FieldID(17),
		FieldName: "project_id",
		Type:      meta.TypeKeyword,
		NumRows:   8192*3 + 5,
		Blocks: []BlockIndex{
			{
				Offset:    0,
				Encoding:  EncodingDict,
				NullCount: 3,
				NumRows:   8192,
				Zone:      ZoneMap{Min: []byte("aaa"), Max: []byte("zzz"), HasNull: true},
			},
			{
				Offset:    4096,
				Encoding:  EncodingDeltaBitpack,
				NullCount: 0,
				NumRows:   5,
				Zone:      ZoneMap{Min: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
			},
		},
	}
	got, err := UnmarshalColumnHeader(MarshalColumnHeader(h))
	require.NoError(t, err)
	assert.Equal(t, h.FieldID, got.FieldID)
	assert.Equal(t, h.FieldName, got.FieldName)
	assert.Equal(t, h.Type, got.Type)
	assert.Equal(t, h.NumRows, got.NumRows)
	require.Len(t, got.Blocks, 2)
	assert.Equal(t, h.Blocks[0], got.Blocks[0])
	assert.Equal(t, h.Blocks[1], got.Blocks[1])
	assert.Equal(t, []byte("aaa"), got.Blocks[0].Zone.Min)
	assert.Equal(t, []byte("zzz"), got.Blocks[0].Zone.Max)
	assert.True(t, got.Blocks[0].Zone.HasNull)
}

func TestR4_WireFooterRoundtrip(t *testing.T) {
	f := &SegmentFooter{
		SegmentID:     types.SegmentID(9_000_000_000_001),
		ShardID:       types.ShardID(77),
		NumRows:       100_000,
		MinSeqNo:      types.SeqNo(512),
		MaxSeqNo:      types.SeqNo(99_999),
		ColumnStats:   []ColumnStats{{FieldID: 1, Min: []byte("a"), Max: []byte("z"), NullCount: 4, Cardinality: 10}},
		CreatedAt:     1_760_000_000_000,
		TTLExpireAt:   -42,
		FormatVersion: FormatVersion,
		Checksum:      []byte{0xDE, 0xAD, 0xBE, 0xEF},
		SourceOffsets: []uint64{0, 10, 35, 100},
	}
	got, err := UnmarshalSegmentFooter(MarshalSegmentFooter(f))
	require.NoError(t, err)
	assert.Equal(t, f.SegmentID, got.SegmentID)
	assert.Equal(t, f.ShardID, got.ShardID)
	assert.Equal(t, f.NumRows, got.NumRows)
	assert.Equal(t, f.MinSeqNo, got.MinSeqNo)
	assert.Equal(t, f.MaxSeqNo, got.MaxSeqNo)
	assert.Equal(t, f.ColumnStats, got.ColumnStats)
	assert.Equal(t, f.CreatedAt, got.CreatedAt)
	assert.Equal(t, f.TTLExpireAt, got.TTLExpireAt) // negative int64 via zigzag
	assert.Equal(t, f.FormatVersion, got.FormatVersion)
	assert.Equal(t, f.Checksum, got.Checksum)
	assert.Equal(t, f.SourceOffsets, got.SourceOffsets)
}

func TestR4_WireUnknownFieldsSkipped(t *testing.T) {
	h := &ColumnHeader{FieldID: 3, Type: meta.TypeLong, NumRows: 10}
	b := MarshalColumnHeader(h)
	// Append a forward-extension field (tag 99, varint) and an unknown
	// length-delimited field (tag 100); readers must skip both.
	b = protowire.AppendTag(b, 99, protowire.VarintType)
	b = protowire.AppendVarint(b, 12345)
	b = protowire.AppendTag(b, 100, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("future payload"))

	got, err := UnmarshalColumnHeader(b)
	require.NoError(t, err)
	assert.Equal(t, h.FieldID, got.FieldID)
	assert.Equal(t, h.Type, got.Type)
	assert.Equal(t, h.NumRows, got.NumRows)

	// Same forward-compatibility guarantee for the footer.
	f := &SegmentFooter{SegmentID: 5, FormatVersion: 1}
	fb := MarshalSegmentFooter(f)
	fb = protowire.AppendTag(fb, 200, protowire.BytesType)
	fb = protowire.AppendBytes(fb, bytes.Repeat([]byte{0xAB}, 32))
	gotF, err := UnmarshalSegmentFooter(fb)
	require.NoError(t, err)
	assert.Equal(t, f.SegmentID, gotF.SegmentID)
}

func TestR4_WireCorruptInputRejected(t *testing.T) {
	_, err := UnmarshalColumnHeader([]byte{0xFF})
	assert.Error(t, err)
	_, err = UnmarshalSegmentFooter([]byte{0x0B}) // field 1, type 3 (start group): invalid
	assert.Error(t, err)
}
