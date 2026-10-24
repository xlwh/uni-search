# 配置参考

## 概述

UniSearch 采用三层配置体系，按优先级从低到高合并：

1. **内置默认值**（`config.Default()`）— 代码中硬编码的安全默认值
2. **YAML 配置文件**（`--config` 指定路径）— 覆盖默认值
3. **环境变量**（`UNISEARCH_` 前缀）— 覆盖 YAML
4. **命令行参数**（最高优先级）— 覆盖环境变量

每一层只覆盖对应字段，未指定的字段保持上层值。`Load` 函数始终从 `Default()` 开始，确保零值不会泄漏到运行时。

## 命令行参数

| 参数 | 说明 | 覆盖字段 |
|------|------|----------|
| `--embedded` | 强制嵌入式单节点模式 | `mode` |
| `--data-dir <path>` | 数据目录 | `data.dir` |
| `--listen <addr>` | HTTP 监听地址 | `listen` |
| `--config <path>` | YAML 配置文件路径 | — |
| `--dump-config` | 打印合并后的有效配置 YAML 并退出 | — |

示例：

```bash
./unisearch --embedded --data-dir /var/lib/unisearch --listen :9200 --config /etc/unisearch/config.yaml
```

### --dump-config

验证最终配置的便捷工具：

```bash
./unisearch --embedded --data-dir ./data --listen :9200 --dump-config
```

输出（YAML 格式）：

```yaml
mode: embedded
listen: :9200
data:
    dir: ./data
    memtable:
        flush_size_mb: 512
        flush_interval: 30s
storage:
    segment_block_rows: 8192
index:
    hnsw:
        m: 16
        ef_construction: 200
        quantization: sq8
tenant:
    default_mode: shared-shard
```

## YAML 配置文件

完整 YAML 模板：

```yaml
# 运行模式: embedded | cloud
mode: embedded

# HTTP 监听地址
listen: ":9200"

# 数据存储配置
data:
  # 数据目录路径
  dir: "./data"
  # MemTable 配置
  memtable:
    # 刷新大小阈值（MB），达到后触发 flush
    flush_size_mb: 512
    # 刷新间隔，定期触发 flush
    flush_interval: 30s

# 存储引擎配置
storage:
  # 段块行数（列存块大小）
  segment_block_rows: 8192

# 索引引擎配置
index:
  # HNSW 向量索引参数
  hnsw:
    # 图连接数，影响内存和召回率
    m: 16
    # 构建时候选数，影响构建质量和速度
    ef_construction: 200
    # 量化策略: sq8 | rabitq
    quantization: sq8

# 多租户配置
tenant:
  # 默认租户模式: shared-shard | dedicated | cold
  default_mode: shared-shard
```

## 配置项详解

### mode

| 属性 | 值 |
|------|------|
| YAML 路径 | `mode` |
| 环境变量 | `UNISEARCH_MODE` |
| 默认值 | `embedded` |
| 可选值 | `embedded`, `cloud` |
| 说明 | 运行模式。`embedded` 为单节点模式，`cloud` 为分布式集群模式 |

### listen

| 属性 | 值 |
|------|------|
| YAML 路径 | `listen` |
| 环境变量 | `UNISEARCH_LISTEN` |
| 默认值 | `:9200` |
| 说明 | HTTP REST API 监听地址 |

### data.dir

| 属性 | 值 |
|------|------|
| YAML 路径 | `data.dir` |
| 环境变量 | `UNISEARCH_DATA_DIR` |
| 默认值 | `./data` |
| 说明 | 数据存储根目录，包含元数据和分片数据 |

### data.memtable.flush_size_mb

| 属性 | 值 |
|------|------|
| YAML 路径 | `data.memtable.flush_size_mb` |
| 环境变量 | `UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB` |
| 默认值 | `512` |
| 说明 | MemTable 内存缓冲区大小阈值（MB），达到后触发 flush 为不可变段 |

### data.memtable.flush_interval

| 属性 | 值 |
|------|------|
| YAML 路径 | `data.memtable.flush_interval` |
| 环境变量 | `UNISEARCH_DATA_MEMTABLE_FLUSH_INTERVAL` |
| 默认值 | `30s` |
| 说明 | MemTable 定期 flush 间隔。支持持续时间字符串（如 `30s`、`5m`）或纳秒整数 |

### storage.segment_block_rows

| 属性 | 值 |
|------|------|
| YAML 路径 | `storage.segment_block_rows` |
| 环境变量 | `UNISEARCH_STORAGE_SEGMENT_BLOCK_ROWS` |
| 默认值 | `8192` |
| 说明 | 列存段块行数，影响 ZoneMap 跳块粒度和压缩率 |

### index.hnsw.m

| 属性 | 值 |
|------|------|
| YAML 路径 | `index.hnsw.m` |
| 环境变量 | `UNISEARCH_INDEX_HNSW_M` |
| 默认值 | `16` |
| 说明 | HNSW 图每个节点的最大连接数。增大可提高召回率但增加内存消耗 |

### index.hnsw.ef_construction

