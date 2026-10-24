# ES 兼容性

## 概述

UniSearch 提供与 Elasticsearch 8.x 兼容的 REST API，旨在降低迁移成本。本文档详细列出了兼容性矩阵、行为差异和迁移指南。

## 兼容性矩阵

### API 端点兼容性

| API 端点 | 兼容级别 | 说明 |
|----------|----------|------|
| `GET /` | 完全兼容 | 版本信息 |
| `GET /_cluster/health` | 完全兼容 | 集群健康 |
| `PUT /{index}` | 完全兼容 | 创建索引 |
| `DELETE /{index}` | 完全兼容 | 删除索引 |
| `GET /{index}` | 完全兼容 | 索引信息 |
| `PUT /{index}/_mapping` | 完全兼容 | 更新 Mapping |
| `GET /{index}/_mapping` | 完全兼容 | 查看 Mapping |
| `PUT /{index}/_settings` | 完全兼容 | 更新 Settings |
| `GET /{index}/_settings` | 完全兼容 | 查看 Settings |
| `POST /{index}/_doc` | 完全兼容 | 索引文档 |
| `PUT /{index}/_doc/{id}` | 完全兼容 | 索引文档（指定 ID） |
| `GET /{index}/_doc/{id}` | 完全兼容 | 获取文档 |
| `DELETE /{index}/_doc/{id}` | 完全兼容 | 删除文档 |
| `POST /_bulk` | 完全兼容 | 批量操作 |
| `POST /{index}/_search` | 完全兼容 | 搜索 |
| `POST /{index}/_knn_search` | 完全兼容 | kNN 搜索 |
| `POST /_msearch` | 完全兼容 | 多搜索 |
| `POST /{index}/_count` | 完全兼容 | 计数 |
| `POST /{index}/_explain/{id}` | 完全兼容 | 解释查询 |
| `POST /{index}/_delete_by_query` | 完全兼容 | 按查询删除 |
| `POST /{index}/_update_by_query` | 完全兼容 | 按查询更新 |
| `POST /_reindex` | 完全兼容 | 重建索引 |
| `POST /{index}/_pit` | 完全兼容 | 创建 PIT |
| `POST /_search/scroll` | 完全兼容 | Scroll 分页 |
| `POST /_aliases` | 完全兼容 | 别名操作 |
| `PUT /_index_template/{name}` | 完全兼容 | 索引模板 |
| `POST /{index}/_refresh` | 完全兼容 | 刷新 |
| `POST /{index}/_flush` | 完全兼容 | 冲刷 |
| `POST /{index}/_forcemerge` | 完全兼容 | 强制合并 |
| `GET /_cat/health` | 完全兼容 | Cat 健康 |
| `GET /_cat/indices` | 完全兼容 | Cat 索引 |
| `GET /_cat/shards` | 完全兼容 | Cat 分片 |
| `GET /_cat/nodes` | 完全兼容 | Cat 节点 |
| `POST /_analyze` | 完全兼容 | 分析器测试 |

### Query DSL 兼容性

| 查询类型 | 兼容级别 | 说明 |
|----------|----------|------|
| `match_all` | 完全兼容 | — |
| `match` | 完全兼容 | — |
| `match_phrase` | 完全兼容 | — |
| `multi_match` | 完全兼容 | — |
| `query_string` | 完全兼容 | — |
| `fuzzy` | 完全兼容 | — |
| `term` | 完全兼容 | — |
| `terms` | 完全兼容 | — |
| `range` | 完全兼容 | — |
| `exists` | 完全兼容 | — |
| `bool` | 完全兼容 | — |
| `knn` | 完全兼容 | — |
| `hybrid` | UniSearch 扩展 | ES 无原生 hybrid 查询 |
| `sparse_vector` | UniSearch 扩展 | — |

### 聚合兼容性

| 聚合类型 | 兼容级别 |
|----------|----------|
| `avg` / `sum` / `min` / `max` | 完全兼容 |
| `stats` / `value_count` | 完全兼容 |
| `cardinality` | 完全兼容 |
| `percentiles` | 完全兼容 |
| `terms` | 完全兼容 |
| `range` | 完全兼容 |
| `date_histogram` | 完全兼容 |
| `histogram` | 完全兼容 |
| `filter` / `filters` | 完全兼容 |

## 行为差异

### 1. hybrid 查询

UniSearch 原生支持 `hybrid` 查询类型，ES 8.x 需要 RRF retriever 或自定义评分：

