// Package raft implements the M2 meta Raft cluster (design
// 05-distributed.md §1). It wraps hashicorp/raft with a MetaState FSM
// that holds index schemas, shard routes, tenant metadata, watermarks,
// and cluster members. FSM commands are protobuf-encoded; snapshots
// are JSON+gzip for human readability.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.1
package raft

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// MetaState is the Raft FSM state (design 05 §1.1). All data lives
// here; the Raft log only carries commands that mutate this state.
type MetaState struct {
	mu         sync.RWMutex
	Version    uint64                              // state machine version
	Schemas    map[types.IndexID]*meta.IndexMeta   // index → metadata
	Routes     map[types.ShardID]*RouteEntry       // shard → replica group
	Tenants    map[types.TenantID]*TenantMeta     // tenant → quota/isolation
	Watermarks map[types.ShardID]types.SeqNo      // per-shard visibility
	Members    []NodeInfo                          // cluster members
}

// RouteEntry maps a shard to its Raft replica group.
type RouteEntry struct {
	ShardID    types.ShardID
	IndexID    types.IndexID
	RaftGroup  []NodeID   // replica group (1 leader + N followers)
	Generation uint64     // split/migrate generation (D4: Gen dual-version)
}

// TenantMeta holds tenant configuration.
type TenantMeta struct {
	ID            types.TenantID
	Mode          string    // "dedicated" | "shared-shard" | "cold"
	Quota         TenantQuota
	ResourceGroup string
}

// TenantQuota is the four-dimensional resource limit.
type TenantQuota struct {
	StorageBytes int64
	VectorCount  int64
	QueryQPS     int
	WriteTPS     int
}

// NodeInfo describes a cluster member.
type NodeInfo struct {
	ID        NodeID
	Addr      string
	Role      string   // "meta" | "coordinator" | "shard"
	LastSeen  int64    // unix timestamp
	Resources ResourceReport
}

// ResourceReport is a node's resource usage snapshot.
type ResourceReport struct {
	CPUUsagePct float64
	MemUsagePct float64
	DiskUsagePct float64
	IOUsagePct  float64
}

// NodeID identifies a cluster node.
type NodeID string

// FSMCmdType enumerates the FSM command types (D1: Protobuf commands).
type FSMCmdType int

const (
	CmdCreateIndex FSMCmdType = iota + 1
	CmdDeleteIndex
	CmdUpdateMapping
	CmdAllocateShard
	CmdSplitShard
	CmdMigrateShard
	CmdUpdateQuota
	CmdRegisterNode
	CmdUnregisterNode
	CmdUpdateWatermark
	CmdAddReplica // @spec: deep-gap-remediation 02_spec.md#3.5
)

// FSMCommand is a single state machine command. The Payload carries
// the protobuf-encoded command-specific data.
type FSMCommand struct {
	Type    FSMCmdType
	Payload []byte
}

// NewMetaState creates an empty MetaState.
func NewMetaState() *MetaState {
	return &MetaState{
		Schemas:    make(map[types.IndexID]*meta.IndexMeta),
		Routes:     make(map[types.ShardID]*RouteEntry),
		Tenants:    make(map[types.TenantID]*TenantMeta),
		Watermarks: make(map[types.ShardID]types.SeqNo),
	}
}

// --- Read operations (thread-safe) ---

// GetIndex returns the IndexMeta for an index by name.
func (s *MetaState) GetIndex(name string) (*meta.IndexMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.Schemas {
		if m.Name == name {
			return m, true
		}
	}
	return nil, false
}

// GetRoute returns the RouteEntry for a shard.
func (s *MetaState) GetRoute(shardID types.ShardID) (*RouteEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.Routes[shardID]
	return r, ok
}

// GetRoutesForIndex returns all shard routes for an index.
func (s *MetaState) GetRoutesForIndex(indexID types.IndexID) []*RouteEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*RouteEntry
	for _, r := range s.Routes {
		if r.IndexID == indexID {
			out = append(out, r)
		}
	}
	return out
}

// GetTenant returns tenant metadata.
func (s *MetaState) GetTenant(id types.TenantID) (*TenantMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.Tenants[id]
	return t, ok
}

// GetMembers returns the current cluster members.
func (s *MetaState) GetMembers() []NodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]NodeInfo, len(s.Members))
	copy(out, s.Members)
	return out
}

// --- Write operations (called by FSM.Apply, single-threaded) ---

