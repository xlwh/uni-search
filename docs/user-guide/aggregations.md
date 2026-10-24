# 聚合

## 概述

UniSearch 支持在查询结果上进行聚合分析，包括指标聚合（metrics aggregation）和桶聚合（bucket aggregation）。聚合 API 与 Elasticsearch 8.x 兼容。

## 基本语法

```json
{
  "query": { "match_all": {} },
  "aggs": {
    "aggregation_name": {
      "aggregation_type": { "field": "field_name" }
    }
  },
  "size": 0
}
```

- `aggs`：聚合定义
- `size: 0`：只返回聚合结果，不返回文档

## 指标聚合

### avg

```json
{
  "aggs": {
    "avg_price": { "avg": { "field": "price" } }
  }
}
```

### sum

```json
{
  "aggs": {
    "total_sales": { "sum": { "field": "sales" } }
  }
}
```

### min / max

```json
{
  "aggs": {
    "min_price": { "min": { "field": "price" } },
    "max_price": { "max": { "field": "price" } }
  }
}
```

### count / value_count

```json
{
  "aggs": {
    "doc_count": { "value_count": { "field": "price" } }
  }
}
```

### stats

一次性返回 min、max、avg、sum、count：

```json
{
  "aggs": {
    "price_stats": { "stats": { "field": "price" } }
  }
}
```

响应：

```json
{
  "aggregations": {
    "price_stats": {
      "count": 100,
      "min": 10.0,
      "max": 500.0,
      "avg": 150.5,
      "sum": 15050.0
    }
  }
}
```

### cardinality

近似去重计数（HyperLogLog++ 算法）：

```json
{
  "aggs": {
    "unique_users": {
      "cardinality": {
        "field": "user_id",
        "precision_threshold": 40000
      }
    }
  }
}
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `precision_threshold` | 40000 | 精度阈值，越大越精确但内存消耗越高 |

### percentiles

百分位数计算：

```json
{
  "aggs": {
    "latency_percentiles": {
      "percentiles": {
        "field": "latency_ms",
        "percents": [50, 95, 99, 99.9]
      }
    }
  }
}
```

## 桶聚合

### terms

按字段值分桶：

```json
{
  "aggs": {
    "by_category": {
      "terms": {
        "field": "category",
        "size": 10
      }
    }
  }
}
```

| 参数 | 说明 |
|------|------|
| `size` | 返回桶数上限（默认 10） |
| `order` | 排序方式 |
| `min_doc_count` | 桶最小文档数 |

按计数降序：

```json
{
  "aggs": {
    "by_category": {
      "terms": {
        "field": "category",
        "size": 10,
        "order": { "_count": "desc" }
      }
    }
  }
}
```

### range

数值范围分桶：

```json
{
  "aggs": {
    "price_ranges": {
      "range": {
        "field": "price",
        "ranges": [
          { "to": 50 },
          { "from": 50, "to": 100 },
          { "from": 100, "to": 200 },
          { "from": 200 }
        ]
      }
    }
  }
}
```

### date_histogram

按日期直方图分桶：

```json
{
  "aggs": {
    "docs_over_time": {
      "date_histogram": {
        "field": "timestamp",
        "calendar_interval": "1d"
      }
    }
  }
}
```

| interval 参数 | 说明 |
|---------------|------|
| `calendar_interval` | 日历间隔：`1d`、`1w`、`1M`、`1Q`、`1y` |
| `fixed_interval` | 固定间隔：`1h`、`30m`、`60s` |

带时区：

```json
{
  "aggs": {
    "docs_over_time": {
      "date_histogram": {
        "field": "timestamp",
        "calendar_interval": "1d",
        "time_zone": "Asia/Shanghai"
      }
    }
  }
}
```

### histogram

数值直方图：

```json
{
  "aggs": {
    "price_histogram": {
      "histogram": {
        "field": "price",
        "interval": 50
      }
    }
  }
}
```

### filter

单过滤器分桶：

```json
{
  "aggs": {
    "published_docs": {
      "filter": { "term": { "status": "published" } }
    }
  }
}
```

### filters

多过滤器分桶：

```json
{
  "aggs": {
    "status_groups": {
      "filters": {
        "filters": {
          "published": { "term": { "status": "published" } },
          "draft": { "term": { "status": "draft" } }
        }
      }
    }
  }
}
```

## 嵌套聚合

桶聚合内可嵌套子聚合：

```json
{
  "aggs": {
    "by_category": {
      "terms": { "field": "category", "size": 10 },
      "aggs": {
        "avg_price": { "avg": { "field": "price" } },
        "by_status": {
          "terms": { "field": "status" },
          "aggs": {
            "max_price": { "max": { "field": "price" } }
          }
        }
      }
    }
  }
}
```

## 聚合 + 查询

聚合可与查询组合，仅对匹配文档进行聚合：

```json
{
  "query": {
    "range": { "timestamp": { "gte": "2026-01-01" } }
  },
  "aggs": {
    "by_category": {
      "terms": { "field": "category", "size": 5 }
    }
  },
  "size": 0
}
```

## 响应格式

```json
{
  "took": 10,
  "hits": {
    "total": { "value": 1000, "relation": "eq" },
    "hits": []
  },
  "aggregations": {
    "by_category": {
      "doc_count_error_upper_bound": 0,
      "sum_other_doc_count": 0,
      "buckets": [
        { "key": "database", "doc_count": 300 },
        { "key": "search", "doc_count": 250 },
        { "key": "vector", "doc_count": 200 }
      ]
    }
  }
}
```

## 性能建议

1. `size: 0`：不需要文档时设为 0，避免返回文档数据
2. `terms` 的 `size` 不要过大：增大内存和 CPU 开销
3. `cardinality` 的 `precision_threshold` 适度：40000 已满足大多数场景
4. 高基数字段避免 `terms` 聚合：如 user_id，使用 `cardinality` 代替
5. 使用 `execution_hint` 优化 terms 聚合：

```json
{
  "aggs": {
    "by_tag": {
      "terms": {
        "field": "tag",
        "execution_hint": "map"
      }
    }
  }
}
```

| execution_hint | 适用场景 |
|----------------|----------|
| `map`（默认） | 高基数字段，少量桶 |
| `global_ordinals` | 低基数字段，大量桶 |
