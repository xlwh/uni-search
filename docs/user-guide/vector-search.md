# 向量搜索

## 概述

UniSearch 内置 HNSW（Hierarchical Navigable Small World）向量索引引擎，支持高维稠密向量的近似最近邻搜索（ANN）。HNSW 引擎使用 CSR 邻接表存储图结构，支持 SQ8/RaBitQ 量化和 SIMD 加速距离计算。

## 创建向量索引

```bash
curl -X PUT http://localhost:9200/vector-index -H 'Content-Type: application/json' -d '{
  "mappings": {
    "properties": {
      "id": { "type": "keyword" },
      "title": { "type": "text" },
      "category": { "type": "keyword" },
      "embedding": {
        "type": "dense_vector",
        "dimensions": 128,
        "similarity": "cosine",
        "index": true,
        "index_options": {
          "type": "hnsw",
          "m": 16,
          "ef_construction": 200,
          "quantization": "sq8"
        }
      }
    }
  }
}'
```

### 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `dimensions` | 必填 | 向量维度（1-4096） |
| `similarity` | `cosine` | 相似度函数 |
| `index` | `true` | 是否构建 HNSW 索引 |
| `index_options.type` | `hnsw` | 索引类型 |
| `index_options.m` | 16 | 图的连通度 |
| `index_options.ef_construction` | 200 | 构建时搜索宽度 |
| `index_options.quantization` | `sq8` | 量化方式 |

## 相似度函数

| 函数 | 公式 | 适用场景 |
|------|------|----------|
| `cosine` | 1 - (a·b)/(\|a\|\|b\|) | 文本语义搜索（归一化向量） |
| `l2` | \sqrt(\sum(a_i - b_i)^2) | 图像特征 |
| `dot_product` | -a·b | 已归一化向量的最大内积搜索 |

## 量化方式

| 量化 | 压缩率 | 召回率影响 | 内存占用 | 适用场景 |
|------|--------|-----------|----------|----------|
| `full` | 1x | 无 | 高 | 召回率优先 |
| `sq8` | 4x | < 2% | 中 | 均衡（默认） |
| `rabitq` | 32x | < 5% | 低 | 大规模数据集 |

量化在索引时生效，查询时自动使用重排序（reranking）补偿精度损失。

## 写入向量数据

### 单条写入

```bash
curl -X POST http://localhost:9200/vector-index/_doc -H 'Content-Type: application/json' -d '{
  "id": "doc-1",
  "title": "introduction to vector search",
  "category": "tutorial",
  "embedding": [0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8,
                 0.9, 1.0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6,
                 0.7, 0.8, 0.9, 1.0, 0.1, 0.2, 0.3, 0.4,
                 0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 0.1, 0.2,
                 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
                 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8,
                 0.9, 1.0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6,
                 0.7, 0.8, 0.9, 1.0, 0.1, 0.2, 0.3, 0.4,
                 0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 0.1, 0.2,
                 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
                 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8,
                 0.9, 1.0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6,
                 0.7, 0.8, 0.9, 1.0, 0.1, 0.2, 0.3, 0.4,
                 0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 0.1, 0.2,
                 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
                 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8]
}'
```

### 批量写入

```bash
curl -X POST http://localhost:9200/vector-index/_bulk -H 'Content-Type: application/x-ndjson' -d '
{"index": {"_id": "doc-1"}}
{"id": "doc-1", "embedding": [0.1, 0.2, ...]}
{"index": {"_id": "doc-2"}}
{"id": "doc-2", "embedding": [0.3, 0.4, ...]}
'
```

## kNN 搜索

### 基本搜索

```bash
curl -X POST http://localhost:9200/vector-index/_search -H 'Content-Type: application/json' -d '{
  "knn": {
    "field": "embedding",
    "query_vector": [0.15, 0.25, 0.35, ...],
    "k": 10,
    "num_candidates": 100
  }
}'
```

| 参数 | 说明 |
|------|------|
| `field` | 向量字段名 |
| `query_vector` | 查询向量 |
| `k` | 返回结果数 |
| `num_candidates` | 搜索候选集大小 |

### _knn_search 端点

```bash
curl -X POST http://localhost:9200/vector-index/_knn_search -H 'Content-Type: application/json' -d '{
  "knn": {
    "field": "embedding",
    "query_vector": [0.15, 0.25, 0.35, ...],
    "k": 10,
    "num_candidates": 100
  },
  "_source": ["id", "title"]
}'
```

