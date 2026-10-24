// Package grpc server_test.go tests the gRPC server handlers.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.4
package grpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	gen "github.com/unisearch-db/unisearch/api/proto/gen"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

func TestNewShardServer(t *testing.T) {
	srv := NewShardServer(esapi.EngineNoop{})
	assert.NotNil(t, srv)
}

func TestNewMetaServer(t *testing.T) {
	srv := NewMetaServer()
	assert.NotNil(t, srv)
}

func TestConvertWriteBatch(t *testing.T) {
	req := &gen.WriteBatch{
		Index: "test-idx",
		Ops: []*gen.WriteOp{
			{Kind: "index", Id: "1", Doc: []byte(`{"name":"test"}`)},
			{Kind: "delete", Id: "2"},
		},
		Refresh: true,
	}

	docs := ConvertWriteBatch(req)
	assert.Len(t, docs, 2)
	assert.Equal(t, "test-idx", docs[0].Index)
	assert.Equal(t, esapi.OpIndex, docs[0].Op)
	assert.Equal(t, "1", docs[0].ID)
	assert.Equal(t, esapi.OpDelete, docs[1].Op)
}

func TestConvertWriteAck(t *testing.T) {
	results := []esapi.WriteResult{
		{ID: "1", Result: "created", SeqNo: 1},
		{ID: "2", Result: "updated", SeqNo: 2},
	}

	ack := ConvertWriteAck(results)
	assert.True(t, ack.Success)
	assert.Len(t, ack.Results, 2)
	assert.Equal(t, "1", ack.Results[0].Id)
	assert.Equal(t, "created", ack.Results[0].Result)
}

func TestConvertSearchResult(t *testing.T) {
	score := float32(1.5)
	result := &esapi.SearchResult{
		Total:    10,
		Relation: "eq",
		MaxScore: &score,
		Hits: []esapi.SearchHit{
			{Index: "test", ID: "1", Score: 1.5, Source: []byte(`{"x":1}`)},
		},
	}

	resp := ConvertSearchResult(result)
	assert.Equal(t, int32(10), resp.Total)
	assert.Equal(t, "eq", resp.Relation)
	assert.Equal(t, float32(1.5), resp.MaxScore)
	assert.Len(t, resp.Hits, 1)
	assert.Equal(t, "test", resp.Hits[0].Index)
	assert.Equal(t, "1", resp.Hits[0].Id)
}

func TestMetaServerApply(t *testing.T) {
	srv := NewMetaServer()
	resp, err := srv.Apply(context.Background(), &gen.MetaCommand{
		CmdType: 1,
		Payload: []byte("{}"),
	})
	assert.NoError(t, err)
	assert.True(t, resp.Success)
}

func TestMetaServerHeartbeat(t *testing.T) {
	srv := NewMetaServer()
	resp, err := srv.Heartbeat(context.Background(), &gen.NodeInfo{
		Id:   "node-1",
		Addr: "localhost:9300",
		Role: "shard",
	})
	assert.NoError(t, err)
	assert.True(t, resp.Acknowledged)
}

func TestConvertRefresh(t *testing.T) {
	assert.Equal(t, "true", convertRefresh(true))
	assert.Equal(t, "", convertRefresh(false))
}
