# Spec: 补齐设计文档 vs 代码审计的 9 个功能缺口

## 1. 概述 (Overview)

本任务补齐设计文档 vs 代码审计中发现的 9 个未实现功能缺口，涵盖可观测性（Prometheus + OTel）、分布式核心（gRPC + K8s Operator）、数据迁移（Online split / Cross-Link merge / 冷热分层）、工程基础设施（CI/CD）和性能优化（goccy/json）。

### 变更清单

| 类型 | 文件路径 | 说明 |
|------|----------|------|
| 新增 | `internal/observability/metrics.go` | Prometheus 五组指标定义 + Register |
| 新增 | `internal/observability/tracing.go` | OTel SDK 初始化 + OTLP gRPC exporter + span 工具函数 |
| 新增 | `internal/observability/middleware.go` | HTTP withMetrics + withTracing middleware |
| 新增 | `internal/jsonx/jsonx.go` | goccy/go-json 统一别名包 (Marshal/Unmarshal/NewEncoder/NewDecoder) |
| 新增 | `api/proto/gen/doc.go` | go:generate 指令触发 protoc 生成 |
| 新增 | `internal/server/grpc/server.go` | gRPC server: ShardService + MetaService 实现 |
| 新增 | `internal/server/grpc/converter.go` | proto message ↔ esapi 类型转换层 |
| 新增 | `internal/server/grpc/interceptor.go` | gRPC interceptor: tracing + metrics |
| 新增 | `deploy/k8s/operator/controller.go` | Reconciler: StatefulSet/Service/ConfigMap 管理 |
| 新增 | `deploy/k8s/operator/main.go` | controller-runtime manager 启动入口 |
| 新增 | `deploy/k8s/operator/scheme.go` | SchemeBuilder + CRD 注册 + DeepCopy |
| 新增 | `scripts/ci.sh` | CI 统一入口 (lint+test+build+coverage) |
| 新增 | `scripts/build.sh` | 交叉编译脚本 (linux/amd64, linux/arm64, darwin/arm64) |
| 新增 | `.golangci.yml` | golangci-lint 配置 |
| 新增 | `internal/raft/split_test.go` | Online split 端到端测试 |
| 新增 | `internal/storage/crosslink_merge_test.go` | Cross-Link 合并 + 召回率测试 |
| 新增 | `internal/storage/tiering/tier_test.go` | 冷热分层迁移测试 |
| 新增 | `internal/observability/metrics_test.go` | 指标注册 + 采集测试 |
| 新增 | `internal/observability/tracing_test.go` | span 创建 + 采样测试 |
| 新增 | `internal/server/grpc/server_test.go` | gRPC handler 单元测试 |
| 修改 | `go.mod` | 新增 prometheus/client_golang, otel, goccy/go-json, controller-runtime 直接依赖 |
| 修改 | `Makefile` | 新增 ci/coverage/cross-build/fuzz/proto target |
| 修改 | `internal/server/esapi/router.go` | Server 新增 observability 字段, middleware 链扩展, /_prometheus/metrics 路由 |
| 修改 | `internal/server/esapi/handlers.go` | encoding/json → jsonx 替换 |
| 修改 | `internal/embedded/engine.go` | json 替换 + tracing span 注入 |
| 修改 | `internal/embedded/docparse.go` | json 替换 |
| 修改 | `internal/engine/dsl/parser.go` | json 替换 |
| 修改 | `internal/storage/flush.go` | flush 指标埋点 |
| 修改 | `internal/index/inverted/wand.go` | WAND pruned ratio 指标 |
| 修改 | `internal/tenant/tenant.go` | quota usage 指标 |
| 修改 | `cmd/unisearch/main.go` | OTel 初始化 + gRPC server 启动 |
| 修改 | `internal/raft/split.go` | 补齐 AdvancePhase 真实逻辑 |
| 修改 | `internal/storage/crosslink_merge.go` | 补齐 ExecuteMerge 真实合并逻辑 |
| 修改 | `internal/storage/tiering/tier_manager.go` | 补齐 ExecuteMigration 真实迁移逻辑 |
| 修改 | `deploy/k8s/operator/types.go` | 补充 DeepCopy 方法 |

---

## 2. 详细设计 (Detailed Design)

### 2.1 G1: Prometheus Metrics

**变更内容**：
新建 `internal/observability/metrics.go`，定义设计 06 §1 的五组指标。在 HTTP middleware、storage flush、index search、tenant quota 路径埋点。

