// Package coordinator implements the M2 Coordinator layer (design
// 05-distributed.md §6). The Coordinator is stateless: it routes
// queries to shard nodes (D3: direct connection), merges results,
// and manages ingest batching/backpressure.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.4
package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// QueryCoordinator routes search queries to shard nodes and merges
// results (D3: direct connection, no intermediate layer).
type QueryCoordinator struct {
	mu        sync.RWMutex
	routes    map[string][]ShardRoute  // index name → shard routes
	shardClients map[string]ShardClient  // node addr → client
}

// ShardRoute describes one shard's routing info.
type ShardRoute struct {
	ShardID  uint64
	NodeID   string
	NodeAddr string
	Leader   bool
}

// ShardClient is the interface for talking to a shard node.
// In production this is a gRPC client; for testing it's a mock.
type ShardClient interface {
	Search(ctx context.Context, body []byte) (*ShardSearchResult, error)
	Get(ctx context.Context, index, id string) ([]byte, bool, error)
}

// ShardSearchResult is the result from one shard.
type ShardSearchResult struct {
	Total  int
	Hits   []ShardHit
}

// ShardHit is one hit from a shard.
type ShardHit struct {
	ID     string
	Score  float32
	Source []byte
}

// NewQueryCoordinator creates a coordinator.
func NewQueryCoordinator() *QueryCoordinator {
	return &QueryCoordinator{
		routes:       make(map[string][]ShardRoute),
		shardClients: make(map[string]ShardClient),
	}
}

// UpdateRoutes replaces the routing table (called from meta Watch).
func (c *QueryCoordinator) UpdateRoutes(index string, routes []ShardRoute) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.routes[index] = routes
}

// RegisterShardClient registers a client for a shard node.
func (c *QueryCoordinator) RegisterShardClient(addr string, client ShardClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shardClients[addr] = client
}

// Search fans out a search to all shards for the given indices and
// merges results by descending score.
func (c *QueryCoordinator) Search(ctx context.Context, indices []string, body []byte) (*MergedSearchResult, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	type shardResult struct {
		index  string
		result *ShardSearchResult
		err    error
	}

	var allHits []MergedHit
	total := 0

	for _, index := range indices {
		routes, ok := c.routes[index]
		if !ok {
			continue
		}
		for _, route := range routes {
			if !route.Leader {
				continue // only query leaders
			}
			client, ok := c.shardClients[route.NodeAddr]
			if !ok {
				continue
			}
			result, err := client.Search(ctx, body)
			if err != nil || result == nil {
				continue
			}
			total += result.Total
			for _, h := range result.Hits {
				allHits = append(allHits, MergedHit{
					Index:  index,
					ID:     h.ID,
					Score:  h.Score,
					Source: h.Source,
				})
			}
		}
	}

	// Sort by descending score.
	sort.Slice(allHits, func(i, j int) bool {
		return allHits[i].Score > allHits[j].Score
	})

	return &MergedSearchResult{
		Total: total,
		Hits:  allHits,
	}, nil
}

// MergedSearchResult is the coordinator's merged result.
type MergedSearchResult struct {
	Total int
	Hits   []MergedHit
}

// MergedHit is a hit with index attribution.
type MergedHit struct {
	Index  string
	ID     string
	Score  float32
	Source []byte
}

// IngestCoordinator manages batch ingestion with per-shard攒批 and
// backpressure (design 05 §6.1).
type IngestCoordinator struct {
	mu        sync.Mutex
	queues    map[uint64]*shardQueue  // shard ID → batch queue
	maxBytes  int64                    // max queue size per shard (64MB)
	maxWait   time.Duration           // max batch wait (2ms)
}

type shardQueue struct {
	mu     sync.Mutex
	buf    []byte
	count  int
	flushAt time.Time
}

