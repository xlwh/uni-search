// Package coordinator coordinator_test.go tests the MPP two-phase
// query execution: fan-out to multiple shards + result merge.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.6
package coordinator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockShardClient implements ShardClient for testing.
type mockShardClient struct {
	result *ShardSearchResult
	err    error
}

func (m *mockShardClient) Search(ctx context.Context, body []byte) (*ShardSearchResult, error) {
	return m.result, m.err
}

func (m *mockShardClient) Get(ctx context.Context, index, id string) ([]byte, bool, error) {
	return nil, false, nil
}

// TestQueryCoordinator_FanOutMerge verifies that the coordinator fans
// out a search to multiple shards and merges results by score.
func TestQueryCoordinator_FanOutMerge(t *testing.T) {
	c := NewQueryCoordinator()

	// Register two shard routes for "test-idx".
	c.UpdateRoutes("test-idx", []ShardRoute{
		{ShardID: 1, NodeID: "node1", NodeAddr: "addr1", Leader: true},
		{ShardID: 2, NodeID: "node2", NodeAddr: "addr2", Leader: true},
	})

	// Register mock clients for each shard.
	c.RegisterShardClient("addr1", &mockShardClient{
		result: &ShardSearchResult{
			Total: 2,
			Hits: []ShardHit{
				{ID: "doc1", Score: 0.9, Source: []byte(`{"t":"a"}`)},
				{ID: "doc2", Score: 0.5, Source: []byte(`{"t":"b"}`)},
			},
		},
	})
	c.RegisterShardClient("addr2", &mockShardClient{
		result: &ShardSearchResult{
			Total: 2,
			Hits: []ShardHit{
				{ID: "doc3", Score: 0.8, Source: []byte(`{"t":"c"}`)},
				{ID: "doc4", Score: 0.3, Source: []byte(`{"t":"d"}`)},
			},
		},
	})

	// Execute search.
	result, err := c.Search(context.Background(), []string{"test-idx"}, []byte(`{}`))
	require.NoError(t, err)

	// Verify merged total.
	assert.Equal(t, 4, result.Total, "merged total should be 4")

	// Verify hits are sorted by descending score.
	require.Len(t, result.Hits, 4, "should have 4 merged hits")
	assert.Equal(t, "doc1", result.Hits[0].ID, "highest score first")
	assert.Equal(t, float32(0.9), result.Hits[0].Score)
	assert.Equal(t, "doc3", result.Hits[1].ID, "second highest")
	assert.Equal(t, float32(0.8), result.Hits[1].Score)
	assert.Equal(t, "doc2", result.Hits[2].ID)
	assert.Equal(t, "doc4", result.Hits[3].ID, "lowest score last")
}

// TestQueryCoordinator_PartialFailure verifies that the coordinator
// continues to return results when one shard fails.
func TestQueryCoordinator_PartialFailure(t *testing.T) {
	c := NewQueryCoordinator()

	c.UpdateRoutes("test-idx", []ShardRoute{
		{ShardID: 1, NodeID: "node1", NodeAddr: "addr1", Leader: true},
		{ShardID: 2, NodeID: "node2", NodeAddr: "addr2", Leader: true},
	})

	// One shard returns results, the other fails.
	c.RegisterShardClient("addr1", &mockShardClient{
		result: &ShardSearchResult{
			Total: 1,
			Hits: []ShardHit{
				{ID: "doc1", Score: 1.0, Source: []byte(`{}`)},
			},
		},
	})
	c.RegisterShardClient("addr2", &mockShardClient{
		err: assert.AnError,
	})

	result, err := c.Search(context.Background(), []string{"test-idx"}, []byte(`{}`))
	require.NoError(t, err, "coordinator should not fail on partial shard failure")
	assert.Equal(t, 1, result.Total, "should return results from healthy shard only")
	assert.Len(t, result.Hits, 1)
}

// TestQueryCoordinator_OnlyQueriesLeaders verifies that follower
// shards are not queried.
func TestQueryCoordinator_OnlyQueriesLeaders(t *testing.T) {
	c := NewQueryCoordinator()

	c.UpdateRoutes("test-idx", []ShardRoute{
		{ShardID: 1, NodeID: "node1", NodeAddr: "addr1", Leader: true},
		{ShardID: 1, NodeID: "node2", NodeAddr: "addr2", Leader: false}, // follower
	})

	callCount := 0
	c.RegisterShardClient("addr1", &mockShardClient{
		result: &ShardSearchResult{Total: 1, Hits: []ShardHit{{ID: "d1", Score: 1}}},
	})
	c.RegisterShardClient("addr2", &mockShardClient{
		result: &ShardSearchResult{Total: 99, Hits: []ShardHit{{ID: "follower", Score: 99}}},
	})

	result, err := c.Search(context.Background(), []string{"test-idx"}, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 1, result.Total, "should only query leader, not follower")
	_ = callCount
}

// TestQueryCoordinator_NoRoutes verifies behavior when no routes exist.
func TestQueryCoordinator_NoRoutes(t *testing.T) {
	c := NewQueryCoordinator()
	result, err := c.Search(context.Background(), []string{"nonexistent"}, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 0, result.Total)
	assert.Empty(t, result.Hits)
}

// TestQueryCoordinator_MultiIndexMerge verifies fan-out across
// multiple indices.
func TestQueryCoordinator_MultiIndexMerge(t *testing.T) {
	c := NewQueryCoordinator()

	c.UpdateRoutes("idx-a", []ShardRoute{
		{ShardID: 1, NodeID: "n1", NodeAddr: "a1", Leader: true},
	})
	c.UpdateRoutes("idx-b", []ShardRoute{
		{ShardID: 2, NodeID: "n2", NodeAddr: "a2", Leader: true},
	})

	c.RegisterShardClient("a1", &mockShardClient{
		result: &ShardSearchResult{
			Total: 1,
			Hits: []ShardHit{{ID: "a1", Score: 0.5}},
		},
	})
	c.RegisterShardClient("a2", &mockShardClient{
		result: &ShardSearchResult{
			Total: 1,
			Hits: []ShardHit{{ID: "b1", Score: 0.7}},
		},
	})

	result, err := c.Search(context.Background(), []string{"idx-a", "idx-b"}, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 2, result.Total)
	require.Len(t, result.Hits, 2)
	// b1 has higher score, should come first.
	assert.Equal(t, "b1", result.Hits[0].ID)
	assert.Equal(t, "idx-b", result.Hits[0].Index)
	assert.Equal(t, "a1", result.Hits[1].ID)
	assert.Equal(t, "idx-a", result.Hits[1].Index)
}
