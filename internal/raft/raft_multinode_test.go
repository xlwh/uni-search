package raft

import (
"context"
"fmt"
"net"
"os"
"path/filepath"
"testing"
"time"

"github.com/stretchr/testify/assert"
"github.com/stretchr/testify/require"

"github.com/unisearch-db/unisearch/internal/meta"
)

// makePeerList builds the peers string list for a 3-node cluster.
func makePeerList(addrs []string) []string {
	peers := make([]string, len(addrs))
	for i, addr := range addrs {
		peers[i] = fmt.Sprintf("node%d@%s", i+1, addr)
	}
	return peers
}

// startNode creates and starts a RaftCluster node.
func startNode(t *testing.T, nodeID string, dir string, addr string, peers []string) *RaftCluster {
	t.Helper()
	rc, err := NewRaftCluster(RaftClusterConfig{
		NodeID:   nodeID,
		DataDir:  dir,
		RaftAddr: addr,
		Peers:    peers,
	})
	require.NoError(t, err, "failed to create %s", nodeID)
	return rc
}

// waitForLeader waits up to 15s for any node to report a leader.
func waitForLeader(t *testing.T, nodes []*RaftCluster, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.Leader() != "" {
				return n.Leader()
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("leader election timeout")
	return ""
}

// TestRaftCluster_3NodeElection verifies that a 3-node Raft cluster
// can bootstrap, elect a leader, and apply commands consistently.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.6
func TestRaftCluster_3NodeElection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 3-node Raft test in short mode")
	}
	// Get 3 free ports.
	var addrs []string
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addrs = append(addrs, l.Addr().String())
		l.Close()
	}
	peers := makePeerList(addrs)

	// Create 3 nodes.
	var nodes []*RaftCluster
	for i := 0; i < 3; i++ {
		dir := filepath.Join(t.TempDir(), fmt.Sprintf("node%d", i+1))
		nodeID := fmt.Sprintf("node%d", i+1)
		rc := startNode(t, nodeID, dir, addrs[i], peers)
		nodes = append(nodes, rc)
	}
	defer func() {
		for _, n := range nodes {
			n.Close()
		}
	}()

	// Wait for leader election.
	leader := waitForLeader(t, nodes, 15*time.Second)
	assert.NotEmpty(t, leader, "a leader should be elected")

	// Verify exactly one leader.
	leaderCount := 0
	for _, n := range nodes {
		if n.IsLeader() {
			leaderCount++
		}
	}
	assert.Equal(t, 1, leaderCount, "exactly one leader expected")

	// Apply a command on the leader and verify it's replicated.
	for _, n := range nodes {
		if n.IsLeader() {
			err := n.Apply(CmdRegisterNode, NodeInfo{
				ID:   "test-node",
				Addr: "localhost:9999",
				Role: "shard",
			})
			require.NoError(t, err, "leader should accept Apply")
			break
		}
	}

	// Wait for replication to all followers.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			members := n.GetMembers()
			if len(members) == 0 {
				return false
			}
		}
		return true
	}, 10*time.Second, 200*time.Millisecond, "all nodes should see the registered member")
}

// TestRaftCluster_LeaderFailover verifies that the cluster survives
// a leader failure and elects a new leader.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.6
func TestRaftCluster_LeaderFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping leader failover test in short mode")
	}
	var addrs []string
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addrs = append(addrs, l.Addr().String())
		l.Close()
	}
	peers := makePeerList(addrs)

	var nodes []*RaftCluster
	for i := 0; i < 3; i++ {
		dir := filepath.Join(t.TempDir(), fmt.Sprintf("node%d", i+1))
		nodeID := fmt.Sprintf("node%d", i+1)
		rc := startNode(t, nodeID, dir, addrs[i], peers)
		nodes = append(nodes, rc)
	}

	// Wait for initial leader.
	initialLeader := waitForLeader(t, nodes, 15*time.Second)
	t.Logf("initial leader: %s", initialLeader)

	// Find and kill the leader node.
	var survivingNodes []*RaftCluster
	for i, n := range nodes {
		if n.IsLeader() {
			t.Logf("killing leader node%d", i+1)
			n.Close()
		} else {
			survivingNodes = append(survivingNodes, n)
		}
	}
	require.Len(t, survivingNodes, 2, "2 nodes should survive")

	// Wait for new leader election among survivors.
	newLeader := waitForLeader(t, survivingNodes, 20*time.Second)
	t.Logf("new leader: %s", newLeader)
	assert.NotEmpty(t, newLeader, "survivors should elect a new leader")

	// Verify the new leader can accept commands.
	for _, n := range survivingNodes {
		if n.IsLeader() {
			err := n.Apply(CmdRegisterNode, NodeInfo{
				ID:   "post-failover-node",
				Addr: "localhost:7777",
				Role: "meta",
			})
			require.NoError(t, err, "new leader should accept Apply")
			break
		}
	}

	// Cleanup surviving nodes.
	for _, n := range survivingNodes {
		n.Close()
	}
}