func (s *MetaState) applyCreateIndex(payload []byte) error {
	var cmd struct {
		IndexID types.IndexID
		Meta    *meta.IndexMeta
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return fmt.Errorf("raft: decode CreateIndex: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Schemas[cmd.IndexID] = cmd.Meta
	s.Version++
	return nil
}

func (s *MetaState) applyDeleteIndex(payload []byte) error {
	var cmd struct {
		IndexID types.IndexID
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return fmt.Errorf("raft: decode DeleteIndex: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Schemas, cmd.IndexID)
	// Remove associated routes
	for sid, r := range s.Routes {
		if r.IndexID == cmd.IndexID {
			delete(s.Routes, sid)
		}
	}
	s.Version++
	return nil
}

func (s *MetaState) applyAllocateShard(payload []byte) error {
	var cmd struct {
		ShardID   types.ShardID
		IndexID   types.IndexID
		RaftGroup []NodeID
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return fmt.Errorf("raft: decode AllocateShard: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Routes[cmd.ShardID] = &RouteEntry{
		ShardID:    cmd.ShardID,
		IndexID:    cmd.IndexID,
		RaftGroup:  cmd.RaftGroup,
		Generation: 1,
	}
	s.Version++
	return nil
}

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.5
func (s *MetaState) applyAddReplica(payload []byte) error {
	var cmd struct {
		ShardID       types.ShardID
		ReplicaNodeID NodeID
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return fmt.Errorf("raft: decode AddReplica: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	route, ok := s.Routes[cmd.ShardID]
	if !ok {
		return fmt.Errorf("raft: AddReplica: shard %d not found", cmd.ShardID)
	}
	// Check if replica already exists (idempotent).
	for _, n := range route.RaftGroup {
		if n == cmd.ReplicaNodeID {
			return nil
		}
	}
	route.RaftGroup = append(route.RaftGroup, cmd.ReplicaNodeID)
	s.Version++
	return nil
}

func (s *MetaState) applySplitShard(payload []byte) error {
	var cmd struct {
		ParentShardID types.ShardID
		ChildShardIDs  []types.ShardID
		NewGeneration  uint64
		RaftGroups     [][]NodeID // one per child
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return fmt.Errorf("raft: decode SplitShard: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Update parent generation
	if parent, ok := s.Routes[cmd.ParentShardID]; ok {
		parent.Generation = cmd.NewGeneration
	}
	// Create child routes
	for i, childID := range cmd.ChildShardIDs {
		var group []NodeID
		if i < len(cmd.RaftGroups) {
			group = cmd.RaftGroups[i]
		}
		s.Routes[childID] = &RouteEntry{
			ShardID:    childID,
			Generation: cmd.NewGeneration,
			RaftGroup:  group,
		}
	}
	s.Version++
	return nil
}

func (s *MetaState) applyRegisterNode(payload []byte) error {
	var node NodeInfo
	if err := json.Unmarshal(payload, &node); err != nil {
		return fmt.Errorf("raft: decode RegisterNode: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Update or append
	found := false
	for i, m := range s.Members {
		if m.ID == node.ID {
			s.Members[i] = node
			found = true
			break
		}
	}
	if !found {
		s.Members = append(s.Members, node)
	}
	s.Version++
	return nil
}

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B1
func (s *MetaState) applyUnregisterNode(payload []byte) error {
	var node NodeInfo
	if err := json.Unmarshal(payload, &node); err != nil {
		return fmt.Errorf("raft: decode UnregisterNode: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.Members {
		if m.ID == node.ID {
			s.Members = append(s.Members[:i], s.Members[i+1:]...)
			s.Version++
			return nil
		}
	}
	return nil // idempotent: node not found is a no-op
}

func (s *MetaState) applyUpdateWatermark(payload []byte) error {
	var cmd struct {
		ShardID   types.ShardID
		Watermark types.SeqNo
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return fmt.Errorf("raft: decode UpdateWatermark: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Watermarks[cmd.ShardID] = cmd.Watermark
	s.Version++
	return nil
}

func (s *MetaState) applyUpdateQuota(payload []byte) error {
	var cmd struct {
		TenantID types.TenantID
		Quota    TenantQuota
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return fmt.Errorf("raft: decode UpdateQuota: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.Tenants[cmd.TenantID]; ok {
		t.Quota = cmd.Quota
	}
	s.Version++
	return nil
}
