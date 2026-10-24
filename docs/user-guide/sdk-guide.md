# Go SDK

## 概述

UniSearch 提供原生 Go SDK（`pkg/unisdk`），封装 HTTP API 调用，提供类型安全的 Go 接口。SDK 支持索引管理、文档 CRUD、搜索和批量操作。

## 安装

```bash
go get github.com/unisearch-db/unisearch/pkg/unisdk
```

## 创建客户端

```go
import "github.com/unisearch-db/unisearch/pkg/unisdk"

client, err := unisdk.NewClient(unisdk.Config{
    Addr: "http://localhost:9200",
})
if err != nil {
    log.Fatal(err)
}
```

### 配置项

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `Addr` | `http://localhost:9200` | 服务地址 |
| `Timeout` | 30s | HTTP 超时 |
| `Username` | — | 用户名（如需认证） |
| `Password` | — | 密码 |

## 索引管理

### 创建索引

```go
err := client.CreateIndex(ctx, "my-index", `{
  "mappings": {
    "properties": {
      "title": { "type": "text" },
      "status": { "type": "keyword" },
      "price": { "type": "float" },
      "embedding": {
        "type": "dense_vector",
        "dimensions": 128,
        "similarity": "cosine"
      }
    }
  },
  "settings": {
    "number_of_shards": 3
  }
}`)
```

### 删除索引

```go
err := client.DeleteIndex(ctx, "my-index")
```

### 刷新索引

```go
err := client.Refresh(ctx, "my-index")
```

## 文档操作

### 索引文档

```go
err := client.Index(ctx, "my-index", "doc-1", `{
  "title": "introduction to vector search",
  "status": "published",
  "price": 99.9,
  "embedding": [0.1, 0.2, 0.3, 0.4]
}`)
```

### 创建文档（不允许覆盖已存在 ID）

```go
err := client.Create(ctx, "my-index", "doc-2", `{...}`)
```

### 获取文档

```go
source, err := client.Get(ctx, "my-index", "doc-1")
// source 为文档的 JSON 字符串
```

### 删除文档

```go
err := client.Delete(ctx, "my-index", "doc-1")
```

## 批量操作

```go
bulkBody := `{"index": {"_index": "my-index", "_id": "doc-1"}}
{"title": "doc 1", "price": 10.0}
{"index": {"_index": "my-index", "_id": "doc-2"}}
{"title": "doc 2", "price": 20.0}
{"delete": {"_index": "my-index", "_id": "doc-3"}}
`

result, err := client.Bulk(ctx, bulkBody)
```

`bulkBody` 为 NDJSON 格式，每行一个操作。

## 搜索

### 基本搜索

```go
result, err := client.Search(ctx, []string{"my-index"}, `{
  "query": {
    "match": { "title": "vector search" }
  },
  "size": 10
}`)
// result 为搜索结果的 JSON 字符串
```

### 向量搜索

```go
result, err := client.Search(ctx, []string{"my-index"}, `{
  "knn": {
    "field": "embedding",
    "query_vector": [0.1, 0.2, 0.3, 0.4],
    "k": 10,
    "num_candidates": 100
  }
}`)
```

### 混合搜索

```go
result, err := client.Search(ctx, []string{"my-index"}, `{
  "query": {
    "hybrid": {
      "queries": [
        { "match": { "title": "vector search" } },
        { "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 50, "num_candidates": 100 } }
      ],
      "rank": { "method": "rrf", "rank_const": 60 },
      "size": 10
    }
  }
}`)
```

### 聚合搜索

```go
result, err := client.Search(ctx, []string{"my-index"}, `{
  "query": { "match_all": {} },
  "size": 0,
  "aggs": {
    "by_category": {
      "terms": { "field": "category", "size": 10 },
      "aggs": {
        "avg_price": { "avg": { "field": "price" } }
      }
    }
  }
}`)
```

## 健康检查

```go
health, err := client.Health(ctx)
// health 为集群健康状态的 JSON 字符串
```

## 完整示例

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/unisearch-db/unisearch/pkg/unisdk"
)

func main() {
    ctx := context.Background()

    // 创建客户端
    client, err := unisdk.NewClient(unisdk.Config{
        Addr: "http://localhost:9200",
    })
    if err != nil {
        log.Fatal(err)
    }

    // 创建索引
    err = client.CreateIndex(ctx, "products", `{
      "mappings": {
        "properties": {
          "title": { "type": "text" },
          "category": { "type": "keyword" },
          "price": { "type": "float" },
          "embedding": {
            "type": "dense_vector",
            "dimensions": 4,
            "similarity": "cosine"
          }
        }
      }
    }`)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("Index created")

    // 批量写入
    bulkBody := `{"index": {"_index": "products", "_id": "1"}}
{"title": "vector database", "category": "database", "price": 99.9, "embedding": [0.1, 0.2, 0.3, 0.4]}
{"index": {"_index": "products", "_id": "2"}}
{"title": "search engine", "category": "search", "price": 49.9, "embedding": [0.5, 0.6, 0.7, 0.8]}
`
    _, err = client.Bulk(ctx, bulkBody)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("Docs indexed")

    // 刷新
    err = client.Refresh(ctx, "products")
    if err != nil {
        log.Fatal(err)
    }

    // 混合搜索
    result, err := client.Search(ctx, []string{"products"}, `{
      "query": {
        "hybrid": {
          "queries": [
            { "match": { "title": "database" } },
            { "knn": { "field": "embedding", "query_vector": [0.1, 0.2, 0.3, 0.4], "k": 5, "num_candidates": 10 } }
          ],
          "rank": { "method": "rrf" },
          "size": 5
        }
      }
    }`)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("Search result:", result)
}
```

## 错误处理

```go
result, err := client.Search(ctx, []string{"my-index"}, query)
if err != nil {
    if errors.Is(err, unisdk.ErrNotFound) {
        // 索引不存在
    } else if errors.Is(err, unisdk.ErrTimeout) {
        // 请求超时
    } else {
        // 其他错误
    }
}
```

## 使用 ES 客户端

由于 UniSearch 与 ES 8.x REST API 兼容，也可以直接使用 ES 官方 Go 客户端：

```go
import "github.com/elastic/go-elasticsearch/v8"

es, err := elasticsearch.NewClient(elasticsearch.Config{
    Addresses: []string{"http://localhost:9200"},
})
```

修改 `Addresses` 指向 UniSearch 地址即可，无需更改现有代码。详见 [ES 兼容性](es-compatibility.md)。
