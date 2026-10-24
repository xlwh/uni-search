# 性能调优

## 概述

本文档覆盖 UniSearch 写入、查询、向量搜索、段管理和内存五个维度的性能调优方法。所有调优参数均通过配置文件或环境变量调整，详见 [配置参考](../user-guide/configuration-reference.md)。

## 写入调优

### 核心参数

| 参数 | 默认值 | 调优方向 | 说明 |
|------|--------|----------|------|
| `memtable.flush_size_mb` | 512 | 增大提升吞吐 | MemTable 触发 flush 的阈值 |
| `memtable.flush_interval` | 30s | 增大减少小段 | 强制 flush 间隔 |
| `wal.group_commit_lag` | 5ms | 减小降低延迟 | Group commit 等待时间 |
| `merge.max_concurrent` | 2 | 增大加速合并 | 最大并发合并任务数 |
| `merge.max_io_mbps` | 100 | 增大加速合并 | 合并 I/O 上限 |

### 高吞吐写入

```yaml
memtable:
  flush_size_mb: 1024        # 增大 MemTable
  flush_interval: 60s        # 减少小段产生
merge:
  max_concurrent: 4          # 增加合并并发
  max_io_mbps: 200           # 提高 I/O 上限
```

### 低延迟写入

```yaml
memtable:
  flush_size_mb: 256         # 减小 MemTable 加速 flush
  flush_interval: 10s        # 更快刷盘
wal:
  group_commit_lag: 1ms      # 降低 group commit 等待
```

### 背压调优

当写入速度超过 flush 速度时，触发 429 拒绝。调优方法：

1. 增大 `memtable.flush_size_mb`，让单个段更大（减少段数量）
2. 增大 `merge.max_concurrent`，加速段合并
3. 增大 `merge.max_io_mbps`，提高合并 I/O 吞吐
4. 客户端侧：减小批量大小或降低并发数

## 查询调优

### 核心参数

| 参数 | 默认值 | 调优方向 | 说明 |
|------|--------|----------|------|
| `query.timeout` | 30s | 根据场景调整 | 查询超时 |
| `query.max_result_window` | 10000 | 不建议增大 | from + size 上限 |
| `segments.max_open` | 256 | 增大减少段打开开销 | 最大打开段数 |

### 查询优化策略

**1. 使用 filter 替代 query**

Filter 子句不参与评分且结果可缓存：

```json
{
  "query": {
    "bool": {
      "must": [{"match": {"title": "search"}}],
      "filter": [{"term": {"status": "published"}}]
    }
  }
}
```

**2. 避免大 from 值**

`from + size` 超过 10000 时使用 PIT + scroll：

```bash
# 创建 PIT
curl -X POST http://localhost:9200/my-index/_pit?keep_alive=10m

# 使用 scroll 分页
curl -X POST "http://localhost:9200/_search?scroll=1m" -H 'Content-Type: application/json' -d '{
  "size": 100,
  "query": { "match_all": {} }
}'
```

**3. 减少返回字段**

```json
{
  "_source": ["title", "id"],
  "query": { "match": { "content": "search" } }
}
```

**4. 合理设置分片数**

| 数据量 | 建议分片数 |
|--------|-----------|
| < 1M 文档 | 1 |
| 1M - 10M | 2-3 |
| 10M - 100M | 3-5 |
| > 100M | 5-10 |

### 聚合优化

```json
{
  "aggs": {
    "by_tag": {
      "terms": {
        "field": "tag",
        "size": 10,
        "execution_hint": "map"
      }
    }
  }
}
```

- `execution_hint: "map"` 适用于高基数聚合
- `size` 不要设置过大
- `cardinality` 聚合的 `precision_threshold` 默认 40000

## 向量搜索调优

### 索引时参数

| 参数 | 默认值 | 调优方向 | 说明 |
|------|--------|----------|------|
| `hnsw.m` | 16 | 增大提升召回率 | 图的连通度 |
| `hnsw.ef_construction` | 200 | 增大提升图质量 | 构建时搜索宽度 |
| `hnsw.quantization` | sq8 | full 提升精度 | 量化方式 |

### 查询时参数

| 参数 | 说明 | 调优方向 |
|------|------|----------|
| `num_candidates` | 候选集大小 | 增大提升召回率，降低性能 |
| `ef_search` | 搜索时宽度 | 增大提升召回率 |
| `k` | 返回结果数 | — |

### 召回率优先

