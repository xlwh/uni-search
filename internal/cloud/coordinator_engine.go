// Package cloud implements the CoordinatorEngine for cloud-native mode.
// The CoordinatorEngine implements esapi.Engine by routing metadata
// operations to the Raft cluster and data operations to shard nodes
// via gRPC (design 05 §6).
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	gen "github.com/unisearch-db/unisearch/api/proto/gen"
	"github.com/unisearch-db/unisearch/internal/coordinator"
	"github.com/unisearch-db/unisearch/internal/jsonx"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/raft"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// CoordinatorEngine implements esapi.Engine for cloud mode. It runs on
// meta nodes and routes writes/searches to shard nodes via gRPC while
// managing index metadata through the Raft cluster.
type CoordinatorEngine struct {
	cluster      *raft.RaftCluster
	mu           sync.RWMutex
	shardClients map[string]*coordinator.GRPCShardClient
}

// NewCoordinatorEngine creates a coordinator engine backed by the given
// Raft cluster.
func NewCoordinatorEngine(cluster *raft.RaftCluster) *CoordinatorEngine {
	return &CoordinatorEngine{
		cluster:      cluster,
		shardClients: make(map[string]*coordinator.GRPCShardClient),
	}
}

// --- helpers ---

// hashRoute computes the shard number for a document ID using the same
// algorithm as coordinator.IngestCoordinator.Route (h = h*31 + byte).
func hashRoute(id string, numShards int) uint64 {
	if numShards <= 0 {
		return 0
	}
	h := uint64(0)
	for _, b := range []byte(id) {
		h = h*31 + uint64(b)
	}
	return h % uint64(numShards)
}

// indexIDFromName generates a deterministic IndexID from an index name.
func indexIDFromName(name string) types.IndexID {
	h := fnv.New32a()
	h.Write([]byte(name))
	return types.IndexID(h.Sum32())
}

// shardIDFromIndex generates a globally-unique ShardID from an IndexID
// and shard number, preventing collisions across indices.
func shardIDFromIndex(indexID types.IndexID, shardNum int) types.ShardID {
	return types.ShardID(uint64(indexID)<<20 | uint64(shardNum))
}

