package raft

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.1

func TestMetaFSM_CreateIndex(t *testing.T) {
	fsm := NewMetaFSM()
	cmd, err := EncodeCommand(CmdCreateIndex, map[string]any{
		"IndexID": uint64(1),
		"Meta":    &meta.IndexMeta{Name: "test-idx"},
	})
	require.NoError(t, err)
	require.NoError(t, fsm.Apply(cmd))

	idx, ok := fsm.State().GetIndex("test-idx")
	require.True(t, ok, "index should exist after CreateIndex")
	assert.Equal(t, "test-idx", idx.Name)
}

func TestMetaFSM_DeleteIndex(t *testing.T) {
	fsm := NewMetaFSM()
	// Create first
	cmd, _ := EncodeCommand(CmdCreateIndex, map[string]any{
		"IndexID": uint64(1),
		"Meta":    &meta.IndexMeta{Name: "test-idx"},
	})
	fsm.Apply(cmd)
	// Delete
	cmd, _ = EncodeCommand(CmdDeleteIndex, map[string]any{
		"IndexID": uint64(1),
	})
	require.NoError(t, fsm.Apply(cmd))

	_, ok := fsm.State().GetIndex("test-idx")
	assert.False(t, ok, "index should not exist after DeleteIndex")
}

func TestMetaFSM_AllocateShard(t *testing.T) {
	fsm := NewMetaFSM()
	cmd, err := EncodeCommand(CmdAllocateShard, map[string]any{
		"ShardID":   uint64(1),
		"IndexID":   uint64(1),
		"RaftGroup": []string{"node1", "node2", "node3"},
	})
	require.NoError(t, err)
	require.NoError(t, fsm.Apply(cmd))

	route, ok := fsm.State().GetRoute(types.ShardID(1))
	require.True(t, ok)
	assert.Equal(t, types.ShardID(1), route.ShardID)
	assert.Equal(t, uint64(1), route.Generation)
	assert.Equal(t, 3, len(route.RaftGroup))
}

func TestMetaFSM_SplitShard(t *testing.T) {
	fsm := NewMetaFSM()
	// Allocate parent shard
	cmd, _ := EncodeCommand(CmdAllocateShard, map[string]any{
		"ShardID":   uint64(1),
		"IndexID":   uint64(1),
		"RaftGroup": []string{"node1", "node2"},
	})
	fsm.Apply(cmd)

	// Split
	cmd, _ = EncodeCommand(CmdSplitShard, map[string]any{
		"ParentShardID": uint64(1),
		"ChildShardIDs": []uint64{2, 3},
		"NewGeneration": uint64(2),
		"RaftGroups":    [][]string{{"node1", "node3"}, {"node2", "node4"}},
	})
	require.NoError(t, fsm.Apply(cmd))

	// Parent generation should be updated
	parent, ok := fsm.State().GetRoute(types.ShardID(1))
	require.True(t, ok)
	assert.Equal(t, uint64(2), parent.Generation, "parent generation should be 2")

	// Children should exist
	child1, ok := fsm.State().GetRoute(types.ShardID(2))
	require.True(t, ok)
	assert.Equal(t, uint64(2), child1.Generation)

	child2, ok := fsm.State().GetRoute(types.ShardID(3))
	require.True(t, ok)
	assert.Equal(t, uint64(2), child2.Generation)
}

func TestMetaFSM_RegisterNode(t *testing.T) {
	fsm := NewMetaFSM()
	node := NodeInfo{
		ID:   "node1",
		Addr: "localhost:8080",
		Role: "meta",
	}
	cmd, err := EncodeCommand(CmdRegisterNode, node)
	require.NoError(t, err)
	require.NoError(t, fsm.Apply(cmd))

	members := fsm.State().GetMembers()
	assert.Len(t, members, 1)
	assert.Equal(t, NodeID("node1"), members[0].ID)
	assert.Equal(t, "localhost:8080", members[0].Addr)

	// Update same node
	node.LastSeen = 1000
	cmd, _ = EncodeCommand(CmdRegisterNode, node)
	fsm.Apply(cmd)
	members = fsm.State().GetMembers()
	assert.Len(t, members, 1, "should not duplicate node on re-register")
}

