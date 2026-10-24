package embedded

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// indexState is the runtime state for one ES index: the Shard (storage
// engine), the field-name→FieldID mapping, the field definitions, and
// the mini inverted index for memtable search (D1/D4).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.1
type indexState struct {
	name      string
	shard     *storage.Shard
	fieldIDs  map[string]types.FieldID   // field name → FieldID
	fieldDefs map[types.FieldID]*meta.FieldDef // FieldID → definition
	miniInv   *miniInvertedIndex          // memtable mini inverted index (D1/D4)
	cancel    context.CancelFunc         // stops shard background goroutine
	mu        sync.RWMutex                // protects fieldIDs/fieldDefs
}

// mappingFn returns the current field definitions for the Shard's
// flush worker (ShardConfig.MappingFn contract).
func (idx *indexState) mappingFn() map[types.FieldID]*meta.FieldDef {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make(map[types.FieldID]*meta.FieldDef, len(idx.fieldDefs))
	for k, v := range idx.fieldDefs {
		out[k] = v
	}
	return out
}

// resolveField returns the FieldID and FieldDef for a field name.
func (idx *indexState) resolveField(name string) (types.FieldID, *meta.FieldDef, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	fid, ok := idx.fieldIDs[name]
	if !ok {
		return 0, nil, false
	}
	return fid, idx.fieldDefs[fid], true
}

// nextFieldID returns the next available FieldID (max + 1).
func (idx *indexState) nextFieldID() types.FieldID {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var max types.FieldID
	for fid := range idx.fieldDefs {
		if fid > max {
			max = fid
		}
	}
	return max + 1
}

// addField allocates a FieldID for a new field and updates the mapping.
// Caller must hold the write lock or be in a single-writer context.
func (idx *indexState) addField(name string, def *meta.FieldDef) types.FieldID {
	fid := idx.nextFieldID()
	idx.mu.Lock()
	idx.fieldIDs[name] = fid
	def.Name = name
	idx.fieldDefs[fid] = def
	idx.mu.Unlock()
	return fid
}

// indexManager manages the runtime state of all indices.
type indexManager struct {
	store   *meta.Store
	indices map[string]*indexState
	dataDir string
	mu      sync.RWMutex
}

func newIndexManager(store *meta.Store, dataDir string) *indexManager {
	return &indexManager{
		store:   store,
		indices: make(map[string]*indexState),
		dataDir: dataDir,
	}
}

// createIndex creates a new index with the given mapping and opens its shard.
func (m *indexManager) createIndex(ctx context.Context, name string, mapping json.RawMessage) (*indexState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.indices[name]; exists {
		return nil, fmt.Errorf("embedded: index %q already exists", name)
	}

	// Parse mapping.
	var mp *meta.Mapping
	if len(mapping) > 0 {
		if err := json.Unmarshal(mapping, &mp); err != nil {
			return nil, fmt.Errorf("embedded: parse mapping: %w", err)
		}
	} else {
		mp = meta.NewMapping()
	}

	// Allocate FieldIDs: _id = 1, then sorted property keys.
	fieldIDs := make(map[string]types.FieldID)
	fieldDefs := make(map[types.FieldID]*meta.FieldDef)
	fieldIDs["_id"] = types.FieldID(1)
	fieldDefs[types.FieldID(1)] = &meta.FieldDef{Name: "_id", Type: meta.TypeKeyword, Index: true}

	nextID := types.FieldID(2)
	keys := make([]string, 0, len(mp.Properties))
	for k := range mp.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fd := mp.Properties[k]
		fd.Name = k
		if fd.Index == false && fd.Type != meta.TypeDenseVector {
			// default index=true for searchable types
		}
		fieldIDs[k] = nextID
		fieldDefs[nextID] = fd
		nextID++
	}

	// Create IndexMeta.
	im := &meta.IndexMeta{
		Name:      name,
		Mapping:   mp,
		Settings:  meta.IndexSettings{NumberOfShards: 1, NumberOfReplicas: 0, RefreshInterval: "1s"},
		Shards:    []types.ShardID{1},
		CreatedAt:  time.Now(),
		FieldIDs:  fieldIDs,
	}

	// Save meta first (so recovery knows about it).
	if err := m.store.Save(im); err != nil {
		return nil, fmt.Errorf("embedded: save meta: %w", err)
	}

	idx, err := m.openShardForIndex(ctx, name, im, fieldIDs, fieldDefs)
	if err != nil {
		_ = m.store.Delete(name)
		return nil, err
	}

	m.indices[name] = idx
	return idx, nil
}

