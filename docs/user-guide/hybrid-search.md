# 混合检索

## 概述

混合检索（Hybrid Search）是 UniSearch 的核心特性之一，将全文检索（BM25）和向量搜索（HNSW）的结果通过融合算法合并排序，兼顾关键词精确匹配和语义相似性召回。

## 基本用法

### hybrid 查询

```bash
curl -X POST http://localhost:9200/my-index/_search -H 'Content-Type: application/json' -d '{
  "query": {
    "hybrid": {
      "queries": [
        { "match": { "title": "vector search engine" } },
        { "knn": {
            "field": "embedding",
            "query_vector": [0.1, 0.2, 0.3, 0.4],
            "k": 50,
            "num_candidates": 100
          }
        }
      ],
      "rank": {
        "method": "rrf",
        "rank_const": 60,
        "window_size": 500
      },
      "size": 10
    }
  }
}'
```

### 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `queries` | 必填 | 多路召回查询数组（2 路或以上） |
| `rank.method` | `rrf` | 融合算法 |
| `rank.rank_const` | 60 | RRF 常数 |
| `rank.weights` | — | Weighted 融合权重 |
| `rank.window_size` | 500 | 融合窗口大小 |
| `size` | 10 | 最终返回结果数 |

### 约束

- `queries` 数组至少包含 2 路查询
- `from + size` 不得超过 10000
- 每路查询独立执行后合并

## 融合算法

### RRF（Reciprocal Rank Fusion）

默认融合算法，基于排名的倒数加权：

```
score(d) = sum(1 / (rank_const + rank_i(d)))
```

- `rank_const`：平滑常数，默认 60
- `rank_i(d)`：文档在第 i 路查询中的排名（从 1 开始）
- 无需归一化，对不同分数量级鲁棒

```json
{
  "rank": {
    "method": "rrf",
    "rank_const": 60
  }
}
```

`rank_const` 调优建议：
- `60`（默认）：标准值，适合大多数场景
- `1`：头部结果权重更大（更看重排名靠前的结果）
- `100`：排名差异更平滑

### Weighted（加权融合）

基于 min-max 归一化后的分数加权求和：

```
score(d) = sum(w_i * normalize(score_i(d)))
```

- `weights`：每路查询的权重数组，长度与 `queries` 一致
- 分数归一化：min-max 归一化到 [0, 1]

```json
{
  "rank": {
    "method": "weighted",
    "weights": [0.3, 0.7]
  }
}
```

适用场景：明确知道全文检索和向量搜索的相对重要性，如语义搜索为主（`weights: [0.2, 0.8]`）。

### RelativeScore（相对分数融合）

基于分数的相对比例融合：

```json
{
  "rank": {
    "method": "relative_score"
  }
}
```

无需手动设置权重，自适应计算各路查询分数的相对贡献。

## 多路召回策略

### 全文 + 向量

最经典的混合检索组合：

```json
{
  "hybrid": {
    "queries": [
      { "match": { "content": "distributed search database" } },
      { "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 50, "num_candidates": 100 } }
    ],
    "rank": { "method": "rrf", "rank_const": 60 },
    "size": 10
  }
}
```

### 多字段全文 + 向量

```json
{
  "hybrid": {
    "queries": [
      { "multi_match": { "query": "vector search", "fields": ["title^2", "content"] } },
      { "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 50, "num_candidates": 100 } }
    ],
    "rank": { "method": "rrf" },
    "size": 10
  }
}
```

### 全文 + 向量 + 过滤

在每路查询中分别添加过滤条件：

```json
{
  "hybrid": {
    "queries": [
      { "bool": { "must": [{"match": {"content": "search"}}], "filter": [{"term": {"category": "article"}}] } },
      { "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 50, "num_candidates": 100, "filter": {"term": {"category": "article"}} } }
    ],
    "rank": { "method": "rrf" },
    "size": 10
  }
}
```

### 三路召回

全文 + 向量 + 稀疏向量：

```json
{
  "hybrid": {
    "queries": [
      { "match": { "content": "machine learning" } },
      { "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 50, "num_candidates": 100 } },
      { "sparse_vector": { "field": "sparse_embedding", "query_vector": {"indices": [0, 5], "values": [0.4, 0.6]} } }
    ],
    "rank": { "method": "rrf", "rank_const": 60 },
    "size": 10
  }
}
```

## window_size 调优

`window_size` 控制每路查询参与融合的候选数量：

| window_size | 效果 |
|-------------|------|
| 100 | 仅头部结果参与融合，速度快但可能遗漏 |
| 500（默认） | 平衡 |
| 1000 | 更多结果参与融合，召回率高但延迟增加 |

```json
{
  "rank": {
    "method": "rrf",
    "window_size": 1000
  }
}
```

## 响应格式

```json
{
  "took": 15,
  "hits": {
    "total": { "value": 10, "relation": "eq" },
    "max_score": 0.0333,
    "hits": [
      {
        "_index": "my-index",
        "_id": "doc-1",
        "_score": 0.0333,
        "_source": {
          "title": "introduction to vector search",
          "content": "..."
        }
      }
    ]
  }
}
```

RRF 融合后的 `_score` 为排名倒数之和，数值越小表示排名越靠前（但 UniSearch 按降序排列，max_score 为最高分）。

## 使用 _knn_search 端点

除 `_search` 端点外，`_knn_search` 也支持 hybrid 查询：

```bash
curl -X POST http://localhost:9200/my-index/_knn_search -H 'Content-Type: application/json' -d '{
  "query": {
    "hybrid": {
      "queries": [
        { "match": { "title": "search" } },
        { "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 50, "num_candidates": 100 } }
      ],
      "rank": { "method": "rrf" },
      "size": 10
    }
  }
}'
```

## 融合算法选择指南

| 场景 | 推荐算法 | 原因 |
|------|----------|------|
| 通用语义搜索 | RRF (default) | 无需调参，对不同分数量级鲁棒 |
| 已知权重比例 | Weighted | 精确控制各路贡献 |
| 不确定权重 | RelativeScore | 自适应计算 |
| 关键词为主 | Weighted [0.7, 0.3] | 全文权重高 |
| 语义为主 | Weighted [0.3, 0.7] | 向量权重高 |

## 性能建议

1. `num_candidates` 不宜过大：100-200 通常足够
2. `window_size` 不宜超过 1000：增加延迟但召回率提升有限
3. `size` 通常 10-50：过大时考虑使用 scroll 分页
4. 多路查询的 `filter` 尽量一致：避免不同路返回不同文档集导致融合偏差

## 与 ES 的差异

UniSearch 的 `hybrid` 查询是原生查询类型，不同于 ES 8.x 需要通过 RRF retriever 或自定义评分实现：

| 特性 | UniSearch | ES 8.x |
|------|-----------|--------|
| hybrid 查询类型 | 原生支持 | 需要 retriever |
| RRF | 内置 | 需要 rrf retriever |
| Weighted 融合 | 内置 | 需自定义 |
| 融合窗口可调 | window_size | rank_window_size |

详见 [ES 兼容性](es-compatibility.md)。