| 属性 | 值 |
|------|------|
| YAML 路径 | `index.hnsw.ef_construction` |
| 环境变量 | `UNISEARCH_INDEX_HNSW_EF_CONSTRUCTION` |
| 默认值 | `200` |
| 说明 | HNSW 构建时的搜索宽度。增大可提高索引质量但降低写入速度 |

### index.hnsw.quantization

| 属性 | 值 |
|------|------|
| YAML 路径 | `index.hnsw.quantization` |
| 环境变量 | `UNISEARCH_INDEX_HNSW_QUANTIZATION` |
| 默认值 | `sq8` |
| 可选值 | `sq8`, `rabitq` |
| 说明 | 向量量化策略。`sq8` 为标量量化（默认），`rabitq` 为 1-bit 量化（Warm 层） |

### tenant.default_mode

| 属性 | 值 |
|------|------|
| YAML 路径 | `tenant.default_mode` |
| 环境变量 | `UNISEARCH_TENANT_DEFAULT_MODE` |
| 默认值 | `shared-shard` |
| 可选值 | `shared-shard`, `dedicated`, `cold` |
| 说明 | 多租户默认隔离模式 |

## 环境变量映射表

| 环境变量 | YAML 路径 | 类型 | 默认值 |
|----------|-----------|------|--------|
| `UNISEARCH_MODE` | `mode` | string | `embedded` |
| `UNISEARCH_LISTEN` | `listen` | string | `:9200` |
| `UNISEARCH_DATA_DIR` | `data.dir` | string | `./data` |
| `UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB` | `data.memtable.flush_size_mb` | int | `512` |
| `UNISEARCH_DATA_MEMTABLE_FLUSH_INTERVAL` | `data.memtable.flush_interval` | duration | `30s` |
| `UNISEARCH_STORAGE_SEGMENT_BLOCK_ROWS` | `storage.segment_block_rows` | int | `8192` |
| `UNISEARCH_INDEX_HNSW_M` | `index.hnsw.m` | int | `16` |
| `UNISEARCH_INDEX_HNSW_EF_CONSTRUCTION` | `index.hnsw.ef_construction` | int | `200` |
| `UNISEARCH_INDEX_HNSW_QUANTIZATION` | `index.hnsw.quantization` | string | `sq8` |
| `UNISEARCH_TENANT_DEFAULT_MODE` | `tenant.default_mode` | string | `shared-shard` |

环境变量键不区分大小写，`UNISEARCH_LISTEN` 和 `unisearch_listen` 等效。未知的 `UNISEARCH_*` 变量会被忽略。

### 其他环境变量

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `UNISEARCH_GRPC_ADDR` | `:9300` | gRPC 监听地址 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | OpenTelemetry Collector 地址 |
| `UNISEARCH_OBJSTORE_ACCESS_KEY` | — | S3 对象存储 AccessKey |
| `UNISEARCH_OBJSTORE_SECRET_KEY` | — | S3 对象存储 SecretKey |

## Duration 格式

`flush_interval` 等 Duration 类型支持两种格式：

- **持续时间字符串**：`30s`、`5m`、`1h`、`500ms`
- **纳秒整数**：`30000000000`（等价于 `30s`）

YAML 中推荐使用字符串形式以提升可读性。

## 配置校验规则

`Validate()` 在配置加载后执行，校验失败返回错误：

| 字段 | 校验规则 |
|------|----------|
| `mode` | 必须为 `embedded` 或 `cloud` |
| `listen` | 不能为空 |
| `data.dir` | 不能为空 |
| `data.memtable.flush_size_mb` | 必须为正数 |
| `data.memtable.flush_interval` | 必须为正数 |
| `storage.segment_block_rows` | 必须为正数 |
| `index.hnsw.m` | 必须为正数 |
| `index.hnsw.ef_construction` | 必须为正数 |
| `index.hnsw.quantization` | 不能为空 |
| `tenant.default_mode` | 不能为空 |

## 配置最佳实践

### 开发环境

```bash
./unisearch --embedded --data-dir ./data --listen :9200
```

或使用最小配置文件 `config-dev.yaml`：

```yaml
mode: embedded
listen: :9200
data:
  dir: ./data
  memtable:
    flush_size_mb: 64
    flush_interval: 10s
```

### 生产环境（单节点）

```yaml
mode: embedded
listen: :9200
data:
  dir: /var/lib/unisearch
  memtable:
    flush_size_mb: 512
    flush_interval: 30s
storage:
  segment_block_rows: 8192
index:
  hnsw:
    m: 16
    ef_construction: 200
    quantization: sq8
```

### Docker 环境

通过环境变量传递配置：

```bash
docker run -d \
  -p 9200:9200 -p 9300:9300 \
  -v unisearch-data:/data \
  -e UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB=256 \
  -e UNISEARCH_INDEX_HNSW_M=32 \
  unisearch:latest
```

### K8s ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: unisearch-config
data:
  config.yaml: |
    mode: embedded
    listen: ":9200"
    data:
      dir: "/data"
      memtable:
        flush_size_mb: 512
        flush_interval: 30s
```
