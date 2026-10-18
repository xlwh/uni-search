package inverted

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR10_FOR_RoundTrip checks the FOR bitpacker round-trips random
// integer arrays of varying bit widths and lengths.
func TestR10_FOR_RoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 9))
	cases := []struct {
		name  string
		width int
	}{{"1-bit", 1}, {"4-bit", 4}, {"8-bit", 8}, {"12-bit", 12}, {"16-bit", 16}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maxVal := uint64(1)<<tc.width - 1
			n := 200
			vals := make([]uint64, n)
			for i := range vals {
				vals[i] = uint64(r.Int64N(int64(maxVal) + 1))
			}
			enc := forEncode(vals)
			dec, err := forDecode(enc, n)
			require.NoError(t, err)
			require.Equal(t, vals, dec)
		})
	}
	// Edge: empty block.
	enc := forEncode(nil)
	require.Equal(t, []byte{0}, enc)
	dec, err := forDecode(enc, 0)
	require.NoError(t, err)
	require.Empty(t, dec)
}

// TestR10_Positions_RoundTrip verifies the per-doc position frame codec
// round-trips assorted position lists (including empty).
func TestR10_Positions_RoundTrip(t *testing.T) {
	cases := [][]uint16{
		{},
		{0},
		{0, 1, 2, 3, 10, 100, 1000},
		{5, 6, 9, 12, 50, 1000, 65535},
	}
	for i, pos := range cases {
		var buf bytes.Buffer
		require.NoError(t, encodePositions(&buf, pos), "case %d", i)
		got, next, err := decodePositions(buf.Bytes(), 0)
		require.NoError(t, err)
		require.Equal(t, pos, got, "case %d", i)
		require.Equal(t, buf.Len(), next, "case %d next offset", i)
	}
}

// TestR10_PostingList_Build_Get verifies the in-memory PostingList
// resolves per-doc data and reports the right doc frequency.
func TestR10_PostingList_Build_Get(t *testing.T) {
	posts := []Posting{
		{DocID: 1, TF: 2, FieldLen: 10, Positions: []uint16{0, 5}},
		{DocID: 5, TF: 1, FieldLen: 8, Positions: []uint16{3}},
		{DocID: 9, TF: 3, FieldLen: 20, Positions: []uint16{1, 2, 7}},
	}
	pl := NewPostingList(posts)
	require.Equal(t, 3, pl.DocFreq())
	require.Equal(t, 3, int(pl.DocIDs().GetCardinality()))

	tf, fl, pos, ok := pl.Get(5)
	require.True(t, ok)
	require.Equal(t, uint8(1), tf)
	require.Equal(t, uint16(8), fl)
	require.Equal(t, []uint16{3}, pos)

	_, _, _, ok = pl.Get(6)
	require.False(t, ok)
}

// TestR10_PostingList_Iterate walks the list in ascending docid order.
func TestR10_PostingList_Iterate(t *testing.T) {
	posts := []Posting{
		{DocID: 9, TF: 3, FieldLen: 20, Positions: []uint16{1, 2, 7}},
		{DocID: 1, TF: 2, FieldLen: 10, Positions: []uint16{0, 5}},
		{DocID: 5, TF: 1, FieldLen: 8, Positions: []uint16{3}},
	}
	pl := NewPostingList(posts)
	it := pl.Iterate()
	var got []uint32
	for it.HasNext() {
		got = append(got, it.Next().DocID)
	}
	require.Equal(t, []uint32{1, 5, 9}, got)
}

// TestR10_PostingList_Builder accumulates per-(term,doc) occurrences
// (tf, positions) and finalizes a sorted PostingList.
func TestR10_PostingList_Builder(t *testing.T) {
	b := NewPostingListBuilder()
	b.Add(3, 0, 8, true)
	b.Add(3, 5, 8, true)
	b.Add(1, 2, 10, true)
	b.Add(1, 0, 10, true)
	b.Add(3, 1, 8, true)
	pl := b.Finish()
	require.Equal(t, 2, pl.DocFreq())

	tf, fl, pos, ok := pl.Get(1)
	require.True(t, ok)
	require.Equal(t, uint8(2), tf)
	require.Equal(t, uint16(10), fl)
	require.Equal(t, []uint16{0, 2}, pos)

	tf, fl, pos, ok = pl.Get(3)
	require.True(t, ok)
	require.Equal(t, uint8(3), tf)
	require.Equal(t, []uint16{0, 1, 5}, pos)
}

// TestR10_PostingList_TFCap ensures tf saturates at maxTF (255) per
// §3.2 ("饱和上限 255").
func TestR10_PostingList_TFCap(t *testing.T) {
	b := NewPostingListBuilder()
	for i := 0; i < 300; i++ {
		b.Add(1, i, 300, false)
	}
	pl := b.Finish()
	tf, _, _, ok := pl.Get(1)
	require.True(t, ok)
	require.Equal(t, uint8(maxTF), tf)
}

// TestR10_PostingList_Serialize round-trips a posting list through the
// §3.2 post.bin format (roaring docids + FOR tf + FOR fldLen).
func TestR10_PostingList_Serialize(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 22))
	posts := make([]Posting, 0, 50)
	for i := 0; i < 50; i++ {
		posts = append(posts, Posting{
			DocID:    uint32(r.IntN(1000)),
			TF:       uint8(r.IntN(255) + 1),
			FieldLen: uint16(r.IntN(500) + 1),
		})
	}
	pl := NewPostingList(posts)
	var buf bytes.Buffer
	require.NoError(t, writePostingList(&buf, 42, pl))
	rdr := &byteReader{data: buf.Bytes()}
	gotID, gotPL, err := readPostingList(rdr)
	require.NoError(t, err)
	require.Equal(t, TermID(42), gotID)
	require.Equal(t, pl.DocFreq(), gotPL.DocFreq())

	it := pl.Iterate()
	gotIt := gotPL.Iterate()
	for it.HasNext() {
		require.True(t, gotIt.HasNext())
		a, b := it.Next(), gotIt.Next()
		require.Equal(t, a.DocID, b.DocID)
		require.Equal(t, a.TF, b.TF)
		require.Equal(t, a.FieldLen, b.FieldLen)
	}
	require.False(t, gotIt.HasNext())
}

// TestR10_Positions_Serialize round-trips per-doc positions through the
// §3.2 pos.bin format.
func TestR10_Positions_Serialize(t *testing.T) {
	posts := []Posting{
		{DocID: 1, TF: 2, FieldLen: 10, Positions: []uint16{0, 5}},
		{DocID: 5, TF: 1, FieldLen: 8, Positions: []uint16{3}},
		{DocID: 9, TF: 3, FieldLen: 20, Positions: []uint16{1, 2, 7}},
	}
	pl := NewPostingList(posts)
	var buf bytes.Buffer
	require.NoError(t, writePositions(&buf, 7, pl))
	gotID, gotPos, off, err := readPositions(buf.Bytes(), 0)
	require.NoError(t, err)
	require.Equal(t, TermID(7), gotID)
	require.Equal(t, buf.Len(), off)
	require.Equal(t, [][]uint16{{0, 5}, {3}, {1, 2, 7}}, gotPos)
}
