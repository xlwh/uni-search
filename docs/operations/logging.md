# 日志

## 概述

UniSearch 使用 Go 标准库 `log/slog` 输出 JSON 格式结构化日志，自动注入 OpenTelemetry `trace_id` 和 `span_id`，支持通过 trace_id 关联日志与分布式追踪。

## 日志格式

日志输出到 stdout，JSON 格式：

```json
{
  "ts": "2026-10-22T10:30:00.123Z",
  "level": "INFO",
  "msg": "starting unisearch",
  "mode": "embedded",
  "listen": ":9200",
  "data_dir": "./data"
}
```

### 字段说明

| 字段 | 说明 |
|------|------|
| `ts` | 时间戳（ISO 8601），通过 `ReplaceAttr` 将默认的 `time` 重命名为 `ts` |
| `level` | 日志级别：`INFO` / `WARN` / `ERROR` |
| `msg` | 消息内容 |
| `trace_id` | OTel trace ID（当请求上下文中存在活跃 span 时自动注入） |
| `span_id` | OTel span ID（同上） |
| 其他 | 业务字段（snake_case 命名） |

## 日志级别

| 级别 | 使用场景 |
|------|----------|
| `INFO` | 服务启动/关闭、请求日志（method/path/duration/status） |
| `WARN` | 慢查询告警、追踪初始化失败、gRPC 监听失败（降级而非阻塞） |
| `ERROR` | HTTP/gRPC 服务错误、关闭异常、flush 失败 |

## trace_id 关联

`traceHandler` 包装器自动从 OTel context 提取 trace_id/span_id 并注入每条日志记录：

```go
type traceHandler struct {
    inner slog.Handler
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
    if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
        r.AddAttrs(
            slog.String("trace_id", span.SpanContext().TraceID().String()),
            slog.String("span_id", span.SpanContext().SpanID().String()),
        )
    }
    return h.inner.Handle(ctx, r)
}
```

**设计约束**：错误日志必带 trace_id。通过 trace_id 可在 Jaeger 中定位完整请求链路。

## 慢查询日志

### 阈值

查询耗时超过 1 秒触发慢查询日志。

### 日志内容

```json
{
  "ts": "2026-10-22T10:30:05.456Z",
  "level": "WARN",
  "msg": "slow query",
  "trace_id": "abc123def456",
  "took_ms": 1500,
  "shards_scanned": 3,
  "rows_read": 50000,
  "plan_summary": "scan+filter+topn(10)"
}
```

| 字段 | 说明 |
|------|------|
| `took_ms` | 查询耗时（毫秒） |
| `shards_scanned` | 扫描分片数 |
| `rows_read` | 读取行数 |
| `plan_summary` | 执行计划摘要 |

### 用途

- R6 跳块效果审计
- 性能优化数据源
- 识别需要优化的查询模式和索引策略

### 代码位置

慢查询检测位于 `internal/observability/slow_query.go`：

```go
const SlowQueryThreshold = 1 * time.Second

func LogSlowQuery(ctx context.Context, took time.Duration, shardsScanned int, rowsRead int64, planSummary string) {
    if took < SlowQueryThreshold {
        return
    }
    slog.WarnContext(ctx, "slow query",
        "took_ms", took.Milliseconds(),
        "shards_scanned", shardsScanned,
        "rows_read", rowsRead,
        "plan_summary", planSummary,
    )
}
```

## 请求日志

每个 HTTP 请求输出一行 INFO 级别日志：

```json
{
  "ts": "2026-10-22T10:30:00.789Z",
  "level": "INFO",
  "msg": "request",
  "method": "POST",
  "path": "/my-index/_search",
  "duration": "5ms",
  "status": 200
}
```

由 `withRequestLog` 中间件实现，记录 method、path、duration 和 status。

## 日志收集

### Docker 环境

容器日志输出到 stdout，通过 Docker 日志驱动收集：

```bash
# 查看日志
docker logs unisearch

# 跟踪日志
docker logs -f unisearch

# 配置日志轮转
docker run -d \
  --log-driver=json-file \
  --log-opt max-size=100m \
  --log-opt max-file=5 \
  unisearch:latest
```

### K8s 环境

```yaml
# Pod 日志通过 kubectl 获取
kubectl logs -f deployment/unisearch

# 或通过 node-level agent 收集（Fluentd/Filebeat）
```

### ELK / Loki 集成

由于日志为 JSON 格式，可直接对接 ELK 或 Loki：

```yaml
# Fluentd 配置示例
<source>
  @type tail
  path /var/log/containers/unisearch*.log
  tag unisearch
  <parse>
    @type json
  </parse>
</source>

<match unisearch>
  @type elasticsearch
  host elasticsearch
  index unisearch-logs
</match>
```

## 日志分析最佳实践

### 按 trace_id 关联

```bash
# 提取特定请求的全部日志
docker logs unisearch 2>&1 | jq 'select(.trace_id == "abc123def456")'
```

### 按级别过滤

```bash
# 仅查看错误日志
docker logs unisearch 2>&1 | jq 'select(.level == "ERROR")'

# 查看警告及以上
docker logs unisearch 2>&1 | jq 'select(.level == "WARN" or .level == "ERROR")'
```

### 慢查询分析

```bash
# 提取慢查询日志
docker logs unisearch 2>&1 | jq 'select(.msg == "slow query")'

# 按 took_ms 降序排列
docker logs unisearch 2>&1 | jq 'select(.msg == "slow query") | .took_ms' | sort -rn | head -20
```

### 服务启动日志

```bash
# 查看启动序列
docker logs unisearch 2>&1 | jq 'select(.msg | startswith("starting") or startswith("listening") or startswith("tracing"))'
```
