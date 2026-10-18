package memtable

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// testRow builds a row with a keyword column (field 1), a long column
// (field 2) and a JSON _source embedding the _id, mirroring the canonical
// write-path document form.
func testRow(id string, seq types.SeqNo) Row {
	return Row{
		ID:     id,
		SeqNo:  seq,
		Source: []byte(fmt.Sprintf(`{"_id":%q,"n":%d}`, id, seq)),
		Values: map[types.FieldID]any{
			1: "val-" + id,
			2: int64(seq),
		},
	}
}

func mustAPIError(t *testing.T, err error) *apierrors.APIError {
	t.Helper()
	require.Error(t, err)
	ae, ok := err.(*apierrors.APIError)
	require.True(t, ok, "expected *apierrors.APIError, got %T: %v", err, err)
	return ae
}

// TestR5_WriteVisibility covers spec R5 scenario 1: a document appended to
// the active table is immediately readable by same-shard queries with no
// flush step, the visible watermark advances per append, MVCC snapshots
// hide newer rows, and superseded _id versions stay readable through older
// snapshots.
func TestR5_WriteVisibility(t *testing.T) {
	mt := New(7, Config{})
	require.NoError(t, mt.AppendBatch(testRow("doc-1", 1), testRow("doc-2", 2), testRow("doc-3", 3)))

	// Write is visible: no freeze/flush happened, the watermark is 3.
	snap := mt.Snapshot()
	assert.EqualValues(t, 3, snap.ReadUpTo)
	assert.EqualValues(t, 3, mt.VisibleSeqNo())
	assert.Equal(t, 0, mt.NumFrozen()) // everything still in active

	// Iteration sees all three rows immediately, in seq order.
	var seen []RowView
	mt.Iterate(snap, func(rv RowView) bool { seen = append(seen, rv); return true })
	require.Len(t, seen, 3)
	for i, rv := range seen {
		wantID := "doc-" + strconv.Itoa(i+1)
		assert.Equal(t, wantID, rv.ID)
		assert.EqualValues(t, i+1, rv.SeqNo)
		assert.Equal(t, testRow(wantID, types.SeqNo(i+1)).Source, rv.Source)
	}

	// Point lookups via pkIndex.
	rv, ok := mt.Lookup("doc-2", snap)
	require.True(t, ok)
	assert.EqualValues(t, 2, rv.SeqNo)
	assert.Equal(t, "doc-2", rv.ID)
	_, ok = mt.Lookup("missing", snap)
	assert.False(t, ok)

	// MVCC: a snapshot at seq 2 hides row 3.
	oldSnap := types.NewSnapshot(2)
	n := 0
	mt.Iterate(oldSnap, func(RowView) bool { n++; return true })
	assert.Equal(t, 2, n)
	_, ok = mt.Lookup("doc-3", oldSnap)
	assert.False(t, ok)

	// Columnar layout: fields aggregated per FieldID, aligned to rows.
	require.NotNil(t, mt.active.Column(1))
	require.NotNil(t, mt.active.Column(2))
	assert.Equal(t, []string{"val-doc-1", "val-doc-2", "val-doc-3"}, mt.active.Column(1).Strings)
	assert.Equal(t, []int64{1, 2, 3}, mt.active.Column(2).Longs)
	assert.Equal(t, []types.RowID{0, 1, 2}, mt.active.Column(1).RowIDs)

	// Superseded _id: latest wins for fresh snapshots, the old version is
	// still readable under an older snapshot.
	require.NoError(t, mt.Append(testRow("doc-1", 4)))
	latest := mt.Snapshot()
	assert.EqualValues(t, 4, latest.ReadUpTo)
	rv, ok = mt.Lookup("doc-1", latest)
	require.True(t, ok)
	assert.EqualValues(t, 4, rv.SeqNo)
	rvOld, ok := mt.Lookup("doc-1", oldSnap)
	require.True(t, ok)
	assert.EqualValues(t, 1, rvOld.SeqNo)
	assert.Equal(t, testRow("doc-1", 1).Source, rvOld.Source)

	// Iteration still yields every appended version (the original doc-1
	// plus its superseding append); version filtering on the query path
	// belongs to the delete tracker (design 01 §6.2).
	total := 0
	mt.Iterate(latest, func(RowView) bool { total++; return true })
	assert.Equal(t, 4, total)
}

