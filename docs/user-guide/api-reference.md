# API 参考

## 概述

UniSearch 提供 Elasticsearch 8.x 兼容的 REST API，默认监听端口 9200。现有的 ES 客户端（elasticsearch-py、go-elasticsearch、curl 等）可直接连接 UniSearch，无需修改代码。

## API 约定

### 基础 URL

```
http://<host>:9200
```

### Content-Type

- 常规 JSON 请求：`Content-Type: application/json`
- `_bulk` / `_msearch` 批量请求：`Content-Type: application/x-ndjson`

### 请求体大小限制

单个请求体上限 100MB。

### 超时

| 请求类型 | 默认超时 |
|----------|----------|
| 常规请求 | 30s |
| `_bulk` 请求 | 60s |

可通过 `?timeout=` 查询参数覆盖默认超时，例如 `?timeout=10s`。

### 索引名解析

路径中的 `{index}` 参数支持以下格式：

- 单个索引名：`my-index`
- 逗号分隔：`index1,index2`
- 通配符：`log-*`（支持 `*` 和 `?`）
- 全部索引：`_all`
- 花括号展开：`logs-{a,b}-*` 展开为 `logs-a-*` 和 `logs-b-*`

### 错误格式

错误响应使用 ES 兼容的 APIError 结构：

```json
{
  "error": {
    "type": "error_type",
    "reason": "错误描述",
    "index": "索引名"
  },
  "status": 400
}
```

常见错误类型包括 `unsupported_capability`（不支持的 API）、`parse_exception`（DSL 解析错误）、`mapper_parsing_exception`（mapping 错误）等。

### 弃用警告

对已弃用的功能（如 `doc_values: false`、`norms`、`scroll`），响应头会添加 ES 风格的 Warning 标记：

```
Warning: 299 unisearch-8.12 "doc_values is always enabled; the false value is deprecated"
```

## 集群 API

### GET /

版本探测，返回集群和版本信息。

```bash
curl http://localhost:9200/
```

响应示例：

```json
{
  "name": "node-1",
  "cluster_name": "unisearch",
  "cluster_uuid": "unisearch-uuid",
  "version": {
    "number": "8.12.0",
    "build_flavor": "default",
    "distribution": "unisearch",
    "lucene_version": "none"
  },
  "tagline": "You Know, for Search"
}
```

### GET /_cluster/health

返回集群健康状态。

```bash
curl http://localhost:9200/_cluster/health
```

响应示例：

```json
{
  "status": "green",
  "cluster_name": "unisearch",
  "number_of_nodes": 1,
  "active_primary_shards": 0,
  "active_shards": 0
}
```

支持的查询参数：

| 参数 | 说明 |
|------|------|
| `wait_for_status` | 阻塞等待指定状态（green/yellow/red） |
| `timeout` | 等待超时时间 |

## _cat API

_cat 端点以紧凑格式返回集群信息，适合命令行查看。

### GET /_cat/health

集群健康（文本格式）。

```bash
# 文本格式（默认）
curl http://localhost:9200/_cat/health

# 带列名
curl http://localhost:9200/_cat/health?v

# JSON 格式
curl http://localhost:9200/_cat/health?format=json
```

### GET /_cat/indices

索引列表。

```bash
# 所有索引
curl http://localhost:9200/_cat/indices

# 指定索引
curl http://localhost:9200/_cat/indices/my-index

# JSON 格式
curl http://localhost:9200/_cat/indices?format=json
```

### GET /_cat/shards

分片信息。

```bash
curl http://localhost:9200/_cat/shards
```

### GET /_cat/nodes

节点信息。

```bash
curl http://localhost:9200/_cat/nodes
```

## 索引管理 API

### PUT /{index}

创建索引，可在请求体中指定 mapping 和 settings。

```bash
curl -X PUT http://localhost:9200/my-index -H 'Content-Type: application/json' -d '{
  "mappings": {
    "properties": {
      "title": { "type": "text" },
      "category": { "type": "keyword" },
      "price": { "type": "float" },
      "embedding": {
        "type": "dense_vector",
        "dimensions": 768,
        "similarity": "cosine"
      }
    }
  },
  "settings": {
    "number_of_shards": 1,
    "number_of_replicas": 0
  }
}'
```

### DELETE /{index}

删除索引及其全部数据。

```bash
curl -X DELETE http://localhost:9200/my-index
```

### GET /{index}/_mapping

获取索引 mapping。

```bash
curl http://localhost:9200/my-index/_mapping
```

### PUT /{index}/_mapping

更新索引 mapping（仅可添加新字段，不可修改已有字段类型）。

```bash
curl -X PUT http://localhost:9200/my-index/_mapping -H 'Content-Type: application/json' -d '{
  "properties": {
    "tags": { "type": "keyword" }
  }
}'
```