func TestMetaFSM_UpdateWatermark(t *testing.T) {
	fsm := NewMetaFSM()
	cmd, err := EncodeCommand(CmdUpdateWatermark, map[string]any{
		"ShardID":   uint64(1),
		"Watermark": uint64(42),
	})
	require.NoError(t, err)
	require.NoError(t, fsm.Apply(cmd))

	state := fsm.State()
	state.mu.RLock()
	wm := state.Watermarks[types.ShardID(1)]
	state.mu.RUnlock()
	assert.Equal(t, types.SeqNo(42), wm)
}

func TestMetaFSM_SnapshotRestore(t *testing.T) {
	fsm := NewMetaFSM()
	// Apply some commands
	cmd, _ := EncodeCommand(CmdCreateIndex, map[string]any{
		"IndexID": uint64(1),
		"Meta":    &meta.IndexMeta{Name: "test-idx"},
	})
	fsm.Apply(cmd)
	cmd, _ = EncodeCommand(CmdAllocateShard, map[string]any{
		"ShardID":   uint64(1),
		"IndexID":   uint64(1),
		"RaftGroup": []string{"node1"},
	})
	fsm.Apply(cmd)

	// Snapshot
	snap, err := fsm.Snapshot()
	require.NoError(t, err)
	assert.NotEmpty(t, snap)

	// Restore into a new FSM
	fsm2 := NewMetaFSM()
	require.NoError(t, fsm2.Restore(snap))
	assert.Equal(t, fsm.State().Version, fsm2.State().Version)
}

func TestMetaFSM_UpdateQuota(t *testing.T) {
	fsm := NewMetaFSM()
	// Create tenant first
	fsm.State().mu.Lock()
	fsm.State().Tenants[types.TenantID(1)] = &TenantMeta{
		ID:   types.TenantID(1),
		Mode: "shared-shard",
	}
	fsm.State().mu.Unlock()

	cmd, err := EncodeCommand(CmdUpdateQuota, map[string]any{
		"TenantID": uint64(1),
		"Quota": map[string]any{
			"StorageBytes": int64(1000000),
			"VectorCount":  int64(10000),
			"QueryQPS":     100,
			"WriteTPS":     50,
		},
	})
	require.NoError(t, err)
	require.NoError(t, fsm.Apply(cmd))

	tenant, ok := fsm.State().GetTenant(types.TenantID(1))
	require.True(t, ok)
	assert.Equal(t, int64(1000000), tenant.Quota.StorageBytes)
	assert.Equal(t, 100, tenant.Quota.QueryQPS)
}

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B1
func TestMetaFSM_UnregisterNode(t *testing.T) {
	fsm := NewMetaFSM()
	// Register two nodes
	node1 := NodeInfo{ID: "node1", Addr: "localhost:8080", Role: "meta"}
	node2 := NodeInfo{ID: "node2", Addr: "localhost:8081", Role: "shard"}
	cmd, err := EncodeCommand(CmdRegisterNode, node1)
	require.NoError(t, err)
	require.NoError(t, fsm.Apply(cmd))
	cmd, _ = EncodeCommand(CmdRegisterNode, node2)
	require.NoError(t, fsm.Apply(cmd))

	members := fsm.State().GetMembers()
	assert.Len(t, members, 2)

	// Unregister node1
	cmd, _ = EncodeCommand(CmdUnregisterNode, node1)
	require.NoError(t, fsm.Apply(cmd))

	members = fsm.State().GetMembers()
	assert.Len(t, members, 1, "should have 1 member after unregistering node1")
	assert.Equal(t, NodeID("node2"), members[0].ID, "remaining node should be node2")

	// Unregister non-existent node (idempotent)
	cmd, _ = EncodeCommand(CmdUnregisterNode, NodeInfo{ID: "nonexistent"})
	require.NoError(t, fsm.Apply(cmd), "unregistering non-existent node should succeed")
	members = fsm.State().GetMembers()
	assert.Len(t, members, 1, "unregistering non-existent node should not change members")
}
