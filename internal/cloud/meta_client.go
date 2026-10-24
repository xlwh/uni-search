package cloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MetaHTTPClient is used by shard nodes to communicate with meta nodes
// via HTTP: register, heartbeat, and fetch routing tables.
type MetaHTTPClient struct {
	metaAddrs  []string // e.g. ["meta-1:9200", "meta-2:9200"]
	nodeID     string
	httpClient *http.Client
	leaderIdx  int // index into metaAddrs of the current leader
}

// NewMetaHTTPClient creates a meta HTTP client.
func NewMetaHTTPClient(metaAddrs []string, nodeID string) *MetaHTTPClient {
	return &MetaHTTPClient{
		metaAddrs:  metaAddrs,
		nodeID:     nodeID,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		leaderIdx:  0,
	}
}

// post sends a POST to the current leader meta node, trying all meta
// addresses on failure.
func (c *MetaHTTPClient) post(path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("meta client: marshal: %w", err)
	}
	var lastErr error
	for i := 0; i < len(c.metaAddrs); i++ {
		idx := (c.leaderIdx + i) % len(c.metaAddrs)
		url := fmt.Sprintf("http://%s%s", c.metaAddrs[idx], path)
		resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(payload))
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("meta client: %s returned %d: %s", path, resp.StatusCode, string(data))
			continue
		}
		c.leaderIdx = idx
		return data, nil
	}
	return nil, fmt.Errorf("meta client: all meta nodes failed for %s: %w", path, lastErr)
}

// get sends a GET to the current leader meta node, trying all meta
// addresses on failure.
func (c *MetaHTTPClient) get(path string) ([]byte, error) {
	var lastErr error
	for i := 0; i < len(c.metaAddrs); i++ {
		idx := (c.leaderIdx + i) % len(c.metaAddrs)
		url := fmt.Sprintf("http://%s%s", c.metaAddrs[idx], path)
		resp, err := c.httpClient.Get(url)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("meta client: %s returned %d", path, resp.StatusCode)
			continue
		}
		c.leaderIdx = idx
		return data, nil
	}
	return nil, fmt.Errorf("meta client: all meta nodes failed for %s: %w", path, lastErr)
}

// Register registers this shard node with the meta cluster.
func (c *MetaHTTPClient) Register(nodeID, grpcAddr, role string) error {
	_, err := c.post("/_internal/register", map[string]any{
		"id":   nodeID,
		"addr": grpcAddr,
		"role": role,
	})
	return err
}

// Heartbeat sends a heartbeat to the meta cluster.
func (c *MetaHTTPClient) Heartbeat(nodeID string) error {
	_, err := c.post("/_internal/heartbeat", map[string]any{
		"id":        nodeID,
		"last_seen": time.Now().Unix(),
	})
	return err
}

// RouteTable represents the cluster routing table returned by the meta node.
type RouteTable struct {
	Members []RouteMember `json:"members"`
	Routes  []RouteEntry  `json:"routes"`
}

type RouteMember struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
	Role string `json:"role"`
}

type RouteEntry struct {
	ShardID   uint64   `json:"shard_id"`
	IndexID   uint32   `json:"index_id"`
	RaftGroup []string `json:"raft_group"`
}

// GetRoutes fetches the current routing table from the meta cluster.
func (c *MetaHTTPClient) GetRoutes() (*RouteTable, error) {
	data, err := c.get("/_internal/routes")
	if err != nil {
		return nil, err
	}
	var rt RouteTable
	if err := json.Unmarshal(data, &rt); err != nil {
		return nil, fmt.Errorf("meta client: parse routes: %w", err)
	}
	return &rt, nil
}

// StartHeartbeatLoop runs a background heartbeat loop until the context
// is cancelled. It sends a heartbeat every interval.
func (c *MetaHTTPClient) StartHeartbeatLoop(done <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			_ = c.Heartbeat(c.nodeID)
		}
	}
}
