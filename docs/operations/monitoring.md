# Prometheus 监控

## 概述

UniSearch 内置 Prometheus 指标体系，通过 `GET /_prometheus/metrics` 端点暴露。指标分为五组：写入、查询、段、索引和租户配额，另保留 Go 运行时原生指标。

## 指标暴露

- **端点**：`GET /_prometheus/metrics`
- **格式**：Prometheus text exposition format
- **前缀**：`unisearch_`
- **不经过中间件包装**（避免自引用导致递归）

该端点直接注册在 Go 1.22+ `ServeMux` 上，不经过 panic recovery / request log / metrics / tracing 中间件链。

## 五组核心指标

### 写入组

| 指标 | 类型 | 标签 | 说明 |
|------|------|------|------|
| `unisearch_write_docs_total` | Counter | tenant, index, shard | 文档写入总数 |
| `unisearch_write_batch_duration_seconds` | Histogram | tenant, index, shard | 批量写入耗时 |
| `unisearch_write_rejected_total` | Counter | tenant, index, shard | 背压拒绝数（429） |

### 查询组

| 指标 | 类型 | 标签 | 说明 |
|------|------|------|------|
| `unisearch_query_requests_total` | Counter | tenant, index, phase | 查询请求总数 |
| `unisearch_query_duration_seconds` | Histogram | tenant, index, phase | 查询耗时 |
| `unisearch_query_rows_scanned_total` | Histogram | tenant, index, phase | 每次查询扫描行数 |
| `unisearch_query_errors_total` | Counter | tenant, index, phase | 查询错误数 |

### 段组

| 指标 | 类型 | 标签 | 说明 |
|------|------|------|------|
| `unisearch_segments_open` | Gauge | index, shard, tier | 打开段数 |
| `unisearch_flush_duration_seconds` | Histogram | index, shard, tier | Flush 耗时 |
| `unisearch_merge_running` | Gauge | index, shard, tier | 运行中合并任务数 |
| `unisearch_merge_bytes_total` | Counter | index, shard, tier | 合并总字节数 |
| `unisearch_deletes_pending` | Gauge | index, shard, tier | 待处理删除位图基数 |

### 索引组

| 指标 | 类型 | 标签 | 说明 |
|------|------|------|------|
| `unisearch_hnsw_search_duration_seconds` | Histogram | field | HNSW 搜索耗时 |
| `unisearch_hnsw_nodes_visited` | Histogram | field | HNSW 每次查询访问节点数 |
| `unisearch_bm25_wand_pruned_ratio` | Histogram | field | WAND 剪枝率（0=无剪枝，1=全剪枝） |

### 租户配额组

| 指标 | 类型 | 标签 | 说明 |
|------|------|------|------|
| `unisearch_tenant_quota_usage_ratio` | Gauge | tenant, dim | 配额使用率（0-1） |
| `unisearch_tenant_throttled_total` | Counter | tenant, dim | 限流次数 |

### Go 运行时指标

`go_*` 原生指标全量保留，包括：

- `go_gc_duration_seconds` — GC 暂停时间直方图
- `go_memstats_*` — 内存统计
- `go_goroutines` — Goroutine 数量
- `go_threads` — 线程数量
- `go_info` — Go 版本信息

## Prometheus 抓取配置

```yaml
scrape_configs:
  - job_name: 'unisearch'
    scrape_interval: 15s
    metrics_path: '/_prometheus/metrics'
    static_configs:
      - targets:
        - 'localhost:9200'
    # 或集群模式下抓取所有节点
    # - targets:
    #   - 'meta-1:9200'
    #   - 'meta-2:9200'
    #   - 'meta-3:9200'
    #   - 'shard-1:9200'
    #   - 'shard-2:9200'
```

## Grafana 面板配置

### 核心仪表盘建议

| 仪表盘 | 关键指标 | 说明 |
|--------|----------|------|
| 写入概览 | `write_docs_total` 速率、`write_batch_duration_seconds` P99、`write_rejected_total` 速率 | 写入吞吐和延迟趋势 |
| 查询性能 | `query_duration_seconds` P50/P95/P99、`query_rows_scanned_total` P99 | 查询延迟分布 |
| 段管理 | `segments_open`、`merge_running`、`deletes_pending` | 段健康度 |
| 索引效率 | `hnsw_search_duration_seconds` P99、`bm25_wand_pruned_ratio` | 索引引擎效率 |
| 租户配额 | `quota_usage_ratio`、`throttled_total` 速率 | 多租户资源使用 |
| Go 运行时 | `gc_duration_seconds` P99、`goroutines` | Go runtime 健康 |

### ES 指标名映射

从 ES 迁移到 UniSearch 时，Grafana 面板的指标名需替换：

| ES 指标 | UniSearch 指标 |
|---------|----------------|
| `http_total_duration` | `unisearch_query_duration_seconds` |
| `indices_indexing_index_total` | `unisearch_write_docs_total` |
| `indices_search_query_time` | `unisearch_query_duration_seconds` |
| `segments_count` | `unisearch_segments_open` |
| `indices_merge_time` | `unisearch_flush_duration_seconds` |

## 告警规则

```yaml
groups:
  - name: unisearch
    rules:
      # 写入拒绝率告警
      - alert: WriteRejectedHigh
        expr: rate(unisearch_write_rejected_total[5m]) > 10
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Write requests being rejected (backpressure)"

      # 查询错误率告警
      - alert: QueryErrorRateHigh
        expr: rate(unisearch_query_errors_total[5m]) / rate(unisearch_query_requests_total[5m]) > 0.05
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Query error rate above 5%"

      # 段数量过高告警
      - alert: TooManySegments
        expr: unisearch_segments_open > 100
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Open segments count high, consider forcemerge"

      # 合并积压告警
      - alert: MergeBacklog
        expr: unisearch_merge_running > 5
        for: 30m
        labels:
          severity: warning
        annotations:
          summary: "Merge tasks accumulating"

      # 租户配额告警
      - alert: TenantQuotaNearLimit
        expr: unisearch_tenant_quota_usage_ratio > 0.9
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Tenant quota usage above 90%"

      # GC 暂停告警
      - alert: GCPauseHigh
        expr: histogram_quantile(0.99, rate(go_gc_duration_seconds_bucket[5m])) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "GC P99 pause above 100ms"
```

## 与 Jaeger 联动

通过 `trace_id` 可关联 Prometheus 指标与 Jaeger 追踪：

1. 在 Prometheus 中发现异常指标（如查询延迟飙升）
2. 通过 `trace_id` 标签在 Jaeger 中定位对应请求链路
3. 分析四级 Span 树定位瓶颈

详见 [分布式追踪](tracing.md)。