**代码示例**：
```go
// internal/observability/metrics.go
package observability

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "net/http"
)

var (
    // 写入组
    WriteDocsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "unisearch_write_docs_total",
        Help: "Total documents written",
    }, []string{"tenant", "index", "shard"})

    WriteBatchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "unisearch_write_batch_duration_seconds",
        Help:    "Write batch duration in seconds",
        Buckets: prometheus.DefBuckets,
    }, []string{"tenant", "index"})

    WriteRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "unisearch_write_rejected_total",
        Help: "Write requests rejected by backpressure",
    }, []string{"tenant", "index"})

    // 查询组
    QueryRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "unisearch_query_requests_total",
        Help: "Total query requests",
    }, []string{"tenant", "index"})

    QueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "unisearch_query_duration_seconds",
        Help:    "Query duration in seconds",
        Buckets: prometheus.DefBuckets,
    }, []string{"tenant", "index", "phase"})

    QueryRowsScanned = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "unisearch_query_rows_scanned_total",
        Help:    "Rows scanned per query",
        Buckets: prometheus.ExponentialBuckets(10, 10, 6),
    }, []string{"tenant", "index"})

    QueryErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "unisearch_query_errors_total",
        Help: "Query errors total",
    }, []string{"tenant", "index"})

    // 段组
    SegmentsOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "unisearch_segments_open",
        Help: "Open segments count",
    }, []string{"index", "shard", "tier"})

    FlushDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "unisearch_flush_duration_seconds",
        Help:    "Flush duration in seconds",
        Buckets: prometheus.DefBuckets,
    }, []string{"index", "shard"})

    MergeRunning = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "unisearch_merge_running",
        Help: "Running merge count",
    }, []string{"index", "shard"})

    MergeBytesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "unisearch_merge_bytes_total",
        Help: "Total bytes merged",
    }, []string{"index", "shard"})

    DeletesPending = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "unisearch_deletes_pending",
        Help: "Pending delete bitmap cardinality",
    }, []string{"index", "shard"})

    // 索引组
    HNSWSearchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "unisearch_hnsw_search_duration_seconds",
        Help:    "HNSW search duration",
        Buckets: prometheus.DefBuckets,
    }, []string{"field"})

    HNSWNodesVisited = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "unisearch_hnsw_nodes_visited",
        Help:    "HNSW nodes visited per query",
        Buckets: prometheus.ExponentialBuckets(10, 2, 10),
    }, []string{"field"})

    BM25WANDPrunedRatio = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "unisearch_bm25_wand_pruned_ratio",
        Help:    "WAND pruning ratio",
        Buckets: prometheus.LinearBuckets(0, 0.1, 11),
    }, []string{"field"})

    // 租户配额组
    TenantQuotaUsageRatio = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "unisearch_tenant_quota_usage_ratio",
        Help: "Tenant quota usage ratio (0-1)",
    }, []string{"tenant", "dim"})

    TenantThrottledTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "unisearch_tenant_throttled_total",
        Help: "Tenant throttled events total",
    }, []string{"tenant", "dim"})
)

// MetricsHandler returns the Prometheus exposition HTTP handler.
func MetricsHandler() http.Handler {
    return promhttp.Handler()
}
```

**middleware 集成** (修改 `internal/server/esapi/router.go`):
```go
// chain 扩展为: withRecovery → withMetrics → withTracing → withRequestLog
mw := chain(withRecovery, withMetrics, withTracing, withRequestLog)

// /_prometheus/metrics 路由（不经过 mw，直接暴露）
s.mux.Handle("GET /_prometheus/metrics", observability.MetricsHandler())
```

**withMetrics middleware** (新增 `internal/observability/middleware.go`):
```go
func WithMetrics(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        ww := &statusWriter{ResponseWriter: w, status: 200}
        next.ServeHTTP(ww, r)
        duration := time.Since(start).Seconds()
        // 按 path 提取 index 标签
        index := extractIndex(r.URL.Path)
        QueryRequestsTotal.WithLabelValues("default", index).Inc()
        QueryDuration.WithLabelValues("default", index, "http").Observe(duration)
        if ww.status >= 400 {
            QueryErrorsTotal.WithLabelValues("default", index).Inc()
        }
    })
}
```

### 2.2 G2: OTel 分布式追踪

**变更内容**：
新建 `internal/observability/tracing.go`，初始化 OTel SDK + OTLP gRPC exporter。四级 span 树注入。

**代码示例**：
```go
// internal/observability/tracing.go
package observability

import (
    "context"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/sdk/resource"
    "go.opentelemetry.io/otel/sdk/trace"
    semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
    "time"
)

// InitTracing initializes OTel SDK with OTLP gRPC exporter.
// endpoint: e.g. "localhost:4317"
func InitTracing(ctx context.Context, endpoint string) (func(), error) {
    exporter, err := otlptracegrpc.New(ctx,
        otlptracegrpc.WithEndpoint(endpoint),
        otlptracegrpc.WithInsecure(),
    )
    if err != nil {
        return nil, err
    }
    res, _ := resource.New(ctx, resource.WithAttributes(
        semconv.ServiceName("unisearch"),
    ))
    tp := trace.NewTracerProvider(
        trace.WithBatcher(exporter,
            trace.WithBatchTimeout(5*time.Second),
            trace.WithMaxQueueSize(2048),
        ),
        trace.WithResource(res),
        // 采样: parent-based + 错误全采 + 慢查询 >1s 全采 + 其余 1%
        trace.WithSampler(trace.ParentBased(
            trace.NewAlwaysOnSampler(), // 简化: 默认全采, 生产环境配 tail sampler
        )),
    )
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.TraceContext{})
    return func() { tp.Shutdown(ctx) }, nil
}

// Span helpers — 每级 span 对应设计 06 §2
func StartHTTPSpan(ctx context.Context, method, path string) (context.Context, trace.Span) {
    return otel.Tracer("unisearch").Start(ctx, "http.request",
        trace.WithAttributes(semconv.HTTPMethod(method), semconv.HTTPTarget(path)),
    )
}

func StartPlanSpan(ctx context.Context) (context.Context, trace.Span) {
    return otel.Tracer("unisearch").Start(ctx, "query.plan")
}

func StartShardSpan(ctx context.Context, shardID string) (context.Context, trace.Span) {
    return otel.Tracer("unisearch").Start(ctx, "shard.search",
        trace.WithAttributes(attribute.String("shard_id", shardID)),
    )
}

func StartSegmentSpan(ctx context.Context, op string) (context.Context, trace.Span) {
    return otel.Tracer("unisearch").Start(ctx, op) // "segment.scan" / "index.hnsw" / "index.bm25" / "agg.partial"
}
```

**withTracing middleware**:
```go
func WithTracing(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx, span := StartHTTPSpan(r.Context(), r.Method, r.URL.Path)
        defer span.End()
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}
```

**引擎层 span 注入** (修改 `internal/embedded/engine.go` Search 方法):
```go
func (e *Engine) Search(ctx context.Context, indices []string, body []byte) (*esapi.SearchResult, error) {
    ctx, span := observability.StartPlanSpan(ctx)
    defer span.End()
    // ... existing plan + execute logic ...
}
```

