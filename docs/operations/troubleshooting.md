# 故障排查

## 概述

本文档覆盖 UniSearch 常见问题的诊断和解决方法，按问题类别分为启动、写入、查询、分布式和内存五大类。

## 启动问题

### 服务无法启动

**现象**：`./unisearch --embedded` 启动后立即退出。

**诊断步骤**：

```bash
# 1. 检查端口是否被占用
lsof -i :9200
lsof -i :9300

# 2. 检查数据目录权限
ls -la ./data

# 3. 检查配置文件语法
./unisearch --config ./config.yaml --dump-config

# 4. 查看启动日志
./unisearch --embedded --data-dir ./data 2>&1 | head -50
```

**常见原因与解决方案**：

| 原因 | 日志特征 | 解决方案 |
|------|----------|----------|
| 端口被占用 | `listen :9200: bind: address already in use` | 停止占用进程或使用 `--listen` 指定其他端口 |
| 数据目录无权限 | `open ./data: permission denied` | `chmod 755 ./data` 或更换目录 |
| 配置文件语法错误 | `yaml: unmarshal errors` | 修正 YAML 语法 |
| 数据目录损坏 | `corrupt WAL header` | 清空数据目录重建，或从备份恢复 |
| 内存不足 | `OOM killed` | 增加内存或降低 `memtable.flush_size_mb` |

### 启动后无法访问

**现象**：进程在运行，但 `curl http://localhost:9200` 无响应。

**诊断**：

```bash
# 检查进程
ps aux | grep unisearch

# 检查监听
netstat -tlnp | grep 9200

# 检查防火墙
iptables -L -n | grep 9200

# 检查健康
curl -v http://localhost:9200/_cluster/health
```

| 原因 | 解决方案 |
|------|----------|
| 监听地址不正确 | 使用 `--listen 0.0.0.0:9200` 替代默认 `:9200` |
| 防火墙阻断 | 开放 9200/9300 端口 |
| Docker 网络问题 | 确认端口映射 `docker run -p 9200:9200` |

## 写入问题

### 写入返回 429

**现象**：`_bulk` 请求返回 HTTP 429。

**原因**：背压机制触发，MemTable 积压超过阈值。

**诊断**：

```bash
# 检查写入拒绝指标
curl http://localhost:9200/_prometheus/metrics | grep write_rejected

# 检查 flush 耗时
curl http://localhost:9200/_prometheus/metrics | grep flush_duration
```

**解决方案**：

| 调整项 | 参数 | 方向 |
|--------|------|------|
| MemTable 大小 | `memtable.flush_size_mb` | 增大（如 1024） |
| Flush 线程数 | `flush.workers` | 增大（如 4） |
| 客户端批大小 | — | 减小批量或降低并发 |
| Merge 速度 | `merge.max_io_mbps` | 增大 |

### 写入延迟过高

**诊断**：

```bash
# 检查写入耗时分布
curl http://localhost:9200/_prometheus/metrics | grep write_batch_duration

# 检查 merge 积压
curl http://localhost:9200/_prometheus/metrics | grep merge_running

# 检查段数量
curl http://localhost:9200/_cat/indices?v
```

**解决方案**：

1. 段数量过多 → 手动 forcemerge：`POST /my-index/_forcemerge?max_num_segments=1`
2. Merge 积压 → 增大 `merge.max_io_mbps` 和 merge 线程数
3. 磁盘 I/O 瓶颈 → 使用更高性能磁盘（SSD/NVMe）

### 文档写入后查不到

**原因**：默认刷新间隔为 1 秒，写入后需等待 refresh 才可搜索。

**解决方案**：

```bash
# 手动刷新
curl -X POST http://localhost:9200/my-index/_refresh

# 或写入时指定 refresh=true
curl -X POST "http://localhost:9200/my-index/_doc?refresh=true" -H 'Content-Type: application/json' -d '{...}'
```

## 查询问题

### 查询超时

**现象**：查询返回 504 或 `SearchTimeoutError`。

**诊断**：

```bash
# 检查慢查询日志
docker logs unisearch 2>&1 | jq 'select(.msg == "slow query")'

# 检查查询耗时
curl http://localhost:9200/_prometheus/metrics | grep query_duration

# 使用 _explain 分析查询
curl -X POST http://localhost:9200/my-index/_explain/<doc_id> -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "test" } }
}'
```

**解决方案**：

| 原因 | 解决方案 |
|------|----------|
| 全表扫描 | 添加 filter 缩小扫描范围 |
| 向量搜索 ef_search 过大 | 降低 `ef_search`（如 100 → 50） |
| from + size 过大 | 使用 PIT + scroll 分页 |
| 段数量过多 | forcemerge 减少段数 |
| 分片不均 | 调整 `number_of_shards` |

### 向量搜索召回率低

**诊断**：

```bash
# 检查 HNSW 搜索参数
curl -X POST http://localhost:9200/my-index/_knn_search -H 'Content-Type: application/json' -d '{
  "knn": {
    "field": "embedding",
    "query_vector": [0.1, 0.2, ...],
    "k": 10,
    "num_candidates": 100
  }
}'
```

**解决方案**：