### num_candidates 调优

| num_candidates | 召回率 | 延迟 | 适用场景 |
|----------------|--------|------|----------|
| 50 | 较低 | 低 | 快速粗排 |
| 100 | 中等 | 中 | 默认 |
| 500 | 高 | 高 | 精确搜索 |
| 1000 | 最高 | 最高 | 召回率优先 |

建议：`num_candidates` 通常设置为 `k` 的 10 倍。

## 带过滤的向量搜索

### 过滤前置

```bash
curl -X POST http://localhost:9200/vector-index/_search -H 'Content-Type: application/json' -d '{
  "knn": {
    "field": "embedding",
    "query_vector": [0.15, 0.25, 0.35, ...],
    "k": 10,
    "num_candidates": 100,
    "filter": {
      "term": { "category": "tutorial" }
    }
  }
}'
```

`filter` 在 HNSW 搜索过程中应用，先搜索 `num_candidates` 个候选，再过滤，最后返回 `k` 个结果。

### 复合过滤

```bash
curl -X POST http://localhost:9200/vector-index/_search -H 'Content-Type: application/json' -d '{
  "knn": {
    "field": "embedding",
    "query_vector": [0.15, 0.25, 0.35, ...],
    "k": 10,
    "num_candidates": 200,
    "filter": {
      "bool": {
        "must": [
          { "term": { "category": "tutorial" } },
          { "range": { "timestamp": { "gte": "2026-01-01" } } }
        ]
      }
    }
  }
}'
```

## 向量搜索 + 全文检索组合

### 布尔组合

```bash
curl -X POST http://localhost:9200/vector-index/_search -H 'Content-Type: application/json' -d '{
  "query": {
    "bool": {
      "must": [
        { "match": { "title": "vector search" } }
      ],
      "filter": [
        { "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 10, "num_candidates": 100 } }
      ]
    }
  }
}'
```

### 混合检索（推荐）

使用 `hybrid` 查询进行融合排序：

```bash
curl -X POST http://localhost:9200/vector-index/_search -H 'Content-Type: application/json' -d '{
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
}'
```

详见 [混合检索](hybrid-search.md)。

## 响应格式

```json
{
  "took": 5,
  "hits": {
    "total": { "value": 10, "relation": "eq" },
    "max_score": 0.98,
    "hits": [
      {
        "_index": "vector-index",
        "_id": "doc-1",
        "_score": 0.98,
        "_source": {
          "id": "doc-1",
          "title": "introduction to vector search"
        }
      }
    ]
  }
}
```

`_score` 为相似度分数（cosine similarity 范围 0-1，越高越相似）。

## 性能优化

### 索引构建优化

| 场景 | m | ef_construction | 量化 | 说明 |
|------|---|-----------------|------|------|
| 高召回率 | 32 | 400 | full | 图质量最优 |
| 均衡 | 16 | 200 | sq8 | 默认 |
| 低延迟 | 12 | 100 | sq8 | 图较小，搜索快 |
| 大规模 | 16 | 200 | rabitq | 内存压缩 32x |

### 查询优化

| 场景 | num_candidates | k | 说明 |
|------|----------------|---|------|
| 召回率优先 | 500-1000 | 10 | 候选集大 |
| 延迟优先 | 50-100 | 10 | 候选集小 |
| 粗排 + 精排 | 100 | 50 | 先粗排再重排 |

### 批量写入优化

- 使用 `_bulk` 批量写入，建议每批 500-1000 条
- 向量维度相同，避免维度不一致错误
- 写入后调用 `_refresh` 确保可搜索

## 稀疏向量

UniSearch 也支持稀疏向量（sparse vector），适用于 SPLADE 等模型：

```json
{
  "mappings": {
    "properties": {
      "sparse_embedding": {
        "type": "sparse_vector"
      }
    }
  }
}
```

写入：

```bash
curl -X POST http://localhost:9200/sparse-index/_doc -H 'Content-Type: application/json' -d '{
  "sparse_embedding": {
    "indices": [0, 5, 100, 256],
    "values": [0.5, 0.3, 0.8, 0.1]
  }
}'
```

搜索：

```json
{
  "query": {
    "sparse_vector": {
      "field": "sparse_embedding",
      "query_vector": {
        "indices": [0, 5, 100],
        "values": [0.4, 0.6, 0.7]
      }
    }
  }
}
```