### 2.3 G3: CI/CD Pipeline

**变更内容**：
平台无关的 Makefile target + scripts/ 脚本。

**Makefile 扩展**:
```makefile
.PHONY: ci coverage cross-build fuzz proto lint

ci: lint test build
	@echo "CI: lint + test + build passed"

coverage:
	CGO_ENABLED=0 $(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

cross-build:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(GO) build -o dist/unisearch-linux-amd64   ./cmd/unisearch
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 $(GO) build -o dist/unisearch-linux-arm64   ./cmd/unisearch
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 $(GO) build -o dist/unisearch-darwin-arm64  ./cmd/unisearch

fuzz:
	CGO_ENABLED=0 $(GO) test -fuzz=FuzzDSLParser -fuzztime=60s ./internal/engine/dsl/

proto:
	cd api/proto && protoc --go_out=gen --go-grpc_out=gen shard.proto meta.proto
```

**.golangci.yml**:
```yaml
linters:
  enable:
    - errcheck
    - govet
    - staticcheck
    - revive
    - gocritic
linters-settings:
  errcheck:
    check-type-assertions: true
issues:
  exclude-rules:
    - path: _test\.go
      linters: [errcheck]
```

### 2.4 G4: gRPC 服务实现

**变更内容**：
从 proto 生成 Go 代码，实现 ShardService + MetaService，同进程双协议启动。

**Proto 代码生成** (`api/proto/gen/doc.go`):
```go
// Package gen contains protoc-generated Go code for UniSearch gRPC.
package gen

//go:generate protoc --go_out=. --go-grpc_out=. ../shard.proto ../meta.proto
```

**gRPC server** (`internal/server/grpc/server.go`):
```go
package grpc

import (
    "context"
    "github.com/unisearch-db/unisearch/api/proto/gen"
    "github.com/unisearch-db/unisearch/internal/server/esapi"
)

// ShardServer implements ShardService by delegating to esapi.Engine.
type ShardServer struct {
    eng esapi.Engine
    gen.UnimplementedShardServiceServer
}

func NewShardServer(eng esapi.Engine) *ShardServer {
    return &ShardServer{eng: eng}
}

// Write delegates to esapi.Engine.Bulk.
func (s *ShardServer) Write(ctx context.Context, req *gen.WriteBatch) (*gen.WriteAck, error) {
    docs := ConvertWriteBatch(req)
    results, err := s.eng.Bulk(ctx, docs, convertRefresh(req.Refresh))
    if err != nil {
        return &gen.WriteAck{Success: false, Error: err.Error()}, nil
    }
    return ConvertWriteAck(results), nil
}

// Search delegates to esapi.Engine.Search.
func (s *ShardServer) Search(ctx context.Context, req *gen.SearchRequest) (*gen.SearchResponse, error) {
    result, err := s.eng.Search(ctx, req.Indices, req.Body)
    if err != nil {
        return nil, err
    }
    return ConvertSearchResult(result), nil
}

// Get delegates to esapi.Engine.Get.
func (s *ShardServer) Get(ctx context.Context, req *gen.GetRequest) (*gen.GetResponse, error) {
    source, seqNo, primary, notFound, err := s.eng.Get(ctx, req.Index, req.Id)
    if err != nil {
        return nil, err
    }
    return &gen.GetResponse{
        Found:   !notFound,
        Source:  source,
        SeqNo:   seqNo,
        Primary: primary,
    }, nil
}

// MetaServer implements MetaService.
type MetaServer struct {
    gen.UnimplementedMetaServiceServer
    // ... meta cluster reference for Apply/Watch/Heartbeat
}
```

**类型转换层** (`internal/server/grpc/converter.go`):
```go
package grpc

import (
    "github.com/unisearch-db/unisearch/api/proto/gen"
    "github.com/unisearch-db/unisearch/internal/server/esapi"
)

// ConvertWriteBatch converts proto WriteBatch to []esapi.IndexDoc.
func ConvertWriteBatch(req *gen.WriteBatch) []esapi.IndexDoc {
    docs := make([]esapi.IndexDoc, len(req.Ops))
    for i, op := range req.Ops {
        docs[i] = esapi.IndexDoc{
            Op:     esapi.DocOp(op.Kind),
            Index:  req.Index,
            ID:     op.Id,
            Source: op.Doc,
        }
    }
    return docs
}

// ConvertWriteAck converts []esapi.WriteResult to proto WriteAck.
func ConvertWriteAck(results []esapi.WriteResult) *gen.WriteAck {
    ack := &gen.WriteAck{Success: true}
    for _, r := range results {
        ack.Results = append(ack.Results, &gen.WriteResult{
            Id:      r.ID,
            Result:  r.Result,
            SeqNo:   r.SeqNo,
        })
    }
    return ack
}

// ConvertSearchResult converts esapi.SearchResult to proto SearchResponse.
func ConvertSearchResult(r *esapi.SearchResult) *gen.SearchResponse {
    hits := make([]*gen.SearchHit, len(r.Hits))
    for i, h := range r.Hits {
        hits[i] = &gen.SearchHit{
            Index:  h.Index,
            Id:     h.ID,
            Score:  h.Score,
            Source: h.Source,
            SeqNo:  h.SeqNo,
        }
    }
    var aggs []byte
    if r.Aggregations != nil {
        aggs, _ = jsonx.Marshal(r.Aggregations)
    }
    return &gen.SearchResponse{
        Total:        int32(r.Total),
        Relation:     r.Relation,
        MaxScore:     float32(ptrOr(r.MaxScore, 0)),
        Hits:         hits,
        Aggregations: aggs,
    }
}
```

