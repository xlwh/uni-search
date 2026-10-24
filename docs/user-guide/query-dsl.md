# Query DSL

## 概述

UniSearch 使用与 Elasticsearch 8.x 兼容的 Query DSL。查询通过 JSON 请求体描述，支持全文检索、词项查询、范围查询、布尔组合、向量搜索和混合检索等查询类型。

## 查询类型总览

| 类别 | 查询类型 | 说明 |
|------|----------|------|
| 全文检索 | match, match_phrase, multi_match, query_string, fuzzy | 文本相关性搜索 |
| 词项查询 | term, terms, range, exists | 精确值匹配 |
| 布尔组合 | bool (must/must_not/should/filter) | 逻辑组合 |
| 向量搜索 | knn, sparse_vector | 向量近邻搜索 |
| 混合检索 | hybrid | 多路召回融合 |
| 全量匹配 | match_all | 匹配所有文档 |

## 全文检索

### match

最基本的全文检索查询，对文本字段进行分词后匹配：

```json
{
  "query": {
    "match": {
      "title": "distributed search engine"
    }
  }
}
```

支持参数：

```json
{
  "query": {
    "match": {
      "title": {
        "query": "distributed search",
        "operator": "and",
        "minimum_should_match": 1,
        "analyzer": "standard"
      }
    }
  }
}
```

| 参数 | 说明 |
|------|------|
| `query` | 查询文本 |
| `operator` | 词项间的逻辑关系（`or` / `and`） |
| `minimum_should_match` | 最少匹配词项数 |
| `analyzer` | 指定分析器 |

### match_phrase

短语查询，匹配连续词项：

```json
{
  "query": {
    "match_phrase": {
      "content": "vector search database"
    }
  }
}
```

支持 `slop` 参数允许词项间距离：

```json
{
  "query": {
    "match_phrase": {
      "content": {
        "query": "vector database",
        "slop": 2
      }
    }
  }
}
```

### multi_match

跨多字段全文检索：

```json
{
  "query": {
    "multi_match": {
      "query": "distributed search",
      "fields": ["title^2", "content", "abstract"]
    }
  }
}
```

字段名后的 `^N` 表示权重提升。

### query_string

支持 Lucene 查询语法的复杂查询：

```json
{
  "query": {
    "query_string": {
      "query": "title:search AND content:vector~2",
      "default_field": "content"
    }
  }
}
```

支持语法：`AND`、`OR`、`NOT`、`*`、`?`、`~`（模糊）、`[ TO ]`（范围）。

### fuzzy

模糊匹配，允许编辑距离：

```json
{
  "query": {
    "fuzzy": {
      "title": {
        "value": "serch",
        "fuzziness": "AUTO"
      }
    }
  }
}
```

`fuzziness` 取值：`AUTO`（自动 0-2）、`0`、`1`、`2`。

## 词项查询

### term

精确值匹配，不分词：

```json
{
  "query": {
    "term": {
      "status": "published"
    }
  }
}
```

### terms

多值匹配（OR 逻辑）：

```json
{
  "query": {
    "terms": {
      "tag": ["database", "search", "vector"]
    }
  }
}
```

### range

范围查询：

```json
{
  "query": {
    "range": {
      "timestamp": {
        "gte": "2026-01-01",
        "lt": "2026-02-01"
      }
    }
  }
}
```

| 参数 | 说明 |
|------|------|
| `gt` | 大于 |
| `gte` | 大于等于 |
| `lt` | 小于 |
| `lte` | 小于等于 |

数值范围示例：

```json
{
  "query": {
    "range": {
      "price": { "gte": 100, "lte": 500 }
    }
  }
}
```

### exists

字段存在查询：

```json
{
  "query": {
    "exists": {
      "field": "embedding"
    }
  }
}
```

## 布尔组合

### bool

```json
{
  "query": {
    "bool": {
      "must": [
        { "match": { "title": "search" } }
      ],
      "filter": [
        { "term": { "status": "published" } }
      ],
      "must_not": [
        { "term": { "deleted": true } }
      ],
      "should": [
        { "match": { "content": "vector" } }
      ],
      "minimum_should_match": 1
    }
  }
}
```