### PUT /_mapping

全局 mapping 更新，应用于所有索引。

### GET /{index}/_settings

获取索引 settings。

```bash
curl http://localhost:9200/my-index/_settings
```

### PUT /{index}/_settings

更新索引 settings。

```bash
curl -X PUT http://localhost:9200/my-index/_settings -H 'Content-Type: application/json' -d '{
  "refresh_interval": "5s"
}'
```

## 文档 API

### POST /{index}/_doc

索引文档，自动生成 ID。

```bash
curl -X POST http://localhost:9200/my-index/_doc -H 'Content-Type: application/json' -d '{
  "title": "文档标题",
  "content": "文档内容"
}'
```

响应示例：

```json
{
  "_index": "my-index",
  "_id": "auto-generated-id",
  "_version": 1,
  "result": "created",
  "_seq_no": 0
}
```

### PUT /{index}/_doc/{id}

索引文档，指定 ID。若文档已存在则覆盖。

```bash
curl -X PUT http://localhost:9200/my-index/_doc/1 -H 'Content-Type: application/json' -d '{
  "title": "文档标题",
  "content": "文档内容"
}'
```

POST 方法同样支持指定 ID：`POST /{index}/_doc/{id}`。

### POST /{index}/_create/{id}

创建文档。若指定 ID 已存在则返回 409 冲突。

```bash
curl -X POST http://localhost:9200/my-index/_create/1 -H 'Content-Type: application/json' -d '{
  "title": "仅创建新文档"
}'
```

### GET /{index}/_doc/{id}

获取文档。

```bash
curl http://localhost:9200/my-index/_doc/1
```

响应示例：

```json
{
  "_index": "my-index",
  "_id": "1",
  "found": true,
  "_source": {
    "title": "文档标题",
    "content": "文档内容"
  }
}
```

文档不存在时 `found` 为 `false`。

### DELETE /{index}/_doc/{id}

删除文档。

```bash
curl -X DELETE http://localhost:9200/my-index/_doc/1
```

响应示例：

```json
{
  "_index": "my-index",
  "_id": "1",
  "result": "deleted",
  "_seq_no": 1
}
```

## 批量 API

### POST /_bulk

批量操作，请求体为 NDJSON 格式（每行一个 JSON 对象）。

```bash
curl -X POST http://localhost:9200/_bulk -H 'Content-Type: application/x-ndjson' -d '
{"index": {"_index": "my-index", "_id": "1"}}
{"title": "文档1", "content": "内容1"}
{"index": {"_index": "my-index", "_id": "2"}}
{"title": "文档2", "content": "内容2"}
{"delete": {"_index": "my-index", "_id": "3"}}
'
```

支持的批量操作类型：

| 操作 | 说明 |
|------|------|
| `index` | 索引文档（存在则覆盖） |
| `create` | 创建文档（存在则冲突） |
| `update` | 更新文档 |
| `delete` | 删除文档 |

也可指定索引：`POST /{index}/_bulk`。

### POST /_mget

批量获取文档。

```bash
curl -X POST http://localhost:9200/_mget -H 'Content-Type: application/json' -d '{
  "docs": [
    { "_index": "my-index", "_id": "1" },
    { "_index": "my-index", "_id": "2" }
  ]
}'
```

也可指定索引：`POST /{index}/_mget`。

## 搜索 API

### POST /{index}/_search

搜索指定索引。GET 方法同样支持。

```bash
curl -X POST http://localhost:9200/my-index/_search -H 'Content-Type: application/json' -d '{
  "query": {
    "match": {
      "title": "搜索关键词"
    }
  },
  "size": 10,
  "from": 0,
  "sort": [{ "_score": "desc" }]
}'
```

全局搜索（跨所有索引）：

```bash
curl -X POST http://localhost:9200/_search -H 'Content-Type: application/json' -d '{
  "query": { "match_all": {} }
}'
```

响应示例：

```json
{
  "took": 5,
  "timed_out": false,
  "_shards": {
    "total": 1,
    "successful": 1,
    "skipped": 0,
    "failed": 0
  },
  "hits": {
    "total": {
      "value": 2,
      "relation": "eq"
    },
    "hits": [
      {
        "_index": "my-index",
        "_id": "1",
        "_score": 0.575,
        "_source": {
          "title": "文档1"
        }
      }
    ]
  }
}
```

请求体支持的字段：

| 字段 | 说明 |
|------|------|
| `query` | 查询 DSL（见 [查询 DSL](query-dsl.md)） |
| `size` | 返回文档数（默认 10，from+size 上限 10000） |
| `from` | 偏移量 |
| `sort` | 排序规则 |
| `aggs` | 聚合定义 |
| `highlight` | 高亮配置 |
| `_source` | 字段过滤 |
| `track_total_hits` | 总命中数追踪 |
| `terminate_after` | 提前终止 |
| `timeout` | 查询超时 |