**同进程双协议启动** (修改 `cmd/unisearch/main.go`):
```go
func runServer(cmd *cobra.Command, args []string) error {
    // ... existing embedded engine setup ...
    esapiServer := esapi.NewServer(engine)

    // OTel init
    ctx := context.Background()
    shutdown, err := observability.InitTracing(ctx, otelEndpoint)
    if err != nil {
        slog.Warn("tracing init failed", "err", err)
    } else {
        defer shutdown()
    }

    // HTTP server (port 9200)
    httpServer := &http.Server{Addr: ":9200", Handler: esapiServer}
    go func() { httpServer.ListenAndServe() }()

    // gRPC server (port 9300)
    grpcServer := grpc.NewServer(
        grpc.UnaryInterceptor(grpcinterceptor.Chain(
            grpcinterceptor.TracingInterceptor,
            grpcinterceptor.MetricsInterceptor,
        )),
    )
    gen.RegisterShardServiceServer(grpcServer, grpcsrv.NewShardServer(engine))
    gen.RegisterMetaServiceServer(grpcServer, grpcsrv.NewMetaServer())
    lis, _ := net.Listen("tcp", ":9300")
    go func() { grpcServer.Serve(lis) }()

    // Graceful shutdown: gRPC first, then HTTP
    <-ctx.Done()
    grpcServer.GracefulStop()
    httpServer.Shutdown(ctx)
}
```

### 2.5 G5: K8s Operator Reconciler

**变更内容**：
单 Reconciler 管理 meta/coordinator/shard 三角色子资源。

**Reconciler** (`deploy/k8s/operator/controller.go`):
```go
package operator

import (
    "context"
    appsv1 "k8s.io/api/apps/v1"
    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/errors"
    "k8s.io/apimachinery/pkg/runtime"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

// UniSearchClusterReconciler reconciles a UniSearchCluster object.
type UniSearchClusterReconciler struct {
    client.Client
    Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=unisearch.db,resources=unisearchclusters,verbs=get;list;watch;create;update;patch;delete
func (r *UniSearchClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    cluster := &UniSearchCluster{}
    if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Ensure meta StatefulSet
    if err := r.ensureStatefulSet(ctx, cluster, "meta", cluster.Spec.MetaReplicas); err != nil {
        return ctrl.Result{}, err
    }
    // Ensure coordinator StatefulSet
    if err := r.ensureStatefulSet(ctx, cluster, "coordinator", cluster.Spec.CoordinatorReplicas); err != nil {
        return ctrl.Result{}, err
    }
    // Ensure shard StatefulSet
    if err := r.ensureStatefulSet(ctx, cluster, "shard", cluster.Spec.ShardReplicas); err != nil {
        return ctrl.Result{}, err
    }
    // Ensure headless Service for Raft discovery
    if err := r.ensureService(ctx, cluster); err != nil {
        return ctrl.Result{}, err
    }
    // Ensure ConfigMap for cluster topology
    if err := r.ensureConfigMap(ctx, cluster); err != nil {
        return ctrl.Result{}, err
    }

    // Update status
    cluster.Status.Phase = "running"
    cluster.Status.Ready = true
    r.Status().Update(ctx, cluster)

    return ctrl.Result{}, nil
}

// ensureStatefulSet creates or updates a StatefulSet for the given role.
func (r *UniSearchClusterReconciler) ensureStatefulSet(ctx context.Context, cluster *UniSearchCluster, role string, replicas int32) error {
    // Build StatefulSet from template based on role
    // ... implementation using appsv1.StatefulSet ...
}

// SetupWithManager registers the controller.
func (r *UniSearchClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&UniSearchCluster{}).
        Owns(&appsv1.StatefulSet{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ConfigMap{}).
        Complete(r)
}
```

### 2.6 G6: Online Split 数据迁移

**变更内容**：
补齐 `SplitCoordinator.AdvancePhase` 各阶段真实逻辑。

```go
// internal/raft/split.go — 补齐 AdvancePhase

func (sc *SplitCoordinator) AdvancePhase(parentID types.ShardID) error {
    sc.mu.Lock()
    defer sc.mu.Unlock()
    state, ok := sc.activeSplits[parentID]
    if !ok {
        return fmt.Errorf("split: no active split for shard %d", parentID)
    }
    switch state.Phase {
    case SplitPhaseInitiated:
        // Phase 1 → 2: Enter dual-read, freeze parent writes
        // Parent shard stops accepting new writes; existing segments shared read-only
        sc.enterDualRead(state)

    case SplitPhaseDualRead:
        // Phase 2 → 3: Create child shards, inherit parent segment refs + DeleteBitmap
        sc.createChildren(state)
        // Children pull incremental WAL from parent
        sc.replayWALToChildren(state)

    case SplitPhaseCatching:
        // Phase 3 → 4: Atomic gen switch, decommission parent
        sc.atomicSwitchGen(state)
        sc.decommissionParent(state)
    }
    state.Phase++
    return nil
}

// enterDualRead freezes the parent shard for new writes.
func (sc *SplitCoordinator) enterDualRead(state *SplitState) {
    // Mark parent as frozen — new writes are rejected/redirected
    // Segments remain shared (read-only reference)
}

// createChildren creates child shard entries that inherit parent segments.
func (sc *SplitCoordinator) createChildren(state *SplitState) {
    // For each child:
    // 1. Create new shard with inherited segment references
    // 2. Copy parent's DeleteBitmap (independent copy for future divergence)
    // 3. Route documents by hash(routing) % len(children) to determine ownership
}

// replayWALToChildren replays parent's WAL entries to appropriate children.
func (sc *SplitCoordinator) replayWALToChildren(state *SplitState) {
    // For each WAL entry from parent (after split point):
    //   1. Determine which child owns the document (by hash)
    //   2. Replay the entry to that child's WAL
    // This catches up children to parent's latest state
}

// atomicSwitchGen atomically switches routing table to new generation.
func (sc *SplitCoordinator) atomicSwitchGen(state *SplitState) {
    // Update RouteEntry.Generation from OldGen to NewGen
    // New writes now go to children instead of parent
}

// decommissionParent transfers segment ref counts and marks parent offline.
func (sc *SplitCoordinator) decommissionParent(state *SplitState) {
    // Transfer segment reference counts from parent to children
    // Parent shard is marked as offline (no new reads/writes)
    // Segments remain alive until all child references are released
}
```