| 子句 | 说明 | 影响评分 |
|------|------|----------|
| `must` | 必须匹配 | 是 |
| `filter` | 必须匹配，结果可缓存 | 否 |
| `must_not` | 必须不匹配 | 否 |
| `should` | 至少匹配 `minimum_should_match` 个 | 是 |

### filter 与 must 的区别

- `filter`：不计算相关性评分，结果可缓存，性能更优
- `must`：计算相关性评分，不缓存
- 建议：精确过滤条件使用 `filter`，全文检索使用 `must`

## 向量搜索

### knn 查询

```json
{
  "query": {
    "knn": {
      "field": "embedding",
      "query_vector": [0.1, 0.2, 0.3, 0.4],
      "k": 10,
      "num_candidates": 100
    }
  }
}
```

| 参数 | 说明 |
|------|------|
| `field` | 向量字段名 |
| `query_vector` | 查询向量 |
| `k` | 返回结果数 |
| `num_candidates` | 搜索候选集大小（影响召回率） |

### 带过滤的 kNN

```json
{
  "query": {
    "knn": {
      "field": "embedding",
      "query_vector": [0.1, 0.2],
      "k": 10,
      "num_candidates": 100,
      "filter": {
        "term": { "category": "article" }
      }
    }
  }
}
```

详见 [向量搜索](vector-search.md)。

## 混合检索

### hybrid 查询

```json
{
  "query": {
    "hybrid": {
      "queries": [
        { "match": { "title": "search engine" } },
        { "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 50 } }
      ],
      "rank": {
        "method": "rrf",
        "rank_const": 60,
        "window_size": 500
      },
      "size": 10
    }
  }
}
```

| 参数 | 说明 |
|------|------|
| `queries` | 多路召回查询数组 |
| `rank.method` | 融合方法：`rrf` / `weighted` / `relative_score` |
| `rank.rank_const` | RRF 常数（默认 60） |
| `rank.weights` | Weighted 融合权重数组 |
| `rank.window_size` | 融合窗口大小（默认 500） |
| `size` | 最终返回结果数 |

详见 [混合检索](hybrid-search.md)。

## 分页

### from + size

```json
{
  "from": 0,
  "size": 10,
  "query": { "match_all": {} }
}
```

限制：`from + size` 不得超过 10000。

### PIT + scroll

深度分页使用 PIT（Point In Time）和 scroll：

```bash
# 1. 创建 PIT
curl -X POST "http://localhost:9200/my-index/_pit?keep_alive=10m"
# 返回 pit_id

# 2. 首次查询
curl -X POST "http://localhost:9200/_search?scroll=1m" -H 'Content-Type: application/json' -d '{
  "size": 100,
  "query": { "match_all": {} },
  "pit": { "id": "<pit_id>", "keep_alive": "10m" }
}'

# 3. 后续查询
curl -X POST "http://localhost:9200/_search?scroll=1m" -H 'Content-Type: application/json' -d '{
  "scroll_id": "<scroll_id>",
  "pit": { "id": "<pit_id>", "keep_alive": "10m" }
}'
```

## 排序

```json
{
  "query": { "match_all": {} },
  "sort": [
    { "timestamp": "desc" },
    { "_score": "desc" }
  ]
}
```

支持多字段排序，按数组顺序优先级递减。

## 结果字段过滤

```json
{
  "query": { "match": { "title": "search" } },
  "_source": ["title", "id", "timestamp"]
}
```

或排除字段：

```json
{
  "_source": { "excludes": ["embedding", "content"] }
}
```

## 高亮

```json
{
  "query": { "match": { "content": "vector search" } },
  "highlight": {
    "fields": {
      "content": {
        "pre_tags": ["<em>"],
        "post_tags": ["</em>"],
        "number_of_fragments": 3,
        "fragment_size": 100
      }
    }
  }
}
```

## 聚合

```json
{
  "query": { "match_all": {} },
  "aggs": {
    "by_category": {
      "terms": { "field": "category", "size": 10 },
      "aggs": {
        "avg_price": { "avg": { "field": "price" } }
      }
    }
  }
}
```

详见 [聚合](aggregations.md)。

## _explain

查看查询匹配详情：

```bash
curl -X POST http://localhost:9200/my-index/_explain/<doc_id> -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "search" } }
}'
```

返回文档匹配与否、匹配词项和相关性评分计算过程。