### POST /{index}/_knn_search

KNN 向量搜索。

```bash
curl -X POST http://localhost:9200/my-index/_knn_search -H 'Content-Type: application/json' -d '{
  "knn": {
    "field": "embedding",
    "vector": [0.1, 0.2, 0.3, ...],
    "k": 10,
    "num_candidates": 100
  }
}'
```

### POST /_msearch

多搜索请求，请求体为 NDJSON 格式。每对行包含一个搜索头和一个搜索体。

```bash
curl -X POST http://localhost:9200/_msearch -H 'Content-Type: application/x-ndjson' -d '
{"index": "my-index"}
{"query": {"match": {"title": "关键词1"}}}
{"index": "my-index"}
{"query": {"match": {"title": "关键词2"}}}
'
```

也可指定索引：`POST /{index}/_msearch`。

### POST /{index}/_count

文档计数。GET 方法同样支持。

```bash
curl -X POST http://localhost:9200/my-index/_count -H 'Content-Type: application/json' -d '{
  "query": { "match": { "category": "electronics" } }
}'
```

响应示例：

```json
{
  "count": 42
}
```

### GET /{index}/_explain/{id}

查询解释，展示指定文档如何匹配查询。

```bash
curl -X GET "http://localhost:9200/my-index/_explain/1" -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "关键词" } }
}'
```

## 查询操作 API

### POST /{index}/_delete_by_query

按查询条件删除文档。

```bash
curl -X POST http://localhost:9200/my-index/_delete_by_query -H 'Content-Type: application/json' -d '{
  "query": { "range": { "created_at": { "lt": "2024-01-01" } } }
}'
```

### POST /{index}/_update_by_query

按查询条件更新文档。

```bash
curl -X POST http://localhost:9200/my-index/_update_by_query -H 'Content-Type: application/json' -d '{
  "query": { "term": { "status": "draft" } },
  "script": { "source": "ctx._source.status = 'published'" }
}'
```

### POST /_reindex

将文档从一个索引重建到另一个索引。

```bash
curl -X POST http://localhost:9200/_reindex -H 'Content-Type: application/json' -d '{
  "source": { "index": "old-index" },
  "dest": { "index": "new-index" }
}'
```

## 分页 API

### POST /{index}/_pit

创建 Point In Time（PIT），用于一致性读和深分页。

```bash
curl -X POST http://localhost:9200/my-index/_pit
```

响应示例：

```json
{
  "id": "pit-id-string"
}
```

在搜索请求中使用 PIT：

```bash
curl -X POST http://localhost:9200/_search -H 'Content-Type: application/json' -d '{
  "pit": { "id": "pit-id-string", "keep_alive": "5m" },
  "query": { "match_all": {} },
  "size": 10,
  "sort": [{ "_shard_doc": "asc" }]
}'
```

### DELETE /pit

删除 PIT 上下文。

```bash
curl -X DELETE http://localhost:9200/pit -H 'Content-Type: application/json' -d '{
  "id": "pit-id-string"
}'
```

### POST /_search/scroll

滚动搜索，用于大批量数据导出。UniSearch 内部将 scroll 转译为 PIT 实现。

```bash
# 初始搜索，获取 scroll_id
curl -X POST http://localhost:9200/my-index/_search -H 'Content-Type: application/json' -d '{
  "scroll": "5m",
  "size": 100,
  "query": { "match_all": {} }
}'
```

后续请求使用 scroll_id：

```bash
curl -X POST http://localhost:9200/_search/scroll -H 'Content-Type: application/json' -d '{
  "scroll": "5m",
  "scroll_id": "scroll-id-string"
}'
```

### DELETE /_search/scroll

删除滚动上下文。

```bash
curl -X DELETE http://localhost:9200/_search/scroll -H 'Content-Type: application/json' -d '{
  "scroll_id": "scroll-id-string"
}'
```

## 别名 API

### PUT /{index}/_alias/{name}

为索引添加别名。

```bash
curl -X PUT http://localhost:9200/my-index/_alias/my-alias
```

也可使用 `PUT /{index}/_alias` 不指定别名名称。

### GET /{index}/_alias

获取索引的别名。

```bash
curl http://localhost:9200/my-index/_alias
```

### GET /_alias

获取所有别名。

```bash
curl http://localhost:9200/_alias
```

### POST /_aliases

原子别名操作，支持 add 和 remove。

```bash
curl -X POST http://localhost:9200/_aliases -H 'Content-Type: application/json' -d '{
  "actions": [
    { "add": { "index": "my-index-v1", "alias": "my-alias" } },
    { "remove": { "index": "my-index-v2", "alias": "my-alias" } }
  ]
}'
```

## 模板与 ILM API

### PUT /_index_template

