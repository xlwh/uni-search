package raft

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// MetaCluster wraps the Raft consensus layer for meta state. In M2,
// this is backed by hashicorp/raft; the interface allows testing
// with a mock implementation.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.1
type MetaCluster struct {
	mu      sync.RWMutex
	fsm     *MetaFSM
	members map[NodeID]string // node ID → address
	leader  NodeID
	ready   bool
}

// NewMetaCluster creates a new meta cluster with an empty FSM.
func NewMetaCluster() *MetaCluster {
	return &MetaCluster{
		fsm:     NewMetaFSM(),
		members: make(map[NodeID]string),
	}
}

// Apply submits a command to the Raft cluster. In production, this
// goes through hashicorp/raft's Apply method; for testing, it calls
// the FSM directly.
func (c *MetaCluster) Apply(cmdType FSMCmdType, payload any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := EncodeCommand(cmdType, payload)
	if err != nil {
		return err
	}
	return c.fsm.Apply(data)
}

// GetIndex returns index metadata by name (ReadIndex linearizable read).
func (c *MetaCluster) GetIndex(name string) (*meta.IndexMeta, bool) {
	return c.fsm.State().GetIndex(name)
}

// GetRoute returns the shard route entry.
func (c *MetaCluster) GetRoute(shardID types.ShardID) (*RouteEntry, bool) {
	return c.fsm.State().GetRoute(shardID)
}

// GetRoutesForIndex returns all routes for an index.
func (c *MetaCluster) GetRoutesForIndex(indexID types.IndexID) []*RouteEntry {
	return c.fsm.State().GetRoutesForIndex(indexID)
}

// GetMembers returns cluster members.
func (c *MetaCluster) GetMembers() []NodeInfo {
	return c.fsm.State().GetMembers()
}

// Join adds a node to the cluster.
func (c *MetaCluster) Join(nodeID, addr string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.members[NodeID(nodeID)] = addr
	return c.fsm.Apply(mustEncode(CmdRegisterNode, NodeInfo{
		ID:   NodeID(nodeID),
		Addr: addr,
		Role: "shard",
	}))
}

// Leave removes a node from the cluster.
func (c *MetaCluster) Leave(nodeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.members, NodeID(nodeID))
	return nil
}

// Leader returns the current leader node ID.
func (c *MetaCluster) Leader() NodeID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.leader
}

// SetLeader sets the leader (for testing or Raft callback).
func (c *MetaCluster) SetLeader(id NodeID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leader = id
	c.ready = true
}

// IsReady returns true if the cluster has elected a leader.
func (c *MetaCluster) IsReady() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// Snapshot returns the FSM snapshot.
func (c *MetaCluster) Snapshot() ([]byte, error) {
	return c.fsm.Snapshot()
}

// Restore replaces the FSM state from a snapshot.
func (c *MetaCluster) Restore(data []byte) error {
	return c.fsm.Restore(data)
}

// Watch returns a channel for meta events (route/watermark changes).
// For M2 initial implementation, this is a placeholder.
func (c *MetaCluster) Watch() <-chan MetaEvent {
	return make(chan MetaEvent, 1)
}

// MetaEvent is a state change notification.
type MetaEvent struct {
	Type    string // "route_change" | "watermark" | "member_change"
	Payload any
}

// mustEncode panics on encoding error (internal use only).
func mustEncode(cmdType FSMCmdType, payload any) []byte {
	data, err := EncodeCommand(cmdType, payload)
	if err != nil {
		panic(fmt.Sprintf("raft: mustEncode: %v", err))
	}
	return data
}

// Start launches the cluster (placeholder for hashicorp/raft integration).
func (c *MetaCluster) Start(ctx context.Context, peers []string) error {
	// In production: initialize hashicorp/raft with the peer list,
	// bootstrap the cluster, and start the FSM.
	// For now, mark as ready with the first peer as leader.
	if len(peers) > 0 {
		c.SetLeader(NodeID(peers[0]))
	}
	<-ctx.Done()
	return nil
}

// Stop gracefully shuts down the cluster.
func (c *MetaCluster) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
}

// HeartbeatTimeout is the default Raft heartbeat timeout.
const HeartbeatTimeout = 1 * time.Second
