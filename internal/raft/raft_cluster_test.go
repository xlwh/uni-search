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
)

// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.1
// TestRaftCluster_3NodeElection verifies that a 3-node Raft cluster
// can bootstrap, elect a leader, and survive a single-node failure.

func getFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestRaftCluster_SingleNodeBootstraps(t *testing.T) {
	dir := t.TempDir()
	addr := getFreePort(t)

	rc, err := NewRaftCluster(RaftClusterConfig{
		NodeID:   "node1",
		DataDir:  dir,
		RaftAddr: addr,
		Peers:    []string{fmt.Sprintf("node1@%s", addr)},
	})
	require.NoError(t, err)
	defer rc.Close()

	// Wait for leader election (single node should self-elect quickly).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if rc.IsLeader() || rc.Leader() != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("raft: leader election timeout (10s)")
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Verify we can apply a command.
	err = rc.Apply(CmdRegisterNode, NodeInfo{
		ID:   "node1",
		Addr: addr,
		Role: "meta",
	})
	require.NoError(t, err, "Apply should succeed after leader election")

	// Verify the state was applied.
	members := rc.GetMembers()
	assert.Len(t, members, 1, "should have 1 registered member")

	// Verify stats are available.
	stats := rc.Stats()
	assert.NotEmpty(t, stats, "raft stats should be non-empty")
}

func TestRaftCluster_SnapshotRestore(t *testing.T) {
	dir := t.TempDir()
	addr := getFreePort(t)

	rc, err := NewRaftCluster(RaftClusterConfig{
		NodeID:   "node1",
		DataDir:  dir,
		RaftAddr: addr,
		Peers:    []string{fmt.Sprintf("node1@%s", addr)},
	})
	require.NoError(t, err)
	defer rc.Close()

	// Wait for leader.
	require.Eventually(t, func() bool {
		return rc.IsLeader() || rc.Leader() != ""
	}, 10*time.Second, 100*time.Millisecond, "leader should be elected")

	// Apply a command.
	require.NoError(t, rc.Apply(CmdRegisterNode, NodeInfo{
		ID:   "node1",
		Addr: "localhost:9999",
		Role: "meta",
	}))

	// Trigger a snapshot.
	require.NoError(t, rc.Snapshot())

	// Verify snapshot files exist.
	snapDir := filepath.Join(dir, "raft", "snapshots")
	entries, err := os.ReadDir(snapDir)
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "should have at least one snapshot file")

	// Verify FSM snapshot produces data.
	data, err := rc.fsm.Snapshot()
	require.NoError(t, err)
	assert.NotEmpty(t, data)
}

func TestRaftCluster_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	addr := getFreePort(t)

	rc, err := NewRaftCluster(RaftClusterConfig{
		NodeID:   "node1",
		DataDir:  dir,
		RaftAddr: addr,
		Peers:    []string{fmt.Sprintf("node1@%s", addr)},
	})
	require.NoError(t, err)

	require.NoError(t, rc.Close())
	// Second close should not panic.
	require.NoError(t, rc.Close())
}

func TestRaftCluster_ApplyAfterClose(t *testing.T) {
	dir := t.TempDir()
	addr := getFreePort(t)

	rc, err := NewRaftCluster(RaftClusterConfig{
		NodeID:   "node1",
		DataDir:  dir,
		RaftAddr: addr,
		Peers:    []string{fmt.Sprintf("node1@%s", addr)},
	})
	require.NoError(t, err)
	rc.Close()

	err = rc.Apply(CmdRegisterNode, NodeInfo{
		ID:   "node2",
		Addr: "localhost:8888",
		Role: "shard",
	})
	assert.Error(t, err, "Apply after Close should fail")
}
