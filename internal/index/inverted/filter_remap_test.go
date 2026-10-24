package inverted

import (
	"testing"

	"github.com/RoaringBitmap/roaring"
	"github.com/stretchr/testify/require"
)

// @spec: 02_spec.md#2.8 F1-T01..T05

func TestF1_FilterAndRemap_NoDeletes(t *testing.T) {
	// Posting list with docIDs 0,1,2,3,4
	posts := []Posting{
		{DocID: 0, TF: 1, FieldLen: 10, Positions: []uint16{0}},
		{DocID: 1, TF: 2, FieldLen: 20, Positions: []uint16{0, 1}},
		{DocID: 2, TF: 3, FieldLen: 30, Positions: []uint16{0, 1, 2}},
		{DocID: 3, TF: 1, FieldLen: 40, Positions: []uint16{0}},
		{DocID: 4, TF: 2, FieldLen: 50, Positions: []uint16{0, 1}},
	}
	pl := NewPostingList(posts)
	// Identity rowMap: rowMap[i] = i
	rowMap := []uint32{0, 1, 2, 3, 4}
	got := pl.FilterAndRemap(nil, rowMap)

	require.Equal(t, 5, got.DocFreq())
	require.Equal(t, uint32(0), got.docIDs.Minimum())
	require.Equal(t, uint32(4), got.docIDs.Maximum())

	// Verify all postings preserved.
	it := got.Iterate()
	i := 0
	for it.HasNext() {
		p := it.Next()
		require.Equal(t, posts[i].DocID, p.DocID)
		require.Equal(t, posts[i].TF, p.TF)
		require.Equal(t, posts[i].FieldLen, p.FieldLen)
		require.Equal(t, posts[i].Positions, p.Positions)
		i++
	}
}

func TestF1_FilterAndRemap_WithDeletes(t *testing.T) {
	posts := []Posting{
		{DocID: 0, TF: 1, FieldLen: 10, Positions: []uint16{0}},
		{DocID: 1, TF: 2, FieldLen: 20, Positions: []uint16{0, 1}},
		{DocID: 2, TF: 3, FieldLen: 30, Positions: []uint16{0, 1, 2}},
		{DocID: 3, TF: 1, FieldLen: 40, Positions: []uint16{0}},
		{DocID: 4, TF: 2, FieldLen: 50, Positions: []uint16{0, 1}},
	}
	pl := NewPostingList(posts)

	// Delete docIDs 1 and 3.
	deletes := roaring.New()
	deletes.Add(1)
	deletes.Add(3)

	// rowMap: 0→0, 1→(deleted), 2→1, 3→(deleted), 4→2
	rowMap := []uint32{0, 999, 1, 999, 2}
	got := pl.FilterAndRemap(deletes, rowMap)

	require.Equal(t, 3, got.DocFreq())

	// Expected: docID 0→0, 2→1, 4→2
	expectedDocIDs := []uint32{0, 1, 2}
	idx := 0
	it := got.Iterate()
	for it.HasNext() {
		p := it.Next()
		require.Equal(t, expectedDocIDs[idx], p.DocID)
		idx++
	}
}

func TestF1_FilterAndRemap_AllDeleted(t *testing.T) {
	posts := []Posting{
		{DocID: 0, TF: 1, FieldLen: 10},
		{DocID: 1, TF: 2, FieldLen: 20},
	}
	pl := NewPostingList(posts)

	deletes := roaring.New()
	deletes.Add(0)
	deletes.Add(1)
	rowMap := []uint32{0, 1}

	got := pl.FilterAndRemap(deletes, rowMap)
	require.Equal(t, 0, got.DocFreq())
}

func TestF1_FilterAndRemap_EmptyPL(t *testing.T) {
	pl := &PostingList{docIDs: roaring.New()}
	rowMap := []uint32{0, 1, 2}
	got := pl.FilterAndRemap(nil, rowMap)
	require.Equal(t, 0, got.DocFreq())
}

func TestF1_FilterAndRemap_NilPL(t *testing.T) {
	var pl *PostingList
	got := pl.FilterAndRemap(nil, nil)
	require.NotNil(t, got)
	require.Equal(t, 0, got.DocFreq())
}

func TestF1_FilterAndRemap_PositionsPreserved(t *testing.T) {
	posts := []Posting{
		{DocID: 0, TF: 3, FieldLen: 100, Positions: []uint16{5, 10, 15}},
		{DocID: 1, TF: 1, FieldLen: 200, Positions: []uint16{7}},
		{DocID: 2, TF: 2, FieldLen: 300, Positions: []uint16{3, 9}},
	}
	pl := NewPostingList(posts)

	// Delete docID 1, remap: 0→0, 2→1
	deletes := roaring.New()
	deletes.Add(1)
	rowMap := []uint32{0, 999, 1}
	got := pl.FilterAndRemap(deletes, rowMap)

	require.Equal(t, 2, got.DocFreq())

	// Check positions.
	it := got.Iterate()
	p0 := it.Next()
	require.Equal(t, uint32(0), p0.DocID)
	require.Equal(t, []uint16{5, 10, 15}, p0.Positions)

	p1 := it.Next()
	require.Equal(t, uint32(1), p1.DocID)
	require.Equal(t, []uint16{3, 9}, p1.Positions)
}
