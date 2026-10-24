# 快速开始

## 概述

本指南帮助你在 5 分钟内完成 UniSearch 的安装、启动和基本操作。UniSearch 提供 Elasticsearch 8.x 兼容的 REST API，你可以使用 curl 或任何 ES 客户端连接。

## 安装与启动

### 方式一：Docker（推荐）

```bash
# 构建镜像（需先编译 Linux 二进制）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o unisearch-linux ./cmd/unisearch
docker build -t unisearch:latest .

# 启动容器
docker run -d --name unisearch -p 9200:9200 -p 9300:9300 -v unisearch-data:/data unisearch:latest
```

详见 [Docker 单节点部署](../deployment/docker-single-node.md)。

### 方式二：源码编译

```bash
# 编译
make build

# 启动
./unisearch --embedded --data-dir ./data --listen :9200
```

详见 [开发入门](../development/getting-started.md)。

### 方式三：Docker Compose

```bash
# 启动完整集群（3 meta + 2 shard + MinIO + Jaeger）
docker-compose up -d
```

详见 [Docker Compose 集群部署](../deployment/docker-compose-cluster.md)。

## 验证服务

```bash
# 版本探测
curl http://localhost:9200/

# 集群健康
curl http://localhost:9200/_cluster/health
# 预期: {"status":"green",...}
```

## 创建索引

```bash
curl -X PUT http://localhost:9200/articles -H 'Content-Type: application/json' -d '{
  "mappings": {
    "properties": {
      "title": { "type": "text" },
      "content": { "type": "text" },
      "category": { "type": "keyword" },
      "price": { "type": "float" },
      "embedding": {
        "type": "dense_vector",
        "dimensions": 128,
        "similarity": "cosine"
      }
    }
  }
}'
```

## 写入文档

### 单文档索引

```bash
curl -X PUT http://localhost:9200/articles/_doc/1 -H 'Content-Type: application/json' -d '{
  "title": "向量搜索入门",
  "content": "本文介绍向量搜索的基本概念",
  "category": "技术",
  "price": 29.9,
  "embedding": [0.1, 0.2, 0.3, 0.4, 0.5]
}'
```

### 批量写入

```bash
curl -X POST http://localhost:9200/_bulk -H 'Content-Type: application/x-ndjson' -d '
{"index": {"_index": "articles", "_id": "2"}}
{"title": "全文检索指南", "content": "BM25 评分算法详解", "category": "技术", "price": 19.9}
{"index": {"_index": "articles", "_id": "3"}}
{"title": "混合检索实践", "content": "融合全文与向量搜索", "category": "技术", "price": 39.9}
'
```

## 搜索文档

### 全文检索

```bash
curl -X POST http://localhost:9200/articles/_search -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "向量搜索" } },
  "size": 10
}'
```

### 向量搜索

```bash
curl -X POST http://localhost:9200/articles/_knn_search -H 'Content-Type: application/json' -d '{
  "knn": {
    "field": "embedding",
    "vector": [0.1, 0.2, 0.3, 0.4, 0.5],
    "k": 5,
    "num_candidates": 50
  }
}'
```

### 混合检索

```bash
curl -X POST http://localhost:9200/articles/_search -H 'Content-Type: application/json' -d '{
  "query": {
    "hybrid": {
      "queries": [
        { "match": { "title": "搜索" } },
        { "knn": { "field": "embedding", "vector": [0.1, 0.2, 0.3, 0.4, 0.5], "k": 10, "num_candidates": 50 } }
      ],
      "rank": { "method": "rrf", "rank_constant": 60, "window_size": 500 }
    }
  }
}'
```

### 过滤搜索

```bash
curl -X POST http://localhost:9200/articles/_search -H 'Content-Type: application/json' -d '{
  "query": {
    "bool": {
      "must": [{ "match": { "content": "技术" } }],
      "filter": [{ "range": { "price": { "lte": 30 } } }]
    }
  }
}'
```

## 聚合查询

### terms 聚合

```bash
curl -X POST http://localhost:9200/articles/_search -H 'Content-Type: application/json' -d '{
  "size": 0,
  "aggs": {
    "categories": { "terms": { "field": "category", "size": 10 } }
  }
}'
```

### stats 聚合

```bash
curl -X POST http://localhost:9200/articles/_search -H 'Content-Type: application/json' -d '{
  "size": 0,
  "aggs": {
    "price_stats": { "stats": { "field": "price" } }
  }
}'
```

## 索引管理

```bash
# 获取 mapping
curl http://localhost:9200/articles/_mapping

# 刷新索引（使新写入文档可搜索）
curl -X POST http://localhost:9200/articles/_refresh

# 刷盘
curl -X POST http://localhost:9200/articles/_flush

# 强制合并
curl -X POST "http://localhost:9200/articles/_forcemerge?max_num_segments=1"

# 删除索引
curl -X DELETE http://localhost:9200/articles
```

## _cat 端点

```bash
# 索引列表
curl http://localhost:9200/_cat/indices?v

# 集群健康
curl http://localhost:9200/_cat/health?v

# 节点信息
curl http://localhost:9200/_cat/nodes?v

# 分片信息
curl http://localhost:9200/_cat/shards?v
```

## 使用 Go SDK

```go
package main

import (
    "context"
    "fmt"
    "github.com/unisearch-db/unisearch/pkg/unisdk"
)

func main() {
    client := unisdk.NewClient("http://localhost:9200")

    // 创建索引
    client.CreateIndex(context.Background(), "my-index", []byte(`{
        "mappings": {"properties": {"title": {"type": "text"}}}
    }`))

    // 写入文档
    result, _ := client.Index(context.Background(), "my-index", "1", map[string]any{
        "title": "Hello UniSearch",
    })
    fmt.Printf("Indexed: %s\n", result.ID)

    // 搜索
    searchResult, _ := client.Search(context.Background(), "my-index", []byte(`{
        "query": {"match": {"title": "Hello"}}
    }`))
    fmt.Printf("Found: %d hits\n", searchResult.Hits.Total.Value)
}
```

详见 [Go SDK 指南](sdk-guide.md)。

## 下一步

- [配置参考](configuration-reference.md) — 了解全部配置项和环境变量
- [API 参考](api-reference.md) — 完整 REST API 端点文档
- [查询 DSL](query-dsl.md) — 查询语法详解
- [向量搜索](vector-search.md) — KNN 向量检索指南
- [混合检索](hybrid-search.md) — 全文+向量融合查询
- [ES 兼容性](es-compatibility.md) — 兼容矩阵与迁移指南
