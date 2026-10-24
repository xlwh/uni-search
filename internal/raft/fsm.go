package raft

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// MetaFSM implements the Raft FSM interface over MetaState. The FSM
// is single-threaded (Raft guarantees Apply is called sequentially),
// so no locking is needed inside Apply methods.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.1
type MetaFSM struct {
	state *MetaState
}

// NewMetaFSM creates an FSM with a fresh MetaState.
func NewMetaFSM() *MetaFSM {
	return &MetaFSM{state: NewMetaState()}
}

// State returns the current MetaState (thread-safe for reads).
func (f *MetaFSM) State() *MetaState { return f.state }

// Apply applies a single FSM command to the state machine. The data
// parameter is the serialized FSMCommand.
func (f *MetaFSM) Apply(data []byte) error {
	var cmd FSMCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return fmt.Errorf("raft: decode FSM command: %w", err)
	}
	switch cmd.Type {
	case CmdCreateIndex:
		return f.state.applyCreateIndex(cmd.Payload)
	case CmdDeleteIndex:
		return f.state.applyDeleteIndex(cmd.Payload)
	case CmdUpdateMapping:
		// Reuse CreateIndex for mapping updates (idempotent)
		return f.state.applyCreateIndex(cmd.Payload)
	case CmdAllocateShard:
		return f.state.applyAllocateShard(cmd.Payload)
	case CmdSplitShard:
		return f.state.applySplitShard(cmd.Payload)
	case CmdMigrateShard:
		// Migrate = update RaftGroup in RouteEntry
		return f.state.applyAllocateShard(cmd.Payload)
	case CmdUpdateQuota:
		return f.state.applyUpdateQuota(cmd.Payload)
	case CmdRegisterNode:
		return f.state.applyRegisterNode(cmd.Payload)
	case CmdUnregisterNode:
		return f.state.applyUnregisterNode(cmd.Payload)
	case CmdAddReplica:
		// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.5
		return f.state.applyAddReplica(cmd.Payload)
	case CmdUpdateWatermark:
		return f.state.applyUpdateWatermark(cmd.Payload)
	default:
		return fmt.Errorf("raft: unknown FSM command type %d", cmd.Type)
	}
}

// Snapshot serializes the MetaState to JSON+gzip (D1: JSON snapshot).
func (f *MetaFSM) Snapshot() ([]byte, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	data, err := json.Marshal(f.state)
	if err != nil {
		return nil, fmt.Errorf("raft: snapshot marshal: %w", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		return nil, fmt.Errorf("raft: snapshot gzip: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("raft: snapshot gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// Restore replaces the MetaState from a JSON+gzip snapshot.
func (f *MetaFSM) Restore(data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("raft: restore gzip: %w", err)
	}
	defer gz.Close()
	raw, err := io.ReadAll(gz)
	if err != nil {
		return fmt.Errorf("raft: restore read: %w", err)
	}
	var state MetaState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("raft: restore unmarshal: %w", err)
	}
	// Initialize maps if nil (can happen after JSON unmarshal)
	if state.Schemas == nil {
		state.Schemas = make(map[types.IndexID]*meta.IndexMeta)
	}
	if state.Routes == nil {
		state.Routes = make(map[types.ShardID]*RouteEntry)
	}
	if state.Tenants == nil {
		state.Tenants = make(map[types.TenantID]*TenantMeta)
	}
	if state.Watermarks == nil {
		state.Watermarks = make(map[types.ShardID]types.SeqNo)
	}
	// Rebuild with proper types
	f.state = NewMetaState()
	f.state.Version = state.Version
	f.state.Schemas = state.Schemas
	f.state.Routes = state.Routes
	f.state.Tenants = state.Tenants
	f.state.Watermarks = state.Watermarks
	f.state.Members = state.Members
	return nil
}

// EncodeCommand creates a serialized FSMCommand.
func EncodeCommand(cmdType FSMCmdType, payload any) ([]byte, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("raft: encode command payload: %w", err)
	}
	cmd := FSMCommand{Type: cmdType, Payload: payloadBytes}
	return json.Marshal(cmd)
}
