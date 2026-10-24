# Go SDK

## 概述

UniSearch 提供原生 Go SDK（`pkg/unisdk`），封装 HTTP API 调用。当前版本覆盖索引创建、文档 CRUD、批量写入、搜索和健康检查的常用路径。

> 当前限制：SDK 尚未内置认证与哨兵错误（`ErrNotFound` / `ErrTimeout`），错误以普通 `error` 返回；`Index` / `Get` / `Search` 等文档操作不区分 HTTP 状态码，解析失败时返回 JSON 解码错误。如需精细错误语义，可直接使用 HTTP 客户端或 ES 官方客户端（见文末）。

## 安装

```bash
go get github.com/unisearch-db/unisearch/pkg/unisdk
```

## 创建客户端

```go
import "github.com/unisearch-db/unisearch/pkg/unisdk"

client := unisdk.NewClient("http://localhost:9200")
```

`NewClient` 接收服务地址（必填），内置 30 秒 HTTP 超时，无错误返回。

## 索引管理

### 创建索引

`CreateIndex` 的 body 为可选的 mapping/settings JSON（`[]byte`）；传 `nil` 创建空索引。

```go
mapping := []byte(`{
  "mappings": {
    "properties": {
      "title": { "type": "text" },
      "status": { "type": "keyword" },
      "price": { "type": "double" },
      "embedding": {
        "type": "dense_vector",
        "dimensions": 128,
        "similarity": "cosine"
      }
    }
  }
}`)
err := client.CreateIndex(ctx, "my-index", mapping)
```

### 刷新索引

```go
err := client.Refresh(ctx, "my-index")
```

## 文档操作

### 索引文档（PUT /{index}/_doc/{id}）

`doc` 为任意可 JSON 序列化的值（结构体 / map / 切片字面量均可）。

```go
result, err := client.Index(ctx, "my-index", "doc-1", map[string]any{
    "title":  "introduction to vector search",
    "status": "published",
    "price":  99.9,
    "embedding": []float64{0.1, 0.2, 0.3, 0.4},
})
// result 是 *unisdk.IndexResult：_id / _index / _version / result / _seq_no
```

### 获取文档

```go
result, err := client.Get(ctx, "my-index", "doc-1")
if err == nil && result.Found {
    fmt.Println(string(result.Source)) // _source 原始 JSON
}
```

### 删除文档

```go
err := client.Delete(ctx, "my-index", "doc-1")
```

## 批量操作

`Bulk` 接收 NDJSON 格式的 `[]byte`，每行一个操作：

```go
bulkBody := []byte(`{"index": {"_index": "my-index", "_id": "doc-1"}}
{"title": "doc 1", "price": 10.0}
{"index": {"_index": "my-index", "_id": "doc-2"}}
{"title": "doc 2", "price": 20.0}
{"delete": {"_index": "my-index", "_id": "doc-3"}}
`)

result, err := client.Bulk(ctx, bulkBody)
// result.Errors 为 true 时表示部分操作失败，逐项检查 result.Items
```

## 搜索

`Search` 接收单个索引名和搜索 body（`[]byte`），返回类型化的 `*unisdk.SearchResult`（`Took` / `Hits.Total` / `Hits.Hits`，每个 hit 为原始 JSON，可自行反序列化目标结构）。

### 基本搜索

```go
result, err := client.Search(ctx, "my-index", []byte(`{
  "query": { "match": { "title": "vector search" } },
  "size": 10
}`))
if err == nil {
    fmt.Println("total:", result.Hits.Total.Value)
    for _, hit := range result.Hits.Hits {
        fmt.Println(string(hit))
    }
}
```

### 向量搜索

```go
result, err := client.Search(ctx, "my-index", []byte(`{
  "knn": {
    "field": "embedding",
    "query_vector": [0.1, 0.2, 0.3, 0.4],
    "k": 10,
    "num_candidates": 100
  }
}`))
```

### 聚合搜索

```go
result, err := client.Search(ctx, "my-index", []byte(`{
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
}`))
```

> 多索引搜索：SDK 的 `Search` 当前仅接收单个索引名。需要跨索引时，可使用逗号分隔的索引模式（服务端支持 `POST /{index-a,index-b}/_search`），或直接发 HTTP 请求。

## 健康检查

```go
health, err := client.Health(ctx)
// health 为 map[string]any：cluster_name / status / number_of_nodes 等
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
    client := unisdk.NewClient("http://localhost:9200")

    // 创建索引
    err := client.CreateIndex(ctx, "products", []byte(`{
      "mappings": {
        "properties": {
          "title":    { "type": "text" },
          "category": { "type": "keyword" },
          "price":    { "type": "double" },
          "embedding": {
            "type": "dense_vector", "dimensions": 4, "similarity": "cosine"
          }
        }
      }
    }`))
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("Index created")

    // 批量写入
    bulkBody := []byte(`{"index": {"_index": "products", "_id": "1"}}
{"title": "vector database", "category": "database", "price": 99.9, "embedding": [0.1, 0.2, 0.3, 0.4]}
{"index": {"_index": "products", "_id": "2"}}
{"title": "search engine", "category": "search", "price": 49.9, "embedding": [0.5, 0.6, 0.7, 0.8]}
`)
    if _, err := client.Bulk(ctx, bulkBody); err != nil {
        log.Fatal(err)
    }
    fmt.Println("Docs indexed")

    // 刷新
    if err := client.Refresh(ctx, "products"); err != nil {
        log.Fatal(err)
    }

    // 搜索
    result, err := client.Search(ctx, "products", []byte(`{
      "query": { "match": { "title": "database" } }
    }`))
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("matched:", result.Hits.Total.Value)
    for _, hit := range result.Hits.Hits {
        fmt.Println(string(hit))
    }
}
```

## 使用 ES 客户端

由于 UniSearch 与 ES 8.x REST API 高度兼容，也可以直接使用 ES 官方 Go 客户端：

```go
import "github.com/elastic/go-elasticsearch/v8"

es, err := elasticsearch.NewClient(elasticsearch.Config{
    Addresses: []string{"http://localhost:9200"},
})
```

修改 `Addresses` 指向 UniSearch 地址即可。兼容范围与差异详见 [ES 兼容性](es-compatibility.md)。
