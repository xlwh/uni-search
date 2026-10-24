// Package storage shard_raft.go implements the M2 Shard Raft group
// (design 05-distributed.md §4). Each shard has one Raft group
// (1 leader + 2 followers default). Writes go through Raft quorum;
// reads default to leader (ReadIndex linearizable). Fault tolerance:
// kill -9 RTO < 30s.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.3
package storage

import (
	"context"
	"fmt"
	"sync"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// ShardRaftGroup wraps a shard's Raft consensus and FSM. In M2, this
// is backed by hashicorp/raft with S3 LogStore; for testing, it
// delegates directly to the Shard.
type ShardRaftGroup struct {
	mu      sync.RWMutex
	shardID types.ShardID
	shard   *Shard
	leader  bool
	peers   []string // peer addresses
	closed  bool
}

// NewShardRaftGroup creates a shard Raft group.
func NewShardRaftGroup(shardID types.ShardID, shard *Shard, peers []string) *ShardRaftGroup {
	return &ShardRaftGroup{
		shardID: shardID,
		shard:   shard,
		leader:  true, // bootstrap as leader
		peers:   peers,
	}
}

// Write applies a batch through Raft consensus. In M2 production,
// this goes through raft.Apply; for now, it delegates to Shard.Write.
func (g *ShardRaftGroup) Write(ctx context.Context, ops []WriteOp) (types.SeqNo, error) {
	if !g.IsLeader() {
		return 0, fmt.Errorf("shard-raft: not leader (forward to leader)")
	}
	return g.shard.Write(ctx, ops)
}

// Snapshot returns the current MVCC snapshot.
func (g *ShardRaftGroup) Snapshot() (types.Snapshot, *SegmentSet) {
	return g.shard.Snapshot()
}

// IsLeader returns true if this node is the Raft leader.
func (g *ShardRaftGroup) IsLeader() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.leader
}

// SetLeader sets the leader status (Raft callback).
func (g *ShardRaftGroup) SetLeader(leader bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.leader = leader
}

// Peers returns the replica group addresses.
func (g *ShardRaftGroup) Peers() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, len(g.peers))
	copy(out, g.peers)
	return out
}

// Close shuts down the Raft group.
func (g *ShardRaftGroup) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	return g.shard.Close()
}

// ShardID returns the shard identifier.
func (g *ShardRaftGroup) ShardID() types.ShardID {
	return g.shardID
}