- 增大 `num_candidates`（如 100 → 500），扩大搜索候选集
- 增大索引时 `ef_construction`（如 200 → 400），提升图质量
- 检查量化配置：SQ8 可能损失精度，对召回率要求高时使用 `full` 量化

### 聚合结果不准确

**原因**：聚合默认使用段级预聚合 + 协调层合并，某些聚合类型存在近似算法。

**解决方案**：

- `cardinality` 聚合：增大 `precision_threshold`（默认 40000，最大 40000）
- `terms` 聚合：增大 `size` 参数
- 精确聚合：确保数据量在单分片内可处理

## 分布式问题

### Raft 选举失败

**现象**：Meta 节点日志出现 `failed to contact peer` 或 `election timeout`。

**诊断**：

```bash
# 检查集群健康
curl http://localhost:9201/_cluster/health

# 检查节点列表
curl http://localhost:9201/_cat/nodes?v

# 检查 Meta 节点日志
docker logs meta-1 2>&1 | grep -i raft
```

**解决方案**：

| 原因 | 解决方案 |
|------|----------|
| 节点间网络不通 | 检查 9300 端口连通性 |
| Raft peers 配置错误 | 检查 `--raft-peers` 参数格式 |
| 节点时钟不同步 | 配置 NTP 时间同步 |
| 磁盘空间不足 | 清理磁盘空间 |

### Shard 节点无法连接 Meta

**现象**：Shard 日志出现 `failed to connect to meta`。

**诊断**：

```bash
# 检查 Meta 节点健康
curl http://meta-1:9200/_cluster/health

# 检查 Shard → Meta 网络连通性
docker exec shard-1 ping meta-1

# 检查 Meta 地址配置
docker logs shard-1 2>&1 | grep meta-addrs
```

**解决方案**：

- 确认 `--meta-addrs` 参数指向正确的 Meta 节点地址
- 检查 Docker 网络或 K8s Service 配置
- 确认 Meta 节点已启动且健康

### 对象存储连接失败

**现象**：Shard 日志出现 `failed to connect to S3` 或 `object store error`。

**诊断**：

```bash
# 检查 MinIO 健康
curl http://minio:9000/minio/health/live

# 检查凭证配置
docker logs shard-1 2>&1 | grep -i objstore

# 检查网络连通性
docker exec shard-1 curl http://minio:9000
```

**解决方案**：

| 原因 | 解决方案 |
|------|----------|
| 凭证错误 | 检查 `UNISEARCH_OBJSTORE_ACCESS_KEY/SECRET_KEY` |
| 网络不通 | 检查 Docker 网络或 K8s Service |
| Bucket 不存在 | 在 MinIO Console 创建 bucket |
| 端点错误 | 确认 `--objstore` 参数格式 |

## 内存问题

### OOM KILLED

**现象**：进程被操作系统杀死，日志出现 `signal: killed`。

**诊断**：

```bash
# 检查内存使用
curl http://localhost:9200/_prometheus/metrics | grep go_memstats

# 检查 Goroutine 数
curl http://localhost:9200/_prometheus/metrics | grep go_goroutines

# 检查系统 dmesg
dmesg | grep -i "killed process"
```

**解决方案**：

| 调整项 | 参数 | 建议 |
|--------|------|------|
| MemTable 大小 | `memtable.flush_size_mb` | 降低（如 512 → 256） |
| 段打开数 | `segments.max_open` | 降低（如 256 → 128） |
| Merge 并发 | `merge.max_concurrent` | 降低（如 4 → 2） |
| HNSW 缓存 | `hnsw.cache_capacity` | 降低 |
| 容器内存限制 | Docker `--memory` | 增大 |

### Goroutine 泄漏

**诊断**：

```bash
# 查看 Goroutine 数量趋势
curl -s http://localhost:9200/_prometheus/metrics | grep go_goroutines

# 如果持续增长，可能存在泄漏
```

**解决方案**：

- 检查查询是否缺少 context 超时
- 确认 HTTP 客户端连接是否正确关闭
- 使用 `pprof` 分析 Goroutine 堆栈

## 诊断工具

### _cat API

```bash
# 索引概况
curl http://localhost:9200/_cat/indices?v

# 分片分布
curl http://localhost:9200/_cat/shards?v

# 节点信息
curl http://localhost:9200/_cat/nodes?v

# 段信息
curl http://localhost:9200/_cat/segments/my-index?v
```

### _explain API

```bash
curl -X POST http://localhost:9200/my-index/_explain/<doc_id> -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "test" } }
}'
```

### _prometheus/metrics

```bash
# 查看所有指标
curl http://localhost:9200/_prometheus/metrics

# 过滤特定指标
curl http://localhost:9200/_prometheus/metrics | grep unisearch_query
```

### Jaeger 追踪

在 Jaeger UI 中按 `unisearch` Service 搜索 trace，定位慢查询的具体 Span 层级。详见 [分布式追踪](tracing.md)。

### 日志分析

```bash
# 错误日志
docker logs unisearch 2>&1 | jq 'select(.level == "ERROR")'

# 慢查询日志
docker logs unisearch 2>&1 | jq 'select(.msg == "slow query")'

# 按 trace_id 过滤
docker logs unisearch 2>&1 | jq 'select(.trace_id == "abc123")'
```

详见 [日志](logging.md)。