```json
// UniSearch 原生
{
  "query": {
    "hybrid": {
      "queries": [...],
      "rank": { "method": "rrf" }
    }
  }
}

// ES 8.x 需要使用 rrf retriever
{
  "retriever": {
    "rrf": {
      "retrievers": [...]
    }
  }
}
```

### 2. 版本号

`GET /` 返回的版本信息中，`distribution` 字段为 `unisearch`：

```json
{
  "version": {
    "number": "8.12.0",
    "distribution": "unisearch"
  }
}
```

ES 返回 `distribution: "elasticsearch"`。

### 3. 分片与副本

UniSearch 云原生模式使用 Raft + 对象存储，分片管理机制与 ES 不同：

| 特性 | UniSearch | ES |
|------|-----------|-----|
| 分片分裂 | 支持（自动分裂） | 需 split API |
| 副本同步 | Raft quorum | 蓝绿复制 |
| 段存储 | 本地 + S3 归档 | 本地 + snapshot |

### 4. _source 过滤

UniSearch 支持 `_source` 字段过滤，但不支持 `includes` / `excludes` 的通配符嵌套语法。

### 5. 分布式查询

UniSearch 云原生模式的查询执行模型为 Coordinator 扇出到 Shard，Shard 返回预聚合结果由 Coordinator 合并。与 ES 的查询协调节点 + 数据节点模型类似，但内部协议不同。

## 不支持的 ES 特性

| 特性 | 状态 | 说明 |
|------|------|------|
| `_search/template` | 不支持 | 查询模板 |
| SQL API | 不支持 | `_sql` 端点 |
| Transform API | 不支持 | — |
| Watcher / Alerting | 不支持 | 使用外部 Prometheus + Alertmanager |
| Machine Learning | 不支持 | — |
| `PUT /_component_template` | 部分支持 | 简化的组件模板 |
| `POST /_ml/...` | 不支持 | — |

## 迁移指南

### 1. 数据迁移

使用 `reindex` 从 ES 迁移数据到 UniSearch：

```bash
# 1. 在 UniSearch 创建目标索引（复制 ES 的 mapping）
curl -X PUT http://localhost:9200/my-index -H 'Content-Type: application/json' -d '{
  "mappings": { ... }
}'

# 2. 使用 reindex 从 ES 拉取数据
curl -X POST http://localhost:9200/_reindex -H 'Content-Type: application/json' -d '{
  "source": {
    "remote": {
      "host": "http://es-cluster:9200",
      "username": "elastic",
      "password": "password"
    },
    "index": "my-index"
  },
  "dest": { "index": "my-index" }
}'
```

### 2. 客户端迁移

| ES 客户端 | UniSearch 兼容性 | 说明 |
|-----------|-------------------|------|
| elasticsearch-py (Python) | 兼容 | 修改 host/port 即可 |
| elasticsearch-js (Node.js) | 兼容 | 修改 host/port |
| elasticsearch-go (Go) | 兼容 | 修改 host/port |
| curl / HTTP 客户端 | 完全兼容 | 无需修改 |
| UniSearch Go SDK | 原生 | `pkg/unisdk` |

### 3. 指标迁移

从 ES 迁移到 UniSearch 时，Grafana 面板指标名需替换：

| ES 指标 | UniSearch 指标 |
|---------|----------------|
| `http_total_duration` | `unisearch_query_duration_seconds` |
| `indices_indexing_index_total` | `unisearch_write_docs_total` |
| `indices_search_query_time` | `unisearch_query_duration_seconds` |
| `segments_count` | `unisearch_segments_open` |

### 4. 兼容性扫描

UniSearch 提供 `escompat` 工具扫描 ES 集群的兼容性：

```bash
# 编译工具
go build -o uniadm ./cmd/uniadm

# 扫描 Mapping 兼容性
./uniadm escompat scan-mapping --host http://es-cluster:9200

# 扫描查询兼容性
./uniadm escompat scan-queries --host http://es-cluster:9200 --duration 1h
```

扫描结果会列出不兼容的 mapping 字段和查询模式，指导迁移工作。

## 金标准测试

UniSearch 内置 27 个金标准测试（golden tests），验证 ES 兼容性。详见 [构建与测试](../development/build-and-test.md)：

```bash
# 运行金标准测试
CGO_ENABLED=0 go test ./test/golden/...
```

金标准测试还支持加载 ES 官方 `rest-api-spec` 进行兼容性验证。
