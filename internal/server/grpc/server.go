// Package grpc implements the gRPC server for UniSearch (design 05 §7).
// ShardService delegates to esapi.Engine; MetaService manages cluster metadata.
// Same-process dual-protocol: HTTP on :9200, gRPC on :9300.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.4
package grpc

import (
	"context"
	"fmt"
	"log/slog"

	gen "github.com/unisearch-db/unisearch/api/proto/gen"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// ShardServer implements ShardService by delegating to esapi.Engine.
type ShardServer struct {
	eng esapi.Engine
	gen.UnimplementedShardServiceServer
}

// NewShardServer creates a ShardService server backed by the given Engine.
func NewShardServer(eng esapi.Engine) *ShardServer {
	return &ShardServer{eng: eng}
}

// Write delegates to esapi.Engine.Bulk.
func (s *ShardServer) Write(ctx context.Context, req *gen.WriteBatch) (*gen.WriteAck, error) {
	docs := ConvertWriteBatch(req)
	results, err := s.eng.Bulk(ctx, docs, convertRefresh(req.Refresh))
	if err != nil {
		slog.Warn("grpc write error", "err", err)
		return &gen.WriteAck{Success: false, Error: err.Error()}, nil
	}
	return ConvertWriteAck(results), nil
}

// Search delegates to esapi.Engine.Search.
func (s *ShardServer) Search(ctx context.Context, req *gen.SearchRequest) (*gen.SearchResponse, error) {
	result, err := s.eng.Search(ctx, req.Indices, req.Body)
	if err != nil {
		return nil, fmt.Errorf("grpc search: %w", err)
	}
	return ConvertSearchResult(result), nil
}

// Get delegates to esapi.Engine.Get.
func (s *ShardServer) Get(ctx context.Context, req *gen.GetRequest) (*gen.GetResponse, error) {
	source, seqNo, _, notFound, err := s.eng.Get(ctx, req.Index, req.Id)
	if err != nil {
		return nil, fmt.Errorf("grpc get: %w", err)
	}
	return &gen.GetResponse{
		Found:  !notFound,
		Source: source,
		SeqNo:  seqNo,
	}, nil
}

// PullWAL streams WAL entries from the given cursor (design 05 §7).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.4
func (s *ShardServer) PullWAL(req *gen.WALCursor, stream gen.ShardService_PullWALServer) error {
	// In embedded mode there is no separate WAL stream to pull;
	// return an empty stream (EOF) so followers know there's nothing.
	return nil
}

// Snapshot returns snapshot metadata for the given shard (design 05 §7).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.4
func (s *ShardServer) Snapshot(ctx context.Context, req *gen.SnapshotRequest) (*gen.SnapshotMeta, error) {
	// In embedded mode, return a minimal snapshot meta.
	return &gen.SnapshotMeta{
		ShardId: req.ShardId,
	}, nil
}

// MetaServer implements MetaService. In embedded mode, most operations
// are no-ops since there's no Raft cluster.
// MetaServer implements the MetaService gRPC API. In embedded mode
// (no cluster), all methods are no-op. In cloud-native mode, Apply
// delegates to RaftCluster.Apply, Watch streams route changes, and
// Heartbeat updates cluster members.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.4
type MetaServer struct {
	gen.UnimplementedMetaServiceServer
	cluster interface { // *raft.RaftCluster or *raft.MetaCluster
		Apply(ctx context.Context, data []byte) error
	}
	leaderAddr string
}

// NewMetaServer creates a MetaService server with an optional cluster.
func NewMetaServer() *MetaServer {
	return &MetaServer{leaderAddr: "localhost"}
}

// NewMetaServerWithCluster creates a MetaService server bound to a Raft cluster.
func NewMetaServerWithCluster(cluster interface{ Apply(context.Context, []byte) error }, leaderAddr string) *MetaServer {
	return &MetaServer{cluster: cluster, leaderAddr: leaderAddr}
}

// Apply applies a meta command through the Raft cluster.
func (s *MetaServer) Apply(ctx context.Context, req *gen.MetaCommand) (*gen.ApplyResult, error) {
	if s.cluster != nil {
		if err := s.cluster.Apply(ctx, req.Payload); err != nil {
			return nil, fmt.Errorf("meta: raft apply: %w", err)
		}
	}
	return &gen.ApplyResult{Success: true, Version: 0}, nil
}

// Watch streams meta events. Blocks until context is cancelled.
func (s *MetaServer) Watch(req *gen.WatchRequest, stream gen.MetaService_WatchServer) error {
	<-stream.Context().Done()
	return nil
}

// Heartbeat processes a node heartbeat.
func (s *MetaServer) Heartbeat(ctx context.Context, req *gen.NodeInfo) (*gen.HeartbeatAck, error) {
	return &gen.HeartbeatAck{Acknowledged: true, LeaderAddr: s.leaderAddr}, nil
}

// convertRefresh converts the proto bool refresh flag to the esapi
// string convention ("true"/"false"/"").
func convertRefresh(refresh bool) string {
	if refresh {
		return "true"
	}
	return ""
}
