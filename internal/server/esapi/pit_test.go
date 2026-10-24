package esapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.4

func TestPIT_CreateAndSnapshot(t *testing.T) {
	mgr := NewPITManager()
	snap := types.NewSnapshot(42)

	pitID, err := mgr.Create("test-idx", snap)
	require.NoError(t, err)
	assert.NotEmpty(t, pitID)

	gotSnap, idx, err := mgr.Snapshot(pitID)
	require.NoError(t, err)
	assert.Equal(t, "test-idx", idx)
	assert.Equal(t, snap.ReadUpTo, gotSnap.ReadUpTo)
}

func TestPIT_Delete(t *testing.T) {
	mgr := NewPITManager()
	snap := types.NewSnapshot(1)

	pitID, _ := mgr.Create("idx", snap)
	require.NoError(t, mgr.Delete(pitID))

	_, _, err := mgr.Snapshot(pitID)
	require.Error(t, err)
}

func TestPIT_Expiry(t *testing.T) {
	mgr := NewPITManager()
	mgr.ttl = 50 * time.Millisecond

	pitID, _ := mgr.Create("idx", types.NewSnapshot(1))
	time.Sleep(60 * time.Millisecond)

	_, _, err := mgr.Snapshot(pitID)
	require.Error(t, err, "expired PIT should not be accessible")
}

func TestPIT_Cleanup(t *testing.T) {
	mgr := NewPITManager()
	mgr.ttl = 50 * time.Millisecond

	mgr.Create("idx1", types.NewSnapshot(1))
	mgr.Create("idx2", types.NewSnapshot(2))
	assert.Equal(t, 2, mgr.Count())

	time.Sleep(60 * time.Millisecond)
	mgr.Cleanup()
	assert.Equal(t, 0, mgr.Count())
}

func TestScroll_CreateAndAdvance(t *testing.T) {
	pitMgr := NewPITManager()
	sm := NewScrollManager(pitMgr)

	scrollID, err := sm.CreateScroll("test-idx", types.NewSnapshot(10), 5*time.Minute, nil, 10)
	require.NoError(t, err)
	assert.NotEmpty(t, scrollID)

	cursor, snap, idx, err := sm.AdvanceScroll(scrollID)
	require.NoError(t, err)
	assert.Equal(t, "test-idx", idx)
	assert.Equal(t, types.SeqNo(10), snap.ReadUpTo)
	assert.Equal(t, 0, cursor.From)
}

func TestScroll_Delete(t *testing.T) {
	pitMgr := NewPITManager()
	sm := NewScrollManager(pitMgr)

	scrollID, _ := sm.CreateScroll("idx", types.NewSnapshot(1), 5*time.Minute, nil, 10)
	require.NoError(t, sm.DeleteScroll(scrollID))

	_, _, _, err := sm.AdvanceScroll(scrollID)
	require.Error(t, err, "scroll after delete should fail")
}
