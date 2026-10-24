package raft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// RaftClusterConfig holds configuration for a real hashicorp/raft cluster.
type RaftClusterConfig struct {
	NodeID      string
	DataDir     string
	RaftAddr    string  // e.g. "127.0.0.1:8080"
	Peers       []string  // peer addresses for bootstrap
	SnapshotDir string
}

// RaftCluster wraps hashicorp/raft with MetaFSM for real consensus.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.1
type RaftCluster struct {
	mu       sync.RWMutex
	raft     *raft.Raft
	fsm      *MetaFSM
	config   RaftClusterConfig
	transport raft.Transport
	logStore  raft.LogStore
	stableStore raft.StableStore
	snapStore  raft.SnapshotStore
	closed    bool
}

// NewRaftCluster creates a real hashicorp/raft-backed meta cluster.
func NewRaftCluster(cfg RaftClusterConfig) (*RaftCluster, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("raft: NodeID required")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("raft: DataDir required")
	}
	if cfg.RaftAddr == "" {
		return nil, fmt.Errorf("raft: RaftAddr required")
	}

	// Create directories.
	raftDir := filepath.Join(cfg.DataDir, "raft")
	if err := os.MkdirAll(raftDir, 0o755); err != nil {
		return nil, fmt.Errorf("raft: create raft dir: %w", err)
	}
	snapDir := cfg.SnapshotDir
	if snapDir == "" {
		snapDir = filepath.Join(raftDir, "snapshots")
	}
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return nil, fmt.Errorf("raft: create snapshot dir: %w", err)
	}

	// FSM — adapt MetaFSM to hashicorp/raft.FSM interface.
	innerFSM := NewMetaFSM()
	raftFSMAdapter := &raftFSM{inner: innerFSM}

	// Transport: TCP-based.  When the bind address is a wildcard (":9300"),
	// resolve the hostname so the advertise address is non-wildcard.
	bindAddr := cfg.RaftAddr
	advertiseAddr := cfg.RaftAddr
	if strings.HasPrefix(bindAddr, ":") {
		// Wildcard bind: determine advertise address from hostname.
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("raft: resolve hostname: %w", err)
		}
		advertiseAddr = hostname + bindAddr
	}
	addr, err := net.ResolveTCPAddr("tcp", advertiseAddr)
	if err != nil {
		return nil, fmt.Errorf("raft: resolve addr: %w", err)
	}
	transport, err := raft.NewTCPTransport(bindAddr, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft: new transport: %w", err)
	}

	// LogStore + StableStore: BoltDB.
	logStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("raft: new bolt store: %w", err)
	}
	stableStore := logStore

	// SnapshotStore: file-based.
	snapStore, err := raft.NewFileSnapshotStore(snapDir, 3, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft: new snapshot store: %w", err)
	}

	// Raft config.
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.SnapshotThreshold = 1024
	raftCfg.TrailingLogs = 256

	// Create Raft instance.
	r, err := raft.NewRaft(raftCfg, raftFSMAdapter, logStore, stableStore, snapStore, transport)
	if err != nil {
		return nil, fmt.Errorf("raft: new raft: %w", err)
	}

	// Bootstrap if peers are provided.
	if len(cfg.Peers) > 0 {
		servers := make([]raft.Server, len(cfg.Peers))
		for i, peer := range cfg.Peers {
			parts := splitPeer(peer)  // "id@addr" → (id, addr)
			servers[i] = raft.Server{
				ID:      raft.ServerID(parts[0]),
				Address: raft.ServerAddress(parts[1]),
			}
		}
		future := r.BootstrapCluster(raft.Configuration{Servers: servers})
		if err := future.Error(); err != nil {
			// Already bootstrapped is OK.
			if err != raft.ErrCantBootstrap {
				return nil, fmt.Errorf("raft: bootstrap: %w", err)
			}
		}
	}

	return &RaftCluster{
		raft:        r,
		fsm:         innerFSM,
		config:      cfg,
		transport:   transport,
		logStore:    logStore,
		snapStore:   snapStore,
		stableStore: stableStore,
	}, nil
}

// Apply submits a command through real Raft consensus.
func (c *RaftCluster) Apply(cmdType FSMCmdType, payload any) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("raft: cluster closed")
	}
	c.mu.RUnlock()

	data, err := EncodeCommand(cmdType, payload)
	if err != nil {
		return err
	}
	future := c.raft.Apply(data, 10*time.Second)
	return future.Error()
}

// GetIndex returns index metadata by name (linearizable read via
// VerifyLeader per design 05 §1.2). Falls back to local FSM read if
// leader verification fails (e.g., during leader election).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.3
func (c *RaftCluster) GetIndex(name string) (*meta.IndexMeta, bool) {
	// Attempt linearizable read via leader verification.
	fut := c.raft.VerifyLeader()
	if err := fut.Error(); err != nil {
		// Verification failed (leader change, not leader, etc.);
		// fall back to local FSM read (eventual consistency).
		return c.fsm.State().GetIndex(name)
	}
	// Leader verified: safe to read from FSM.
	return c.fsm.State().GetIndex(name)
}