### 2.7 G7: Cross-Link 增量合并

**变更内容**：
补齐 `ExecuteMerge` 真实合并逻辑 + `RebuildCrossLinks`。

```go
// internal/storage/crosslink_merge.go — 补齐 ExecuteMerge

func (m *CrossLinkMerger) ExecuteMerge(ctx context.Context, plan *MergePlan, entries []*SegmentEntry) (*SegmentEntry, error) {
    // 1. Merge column data from all source segments
    mergedReader := m.mergeColumnData(entries)

    // 2. Merge HNSW graph — combine all nodes, rebuild adjacency
    mergedHNSW := m.mergeHNSWGraphs(entries)

    // 3. Merge inverted/scalar indexes
    mergedInverted := m.mergeInvertedIndexes(entries)
    mergedScalar := m.mergeScalarIndexes(entries)

    // 4. Preserve SegmentID (reuse first source's ID for crosslink stability)
    mergedSegID := plan.TargetSegID

    // 5. Rebuild crosslinks — scan merged HNSW for cross-segment edges
    crossLinks := RebuildCrossLinks(mergedSegID, mergedHNSW)

    // 6. Atomically replace source segments in SegmentSet
    merged := &SegmentEntry{
        Reader:  mergedReader,
        Indexes: mergeIndexMap(mergedInverted, mergedScalar, mergedHNSW),
    }
    // Caller (Shard) atomically swaps sources → merged in SegmentSet

    return merged, nil
}

// RebuildCrossLinks scans HNSW graph for edges crossing segment boundaries.
func RebuildCrossLinks(mergedSegID types.SegmentID, graphData []byte) []CrossLinkRef {
    // Parse HNSW CSR graph
    // For each node, check if any neighbor's SegmentID != mergedSegID
    // Those edges become CrossLinkRefs
    // In production: iterate graph adjacency, compare segment IDs
    var refs []CrossLinkRef
    // ... HNSW CSR scan logic ...
    return refs
}
```

### 2.8 G8: 冷热分层迁移

**变更内容**：
补齐 `ExecuteMigration` 真实迁移逻辑。

```go
// internal/storage/tiering/tier_manager.go — 新增 ExecuteMigration

// ExecuteMigration performs the actual data migration between tiers.
func (tm *TierManager) ExecuteMigration(ctx context.Context, action MigrationAction, seg *SegmentEntry) error {
    switch action {
    case ActionHotToWarm:
        return tm.hotToWarm(ctx, seg)
    case ActionWarmToCold:
        return tm.warmToCold(ctx, seg)
    default:
        return nil
    }
}

// hotToWarm quantizes HNSW graph to RaBitQ 1-bit representation.
func (tm *TierManager) hotToWarm(ctx context.Context, seg *SegmentEntry) error {
    // 1. Load full-precision HNSW graph from segment
    // 2. Apply RaBitQ quantization (already implemented in internal/index/hnsw/rabitq.go)
    quantizer := hnsw.NewRaBitQuantizer()
    quantized := quantizer.Quantize(seg.HNSWGraph)
    // 3. Replace full-precision vectors with quantized representation
    seg.Indexes["hnsw"] = quantized
    // 4. Release full-precision vector memory
    // Memory reduction: ~32x (1-bit vs float32)
    return nil
}

// warmToCold uploads segment files to object storage, keeps footer index locally.
func (tm *TierManager) warmToCold(ctx context.Context, seg *SegmentEntry) error {
    // 1. Serialize segment data (column files + index files)
    data := seg.Reader.Serialize()
    // 2. Upload to S3-compatible object storage
    // Use internal/objstore.S3Client (already implemented)
    err := tm.s3Client.Put(ctx, segPath(seg.ID), data)
    if err != nil {
        return err
    }
    // 3. Keep only footer/index metadata locally for on-demand loading
    seg.Reader = seg.Reader.FootprintOnly() // lightweight wrapper
    // 4. Full data loaded on-demand when segment is accessed
    return nil
}
```

### 2.9 G9: goccy/json 热路径加速

**变更内容**：
新建 `internal/jsonx/jsonx.go` 统一别名包。

```go
// internal/jsonx/jsonx.go
package jsonx

import "github.com/goccy/go-json"

// Marshal is a drop-in replacement for encoding/json.Marshal.
func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

// Unmarshal is a drop-in replacement for encoding/json.Unmarshal.
func Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// NewEncoder mirrors encoding/json.NewEncoder.
func NewEncoder(w io.Writer) *json.Encoder { return json.NewEncoder(w) }

// NewDecoder mirrors encoding/json.NewDecoder.
func NewDecoder(r io.Reader) *json.Decoder { return json.NewDecoder(r) }
```

**替换方式**：在 `internal/server/esapi/handlers.go`、`internal/embedded/engine.go`、`internal/embedded/docparse.go`、`internal/engine/dsl/parser.go` 中：
- `"encoding/json"` → `"github.com/unisearch-db/unisearch/internal/jsonx"`
- `json.Marshal` → `jsonx.Marshal` (函数名不变，包名变)
- `json.Unmarshal` → `jsonx.Unmarshal`

回退方式：修改 `internal/jsonx/jsonx.go` 一个文件，将 `github.com/goccy/go-json` 替换为 `encoding/json`。

