package tenant

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.8
func TestQuotaCheck(t *testing.T) {
	rg := NewResourceGroup("t1", Quota{
		StorageBytes: 1000,
		VectorCount:  100,
		QueryQPS:     10,
		WriteTPS:     5,
	})

	// Storage quota check (no args — checks current usage vs quota).
	require.NoError(t, rg.CheckStorage())
	rg.AddStorage(1100)
	assert.Error(t, rg.CheckStorage(), "should exceed storage quota")

	// Vector quota check.
	require.NoError(t, rg.CheckVectors())
	rg.AddVectors(101)
	assert.Error(t, rg.CheckVectors(), "should exceed vector quota")
}

func TestQuerySemaphore(t *testing.T) {
	rg := NewResourceGroup("t1", Quota{QueryQPS: 2})
	require.NoError(t, rg.AcquireQuery())
	require.NoError(t, rg.AcquireQuery())
	err := rg.AcquireQuery()
	assert.Error(t, err, "third acquire should fail")
	rg.ReleaseQuery()
	require.NoError(t, rg.AcquireQuery(), "after release should succeed")
}

func TestWriteTokenBucket(t *testing.T) {
	rg := NewResourceGroup("t1", Quota{WriteTPS: 3})
	require.NoError(t, rg.AcquireWrite())
	require.NoError(t, rg.AcquireWrite())
	require.NoError(t, rg.AcquireWrite())
	err := rg.AcquireWrite()
	assert.Error(t, err, "fourth write should fail")
	rg.RefillWriteTokens()
	require.NoError(t, rg.AcquireWrite(), "after refill should succeed")
}

func TestManager(t *testing.T) {
	mgr := NewManager()
	mgr.Register("tenant-1", Quota{StorageBytes: 1000, QueryQPS: 5})
	rg := mgr.Get("tenant-1")
	require.NotNil(t, rg)
	require.NoError(t, rg.AcquireQuery())
	rg.ReleaseQuery()
}

func TestDedicatedAllocator(t *testing.T) {
	da := NewDedicatedAllocator()
	require.NoError(t, da.Assign("tenant-1", "node-1"))
	node, ok := da.GetNode("tenant-1")
	require.True(t, ok)
	assert.Equal(t, "node-1", node)
	err := da.Assign("tenant-2", "node-1")
	assert.Error(t, err)
	da.Release("tenant-1")
	err = da.Assign("tenant-2", "node-1")
	assert.NoError(t, err, "after release should be available")
}

func TestColdManager(t *testing.T) {
	cm := NewColdManager()
	require.NoError(t, cm.Pause("tenant-1", []string{"seg-1", "seg-2"}))
	assert.True(t, cm.IsPaused("tenant-1"))
	require.NoError(t, cm.Wakeup("tenant-1"))
	assert.False(t, cm.IsPaused("tenant-1"))
	err := cm.Pause("tenant-1", []string{"seg-1"})
	require.NoError(t, err)
	err = cm.Pause("tenant-1", []string{"seg-2"})
	assert.Error(t, err, "double pause should fail")
}

func TestColdManagerCallbacks(t *testing.T) {
	cm := NewColdManager()
	closed := make([]string, 0)
	reopened := make([]string, 0)
	cm.SetCallbacks(
		func(segID string) error { closed = append(closed, segID); return nil },
		func(segID string) error { reopened = append(reopened, segID); return nil },
	)
	cm.Pause("t1", []string{"seg-a", "seg-b"})
	assert.Equal(t, []string{"seg-a", "seg-b"}, closed)
	cm.Wakeup("t1")
	assert.Equal(t, []string{"seg-a", "seg-b"}, reopened)
}

func TestRebalanceCoordinator(t *testing.T) {
	rc := NewRebalanceCoordinator()
	require.NoError(t, rc.StartMigration("shard-1", "node-1", "node-2"))
	state, ok := rc.GetState("shard-1")
	require.True(t, ok)
	assert.Equal(t, RebalancePhaseSnapshot, state.Phase)
	require.NoError(t, rc.AdvancePhase("shard-1"))
	state, _ = rc.GetState("shard-1")
	assert.Equal(t, RebalancePhaseIncremental, state.Phase)
}
