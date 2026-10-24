package coordinator

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/unisearch-db/unisearch/api/proto/gen"
)

// grpc_client.go implements the real gRPC ShardClient (design 05 §6.2).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.2

// GRPCShardClient connects to a shard node via gRPC.
type GRPCShardClient struct {
	conn   *grpc.ClientConn
	client pb.ShardServiceClient
	addr   string
}

// NewGRPCShardClient dials a shard node and returns a connected client.
func NewGRPCShardClient(addr string) (*GRPCShardClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("coordinator: dial %s: %w", addr, err)
	}
	return &GRPCShardClient{
		conn:   conn,
		client: pb.NewShardServiceClient(conn),
		addr:   addr,
	}, nil
}

// Search sends a search request to the shard node.
func (c *GRPCShardClient) Search(ctx context.Context, body []byte) (*ShardSearchResult, error) {
	resp, err := c.client.Search(ctx, &pb.SearchRequest{Body: body})
	if err != nil {
		return nil, fmt.Errorf("coordinator: grpc search: %w", err)
	}
	hits := make([]ShardHit, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		hits = append(hits, ShardHit{
			ID:     h.Id,
			Score:  h.Score,
			Source: h.Source,
		})
	}
	return &ShardSearchResult{
		Total: int(resp.Total),
		Hits:  hits,
	}, nil
}

// Get retrieves a single document from the shard node.
func (c *GRPCShardClient) Get(ctx context.Context, index, id string) ([]byte, bool, error) {
	resp, err := c.client.Get(ctx, &pb.GetRequest{Index: index, Id: id})
	if err != nil {
		return nil, false, fmt.Errorf("coordinator: grpc get: %w", err)
	}
	return resp.Source, resp.Found, nil
}

// Write sends a write batch to the shard node via gRPC.
func (c *GRPCShardClient) Write(ctx context.Context, req *pb.WriteBatch) (*pb.WriteAck, error) {
	return c.client.Write(ctx, req)
}

// SearchRaw sends a search request and returns the raw proto response
// (includes Aggregations bytes for full-fidelity result merging).
func (c *GRPCShardClient) SearchRaw(ctx context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	return c.client.Search(ctx, req)
}

// Close shuts down the gRPC connection.
func (c *GRPCShardClient) Close() error { return c.conn.Close() }

// Addr returns the shard node address.
func (c *GRPCShardClient) Addr() string { return c.addr }