---

## 3. 接口设计 (Interface Design)

### 3.1 Prometheus Metrics Endpoint

```
GET /_prometheus/metrics
```

**返回**: `text/plain; version=0.0.4; charset=utf-8`，Prometheus exposition format。

| 指标组 | 指标名 | 类型 | 标签 |
|--------|--------|------|------|
| 写入 | `unisearch_write_docs_total` | counter | tenant, index, shard |
| 写入 | `unisearch_write_batch_duration_seconds` | histogram | tenant, index |
| 查询 | `unisearch_query_duration_seconds` | histogram | tenant, index, phase |
| 段 | `unisearch_segments_open` | gauge | index, shard, tier |
| 索引 | `unisearch_hnsw_nodes_visited` | histogram | field |
| 租户 | `unisearch_tenant_quota_usage_ratio` | gauge | tenant, dim |

### 3.2 OTel Tracing Span API

```go
// 顶级 span
func StartHTTPSpan(ctx context.Context, method, path string) (context.Context, trace.Span)

// 二级 span
func StartPlanSpan(ctx context.Context) (context.Context, trace.Span)

// 三级 span
func StartShardSpan(ctx context.Context, shardID string) (context.Context, trace.Span)

// 四级 span
func StartSegmentSpan(ctx context.Context, op string) (context.Context, trace.Span)
// op: "segment.scan" | "index.hnsw" | "index.bm25" | "agg.partial"
```

### 3.3 gRPC Service

proto 已定义，生成后的 Go 接口：
```go
// ShardService
func (s *ShardServer) Write(ctx context.Context, req *gen.WriteBatch) (*gen.WriteAck, error)
func (s *ShardServer) Search(ctx context.Context, req *gen.SearchRequest) (*gen.SearchResponse, error)
func (s *ShardServer) Get(ctx context.Context, req *gen.GetRequest) (*gen.GetResponse, error)

// MetaService
func (s *MetaServer) Apply(ctx context.Context, req *gen.MetaCommand) (*gen.ApplyResult, error)
func (s *MetaServer) Watch(req *gen.WatchRequest, stream gen.MetaService_WatchServer) error
func (s *MetaServer) Heartbeat(ctx context.Context, req *gen.NodeInfo) (*gen.HeartbeatAck, error)
```

### 3.4 Tier Migration API