// GetRoute returns the shard route entry.
func (c *RaftCluster) GetRoute(shardID types.ShardID) (*RouteEntry, bool) {
	return c.fsm.State().GetRoute(shardID)
}

// GetRoutesForIndex returns all routes for an index.
func (c *RaftCluster) GetRoutesForIndex(indexID types.IndexID) []*RouteEntry {
	return c.fsm.State().GetRoutesForIndex(indexID)
}

// GetMembers returns cluster members.
func (c *RaftCluster) GetMembers() []NodeInfo {
	return c.fsm.State().GetMembers()
}

// Leader returns the current Raft leader address.
func (c *RaftCluster) Leader() string {
	addr := c.raft.Leader()
	return string(addr)
}

// IsLeader returns true if this node is the Raft leader.
func (c *RaftCluster) IsLeader() bool {
	return c.raft.State() == raft.Leader
}

// IsReady returns true if a leader has been elected.
func (c *RaftCluster) IsReady() bool {
	return c.raft.State() == raft.Leader || c.raft.State() == raft.Follower
}

// ListIndices returns all index metadata from the FSM state.
func (c *RaftCluster) ListIndices() []*meta.IndexMeta {
	state := c.fsm.State()
	state.mu.RLock()
	defer state.mu.RUnlock()
	out := make([]*meta.IndexMeta, 0, len(state.Schemas))
	for _, im := range state.Schemas {
		out = append(out, im)
	}
	return out
}

// FSMState returns the underlying MetaState for direct read access.
func (c *RaftCluster) FSMState() *MetaState {
	return c.fsm.State()
}

// ListRoutes returns all shard route entries from the FSM state.
func (c *RaftCluster) ListRoutes() []*RouteEntry {
	state := c.fsm.State()
	state.mu.RLock()
	defer state.mu.RUnlock()
	out := make([]*RouteEntry, 0, len(state.Routes))
	for _, r := range state.Routes {
		out = append(out, r)
	}
	return out
}

// Snapshot triggers a manual snapshot.
func (c *RaftCluster) Snapshot() error {
	future := c.raft.Snapshot()
	return future.Error()
}

// Restore is not needed for Raft (handled automatically).
func (c *RaftCluster) Restore(data []byte) error {
	return c.fsm.Restore(data)
}

// Join adds a voter to the cluster (leader only).
func (c *RaftCluster) Join(nodeID, addr string) error {
	if !c.IsLeader() {
		return fmt.Errorf("raft: not leader")
	}
	future := c.raft.AddVoter(
		raft.ServerID(nodeID),
		raft.ServerAddress(addr),
		0, 10*time.Second,
	)
	return future.Error()
}

// Leave removes a voter from the cluster (leader only).
func (c *RaftCluster) Leave(nodeID string) error {
	if !c.IsLeader() {
		return fmt.Errorf("raft: not leader")
	}
	future := c.raft.RemoveServer(raft.ServerID(nodeID), 0, 10*time.Second)
	return future.Error()
}

// Close shuts down the Raft cluster.
func (c *RaftCluster) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.raft != nil {
		f := c.raft.Shutdown()
		_ = f.Error()
	}
	return nil
}

// Stats returns Raft cluster statistics.
func (c *RaftCluster) Stats() map[string]string {
	return c.raft.Stats()
}

// Start runs the cluster (blocks until context is cancelled).
func (c *RaftCluster) Start(ctx context.Context) error {
	<-ctx.Done()
	return c.Close()
}

// --- FSM adapter for hashicorp/raft ---

// raftFSM adapts MetaFSM to the hashicorp/raft.FSM interface.
type raftFSM struct {
	inner *MetaFSM
}

func (f *raftFSM) Apply(log *raft.Log) any {
	return f.inner.Apply(log.Data)
}

func (f *raftFSM) Snapshot() (raft.FSMSnapshot, error) {
	data, err := f.inner.Snapshot()
	if err != nil {
		return nil, err
	}
	return &raftSnapshot{data: data}, nil
}

func (f *raftFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	return f.inner.Restore(data)
}

type raftSnapshot struct {
	data []byte
}

func (s *raftSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *raftSnapshot) Release() {}

// splitPeer parses "id@addr" into (id, addr).
func splitPeer(peer string) [2]string {
	for i, c := range peer {
		if c == '@' {
			return [2]string{peer[:i], peer[i+1:]}
		}
	}
	return [2]string{peer, peer}
}

// Ensure json import is used.
var _ = json.Marshal
