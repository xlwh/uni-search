// Package esapi implements the Elasticsearch-compatible REST API
// surface for M0 (design 04-es-compat-layer §2.2/§6). Handlers are
// thin: they decode JSON, dispatch to the Engine interface, and
// assemble ES-shaped responses. All errors flow through APIError so
// the wire body is ES-compatible (00 §6.1).
package esapi

import (
	"context"
	"errors"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// DocOp is the action code for one _bulk line ("index"/"create"/"update"/"delete").
type DocOp string

const (
	OpIndex  DocOp = "index"
	OpCreate DocOp = "create"
	OpUpdate DocOp = "update"
	OpDelete DocOp = "delete"
)

// IndexDoc is one document in a write batch.
type IndexDoc struct {
	Op      DocOp
	Index   string
	ID      string
	Source  []byte
	IfSeqNo *int64 // when set, optimistic locking (409 on mismatch)
}

// WriteResult is the per-document outcome of Index/Bulk.
type WriteResult struct {
	Index   string
	ID      string
	Op      DocOp
	SeqNo   int64
	Version int64
	Result  string // "created" / "updated" / "deleted" / "not_found" / "conflict"
	Err     error
}

// SearchHit is one row in the _search response.
type SearchHit struct {
	Index      string
	ID         string
	Score      float32
	Source     []byte
	SeqNo      int64
	Primary    int64
	SortValues []any
}

// SearchResult is the engine-side search outcome; the handler
// reshapes it into the ES 8.x response skeleton (§6.2).
type SearchResult struct {
	Took      int
	TimedOut  bool
	Shards    ShardStats
	Total     int
	Relation  string // "eq" | "gte"
	MaxScore  *float32
	Hits      []SearchHit
}

// ShardStats is the _shards block of the search response.
type ShardStats struct {
	Total     int
	Successful int
	Skipped   int
	Failed    int
}

// IndexInfo is the row returned by _cat/indices.
type IndexInfo struct {
	Health      string
	Status      string
	Index       string
	UUID        string
	Primaries   int
	Replicas    int
	DocsCount   int64
	DocsDeleted int64
	StoreSize   string
}

// ClusterHealth is the _cluster/health body.
type ClusterHealth struct {
	ClusterName        string `json:"cluster_name"`
	Status             string `json:"status"`
	TimedOut          bool   `json:"timed_out"`
	NumberOfNodes      int    `json:"number_of_nodes"`
	NumberOfDataNodes  int    `json:"number_of_data_nodes"`
	ActivePrimaryShards int  `json:"active_primary_shards"`
	ActiveShards       int   `json:"active_shards"`
	RelocatingShards   int   `json:"relocating_shards"`
	InitializingShards int   `json:"initializing_shards"`
	UnassignedShards   int   `json:"unassigned_shards"`
}

// AnalyzeToken is the ES _analyze token structure.
type AnalyzeToken struct {
	Token       string `json:"token"`
	StartOffset int    `json:"start_offset"`
	EndOffset   int    `json:"end_offset"`
	Type        string `json:"type"`
	Position    int    `json:"position"`
}

// Engine is the storage/index/query surface the handlers call. The
// embedded wiring (cmd/unisearch) constructs a concrete engine and
// the unit tests pass a mock implementation.
type Engine interface {
	// Write dispatches a single index/create/update/delete. The
	// returned WriteResult carries the new seq_no or a 409-style
	// error if IfSeqNo conflicts with the current head.
	Write(ctx context.Context, doc IndexDoc, refresh string) (*WriteResult, error)

	// Bulk dispatches a batch. Each input position has a parallel
	// output position. The handler aggregates per-item errors.
	Bulk(ctx context.Context, docs []IndexDoc, refresh string) ([]WriteResult, error)

	// Get returns the _source for one _id. notFound=true signals a 404.
	Get(ctx context.Context, index, id string) (source []byte, seqNo int64, primary int64, notFound bool, err error)

	// MGet resolves multiple (index, id) pairs in one call.
	MGet(ctx context.Context, reqs []GetRequest) ([]GetResult, error)

	// Delete removes a document by _id.
	Delete(ctx context.Context, index, id string, ifSeqNo *int64, refresh string) (*WriteResult, error)

	// Search runs one DSL-parsed query against one or more indices.
	// body is the raw ES search request JSON; the engine parses it
	// via dsl.ParseSearchBody internally.
	Search(ctx context.Context, indices []string, body []byte) (*SearchResult, error)

	// Count returns the document count matching the query (or all
	// docs when the query is empty).
	Count(ctx context.Context, indices []string, body []byte) (int64, error)

	// PutMapping applies explicit field definitions to an index.
	PutMapping(ctx context.Context, index string, body []byte) error
	// GetMapping returns the stored mapping JSON for an index.
	GetMapping(ctx context.Context, index string) ([]byte, error)

	// PutSettings applies a settings body subset (M0 ignores most).
	PutSettings(ctx context.Context, index string, body []byte) error
	GetSettings(ctx context.Context, index string) ([]byte, error)

	// PutAlias / GetAliases manage the alias table.
	PutAlias(ctx context.Context, index, alias string) error
	GetAliases(ctx context.Context, index string) (map[string][]string, error)

	// Analyze runs the analyzer for one (index?, field?, text).
	Analyze(ctx context.Context, index, analyzer, text string) ([]AnalyzeToken, error)

	// Refresh is a no-op in M0 (MemTable is visible without flush)
	// but still drains writes for refresh=true semantics.
	Refresh(ctx context.Context, index string) error

	// Indices returns the per-index info for _cat/indices.
	Indices(ctx context.Context, pattern string) ([]IndexInfo, error)

	// Health returns the cluster health snapshot.
	Health(ctx context.Context) (*ClusterHealth, error)
}

// GetRequest is one entry of an _mget batch.
type GetRequest struct {
	Index string
	ID    string
}

// GetResult is the per-item outcome of MGet.
type GetResult struct {
	Index    string
	ID       string
	Found    bool
	Source   []byte
	SeqNo    int64
	Primary  int64
}

// EngineNoop is a sentinel Engine that returns "not implemented"
// for every call. Used by the unsupported.go handler to surface
// unfinished endpoints with a clear error.
type EngineNoop struct{}

var _ Engine = EngineNoop{}

func (EngineNoop) Write(context.Context, IndexDoc, string) (*WriteResult, error) {
	return nil, errNotImplemented
}
func (EngineNoop) Bulk(context.Context, []IndexDoc, string) ([]WriteResult, error) {
	return nil, errNotImplemented
}
func (EngineNoop) Get(context.Context, string, string) ([]byte, int64, int64, bool, error) {
	return nil, 0, 0, false, errNotImplemented
}
func (EngineNoop) MGet(context.Context, []GetRequest) ([]GetResult, error) {
	return nil, errNotImplemented
}
func (EngineNoop) Delete(context.Context, string, string, *int64, string) (*WriteResult, error) {
	return nil, errNotImplemented
}
func (EngineNoop) Search(context.Context, []string, []byte) (*SearchResult, error) {
	return nil, errNotImplemented
}
func (EngineNoop) Count(context.Context, []string, []byte) (int64, error) {
	return 0, errNotImplemented
}
func (EngineNoop) PutMapping(context.Context, string, []byte) error  { return errNotImplemented }
func (EngineNoop) GetMapping(context.Context, string) ([]byte, error) { return nil, errNotImplemented }
func (EngineNoop) PutSettings(context.Context, string, []byte) error { return errNotImplemented }
func (EngineNoop) GetSettings(context.Context, string) ([]byte, error) {
	return nil, errNotImplemented
}
func (EngineNoop) PutAlias(context.Context, string, string) error { return errNotImplemented }
func (EngineNoop) GetAliases(context.Context, string) (map[string][]string, error) {
	return nil, errNotImplemented
}
func (EngineNoop) Analyze(context.Context, string, string, string) ([]AnalyzeToken, error) {
	return nil, errNotImplemented
}
func (EngineNoop) Refresh(context.Context, string) error { return errNotImplemented }
func (EngineNoop) Indices(context.Context, string) ([]IndexInfo, error) {
	return nil, errNotImplemented
}
func (EngineNoop) Health(context.Context) (*ClusterHealth, error) { return nil, errNotImplemented }

// versionInfo is the static version block returned by GET /.
var versionInfo = map[string]any{
	"name":          "node-1",
	"cluster_name":  "unisearch",
	"cluster_uuid":  "unisearch-uuid",
	"version": map[string]any{
		"number":         "8.12.0",
		"build_flavor":   "default",
		"distribution":   "unisearch",
		"lucene_version":  "none",
	},
	"tagline": "You Know, for Search",
}

// sentinel types used by the no-op engine.
var (
	errNotImplemented = errors.New("esapi: endpoint not implemented")
	_                 = types.SegmentID(0)
)