// ensureIndex returns the existing index or creates an empty one.
func (m *indexManager) ensureIndex(ctx context.Context, name string) (*indexState, error) {
	m.mu.RLock()
	idx, ok := m.indices[name]
	m.mu.RUnlock()
	if ok {
		return idx, nil
	}
	return m.createIndex(ctx, name, nil)
}

// getIndex returns the index state, or nil if not found.
func (m *indexManager) getIndex(name string) (*indexState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	idx, ok := m.indices[name]
	return idx, ok
}

// deleteIndex closes the shard and removes the index.
func (m *indexManager) deleteIndex(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.indices[name]
	if !ok {
		return fmt.Errorf("embedded: index %q not found", name)
	}
	if idx.cancel != nil {
		idx.cancel()
	}
	if err := idx.shard.Close(); err != nil {
		return fmt.Errorf("embedded: close shard: %w", err)
	}
	// Remove shard data directory.
	shardDir := filepath.Join(m.dataDir, "shards", "1")
	_ = os.RemoveAll(shardDir)
	_ = m.store.Delete(name)
	delete(m.indices, name)
	return nil
}

// recoverAll loads all index metas from the store and reopens their shards.
func (m *indexManager) recoverAll(ctx context.Context) error {
	metas, err := m.store.List()
	if err != nil {
		return fmt.Errorf("embedded: list metas: %w", err)
	}
	for _, im := range metas {
		// Rebuild fieldIDs/fieldDefs from meta.
		fieldIDs := im.FieldIDs
		if fieldIDs == nil {
			// Backward compatibility: rebuild from mapping properties.
			fieldIDs = make(map[string]types.FieldID)
			fieldIDs["_id"] = types.FieldID(1)
			nextID := types.FieldID(2)
			keys := make([]string, 0, len(im.Mapping.Properties))
			for k := range im.Mapping.Properties {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fieldIDs[k] = nextID
				nextID++
			}
			im.FieldIDs = fieldIDs
			_ = m.store.Save(im)
		}
		fieldDefs := make(map[types.FieldID]*meta.FieldDef)
		for name, fid := range fieldIDs {
			fd := im.Mapping.Properties[name]
			if fd == nil {
				fd = &meta.FieldDef{Name: name, Type: meta.TypeKeyword, Index: true}
			}
			fd.Name = name
			fieldDefs[fid] = fd
		}
		idx, err := m.openShardForIndex(ctx, im.Name, im, fieldIDs, fieldDefs)
		if err != nil {
			return fmt.Errorf("embedded: recover index %q: %w", im.Name, err)
		}
		m.indices[im.Name] = idx
	}
	return nil
}

// openShardForIndex creates a shard for the given index and starts its
// background flush loop.
func (m *indexManager) openShardForIndex(ctx context.Context, name string, im *meta.IndexMeta, fieldIDs map[string]types.FieldID, fieldDefs map[types.FieldID]*meta.FieldDef) (*indexState, error) {
	idx := &indexState{
		name:      name,
		fieldIDs:  fieldIDs,
		fieldDefs: fieldDefs,
		miniInv:   newMiniInvertedIndex(),
	}

	// Each index gets its own subdirectory to isolate WAL/segment data.
	// Without this, all indices share the same shard directory and their
	// documents leak across index boundaries (critical isolation bug).
	indexDataDir := filepath.Join(m.dataDir, "indices", name)
	shardCtx, cancel := context.WithCancel(ctx)
	shard, err := storage.OpenShard(shardCtx, storage.ShardConfig{
		ShardID:     1,
		DataDir:     indexDataDir,
		ApplyInsert: applyInsertFn(idx),
		MappingFn:   idx.mappingFn,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("embedded: open shard for %q: %w", name, err)
	}
	idx.shard = shard
	idx.cancel = cancel

	// Start background flush loop.
	go shard.Run(shardCtx)

	return idx, nil
}

// closeAll closes all shards and cancels their background goroutines.
func (m *indexManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, idx := range m.indices {
		if idx.cancel != nil {
			idx.cancel()
		}
		_ = idx.shard.Close()
	}
	m.indices = make(map[string]*indexState)
}

// allIndices returns a snapshot of all index states.
func (m *indexManager) allIndices() []*indexState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*indexState, 0, len(m.indices))
	for _, idx := range m.indices {
		out = append(out, idx)
	}
	return out
}