// TestR5_FreezeTriggers verifies the §2.2 double-buffer triggers: the byte
// threshold and the age threshold both move active into frozen and start a
// fresh active table, frozen tables stay queryable, and RemoveFrozen hands
// them to the flush worker.
func TestR5_FreezeTriggers(t *testing.T) {
	// Byte threshold: a row larger than MaxActiveBytes forces the next
	// append to freeze first.
	mt := New(1, Config{MaxActiveBytes: 64})
	big := Row{ID: "big", SeqNo: 1, Source: make([]byte, 200), Values: map[types.FieldID]any{1: "x"}}
	require.NoError(t, mt.Append(big))
	// estimate: source 200 + id 3 + row overhead 16 + one value (8 + len("x"))
	assert.EqualValues(t, 228, mt.ActiveBytes())
	require.NoError(t, mt.Append(testRow("b", 2)))
	require.Equal(t, 1, mt.NumFrozen())
	frozen := mt.Frozen()
	require.Len(t, frozen, 1)
	assert.Equal(t, 1, frozen[0].NumRows())
	assert.Equal(t, "big", frozen[0].ID(0))
	assert.Equal(t, 1, mt.ActiveRows()) // fresh active holds only row "b"
	assert.NotSame(t, frozen[0], mt.active)

	// Frozen data remains visible to queries until removal (design 01 §5).
	snap := mt.Snapshot()
	_, ok := mt.Lookup("big", snap)
	assert.True(t, ok)
	_, ok = mt.Lookup("b", snap)
	assert.True(t, ok)

	// Age threshold: an active table older than MaxActiveAge freezes even
	// though it is far below the byte threshold.
	mt2 := New(1, Config{MaxActiveAge: 10 * time.Millisecond})
	require.NoError(t, mt2.Append(testRow("c", 1)))
	require.Equal(t, 0, mt2.NumFrozen())
	time.Sleep(30 * time.Millisecond)
	require.NoError(t, mt2.Append(testRow("d", 2)))
	require.Equal(t, 1, mt2.NumFrozen())
	assert.Equal(t, "c", mt2.Frozen()[0].ID(0))
	assert.Equal(t, 1, mt2.ActiveRows())

	// MaybeFreeze applies the age trigger without a write.
	mt3 := New(1, Config{MaxActiveAge: 10 * time.Millisecond})
	require.NoError(t, mt3.Append(testRow("e", 1)))
	time.Sleep(30 * time.Millisecond)
	mt3.MaybeFreeze()
	assert.Equal(t, 1, mt3.NumFrozen())

	// Manual Freeze + RemoveFrozen: the flush worker lifecycle.
	mt4 := New(1, Config{})
	require.NoError(t, mt4.AppendBatch(testRow("f", 1), testRow("g", 2)))
	require.NoError(t, mt4.Freeze())
	fz := mt4.Frozen()
	require.Len(t, fz, 1)
	require.Equal(t, 2, fz[0].NumRows())
	assert.True(t, mt4.RemoveFrozen(fz[0]))
	assert.Equal(t, 0, mt4.NumFrozen())
	assert.False(t, mt4.RemoveFrozen(fz[0])) // second removal: not found
}

// TestR5_Backpressure verifies §2.2 rule 2: once the frozen list is full,
// writes fail with the 429 es_rejected_execution_exception-style error
// until the flush worker removes a frozen table.
func TestR5_Backpressure(t *testing.T) {
	mt := New(1, Config{})
	for i := 0; i < 2; i++ {
		require.NoError(t, mt.Append(testRow(fmt.Sprintf("r-%d", i), types.SeqNo(i+1))))
		require.NoError(t, mt.Freeze())
	}
	require.Equal(t, 2, mt.NumFrozen())

	// Frozen full: append rejected with 429.
	err := mt.Append(testRow("r-2", 3))
	ae := mustAPIError(t, err)
	assert.Equal(t, 429, ae.Status)
	assert.Equal(t, "es_rejected_execution_exception", ae.Type)

	// Manual freeze is backpressured too.
	_ = mustAPIError(t, mt.Freeze())

	// Frozen tables stay queryable while backpressured.
	snap := mt.Snapshot()
	_, ok := mt.Lookup("r-0", snap)
	assert.True(t, ok)

	// Drain one frozen table (flush worker): writes are admitted again.
	assert.True(t, mt.RemoveFrozen(mt.Frozen()[0]))
	require.NoError(t, mt.Append(testRow("r-2", 3)))
	assert.EqualValues(t, 3, mt.VisibleSeqNo())
}

// TestR5_ConcurrentAppend hammers Append from many goroutines (no WAL
// involved: seq numbers are pre-assigned by the caller here) and verifies
// every row lands exactly once and is visible afterwards.
func TestR5_ConcurrentAppend(t *testing.T) {
	mt := New(3, Config{})
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = mt.Append(testRow(fmt.Sprintf("c-%d", idx), types.SeqNo(idx+1)))
		}(i)
	}
	wg.Wait()

	snap := mt.Snapshot()
	assert.EqualValues(t, n, snap.ReadUpTo)
	seen := make(map[string]bool)
	mt.Iterate(snap, func(rv RowView) bool { seen[rv.ID] = true; return true })
	assert.Len(t, seen, n)
	for i := 0; i < n; i++ {
		rv, ok := mt.Lookup(fmt.Sprintf("c-%d", i), snap)
		require.True(t, ok, "row c-%d missing", i)
		assert.EqualValues(t, i+1, rv.SeqNo)
	}
}

// TestR5_ValueTypeGuard verifies the columnar kind guard: mixing Go types
// inside one field is rejected with a 400 document_parsing_exception and
// leaves the table unmodified.
func TestR5_ValueTypeGuard(t *testing.T) {
	mt := New(1, Config{})
	require.NoError(t, mt.Append(testRow("a", 1)))
	before := mt.ActiveRows()
	err := mt.Append(Row{ID: "b", SeqNo: 2, Values: map[types.FieldID]any{1: int64(7)}})
	ae := mustAPIError(t, err)
	assert.Equal(t, 400, ae.Status)
	assert.Equal(t, apierrors.TypeDocumentParsing, ae.Type)
	assert.Equal(t, before, mt.ActiveRows())
	assert.Equal(t, []string{"val-a"}, mt.active.Column(1).Strings)
}