// TestRaftCluster_LogReplication verifies that a command applied on
// the leader is replicated to all followers.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.6
func TestRaftCluster_LogReplication(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping log replication test in short mode")
	}
	var addrs []string
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addrs = append(addrs, l.Addr().String())
		l.Close()
	}
	peers := makePeerList(addrs)

	var nodes []*RaftCluster
	for i := 0; i < 3; i++ {
		dir := filepath.Join(t.TempDir(), fmt.Sprintf("node%d", i+1))
		nodeID := fmt.Sprintf("node%d", i+1)
		rc := startNode(t, nodeID, dir, addrs[i], peers)
		nodes = append(nodes, rc)
	}
	defer func() {
		for _, n := range nodes {
			n.Close()
		}
	}()

	waitForLeader(t, nodes, 15*time.Second)

	// Apply multiple commands on the leader.
	for _, n := range nodes {
		if n.IsLeader() {
			require.NoError(t, n.Apply(CmdCreateIndex, map[string]any{
				"IndexID": uint64(1),
				"Meta":    &meta.IndexMeta{Name: "replicated-idx"},
			}))
			require.NoError(t, n.Apply(CmdAllocateShard, map[string]any{
				"ShardID":   uint64(1),
				"IndexID":   uint64(1),
				"RaftGroup": []string{"node1", "node2", "node3"},
			}))
			break
		}
	}

	// Verify all nodes eventually see the applied state.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			state := n.fsm.State()
			_, ok := state.GetIndex("replicated-idx")
			if !ok {
				return false
			}
			_, ok = state.GetRoute(1)
			if !ok {
				return false
			}
		}
		return true
	}, 15*time.Second, 200*time.Millisecond, "all nodes should replicate the index and route")
}

// TestRaftCluster_SnapshotPersist verifies that snapshots persist
// across restarts.
// @spec: ./active_apps/2026-10-20-gap-fill/02_spec.md#2.6
func TestRaftCluster_SnapshotPersist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping snapshot persist test in short mode")
	}
	dir := t.TempDir()
	addr := getFreePort(t)

	// Start node, apply commands, snapshot, close.
	rc := startNode(t, "node1", dir, addr, []string{fmt.Sprintf("node1@%s", addr)})
	require.Eventually(t, func() bool {
		return rc.IsLeader() || rc.Leader() != ""
	}, 10*time.Second, 100*time.Millisecond)

	require.NoError(t, rc.Apply(CmdCreateIndex, map[string]any{
		"IndexID": uint64(1),
		"Meta":    &meta.IndexMeta{Name: "snap-idx"},
	}))
	require.NoError(t, rc.Snapshot())

	// Verify snapshot files exist.
	snapDir := filepath.Join(dir, "raft", "snapshots")
	entries, err := os.ReadDir(snapDir)
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "snapshot files should exist")

	rc.Close()

	// Restart and verify state is restored.
	rc2, err := NewRaftCluster(RaftClusterConfig{
		NodeID:   "node1",
		DataDir:  dir,
		RaftAddr: getFreePort(t),
		Peers:    []string{fmt.Sprintf("node1@%s", addr)},
	})
	require.NoError(t, err)
	defer rc2.Close()

	// Wait for leader.
	_, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.Eventually(t, func() bool {
		return rc2.IsLeader() || rc2.Leader() != ""
	}, 10*time.Second, 100*time.Millisecond, "restarted node should elect itself")

	// Verify the index from before restart is still present.
	require.Eventually(t, func() bool {
		_, ok := rc2.fsm.State().GetIndex("snap-idx")
		return ok
	}, 5*time.Second, 200*time.Millisecond, "snap-idx should persist across restart")
}