```go
// ExecuteMigration performs the actual data migration between tiers.
func (tm *TierManager) ExecuteMigration(ctx context.Context, action MigrationAction, seg *SegmentEntry) error
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| ctx | context.Context | 是 | 上下文 |
| action | MigrationAction | 是 | 迁移动作 (ActionHotToWarm / ActionWarmToCold) |
| seg | *SegmentEntry | 是 | 待迁移段 |

### 3.5 CI/CD Makefile Targets

| Target | 说明 |
|--------|------|
| `make ci` | lint + test + build（CI 阻塞门禁） |
| `make coverage` | 生成覆盖率报告 (coverage.out + coverage.html) |
| `make cross-build` | 交叉编译三平台二进制到 dist/ |
| `make fuzz` | 模糊测试种子运行 (60s) |
| `make proto` | 从 proto 生成 Go 代码 |

---

## 4. 内部设计 (Internal Design)

### 4.1 observability 包

**职责**: 指标定义 + tracing 初始化 + HTTP middleware

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `MetricsHandler` | `() -> http.Handler` | Prometheus exposition handler |
| `InitTracing` | `(ctx, endpoint) -> (func(), error)` | OTel SDK 初始化 |
| `WithMetrics` | `(http.Handler) -> http.Handler` | HTTP metrics middleware |
| `WithTracing` | `(http.Handler) -> http.Handler` | HTTP tracing middleware |
| `StartHTTPSpan` | `(ctx, method, path) -> (ctx, span)` | 顶级 span |
| `StartPlanSpan` | `(ctx) -> (ctx, span)` | 二级 span (查询计划) |
| `StartShardSpan` | `(ctx, shardID) -> (ctx, span)` | 三级 span (分片搜索) |
| `StartSegmentSpan` | `(ctx, op) -> (ctx, span)` | 四级 span (段/算子级) |

### 4.2 grpc 包

**职责**: gRPC server 实现 + 类型转换

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `NewShardServer` | `(esapi.Engine) -> *ShardServer` | 创建 ShardService server |
| `NewMetaServer` | `() -> *MetaServer` | 创建 MetaService server |
| `ConvertWriteBatch` | `(*gen.WriteBatch) -> []esapi.IndexDoc` | proto→esapi 转换 |
| `ConvertWriteAck` | `([]esapi.WriteResult) -> *gen.WriteAck` | esapi→proto 转换 |
| `ConvertSearchResult` | `(*esapi.SearchResult) -> *gen.SearchResponse` | esapi→proto 转换 |

### 4.3 Operator Reconciler

**职责**: K8s 集群生命周期管理

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `Reconcile` | `(ctx, req) -> (Result, error)` | 主调谐循环 |
| `ensureStatefulSet` | `(ctx, cluster, role, replicas) -> error` | 确保三角色 StatefulSet |
| `ensureService` | `(ctx, cluster) -> error` | 确保 Headless Service |
| `ensureConfigMap` | `(ctx, cluster) -> error` | 确保拓扑 ConfigMap |
| `SetupWithManager` | `(mgr) -> error` | 注册 controller |

### 4.4 SplitCoordinator (扩展)

**职责**: 在线分片分裂协议执行

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `enterDualRead` | `(*SplitState)` | 冻结父分片写入 |
| `createChildren` | `(*SplitState)` | 创建子分片 + 继承段引用 |
| `replayWALToChildren` | `(*SplitState)` | 子分片拉取增量 WAL 回放 |
| `atomicSwitchGen` | `(*SplitState)` | 原子切换路由代数 |
| `decommissionParent` | `(*SplitState)` | 父分片下线 + 引用计数转移 |

---

## 5. 数据模型 (Data Model)

### 5.1 SplitState (已有，补充字段)

```go
type SplitState struct {
    ParentID    types.ShardID
    ChildIDs    []types.ShardID
    OldGen      uint64
    NewGen      uint64
    SplitKey    uint32
    Phase       SplitPhase
    // 新增字段
    ParentSegments []*SegmentEntry  // 父分片段引用
    ChildBitmaps   []roaring.Bitmap  // 子分片继承的 DeleteBitmap 副本
    WALCursor      int64             // WAL 回放游标
}
```

### 5.2 CrossLinkRef (已有，无变更)

```go
type CrossLinkRef struct {
    SourceSegID types.SegmentID
    TargetSegID types.SegmentID
    NodeOffset  int32
}
```

### 5.3 MigrationContext (新增)

```go
type MigrationContext struct {
    SegmentID   types.SegmentID
    FromTier     Tier
    ToTier       Tier
    S3Client     S3Client   // for warm→cold
    Quantizer    *RaBitQuantizer // for hot→warm
}
```

---

## 6. 错误处理 (Error Handling)

### 6.1 错误类型定义

| 错误 | 触发条件 | 处理方式 |
|------|----------|----------|
| `errTracingInitFailed` | OTel exporter 连接失败 | 降级为 no-op tracer，记录 warning |
| `errGRPCConnFailed` | gRPC listener 绑定失败 | 日志记录 + 返回错误 |
| `errSplitInProgress` | 分片已有活跃 split | 返回错误，拒绝重复 split |
| `errMergeFailed` | 段合并失败 | 回滚到合并前状态 |
| `errMigrationFailed` | 段迁移失败 | 段保持原 tier，记录错误 |
| `errProtoNotFound` | protoc 未安装 | `make proto` 返回提示信息 |

### 6.2 边界情况处理

| 场景 | 期望行为 |
|------|----------|
| OTel collector 不可用 | span 丢弃，业务不受影响 |
| Prometheus scrape 超时 | 仅影响监控，返回空响应 |
| gRPC server 启动失败 | HTTP server 正常运行，日志记录 |
| Online split 期间父分片收到写入 | 返回 307 redirect 到子分片 |
| Cross-Link 合并时段被并发访问 | 引用计数保护，合并完成后旧段 GC |
| 冷热迁移时段正在被查询 | 延迟迁移到查询完成 |

---

## 7. 测试规划 (Test Plan)

### 7.1 测试策略表

| 测试类型 | 覆盖范围 | 目标覆盖率 | 说明 |
|----------|----------|------------|------|
| 单元测试 | observability (metrics/tracing) | ≥ 90% | 指标注册、span 创建、middleware |
| 单元测试 | grpc converter | ≥ 90% | proto↔esapi 类型转换 |
| 单元测试 | jsonx | ≥ 70% | Marshal/Unmarshal 往返一致性 |
| 单元测试 | operator controller | ≥ 70% | Reconciler 逻辑 (fake client) |
| 集成测试 | online split | - | 端到端 split → catchup → routing switch |
| 集成测试 | cross-link merge | - | 合并前后召回率 ≥ baseline-2% |
| 集成测试 | tier migration | - | Hot→Warm→Cold 实际数据迁移 |
| 集成测试 | gRPC end-to-end | - | Write→Search→Get via gRPC |
| Benchmark | goccy vs encoding/json | - | _search handler 序列化对比 |

### 7.2 关键测试场景清单

#### 正向用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| Prometheus metrics 暴露 | GET /_prometheus/metrics | 200 + unisearch_* 指标 | P0 |
| OTel span 创建 | 发送 _search 请求 | Jaeger 可见四级 span | P0 |
| gRPC Write | WriteBatch{index, ops} | WriteAck{success, results} | P0 |
| gRPC Search | SearchRequest{indices, body} | SearchResponse{hits} | P0 |
| Online split 完成 | InitiateSplit → AdvancePhase×3 | 子分片独立可服务 | P1 |
| Cross-Link 合并 | PlanMerge → ExecuteMerge | 召回率 ≥ baseline-2% | P1 |
| Hot→Warm 迁移 | ExecuteMigration(HotToWarm) | 段内存减少 ~32x | P1 |
| make ci | 执行 | lint+test+build 全绿 | P0 |

#### 边界用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| metrics endpoint 不被 instrument | GET /_prometheus/metrics | 无 unisearch_query_* 指标自增 | P0 |
| OTel collector 不可用 | 启动时 endpoint 无效 | 降级 no-op，业务正常 | P0 |
| split 期间并发写入 | split 进行中 + write | 返回 redirect 或 429 | P1 |
| 合并 0 个段 | ExecuteMerge(空数组) | 返回错误 | P1 |
| 冷热迁移时段被查询 | 迁移中 + search | 延迟迁移或返回旧 tier 数据 | P2 |

#### 异常用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| gRPC 端口被占用 | :9300 已用 | HTTP 正常，gRPC 记录错误 | P0 |
| 重复 split | 同分片两次 InitiateSplit | 返回 errSplitInProgress | P1 |
| goccy/json 无法处理的类型 | 含 channel 的 struct | 返回 error（与标准库一致） | P2 |

### 7.3 Mock 清单

| 依赖项 | Mock 方式 | 模拟行为 | 使用场景 |
|--------|-----------|----------|----------|
| OTel exporter | no-op tracer provider | 不发送 span | 单元测试、无 collector 环境 |
| S3 client | MemS3Client (已有) | 内存存储 | 冷热分层迁移测试 |
| K8s client | fake.NewClientBuilder() | 内存 K8s 对象 | Operator reconciler 测试 |
| esapi.Engine | mock 实现 (已有 EngineNoop) | 返回固定结果 | gRPC handler 单元测试 |

### 7.4 测试数据设计

- **split_test**: 创建 3 个段（各 100 文档），split 后验证子分片各 ~150 文档
- **crosslink_merge_test**: 创建 2 个段（各 50 个 768d 向量），合并后验证 k=10 召回率
- **tier_test**: 创建 1 个段（100 个 768d 向量），Hot→Warm 后验证 RaBitQ 搜索可用

### 7.5 验证方式

- **本地验证**: `make ci` 全绿 + `make build && ./unisearch --embedded` + `curl localhost:9200/_prometheus/metrics`
- **gRPC 验证**: `grpcurl -plaintext -d '{"index":"test","id":"1"}' localhost:9300 unisearch.ShardService/Get`
- **OTel 验证**: 启动 Jaeger (`docker run -p 4317:4317 jaegertracing/all-in-one`) → 发送查询 → Jaeger UI 查看 span 树
- **CI 验证**: `make ci` && `make cross-build` && `make coverage`

---

## 8. 验收标准 (Acceptance Criteria)

- [ ] **可观测性**: `/_prometheus/metrics` 返回五组 `unisearch_*` 指标 + `go_*` 运行时指标
- [ ] **可观测性**: OTel trace 在 Jaeger 可见 `http.request → query.plan → shard.search → segment.scan` 四级 span 树
- [ ] **gRPC**: `grpcurl` 可通过 :9300 调用 ShardService.Write/Search/Get
- [ ] **gRPC**: HTTP (:9200) 和 gRPC (:9300) 同进程运行，互不干扰
- [ ] **K8s Operator**: Reconciler 可创建三角色 StatefulSet + Headless Service + ConfigMap
- [ ] **Online split**: InitiateSplit → AdvancePhase×3 后，子分片可独立响应 Search
- [ ] **Cross-Link merge**: ExecuteMerge 后 k=10 召回率 ≥ 合并前 -2%
- [ ] **冷热分层**: Hot→Warm 后段内存减少；Warm→Cold 后段数据在 S3 可验证
- [ ] **CI/CD**: `make ci` 全绿（lint + test + build）
- [ ] **CI/CD**: `make cross-build` 产出 dist/unisearch-{linux-amd64,linux-arm64,darwin-arm64}
- [ ] **goccy/json**: 所有热路径 JSON 序列化通过 jsonx，`go build` 编译通过
- [ ] **审计覆盖率**: 117 功能点中完全实现占比 ≥92%
- [ ] **编译测试**: `go build ./...` + `go test ./...` + `go vet ./...` 全绿

---

## 9. 实现清单 (Implementation Checklist)

### Phase 1: P0 基础设施 (可并行)
- [ ] G1: `internal/observability/metrics.go` — 五组指标定义 + MetricsHandler
- [ ] G1: `internal/observability/middleware.go` — WithMetrics middleware
- [ ] G1: 修改 `router.go` — 注册 /_prometheus/metrics + middleware 链扩展
- [ ] G1: 修改 `flush.go` / `wand.go` / `tenant.go` — 业务路径埋点
- [ ] G2: `internal/observability/tracing.go` — InitTracing + span helpers
- [ ] G2: `internal/observability/middleware.go` — WithTracing middleware
- [ ] G2: 修改 `engine.go` — Search 方法注入 plan span
- [ ] G3: `.golangci.yml` + `scripts/ci.sh` + `scripts/build.sh`
- [ ] G3: 修改 `Makefile` — ci/coverage/cross-build/fuzz/proto target
- [ ] G9: `internal/jsonx/jsonx.go` — goccy/go-json 别名包
- [ ] G9: 修改 4 个热路径文件 — encoding/json → jsonx 替换

### Phase 2: P1 分布式核心 (串行)
- [ ] G4: `api/proto/gen/doc.go` — go:generate 指令
- [ ] G4: 执行 `make proto` 生成 Go 代码
- [ ] G4: `internal/server/grpc/server.go` — ShardServer + MetaServer
- [ ] G4: `internal/server/grpc/converter.go` — 类型转换层
- [ ] G4: `internal/server/grpc/interceptor.go` — tracing + metrics interceptor
- [ ] G4: 修改 `cmd/unisearch/main.go` — gRPC server 启动 + OTel 初始化
- [ ] G5: 修改 `deploy/k8s/operator/types.go` — DeepCopy 方法
- [ ] G5: `deploy/k8s/operator/scheme.go` — SchemeBuilder 注册
- [ ] G5: `deploy/k8s/operator/controller.go` — Reconciler 实现
- [ ] G5: `deploy/k8s/operator/main.go` — manager 启动入口

### Phase 3: P2 数据迁移 (可并行)
- [ ] G6: 修改 `internal/raft/split.go` — 补齐 AdvancePhase 五个方法
- [ ] G6: `internal/raft/split_test.go` — 端到端测试
- [ ] G7: 修改 `internal/storage/crosslink_merge.go` — 补齐 ExecuteMerge
- [ ] G7: `internal/storage/crosslink_merge_test.go` — 召回率测试
- [ ] G8: 修改 `internal/storage/tiering/tier_manager.go` — 补齐 ExecuteMigration
- [ ] G8: `internal/storage/tiering/tier_test.go` — 迁移测试

### Phase 4: 测试 + 验证
- [ ] `internal/observability/metrics_test.go`
- [ ] `internal/observability/tracing_test.go`
- [ ] `internal/server/grpc/server_test.go`
- [ ] `go build ./...` 全绿
- [ ] `go test ./...` 全绿
- [ ] `go vet ./...` 全绿
- [ ] `make ci` 全绿
- [ ] 审计覆盖率验证 ≥92%