// NewIngestCoordinator creates an ingest coordinator.
func NewIngestCoordinator(maxBytes int64, maxWait time.Duration) *IngestCoordinator {
	if maxBytes <= 0 {
		maxBytes = 64 << 20 // 64MB default
	}
	if maxWait <= 0 {
		maxWait = 2 * time.Millisecond
	}
	return &IngestCoordinator{
		queues:   make(map[uint64]*shardQueue),
		maxBytes: maxBytes,
		maxWait:  maxWait,
	}
}

// Route determines the target shard for a document (hash routing).
func (c *IngestCoordinator) Route(index string, numShards int, id string) uint64 {
	if numShards <= 0 {
		return 0
	}
	// Simple hash routing (same as ES: hash(routing or _id) % shards)
	h := uint64(0)
	for _, b := range []byte(id) {
		h = h*31 + uint64(b)
	}
	return h % uint64(numShards)
}

// Enqueue adds a document to the batch queue for a shard.
// Returns the batch if it's ready to flush (size or time threshold).
// Returns (nil, false, true) when the queue is full (backpressure: caller
// should return 429 to upstream). Design 05 §6.1: "背压 per-shard 队列水位
// （上限 64MB）→ 信用流控 → 上游 429".
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.9
func (c *IngestCoordinator) Enqueue(shardID uint64, data []byte) ([]byte, bool, bool) {
	c.mu.Lock()
	q, ok := c.queues[shardID]
	if !ok {
		q = &shardQueue{flushAt: time.Now().Add(c.maxWait)}
		c.queues[shardID] = q
	}
	c.mu.Unlock()

	q.mu.Lock()
	defer q.mu.Unlock()

	// Backpressure: reject if queue would exceed maxBytes (design 05 §6.1).
	newSize := int64(len(q.buf) + len(data) + 1)
	if newSize > c.maxBytes {
		return nil, false, true // rejected — caller returns 429
	}

	q.buf = append(q.buf, data...)
	q.buf = append(q.buf, '\n')
	q.count++

	// Flush if size threshold exceeded or time threshold reached.
	shouldFlush := int64(len(q.buf)) >= c.maxBytes || time.Now().After(q.flushAt)
	if shouldFlush {
		batch := q.buf
		q.buf = nil
		q.count = 0
		q.flushAt = time.Now().Add(c.maxWait)
		return batch, true, false
	}
	return nil, false, false
}

// IsRejected checks the rejected return value from Enqueue.
func (c *IngestCoordinator) IsRejected(rejected bool) bool { return rejected }

// FlushAll returns all pending batches for all shards.
func (c *IngestCoordinator) FlushAll() map[uint64][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[uint64][]byte)
	for shardID, q := range c.queues {
		q.mu.Lock()
		if len(q.buf) > 0 {
			result[shardID] = q.buf
			q.buf = nil
			q.count = 0
		}
		q.mu.Unlock()
	}
	return result
}

// MockShardClient is a test ShardClient.
type MockShardClient struct {
	SearchFn func(ctx context.Context, body []byte) (*ShardSearchResult, error)
	GetFn    func(ctx context.Context, index, id string) ([]byte, bool, error)
}

func (m *MockShardClient) Search(ctx context.Context, body []byte) (*ShardSearchResult, error) {
	if m.SearchFn != nil {
		return m.SearchFn(ctx, body)
	}
	return &ShardSearchResult{}, nil
}

func (m *MockShardClient) Get(ctx context.Context, index, id string) ([]byte, bool, error) {
	if m.GetFn != nil {
		return m.GetFn(ctx, index, id)
	}
	return nil, false, nil
}

// Helper: parse search body to extract query for routing.
func parseSearchBody(body []byte) (string, int, int, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return "", 0, 10, fmt.Errorf("coordinator: parse search body: %w", err)
	}
	from, size := 0, 10
	if v, ok := top["from"]; ok {
		json.Unmarshal(v, &from)
	}
	if v, ok := top["size"]; ok {
		json.Unmarshal(v, &size)
	}
	return "", from, size, nil
}
