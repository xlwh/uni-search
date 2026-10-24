# 分布式追踪

## 概述

UniSearch 使用 OpenTelemetry (OTel) 实现分布式追踪，通过 OTLP gRPC 协议导出到 Jaeger。追踪覆盖 HTTP 请求、查询计划、分片搜索和段级操作四个层级，支持慢查询分析和性能瓶颈定位。

## 追踪架构

- **SDK**：`go.opentelemetry.io/otel`
- **导出协议**：OTLP gRPC
- **默认端点**：`localhost:4317`（通过 `OTEL_EXPORTER_OTLP_ENDPOINT` 配置）
- **降级策略**：Collector 不可达时 tracing 降级为 no-op，业务不受影响
- **Service Name**：`unisearch`

初始化代码位于 `cmd/unisearch/server.go`：

```go
otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
if otelEndpoint == "" {
    otelEndpoint = "localhost:4317"
}
tracingShutdown, err := observability.InitTracing(ctx, otelEndpoint)
if err != nil {
    slog.Warn("tracing init failed, degrading to no-op", "err", err)
}
```

## 四级 Span 树

| 层级 | Span 名称 | 属性 | 说明 |
|------|-----------|------|------|
| 1 | `http.request` | method, path | HTTP/gRPC 入口 |
| 2 | `query.plan` | 优化器规则命中 | 查询计划与优化 |
| 3 | `shard.search` | shard_id | per 分片搜索 |
| 4 | `segment.scan` | — | 段扫描 |
| 4 | `index.hnsw` | field | HNSW 向量搜索 |
| 4 | `index.bm25` | field | BM25 倒排搜索 |
| 4 | `agg.partial` | — | 分片预聚合 |

### Span 创建 API

```go
// Level 1: HTTP 请求
ctx, span := observability.StartHTTPSpan(ctx, method, path)

// Level 2: 查询计划
ctx, span := observability.StartPlanSpan(ctx)

// Level 3: 分片搜索
ctx, span := observability.StartShardSpan(ctx, shardID)

// Level 4: 段/算子级
ctx, span := observability.StartSegmentSpan(ctx, "index.hnsw")
```

## 采样策略

| 策略 | 说明 |
|------|------|
| Parent-based | 继承父 Span 采样决策 |
| 基线比率 | 1% TraceIDRatioBased |
| 错误全采 | 错误请求 100% 采样 |
| 慢查询全采 | >1s 请求 100% 采样 |

采样配置位于 `internal/observability/tracing.go`：

```go
sdktrace.WithSampler(
    sdktrace.ParentBased(
        sdktrace.TraceIDRatioBased(0.01), // 1% 基线
    ),
)
```

Batch 配置：

- 批次超时：5s
- 最大队列：2048

## Jaeger 部署

### Docker Compose

`docker-compose.yml` 中已包含 Jaeger 配置：

```yaml
jaeger:
  image: jaegertracing/all-in-one:1.55
  ports: ["16686:16686", "4317:4317"]
  environment:
    - COLLECTOR_OTLP_ENABLED=true
```

| 端口 | 用途 |
|------|------|
| 16686 | Jaeger UI |
| 4317 | OTLP gRPC（接收 trace） |

### 独立部署

```bash
docker run -d --name jaeger \
  -p 16686:16686 -p 4317:4317 \
  -e COLLECTOR_OTLP_ENABLED=true \
  jaegertracing/all-in-one:1.55
```

### K8s 部署

```bash
kubectl create deployment jaeger --image=jaegertracing/all-in-one:1.55
kubectl expose deployment jaeger --port=16686 --port=4317
```

## 追踪配置

### 环境变量

```bash
# 设置 OTel Collector 端点
export OTEL_EXPORTER_OTLP_ENDPOINT=jaeger:4317

# 启动 UniSearch
./unisearch --embedded --data-dir ./data
```

### Docker

```bash
docker run -d \
  -p 9200:9200 \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=jaeger:4317 \
  unisearch:latest
```

### docker-compose

UniSearch 节点需与 Jaeger 在同一网络：

```yaml
services:
  unisearch:
    environment:
      - OTEL_EXPORTER_OTLP_ENDPOINT=jaeger:4317
```

## 追踪数据分析

### 在 Jaeger UI 中搜索

1. 访问 `http://localhost:16686`
2. Service 选择 `unisearch`
3. 按 Operation 过滤（`http.request` / `query.plan` / `shard.search`）
4. 按 Tags 过滤（如 `error=true` 查看错误请求）
5. 按 Duration 过滤（如 `>1s` 查看慢查询）

### Span 属性解读

| Span | 关键属性 | 用途 |
|------|----------|------|
| `http.request` | http.method, http.target | 请求方法和路径 |
| `query.plan` | optimizer.rules | 命中的优化规则 |
| `shard.search` | shard.id | 分片标识 |
| `index.hnsw` | field, k, ef_search | 向量搜索参数 |
| `index.bm25` | field, wand.pruned | BM25 搜索和剪枝率 |

### 通过 trace_id 关联日志

每条日志记录自动注入 `trace_id` 和 `span_id`：

```json
{"ts":"...","level":"INFO","msg":"search completed","trace_id":"abc123","span_id":"def456","took_ms":150}
```

在 Jaeger UI 中获取 trace_id，然后在日志中搜索该 ID 关联所有相关日志。

### 分析慢查询链路

1. 在 Jaeger UI 中按 Duration > 1s 过滤
2. 展开四级 Span 树
3. 定位耗时最长的 Level 4 Span
4. 根据 Span 类型（`index.hnsw` / `index.bm25` / `segment.scan`）判断瓶颈
5. 结合 Prometheus 指标（如 `hnsw_search_duration_seconds`）进一步分析

## gRPC 追踪

gRPC 拦截器链自动注入 trace context：

```go
grpc.NewServer(
    grpc.UnaryInterceptor(grpcsrv.ChainUnaryInterceptors(
        grpcsrv.TracingInterceptor,
        grpcsrv.MetricsInterceptor,
    )),
)
```

在云原生模式下，trace context 通过 `traceparent` header 跨进程传播，串联 Coordinator → Shard 的完整调用链。

## 与 profile 的关系

追踪和 [性能调优](performance-tuning.md) 中的 profile API 共享计时采集点。追踪系统自动收集 span 级别的时间数据，无需额外配置。详见设计文档 `go-design/03-query-engine.md §11`。