```yaml
index:
  hnsw:
    m: 32                    # 增大连通度
    ef_construction: 400     # 增大构建宽度
    quantization: full        # 不量化，保持精度
```

查询：

```json
{
  "knn": {
    "field": "embedding",
    "query_vector": [0.1, 0.2],
    "k": 10,
    "num_candidates": 500
  }
}
```

### 性能优先

```yaml
index:
  hnsw:
    m: 12                    # 减小连通度
    ef_construction: 100     # 减小构建宽度
    quantization: sq8        # SQ8 量化，4x 压缩
```

查询：

```json
{
  "knn": {
    "field": "embedding",
    "query_vector": [0.1, 0.2],
    "k": 10,
    "num_candidates": 50
  }
}
```

### 量化方式对比

| 量化 | 压缩率 | 召回率损失 | 适用场景 |
|------|--------|-----------|----------|
| `full` | 1x | 0% | 召回率优先 |
| `sq8` | 4x | < 2% | 均衡 |
| `rabitq` | 32x | < 5% | 内存敏感 |

## 段管理调优

### 段合并策略

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `merge.max_concurrent` | 2 | 并发合并数 |
| `merge.max_io_mbps` | 100 | I/O 上限 |
| `segments.max_open` | 256 | 最大打开段数 |

### 手动合并

```bash
# 强制合并到 1 个段（适合静态索引）
curl -X POST "http://localhost:9200/my-index/_forcemerge?max_num_segments=1"

# 合并到 3 个段（适合写入中的索引）
curl -X POST "http://localhost:9200/my-index/_forcemerge?max_num_segments=3"
```

> 注意：`_forcemerge` 会产生大量 I/O，建议在低峰期执行。对正在写入的索引慎用 `max_num_segments=1`。

### 冷热分层

```yaml
storage:
  tiering:
    hot:
      max_age: 24h            # 24 小时内为热层
    warm:
      max_age: 7d             # 7 天内为温层
    cold:
      max_age: 30d            # 超过 30 天为冷层
```

热层使用本地 SSD，冷层可下沉到对象存储。

## 内存调优

### 关键内存区域

| 区域 | 参数 | 默认值 | 说明 |
|------|------|--------|------|
| MemTable | `memtable.flush_size_mb` | 512 | 写缓冲区 |
| 段缓存 | `segments.max_open` | 256 | 打开段数 |
| HNSW 缓存 | `hnsw.cache_capacity` | — | 向量图缓存 |
| Merge 缓冲 | `merge.max_concurrent` | 2 | 合并内存 |

### 内存不足应对

```yaml
# 降低内存使用
memtable:
  flush_size_mb: 128          # 减小 MemTable
segments:
  max_open: 64                # 减少打开段数
merge:
  max_concurrent: 1           # 降低合并并发
```

### 内存估算

```
总内存 ≈ MemTable × 活跃索引数 + 段文件 mmap + HNSW 图缓存 + Go runtime

# 示例（10 个活跃索引）
MemTable:  512MB × 10 = 5GB
段 mmap:   ~2GB（取决于段数量和大小）
HNSW:     ~1GB（取决于向量数量和维度）
Go:       ~500MB
总计:     ~8.5GB
```

## 监控与诊断

### 性能指标

```bash
# 查询 P99 延迟
curl -s http://localhost:9200/_prometheus/metrics | grep query_duration_seconds_bucket

# 写入 P99 延迟
curl -s http://localhost:9200/_prometheus/metrics | grep write_batch_duration_seconds_bucket

# HNSW 搜索耗时
curl -s http://localhost:9200/_prometheus/metrics | grep hnsw_search_duration_seconds

# WAND 剪枝率
curl -s http://localhost:9200/_prometheus/metrics | grep bm25_wand_pruned_ratio
```

### 慢查询分析

```bash
# 查看慢查询日志
docker logs unisearch 2>&1 | jq 'select(.msg == "slow query") | {took_ms, plan_summary, rows_read}'
```

详见 [日志](logging.md) 和 [分布式追踪](tracing.md)。

## 性能基准

项目内置 10 个性能基准（B1-B10），详见 [构建与测试](../development/build-and-test.md) 和 [性能保障设计](../go-design/06-quality-and-observability.md)：

| 基准 | 场景 | 目标 |
|------|------|------|
| B1 | 写入吞吐 | 50K docs/s |
| B2 | 向量 P99 | < 5ms |
| B6 | 可见性 P99 | < 300ms |

CI 中的 `bench-regression` Job 执行微缩版基准，监控性能回归。
