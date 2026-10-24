# Mappings 与 Settings

## 概述

Mapping 定义索引的字段结构和类型，Settings 定义索引级别的配置。UniSearch 使用与 Elasticsearch 8.x 兼容的 Mapping 和 Settings API。

## 字段类型

### 核心类型

| 类型 | 说明 | 示例 |
|------|------|------|
| `text` | 全文检索文本（分词） | `"title"` |
| `keyword` | 精确匹配字符串（不分词） | `"status"` |
| `integer` / `long` | 整数 | `42` |
| `float` / `double` | 浮点数 | `3.14` |
| `boolean` | 布尔值 | `true` |
| `date` | 日期 | `"2026-01-01"` |
| `dense_vector` | 稠密向量 | `[0.1, 0.2, ...]` |
| `sparse_vector` | 稀疏向量 | `{"indices": [0, 5], "values": [0.5, 0.3]}` |
| `object` | 嵌套对象 | `{"nested": {...}}` |
| `ip` | IP 地址 | `"192.168.1.1"` |
| `geo_point` | 地理坐标 | `{"lat": 39.9, "lon": 116.4}` |

### dense_vector 字段

```json
{
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
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `dimensions` | 必填 | 向量维度 |
| `similarity` | `cosine` | 相似度函数：`cosine` / `l2` / `dot_product` |
| `index` | `true` | 是否构建索引 |
| `index_options.type` | `hnsw` | 索引类型 |
| `index_options.m` | 16 | HNSW 连通度 |
| `index_options.ef_construction` | 200 | 构建时搜索宽度 |
| `index_options.quantization` | `sq8` | 量化方式：`full` / `sq8` / `rabitq` |

### text 字段

```json
{
  "content": {
    "type": "text",
    "analyzer": "standard",
    "search_analyzer": "standard"
  }
}
```

### keyword 字段

```json
{
  "status": {
    "type": "keyword",
    "ignore_above": 256
  }
}
```

`ignore_above` 超过指定长度的值不被索引。

### 多字段（multi-fields）

```json
{
  "title": {
    "type": "text",
    "fields": {
      "keyword": { "type": "keyword", "ignore_above": 256 }
    }
  }
}
```

`title` 用于全文检索，`title.keyword` 用于精确匹配和聚合。

## 创建索引

```bash
curl -X PUT http://localhost:9200/my-index -H 'Content-Type: application/json' -d '{
  "mappings": {
    "properties": {
      "title": { "type": "text" },
      "content": { "type": "text", "analyzer": "standard" },
      "status": { "type": "keyword" },
      "price": { "type": "float" },
      "timestamp": { "type": "date" },
      "embedding": {
        "type": "dense_vector",
        "dimensions": 128,
        "similarity": "cosine",
        "index_options": {
          "type": "hnsw",
          "m": 16,
          "ef_construction": 200,
          "quantization": "sq8"
        }
      }
    }
  },
  "settings": {
    "number_of_shards": 3,
    "number_of_replicas": 1,
    "refresh_interval": "1s"
  }
}'
```

## 动态映射

默认情况下，UniSearch 自动推断新字段的类型（dynamic mapping）：

| JSON 值类型 | 推断字段类型 |
|-------------|-------------|
| 字符串 | `text` + `keyword`（multi-field） |
| 整数 | `long` |
| 浮点数 | `float` |
| 布尔值 | `boolean` |
| 对象 | `object` |

### 动态映射策略

```json
{
  "mappings": {
    "dynamic": "strict",
    "properties": { ... }
  }
}
```

| 策略 | 说明 |
|------|------|
| `true` | 自动推断并添加新字段（默认） |
| `false` | 忽略新字段（不索引） |
| `strict` | 遇到新字段时报错 |

### 动态模板

```json
{
  "mappings": {
    "dynamic_templates": [
      {
        "strings_as_keyword": {
          "match_mapping_type": "string",
          "mapping": { "type": "keyword", "ignore_above": 256 }
        }
      }
    ]
  }
}
```

## 修改 Mapping

### 添加字段

```bash
curl -X PUT http://localhost:9200/my-index/_mapping -H 'Content-Type: application/json' -d '{
  "properties": {
    "new_field": { "type": "keyword" }
  }
}'
```

> 注意：已存在字段的类型不可修改。如需修改字段类型，必须通过 reindex 创建新索引。

### 查看 Mapping

```bash
curl http://localhost:9200/my-index/_mapping
```

## Settings

### 索引级 Settings

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `number_of_shards` | 1 | 主分片数（创建后不可修改） |
| `number_of_replicas` | 1 | 副本数 |
| `refresh_interval` | 1s | 刷新间隔 |

### 修改 Settings

```bash
curl -X PUT http://localhost:9200/my-index/_settings -H 'Content-Type: application/json' -d '{
  "refresh_interval": "5s",
  "number_of_replicas": 2
}'
```

### 查看 Settings

```bash
curl http://localhost:9200/my-index/_settings
```

## 分析器

### 内置分析器

| 分析器 | 说明 |
|--------|------|
| `standard` | 默认分析器，Unicode 分词 + 小写 + 停用词 |
| `simple` | 按非字母分词 + 小写 |
| `whitespace` | 按空格分词 |
| `keyword` | 不分词（整个内容作为一个词项） |
| `chinese` | 中文分词（基于 gse） |

### 自定义分析器

```json
{
  "settings": {
    "analysis": {
      "analyzer": {
        "my_analyzer": {
          "type": "custom",
          "tokenizer": "standard",
          "filter": ["lowercase", "stop"]
        }
      }
    }
  },
  "mappings": {
    "properties": {
      "content": {
        "type": "text",
        "analyzer": "my_analyzer"
      }
    }
  }
}
```

### 分析器热重载

UniSearch 支持分析器热重载，修改分析器配置后无需重建索引：

```bash
# 更新分析器配置
curl -X PUT http://localhost:9200/my-index/_settings -H 'Content-Type: application/json' -d '{
  "analysis": {
    "analyzer": {
      "my_analyzer": { "type": "custom", "tokenizer": "standard", "filter": ["lowercase"] }
    }
  }
}'
```

## 索引模板

索引模板在创建新索引时自动应用 Mapping 和 Settings：

```bash
curl -X PUT http://localhost:9200/_index_template/logs_template -H 'Content-Type: application/json' -d '{
  "index_patterns": ["logs-*"],
  "template": {
    "mappings": {
      "properties": {
        "@timestamp": { "type": "date" },
        "level": { "type": "keyword" },
        "message": { "type": "text" }
      }
    },
    "settings": {
      "number_of_shards": 1,
      "refresh_interval": "5s"
    }
  }
}'
```

## 别名

```bash
# 创建别名
curl -X POST http://localhost:9200/_aliases -H 'Content-Type: application/json' -d '{
  "actions": [
    { "add": { "index": "my-index-v1", "alias": "my-index" } }
  ]
}'

# 原子切换（零停机迁移）
curl -X POST http://localhost:9200/_aliases -H 'Content-Type: application/json' -d '{
  "actions": [
    { "remove": { "index": "my-index-v1", "alias": "my-index" } },
    { "add": { "index": "my-index-v2", "alias": "my-index" } }
  ]
}'
```