创建索引模板，匹配模式的新索引自动应用模板配置。

```bash
curl -X PUT http://localhost:9200/_index_template -H 'Content-Type: application/json' -d '{
  "index_patterns": ["logs-*"],
  "template": {
    "mappings": {
      "properties": {
        "timestamp": { "type": "date" },
        "level": { "type": "keyword" },
        "message": { "type": "text" }
      }
    }
  }
}'
```

### GET /_index_template

获取索引模板。

### PUT /_ilm/policy

创建 Index Lifecycle Management 策略。

```bash
curl -X PUT http://localhost:9200/_ilm/policy -H 'Content-Type: application/json' -d '{
  "policy": {
    "phases": {
      "hot": { "actions": {} },
      "delete": {
        "actions": { "delete": {} }
      }
    }
  }
}'
```

### GET /_ilm/policy

获取 ILM 策略。

## 分析与元数据 API

### POST /{index}/_analyze

文本分析，展示分词结果。

```bash
curl -X POST http://localhost:9200/my-index/_analyze -H 'Content-Type: application/json' -d '{
  "analyzer": "standard",
  "text": "Hello World, UniSearch!"
}'
```

响应示例：

```json
{
  "tokens": [
    { "token": "hello", "start_offset": 0, "end_offset": 5, "type": "<ALPHANUM>", "position": 0 },
    { "token": "world", "start_offset": 6, "end_offset": 11, "type": "<ALPHANUM>", "position": 1 },
    { "token": "unisearch", "start_offset": 14, "end_offset": 23, "type": "<ALPHANUM>", "position": 2 }
  ]
}
```

也可不指定索引使用全局分析：`POST /_analyze`。

### GET /{index}/_field_caps

字段能力查询，返回索引中各字段支持的查询和聚合能力。

```bash
curl http://localhost:9200/my-index/_field_caps
```

也可查询全部索引：`GET /_field_caps`。

### GET /{index}/_terms_enum

词项枚举，返回指定字段的词项列表。

```bash
curl "http://localhost:9200/my-index/_terms_enum?field=category"
```

## 管理操作 API

### POST /{index}/_refresh

刷新索引，使最近写入的文档可被搜索到。

```bash
curl -X POST http://localhost:9200/my-index/_refresh
```

刷新全部索引：

```bash
curl -X POST http://localhost:9200/_refresh
```

### POST /{index}/_flush

刷盘，将 MemTable 数据持久化到段文件。

```bash
curl -X POST http://localhost:9200/my-index/_flush
```

全局刷盘：

```bash
curl -X POST http://localhost:9200/_flush
```

### POST /{index}/_forcemerge

强制合并段文件，减少段数量。

```bash
curl -X POST "http://localhost:9200/my-index/_forcemerge?max_num_segments=1"
```

全局强制合并：

```bash
curl -X POST "http://localhost:9200/_forcemerge?max_num_segments=1"
```

## 监控端点

### GET /_prometheus/metrics

Prometheus 格式指标暴露端点。

```bash
curl http://localhost:9200/_prometheus/metrics
```

该端点直接暴露 Prometheus 指标，不经过中间件包装以避免自引用。指标分为写入、查询、段、索引和租户配额五组。详见 [Prometheus 监控](../operations/monitoring.md)。

## 响应格式约定

### 搜索响应

```json
{
  "took": 5,
  "timed_out": false,
  "_shards": {
    "total": 1,
    "successful": 1,
    "skipped": 0,
    "failed": 0
  },
  "hits": {
    "total": {
      "value": 100,
      "relation": "eq"
    },
    "max_score": 1.5,
    "hits": [
      {
        "_index": "my-index",
        "_id": "1",
        "_score": 1.5,
        "_source": { "field": "value" }
      }
    ]
  },
  "aggregations": {
    "my_agg": { "value": 42.0 }
  }
}
```

### 写入响应

```json
{
  "_index": "my-index",
  "_id": "1",
  "_version": 1,
  "result": "created",
  "_seq_no": 0
}
```

`result` 字段取值：

| 值 | 说明 |
|------|------|
| `created` | 文档已创建 |
| `updated` | 文档已更新 |
| `deleted` | 文档已删除 |
| `not_found` | 文档未找到 |
| `conflict` | 版本冲突（409） |

### 错误响应

```json
{
  "error": {
    "type": "mapper_parsing_exception",
    "reason": "failed to parse field [price] of type [float]",
    "index": "my-index"
  },
  "status": 400
}
```

### _bulk 响应

```json
{
  "took": 10,
  "errors": false,
  "items": [
    {
      "index": {
        "_index": "my-index",
        "_id": "1",
        "result": "created",
        "_seq_no": 0
      }
    }
  ]
}
```

`errors` 为 `true` 时，检查 `items` 数组中各条目的 `error` 字段定位失败项。
