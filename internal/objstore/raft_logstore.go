package objstore

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// raft_logstore.go implements the hashicorp/raft LogStore and
// StableStore interfaces over local NVMe files, enabling the
// object-storage WAL to serve as Raft's log storage (design 05 §2.2).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.1

// RaftLogStore implements both raft.LogStore and raft.StableStore
// over local NVMe files. Each log entry is stored as a separate file
// named by its index. Stable store uses a single key-value file.
type RaftLogStore struct {
	mu       sync.RWMutex
	baseDir  string
	stableKV map[string][]byte // in-memory stable store (persisted on Set)
}

// NewRaftLogStore creates a Raft log/stable store rooted at baseDir.
func NewRaftLogStore(baseDir string) (*RaftLogStore, error) {
	if err := os.MkdirAll(filepath.Join(baseDir, "logs"), 0o755); err != nil {
		return nil, fmt.Errorf("objstore: create raft log dir: %w", err)
	}
	rls := &RaftLogStore{
		baseDir:  baseDir,
		stableKV: make(map[string][]byte),
	}
	// Load stable store from disk if it exists.
	rls.loadStable()
	return rls, nil
}

// --- raft.LogStore interface ---

func (s *RaftLogStore) FirstIndex() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	logs := s.listLogs()
	if len(logs) == 0 {
		return 0, nil
	}
	return logs[0], nil
}

func (s *RaftLogStore) LastIndex() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	logs := s.listLogs()
	if len(logs) == 0 {
		return 0, nil
	}
	return logs[len(logs)-1], nil
}

func (s *RaftLogStore) GetLog(index uint64, log *raft.Log) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path := s.logPath(index)
	data, err := os.ReadFile(path)
	if err != nil {
		return raft.ErrLogNotFound
	}
	return decodeRaftLog(data, log)
}

func (s *RaftLogStore) StoreLog(log *raft.Log) error {
	return s.StoreLogs([]*raft.Log{log})
}

func (s *RaftLogStore) StoreLogs(logs []*raft.Log) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, log := range logs {
		data := encodeRaftLog(log)
		path := s.logPath(log.Index)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("objstore: store log %d: %w", log.Index, err)
		}
	}
	return nil
}

func (s *RaftLogStore) DeleteRange(min, max uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := min; i <= max; i++ {
		_ = os.Remove(s.logPath(i))
	}
	return nil
}

// --- raft.StableStore interface ---

func (s *RaftLogStore) Set(key []byte, val []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stableKV[string(key)] = append([]byte(nil), val...)
	return s.persistStable()
}

func (s *RaftLogStore) Get(key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.stableKV[string(key)]; ok {
		return append([]byte(nil), v...), nil
	}
	return nil, fmt.Errorf("objstore: stable key not found")
}

func (s *RaftLogStore) SetUint64(key []byte, val uint64) error {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, val)
	return s.Set(key, buf)
}

func (s *RaftLogStore) GetUint64(key []byte) (uint64, error) {
	val, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	if len(val) < 8 {
		return 0, fmt.Errorf("objstore: stable value too short")
	}
	return binary.LittleEndian.Uint64(val[:8]), nil
}

// --- helpers ---

func (s *RaftLogStore) logPath(index uint64) string {
	return filepath.Join(s.baseDir, "logs", fmt.Sprintf("%020d.log", index))
}

func (s *RaftLogStore) listLogs() []uint64 {
	dir := filepath.Join(s.baseDir, "logs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var indices []uint64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var idx uint64
		if _, err := fmt.Sscanf(e.Name(), "%020d.log", &idx); err == nil {
			indices = append(indices, idx)
		}
	}
	// Simple sort (indices are zero-padded so lexical = numeric).
	return indices
}

func (s *RaftLogStore) stablePath() string {
	return filepath.Join(s.baseDir, "stable.kv")
}

func (s *RaftLogStore) persistStable() error {
	// Simple persistence: write all key-value pairs as length-prefixed.
	path := s.stablePath()
	var buf []byte
	for k, v := range s.stableKV {
		kl := uint32(len(k))
		vl := uint32(len(v))
		header := make([]byte, 8)
		binary.LittleEndian.PutUint32(header[:4], kl)
		binary.LittleEndian.PutUint32(header[4:], vl)
		buf = append(buf, header...)
		buf = append(buf, k...)
		buf = append(buf, v...)
	}
	return os.WriteFile(path, buf, 0o644)
}

func (s *RaftLogStore) loadStable() {
	path := s.stablePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for len(data) >= 8 {
		kl := binary.LittleEndian.Uint32(data[:4])
		vl := binary.LittleEndian.Uint32(data[4:8])
		data = data[8:]
		if uint32(len(data)) < kl+vl {
			break
		}
		key := string(data[:kl])
		val := append([]byte(nil), data[kl:kl+vl]...)
		s.stableKV[key] = val
		data = data[kl+vl:]
	}
}

// encodeRaftLog serializes a raft.Log for file storage.
func encodeRaftLog(log *raft.Log) []byte {
	// Format: u64 index | u64 term | u8 type | u32 dataLen | data
	var buf []byte
	buf = binary.LittleEndian.AppendUint64(buf, log.Index)
	buf = binary.LittleEndian.AppendUint64(buf, log.Term)
	buf = append(buf, byte(log.Type))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(log.Data)))
	buf = append(buf, log.Data...)
	// Append extensions if present.
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(log.Extensions)))
	buf = append(buf, log.Extensions...)
	// Append appendedAt if present (u64 nanos).
	var appendedAt uint64
	if !log.AppendedAt.IsZero() {
		appendedAt = uint64(log.AppendedAt.UnixNano())
	}
	buf = binary.LittleEndian.AppendUint64(buf, appendedAt)
	return buf
}

// decodeRaftLog deserializes a raft.Log from file storage.
func decodeRaftLog(data []byte, log *raft.Log) error {
	if len(data) < 21 {
		return fmt.Errorf("objstore: corrupt raft log (too short)")
	}
	log.Index = binary.LittleEndian.Uint64(data[:8])
	log.Term = binary.LittleEndian.Uint64(data[8:16])
	log.Type = raft.LogType(data[16])
	dl := binary.LittleEndian.Uint32(data[17:21])
	if uint32(len(data)) < 21+dl+4 {
		return fmt.Errorf("objstore: corrupt raft log (data truncated)")
	}
	log.Data = append([]byte(nil), data[21:21+dl]...)
	off := 21 + dl
	el := binary.LittleEndian.Uint32(data[off : off+4])
	off += 4
	if uint32(len(data)) >= off+el+8 {
		log.Extensions = append([]byte(nil), data[off:off+el]...)
		off += el
		appendedAt := binary.LittleEndian.Uint64(data[off : off+8])
		if appendedAt > 0 {
			log.AppendedAt = time.Unix(0, int64(appendedAt))
		}
	}
	return nil
}

// Compile-time assertions.
var (
	_ raft.LogStore   = (*RaftLogStore)(nil)
	_ raft.StableStore = (*RaftLogStore)(nil)
)

// Unused import guard.
var _ = types.SegmentID(0)