// getShardClient returns a cached or newly-created gRPC client for the
// given shard node address.
func (e *CoordinatorEngine) getShardClient(addr string) (*coordinator.GRPCShardClient, error) {
	e.mu.RLock()
	if c, ok := e.shardClients[addr]; ok {
		e.mu.RUnlock()
		return c, nil
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	// Double-check after acquiring write lock.
	if c, ok := e.shardClients[addr]; ok {
		return c, nil
	}
	c, err := coordinator.NewGRPCShardClient(addr)
	if err != nil {
		return nil, fmt.Errorf("cloud: dial shard %s: %w", addr, err)
	}
	e.shardClients[addr] = c
	return c, nil
}

// getShardNodes returns the IDs of all registered shard-role nodes.
func (e *CoordinatorEngine) getShardNodes() []raft.NodeID {
	members := e.cluster.GetMembers()
	var nodes []raft.NodeID
	for _, m := range members {
		if m.Role == "shard" {
			nodes = append(nodes, m.ID)
		}
	}
	return nodes
}

// nodeAddrByID looks up a node's gRPC address by its NodeID.
func (e *CoordinatorEngine) nodeAddrByID(id raft.NodeID) (string, bool) {
	members := e.cluster.GetMembers()
	for _, m := range members {
		if m.ID == id {
			return m.Addr, true
		}
	}
	return "", false
}

// getNumShards returns the configured number of shards for an index.
// Falls back to counting actual routes if index metadata is unavailable.
func (e *CoordinatorEngine) getNumShards(indexName string) int {
	im, ok := e.cluster.GetIndex(indexName)
	if ok && im.Settings.NumberOfShards > 0 {
		return im.Settings.NumberOfShards
	}
	// Fallback: count routes for this index.
	indexID := indexIDFromName(indexName)
	routes := e.cluster.GetRoutesForIndex(indexID)
	if len(routes) > 0 {
		return len(routes)
	}
	return 1
}

// getShardAddr returns the gRPC address of the primary node for a given
// shard number within an index.
func (e *CoordinatorEngine) getShardAddr(indexName string, shardNum int) (string, error) {
	indexID := indexIDFromName(indexName)
	sid := shardIDFromIndex(indexID, shardNum)
	route, ok := e.cluster.GetRoute(sid)
	if !ok {
		return "", fmt.Errorf("cloud: no route for shard %d (index %q)", shardNum, indexName)
	}
	if len(route.RaftGroup) == 0 {
		return "", fmt.Errorf("cloud: empty raft group for shard %d", shardNum)
	}
	addr, ok := e.nodeAddrByID(route.RaftGroup[0])
	if !ok {
		return "", fmt.Errorf("cloud: shard node %s not found in members", route.RaftGroup[0])
	}
	return addr, nil
}

// getAllShardAddrs returns the gRPC addresses of all primary shard nodes
// for the given index, deduplicated.
func (e *CoordinatorEngine) getAllShardAddrs(indexName string) []string {
	indexID := indexIDFromName(indexName)
	routes := e.cluster.GetRoutesForIndex(indexID)
	seen := make(map[string]bool)
	var addrs []string
	for _, r := range routes {
		if len(r.RaftGroup) == 0 {
			continue
		}
		addr, ok := e.nodeAddrByID(r.RaftGroup[0])
		if ok && !seen[addr] {
			seen[addr] = true
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// ensureIndex checks if an index exists. If it doesn't, the caller
// should have created it via PutMapping first. We do NOT auto-create
// a 1-shard index here because that would overwrite the routing table
// for a multi-shard index created via PUT /{index}.
func (e *CoordinatorEngine) ensureIndex(indexName string) error {
	if _, ok := e.cluster.GetIndex(indexName); ok {
		return nil
	}
	// Index metadata not found, but routes may still exist (e.g., if
	// the index was created on a different meta node). Don't create a
	// new index — just return nil and let the routing logic use the
	// existing routes.
	return nil
}

// createIndex creates an index in the Raft FSM and allocates shards to
// registered shard nodes.
func (e *CoordinatorEngine) createIndex(indexName string, numShards int, mapping *meta.Mapping, settingsBody []byte) error {
	indexID := indexIDFromName(indexName)
	if mapping == nil {
		mapping = meta.NewMapping()
	}
	if numShards <= 0 {
		numShards = 1
	}

	// Parse optional settings (refresh_interval, etc.).
	refreshInterval := "1s"
	if settingsBody != nil {
		var s struct {
			RefreshInterval string `json:"refresh_interval"`
		}
		_ = jsonx.Unmarshal(settingsBody, &s)
		if s.RefreshInterval != "" {
			refreshInterval = s.RefreshInterval
		}
	}

	shardIDs := make([]types.ShardID, numShards)
	for i := 0; i < numShards; i++ {
		shardIDs[i] = shardIDFromIndex(indexID, i)
	}

	im := &meta.IndexMeta{
		ID:        types.TenantID(1),
		Name:      indexName,
		Mapping:   mapping,
		Settings:  meta.IndexSettings{NumberOfShards: numShards, NumberOfReplicas: 0, RefreshInterval: refreshInterval},
		Shards:    shardIDs,
		CreatedAt: time.Now(),
	}

	if err := e.cluster.Apply(raft.CmdCreateIndex, struct {
		IndexID types.IndexID
		Meta    *meta.IndexMeta
	}{indexID, im}); err != nil {
		return fmt.Errorf("cloud: raft CreateIndex: %w", err)
	}

	shardNodes := e.getShardNodes()
	if len(shardNodes) == 0 {
		return fmt.Errorf("cloud: no shard nodes registered")
	}

	for i := 0; i < numShards; i++ {
		sid := shardIDs[i]
		nodeID := shardNodes[i%len(shardNodes)]
		if err := e.cluster.Apply(raft.CmdAllocateShard, struct {
			ShardID   types.ShardID
			IndexID   types.IndexID
			RaftGroup []raft.NodeID
		}{sid, indexID, []raft.NodeID{nodeID}}); err != nil {
			return fmt.Errorf("cloud: raft AllocateShard %d: %w", i, err)
		}
	}
	return nil
}

// --- Engine interface: Write path ---

// Write routes a single document to the correct shard via gRPC.
func (e *CoordinatorEngine) Write(ctx context.Context, doc esapi.IndexDoc, refresh string) (*esapi.WriteResult, error) {
	if err := e.ensureIndex(doc.Index); err != nil {
		return nil, err
	}
	numShards := e.getNumShards(doc.Index)
	shardNum := int(hashRoute(doc.ID, numShards))
	addr, err := e.getShardAddr(doc.Index, shardNum)
	if err != nil {
		return nil, err
	}
	client, err := e.getShardClient(addr)
	if err != nil {
		return nil, err
	}

	batch := &gen.WriteBatch{
		Index: doc.Index,
		Ops:   []*gen.WriteOp{{Kind: string(doc.Op), Id: doc.ID, Doc: doc.Source}},
	}
	if refresh == "true" {
		batch.Refresh = true
	}

	ack, err := client.Write(ctx, batch)
	if err != nil {
		return nil, fmt.Errorf("cloud: grpc write to %s: %w", addr, err)
	}

	result := &esapi.WriteResult{Index: doc.Index, ID: doc.ID, Op: doc.Op}
	if len(ack.Results) > 0 {
		result.Result = ack.Results[0].Result
		result.SeqNo = ack.Results[0].SeqNo
	}
	if !ack.Success {
		result.Result = "error"
	}
	result.Version = 1
	return result, nil
}

// Bulk groups documents by shard and sends batches via gRPC.
func (e *CoordinatorEngine) Bulk(ctx context.Context, docs []esapi.IndexDoc, refresh string) ([]esapi.WriteResult, error) {
	results := make([]esapi.WriteResult, len(docs))
	// Group by (index, shardNum) → positions.
	type groupKey struct {
		index    string
		shardNum int
	}
	groups := make(map[groupKey][]int)
	for i, doc := range docs {
		if err := e.ensureIndex(doc.Index); err != nil {
			results[i] = esapi.WriteResult{Index: doc.Index, ID: doc.ID, Op: doc.Op, Result: "error", Err: err}
			continue
		}
		numShards := e.getNumShards(doc.Index)
		shardNum := int(hashRoute(doc.ID, numShards))
		key := groupKey{doc.Index, shardNum}
		groups[key] = append(groups[key], i)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for key, positions := range groups {
		wg.Add(1)
		go func(key groupKey, positions []int) {
			defer wg.Done()
			addr, err := e.getShardAddr(key.index, key.shardNum)
			if err != nil {
				mu.Lock()
				for _, pos := range positions {
					results[pos] = esapi.WriteResult{Index: key.index, ID: docs[pos].ID, Op: docs[pos].Op, Result: "error", Err: err}
				}
				mu.Unlock()
				return
			}
			client, err := e.getShardClient(addr)
			if err != nil {
				mu.Lock()
				for _, pos := range positions {
					results[pos] = esapi.WriteResult{Index: key.index, ID: docs[pos].ID, Op: docs[pos].Op, Result: "error", Err: err}
				}
				mu.Unlock()
				return
			}
			ops := make([]*gen.WriteOp, len(positions))
			for i, pos := range positions {
				ops[i] = &gen.WriteOp{Kind: string(docs[pos].Op), Id: docs[pos].ID, Doc: docs[pos].Source}
			}
			batch := &gen.WriteBatch{Index: key.index, Ops: ops}
			if refresh == "true" {
				batch.Refresh = true
			}
			ack, err := client.Write(ctx, batch)
			if err != nil {
				mu.Lock()
				for _, pos := range positions {
					results[pos] = esapi.WriteResult{Index: key.index, ID: docs[pos].ID, Op: docs[pos].Op, Result: "error", Err: err}
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			for i, pos := range positions {
				r := esapi.WriteResult{Index: key.index, ID: docs[pos].ID, Op: docs[pos].Op, Version: 1}
				if i < len(ack.Results) {
					r.Result = ack.Results[i].Result
					r.SeqNo = ack.Results[i].SeqNo
				}
				if !ack.Success {
					r.Result = "error"
				}
				results[pos] = r
			}
			mu.Unlock()
		}(key, positions)
	}
	wg.Wait()
	return results, nil
}

// --- Engine interface: Get path ---

// Get retrieves a single document via gRPC from the correct shard.
func (e *CoordinatorEngine) Get(ctx context.Context, index, id string) ([]byte, int64, int64, bool, error) {
	if err := e.ensureIndex(index); err != nil {
		return nil, 0, 0, true, err
	}
	numShards := e.getNumShards(index)
	shardNum := int(hashRoute(id, numShards))
	addr, err := e.getShardAddr(index, shardNum)
	if err != nil {
		return nil, 0, 0, true, err
	}
	client, err := e.getShardClient(addr)
	if err != nil {
		return nil, 0, 0, true, err
	}
	source, found, err := client.Get(ctx, index, id)
	if err != nil {
		return nil, 0, 0, true, fmt.Errorf("cloud: grpc get: %w", err)
	}
	return source, 0, 1, !found, nil
}

// MGet resolves multiple (index, id) pairs by routing each to its shard.
func (e *CoordinatorEngine) MGet(ctx context.Context, reqs []esapi.GetRequest) ([]esapi.GetResult, error) {
	results := make([]esapi.GetResult, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func(idx int, req esapi.GetRequest) {
			defer wg.Done()
			source, seqNo, primary, notFound, err := e.Get(ctx, req.Index, req.ID)
			if err != nil {
				results[idx] = esapi.GetResult{Index: req.Index, ID: req.ID, Found: false}
				return
			}
			results[idx] = esapi.GetResult{Index: req.Index, ID: req.ID, Found: !notFound, Source: source, SeqNo: seqNo, Primary: primary}
		}(i, req)
	}
	wg.Wait()
	return results, nil
}

// --- Engine interface: Delete ---

// Delete routes a delete operation to the correct shard via gRPC.
func (e *CoordinatorEngine) Delete(ctx context.Context, index, id string, ifSeqNo *int64, refresh string) (*esapi.WriteResult, error) {
	if err := e.ensureIndex(index); err != nil {
		return nil, err
	}
	numShards := e.getNumShards(index)
	shardNum := int(hashRoute(id, numShards))
	addr, err := e.getShardAddr(index, shardNum)
	if err != nil {
		return nil, err
	}
	client, err := e.getShardClient(addr)
	if err != nil {
		return nil, err
	}
	batch := &gen.WriteBatch{
		Index: index,
		Ops:   []*gen.WriteOp{{Kind: "delete", Id: id}},
	}
	if refresh == "true" {
		batch.Refresh = true
	}
	ack, err := client.Write(ctx, batch)
	if err != nil {
		return nil, fmt.Errorf("cloud: grpc delete: %w", err)
	}
	result := &esapi.WriteResult{Index: index, ID: id, Op: esapi.OpDelete, Version: 1, Result: "deleted"}
	if len(ack.Results) > 0 {
		result.Result = ack.Results[0].Result
		result.SeqNo = ack.Results[0].SeqNo
	}
	return result, nil
}

// --- Engine interface: Search ---

// Search fans out to all shard nodes for the given indices and merges
// results by descending score.
func (e *CoordinatorEngine) Search(ctx context.Context, indices []string, body []byte) (*esapi.SearchResult, error) {
	// Parse from/size from the body for global pagination.
	var parsed struct {
		From int `json:"from"`
		Size int `json:"size"`
	}
	_ = jsonx.Unmarshal(body, &parsed)
	from := parsed.From
	size := parsed.Size
	if size <= 0 {
		size = 10
	}

	// Collect unique shard addresses across all indices.
	// Do NOT call ensureIndex here — search should not auto-create indexes.
	addrSet := make(map[string]bool)
	for _, idx := range indices {
		for _, addr := range e.getAllShardAddrs(idx) {
			addrSet[addr] = true
		}
	}

	if len(addrSet) == 0 {
		return &esapi.SearchResult{
			Shards:   esapi.ShardStats{Total: 0, Successful: 0},
			Total:    0,
			Relation: "eq",
			Hits:     []esapi.SearchHit{},
		}, nil
	}

	type shardResult struct {
		resp *gen.SearchResponse
		err  error
	}
	results := make([]shardResult, 0, len(addrSet))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for addr := range addrSet {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			client, err := e.getShardClient(addr)
			if err != nil {
				mu.Lock()
				results = append(results, shardResult{nil, err})
				mu.Unlock()
				return
			}
			resp, err := client.SearchRaw(ctx, &gen.SearchRequest{Indices: indices, Body: body})
			mu.Lock()
			results = append(results, shardResult{resp, err})
			mu.Unlock()
		}(addr)
	}
	wg.Wait()

	// Merge results.
	var allHits []esapi.SearchHit
	total := 0
	relation := "eq"
	var maxScore *float32
	var aggs map[string]any
	successful := 0
	failed := 0

	for _, sr := range results {
		if sr.err != nil || sr.resp == nil {
			failed++
			continue
		}
		successful++
		total += int(sr.resp.Total)
		if sr.resp.Relation != "" {
			relation = sr.resp.Relation
		}
		if sr.resp.MaxScore > 0 {
			s := sr.resp.MaxScore
			if maxScore == nil || s > *maxScore {
				maxScore = &s
			}
		}
		for _, h := range sr.resp.Hits {
			allHits = append(allHits, esapi.SearchHit{
				Index:  h.Index,
				ID:     h.Id,
				Score:  h.Score,
				Source: h.Source,
				SeqNo:  h.SeqNo,
			})
		}
		if aggs == nil && len(sr.resp.Aggregations) > 0 {
			_ = jsonx.Unmarshal(sr.resp.Aggregations, &aggs)
		}
	}

	// Sort by score descending.
	sort.Slice(allHits, func(i, j int) bool {
		return allHits[i].Score > allHits[j].Score
	})

	// Apply global from/size pagination.
	end := from + size
	if end > len(allHits) {
		end = len(allHits)
	}
	if from > len(allHits) {
		from = len(allHits)
	}
	paged := allHits[from:end]

	return &esapi.SearchResult{
		Shards:       esapi.ShardStats{Total: len(addrSet), Successful: successful, Failed: failed},
		Total:        total,
		Relation:     relation,
		MaxScore:     maxScore,
		Hits:         paged,
		Aggregations: aggs,
	}, nil
}

// Count fans out to all shard nodes and sums the counts.
func (e *CoordinatorEngine) Count(ctx context.Context, indices []string, body []byte) (int64, error) {
	// Default to match_all when body is empty (GET /_count has no body).
	if len(body) == 0 {
		body = []byte(`{"query":{"match_all":{}}}`)
	}
	// Do NOT call ensureIndex — count should not auto-create indexes.
	addrSet := make(map[string]bool)
	for _, idx := range indices {
		for _, addr := range e.getAllShardAddrs(idx) {
			addrSet[addr] = true
		}
	}

	if len(addrSet) == 0 {
		return 0, nil
	}

	var total int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for addr := range addrSet {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			client, err := e.getShardClient(addr)
			if err != nil {
				return
			}
			resp, err := client.SearchRaw(ctx, &gen.SearchRequest{Indices: indices, Body: body})
			if err != nil || resp == nil {
				return
			}
			mu.Lock()
			total += int64(resp.Total)
			mu.Unlock()
		}(addr)
	}
	wg.Wait()
	return total, nil
}

// --- Engine interface: Metadata ---

// PutMapping creates or updates an index mapping. In cloud mode this
// applies CreateIndex to the Raft cluster and allocates shards.
func (e *CoordinatorEngine) PutMapping(ctx context.Context, index string, body []byte) error {
	var spec struct {
		Mappings *meta.Mapping   `json:"mappings"`
		Settings json.RawMessage `json:"settings"`
	}
	if err := jsonx.Unmarshal(body, &spec); err != nil {
		return fmt.Errorf("cloud: parse mapping body: %w", err)
	}

	numShards := 1
	if spec.Settings != nil {
		var s struct {
			NumberOfShards int `json:"number_of_shards"`
		}
		_ = jsonx.Unmarshal(spec.Settings, &s)
		if s.NumberOfShards > 0 {
			numShards = s.NumberOfShards
		}
	}

	// If index already exists, this is a mapping update — for simplicity
	// we skip re-allocation and just update the FSM.
	if _, ok := e.cluster.GetIndex(index); ok {
		indexID := indexIDFromName(index)
		im, _ := e.cluster.GetIndex(index)
		if spec.Mappings != nil {
			im.Mapping = spec.Mappings
		}
		return e.cluster.Apply(raft.CmdUpdateMapping, struct {
			IndexID types.IndexID
			Meta    *meta.IndexMeta
		}{indexID, im})
	}

	return e.createIndex(index, numShards, spec.Mappings, spec.Settings)
}

// GetMapping returns the stored mapping JSON for an index.
func (e *CoordinatorEngine) GetMapping(ctx context.Context, index string) ([]byte, error) {
	im, ok := e.cluster.GetIndex(index)
	if !ok {
		return nil, fmt.Errorf("cloud: index %q not found", index)
	}
	out := map[string]any{
		index: map[string]any{
			"mappings": im.Mapping,
		},
	}
	return jsonx.Marshal(out)
}

// DeleteIndex removes an index and all its data via Raft.
func (e *CoordinatorEngine) DeleteIndex(ctx context.Context, index string) error {
	indexID := indexIDFromName(index)
	return e.cluster.Apply(raft.CmdDeleteIndex, struct {
		IndexID types.IndexID
	}{indexID})
}

// PutSettings updates index settings (simplified: stored in FSM).
func (e *CoordinatorEngine) PutSettings(ctx context.Context, index string, body []byte) error {
	im, ok := e.cluster.GetIndex(index)
	if !ok {
		return fmt.Errorf("cloud: index %q not found", index)
	}
	var s struct {
		NumberOfReplicas int    `json:"number_of_replicas"`
		RefreshInterval  string `json:"refresh_interval"`
	}
	_ = jsonx.Unmarshal(body, &s)
	if s.RefreshInterval != "" {
		im.Settings.RefreshInterval = s.RefreshInterval
	}
	if s.NumberOfReplicas >= 0 {
		im.Settings.NumberOfReplicas = s.NumberOfReplicas
	}
	indexID := indexIDFromName(index)
	return e.cluster.Apply(raft.CmdUpdateMapping, struct {
		IndexID types.IndexID
		Meta    *meta.IndexMeta
	}{indexID, im})
}

// GetSettings returns the stored settings JSON for an index.
func (e *CoordinatorEngine) GetSettings(ctx context.Context, index string) ([]byte, error) {
	im, ok := e.cluster.GetIndex(index)
	if !ok {
		return nil, fmt.Errorf("cloud: index %q not found", index)
	}
	out := map[string]any{
		index: map[string]any{
			"settings": map[string]any{
				"index": map[string]any{
					"number_of_shards":   im.Settings.NumberOfShards,
					"number_of_replicas": im.Settings.NumberOfReplicas,
					"refresh_interval":   im.Settings.RefreshInterval,
				},
			},
		},
	}
	return jsonx.Marshal(out)
}

// --- Engine interface: Aliases ---

func (e *CoordinatorEngine) PutAlias(ctx context.Context, index, alias string) error {
	im, ok := e.cluster.GetIndex(index)
	if !ok {
		return fmt.Errorf("cloud: index %q not found", index)
	}
	for _, a := range im.Aliases {
		if a == alias {
			return nil
		}
	}
	im.Aliases = append(im.Aliases, alias)
	indexID := indexIDFromName(index)
	return e.cluster.Apply(raft.CmdUpdateMapping, struct {
		IndexID types.IndexID
		Meta    *meta.IndexMeta
	}{indexID, im})
}

func (e *CoordinatorEngine) RemoveAlias(ctx context.Context, index, alias string) error {
	im, ok := e.cluster.GetIndex(index)
	if !ok {
		return fmt.Errorf("cloud: index %q not found", index)
	}
	filtered := im.Aliases[:0]
	for _, a := range im.Aliases {
		if a != alias {
			filtered = append(filtered, a)
		}
	}
	im.Aliases = filtered
	indexID := indexIDFromName(index)
	return e.cluster.Apply(raft.CmdUpdateMapping, struct {
		IndexID types.IndexID
		Meta    *meta.IndexMeta
	}{indexID, im})
}

func (e *CoordinatorEngine) GetAliases(ctx context.Context, index string) (map[string][]string, error) {
	im, ok := e.cluster.GetIndex(index)
	if !ok {
		return map[string][]string{}, nil
	}
	return map[string][]string{index: im.Aliases}, nil
}

// --- Engine interface: Misc ---

// Analyze splits text into tokens (simplified: whitespace split).
func (e *CoordinatorEngine) Analyze(_ context.Context, _, _, text string) ([]esapi.AnalyzeToken, error) {
	var tokens []esapi.AnalyzeToken
	start := 0
	for i, c := range text {
		if c == ' ' {
			if i > start {
				tokens = append(tokens, esapi.AnalyzeToken{
					Token:       text[start:i],
					StartOffset: start,
					EndOffset:   i,
					Type:        "word",
					Position:    len(tokens),
				})
			}
			start = i + 1
		}
	}
	if start < len(text) {
		tokens = append(tokens, esapi.AnalyzeToken{
			Token:       text[start:],
			StartOffset: start,
			EndOffset:   len(text),
			Type:        "word",
			Position:    len(tokens),
		})
	}
	return tokens, nil
}

// Refresh is a no-op in cloud mode (shard nodes handle refresh locally).
func (e *CoordinatorEngine) Refresh(_ context.Context, _ string) error { return nil }

// Indices returns the list of all indices from the Raft FSM.
func (e *CoordinatorEngine) Indices(_ context.Context, pattern string) ([]esapi.IndexInfo, error) {
	all := e.cluster.ListIndices()
	var out []esapi.IndexInfo
	for _, im := range all {
		if pattern != "" && pattern != "*" {
			// Simple wildcard match (prefix*).
			if len(pattern) > 0 && pattern[len(pattern)-1] == '*' {
				prefix := pattern[:len(pattern)-1]
				if len(im.Name) < len(prefix) || im.Name[:len(prefix)] != prefix {
					continue
				}
			} else if im.Name != pattern {
				continue
			}
		}
		out = append(out, esapi.IndexInfo{
			Health:    "green",
			Status:    "open",
			Index:     im.Name,
			Primaries: im.Settings.NumberOfShards,
			Replicas:  im.Settings.NumberOfReplicas,
		})
	}
	return out, nil
}

// Health returns the cluster health snapshot.
func (e *CoordinatorEngine) Health(_ context.Context) (*esapi.ClusterHealth, error) {
	members := e.cluster.GetMembers()
	indices := e.cluster.ListIndices()
	totalShards := 0
	for range indices {
		totalShards++
	}
	dataNodes := 0
	for _, m := range members {
		if m.Role == "shard" {
			dataNodes++
		}
	}
	status := "green"
	if dataNodes == 0 {
		status = "red"
	}
	return &esapi.ClusterHealth{
		ClusterName:         "unisearch",
		Status:              status,
		NumberOfNodes:       len(members),
		NumberOfDataNodes:   dataNodes,
		ActivePrimaryShards: totalShards,
		ActiveShards:        totalShards,
	}, nil
}

// CurrentSnapshot returns a minimal snapshot for PIT operations.
func (e *CoordinatorEngine) CurrentSnapshot(_ context.Context, _ []string) (types.Snapshot, error) {
	return types.NewSnapshot(0), nil
}

// Close shuts down all cached gRPC clients.
func (e *CoordinatorEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.shardClients {
		_ = c.Close()
	}
	e.shardClients = make(map[string]*coordinator.GRPCShardClient)
	return nil
}
