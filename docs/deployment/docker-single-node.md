# Docker 单节点部署

## 概述

UniSearch 支持以 Docker 容器方式快速部署嵌入式单节点实例。该模式适合开发测试、小规模生产环境以及 CI/CD 流水线集成。单节点模式下，存储引擎、索引引擎、查询引擎全栈运行于单一进程中，无需外部依赖。

## 前置条件

- Docker 20.10+ 或 Docker Desktop
- 端口 9200（HTTP REST API）和 9300（gRPC）可用
- 磁盘空间视数据量而定，建议至少 10GB

## 镜像构建

### 预编译二进制

Dockerfile 使用预编译的二进制文件，需先在本地或 CI 环境中交叉编译：

```bash
# 编译 Linux ARM64 二进制
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o unisearch-linux ./cmd/unisearch

# 编译 Linux AMD64 二进制（x86 环境）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o unisearch-linux ./cmd/unisearch
```

> `CGO_ENABLED=0` 是项目全局约束，确保产出纯静态二进制，不依赖任何 C 库。

### 构建 Docker 镜像

```bash
docker build -t unisearch:latest .
```

构建完成后验证镜像：

```bash
docker images unisearch:latest
# 预期大小约 60MB（Alpine 基础镜像 + 静态二进制）
```

## 快速启动

### 基本启动

```bash
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  -p 9300:9300 \
  -v unisearch-data:/data \
  unisearch:latest
```

### 验证服务

```bash
# 版本探测
curl http://localhost:9200/

# 集群健康
curl http://localhost:9200/_cluster/health
# 预期: {"status":"green", ...}

# 查看节点信息
curl http://localhost:9200/_cat/nodes
```

## Dockerfile 详解

```dockerfile
FROM alpine:3.19
RUN mkdir /data && chmod 777 /data
COPY unisearch-linux /usr/local/bin/unisearch
RUN chmod +x /usr/local/bin/unisearch
EXPOSE 9200 9300
ENTRYPOINT ["unisearch"]
CMD ["--embedded", "--data-dir", "/data", "--listen", ":9200"]
```

| 指令 | 说明 |
|------|------|
| `FROM alpine:3.19` | 极小基础镜像（~7MB），与静态二进制配合实现最小镜像体积 |
| `RUN mkdir /data && chmod 777 /data` | 创建数据目录并开放权限，供非 root 用户写入 |
| `COPY unisearch-linux` | 将预编译二进制复制到容器中 |
| `EXPOSE 9200 9300` | 声明 HTTP 和 gRPC 端口 |
| `ENTRYPOINT ["unisearch"]` | 固定入口为 unisearch 二进制 |
| `CMD ["--embedded", ...]` | 默认以嵌入式模式启动，数据目录 `/data`，监听 `:9200` |

CMD 中的参数可通过 `docker run` 命令行覆盖。

## 端口说明

| 端口 | 协议 | 用途 | 容器内监听 |
|------|------|------|------------|
| 9200 | HTTP | ES 兼容 REST API | :9200 |
| 9300 | gRPC | 分片间通信 + 元数据服务 | :9300 |

gRPC 端口在单节点模式下主要用于内部通信协议暴露，外部客户端通常只需访问 9200 端口。

## 数据持久化

### Docker Volume

使用命名 Volume 持久化数据：

```bash
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  -p 9300:9300 \
  -v unisearch-data:/data \
  unisearch:latest
```

查看 Volume：

```bash
docker volume inspect unisearch-data
```

### Bind Mount

将数据目录挂载到宿主机文件系统：

```bash
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  -p 9300:9300 \
  -v /opt/unisearch/data:/data \
  unisearch:latest
```

> 使用 bind mount 时需确保宿主机目录存在且具有写入权限：`mkdir -p /opt/unisearch/data && chmod 777 /opt/unisearch/data`

### 数据目录结构

```
/data/
├── meta/              # 索引元数据（JSON 文件）
│   └── {index}.json
└── shards/           # 分片数据
    └── {shard_id}/
        ├── wal/       # WAL 预写日志
        └── segments/ # 不可变段文件
```

## 环境变量配置

通过 `-e` 参数传递 `UNISEARCH_` 前缀环境变量，覆盖默认配置：

```bash
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  -p 9300:9300 \
  -v unisearch-data:/data \
  -e UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB=256 \
  -e UNISEARCH_DATA_MEMTABLE_FLUSH_INTERVAL=15s \
  -e UNISEARCH_INDEX_HNSW_M=32 \
  -e UNISEARCH_INDEX_HNSW_EF_CONSTRUCTION=400 \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=jaeger:4317 \
  -e UNISEARCH_GRPC_ADDR=:9300 \
  unisearch:latest
```

### 常用环境变量

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `UNISEARCH_LISTEN` | `:9200` | HTTP 监听地址 |
| `UNISEARCH_GRPC_ADDR` | `:9300` | gRPC 监听地址 |
| `UNISEARCH_DATA_DIR` | `./data` | 数据目录 |
| `UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB` | `512` | MemTable 刷新大小阈值（MB） |
| `UNISEARCH_DATA_MEMTABLE_FLUSH_INTERVAL` | `30s` | MemTable 刷新间隔 |
| `UNISEARCH_STORAGE_SEGMENT_BLOCK_ROWS` | `8192` | 段块行数 |
| `UNISEARCH_INDEX_HNSW_M` | `16` | HNSW 图连接数 |
| `UNISEARCH_INDEX_HNSW_EF_CONSTRUCTION` | `200` | HNSW 构建候选数 |
| `UNISEARCH_INDEX_HNSW_QUANTIZATION` | `sq8` | 向量量化策略 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | OpenTelemetry 追踪端点 |

完整配置项参见 [配置参考](../user-guide/configuration-reference.md)。

## 生产环境建议

### 资源限制

```bash
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  -p 9300:9300 \
  -v unisearch-data:/data \
  --memory=4g \
  --cpus=2.0 \
  --memory-swappiness=0 \
  unisearch:latest
```

### 重启策略

```bash
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  -p 9300:9300 \
  -v unisearch-data:/data \
  --restart unless-stopped \
  unisearch:latest
```

### 健康检查

Dockerfile 未内置 HEALTHCHECK，建议通过 `--health-cmd` 添加：

```bash
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  -p 9300:9300 \
  -v unisearch-data:/data \
  --health-cmd="curl -f http://localhost:9200/_cluster/health || exit 1" \
  --health-interval=10s \
  --health-timeout=5s \
  --health-retries=3 \
  --restart unless-stopped \
  unisearch:latest
```

### 日志收集

容器日志输出到 stdout（JSON 格式），通过 Docker 日志驱动收集：

```bash
# 查看日志
docker logs unisearch

# 跟踪日志
docker logs -f unisearch

# 使用 json-file 驱动配置日志轮转
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  -v unisearch-data:/data \
  --log-driver=json-file \
  --log-opt max-size=100m \
  --log-opt max-file=5 \
  unisearch:latest
```

日志格式为 `log/slog` JSON，每条记录包含 `ts`/`level`/`msg` 字段，以及可选的 `trace_id`/`span_id`。详见 [日志文档](../operations/logging.md)。

## Docker Compose 单节点

使用 `docker-compose.yml` 简化管理：

```yaml
version: "3.8"
services:
  unisearch:
    build: .
    image: unisearch:latest
    ports:
      - "9200:9200"
      - "9300:9300"
    volumes:
      - unisearch-data:/data
    environment:
      - UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB=256
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9200/_cluster/health"]
      interval: 10s
      timeout: 5s
      retries: 3

volumes:
  unisearch-data:
```

启动与停止：

```bash
# 启动
docker-compose up -d

# 查看状态
docker-compose ps

# 停止
docker-compose down

# 停止并删除数据
docker-compose down -v
```

## 验证部署

部署完成后，执行以下验证步骤：

```bash
# 1. 版本探测
curl http://localhost:9200/
# 预期: 返回 version.distribution = "unisearch"

# 2. 集群健康
curl http://localhost:9200/_cluster/health
# 预期: status = "green"

# 3. 创建测试索引
curl -X PUT http://localhost:9200/test-index -H 'Content-Type: application/json' -d '{
  "mappings": {
    "properties": {
      "title": { "type": "text" },
      "content": { "type": "text" }
    }
  }
}'

# 4. 写入测试文档
curl -X POST http://localhost:9200/test-index/_doc -H 'Content-Type: application/json' -d '{
  "title": "Hello UniSearch",
  "content": "Docker deployment test"
}'

# 5. 搜索验证
curl -X POST http://localhost:9200/test-index/_search -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "Hello" } }
}'

# 6. 清理测试索引
curl -X DELETE http://localhost:9200/test-index
```

## 升级到集群

当单节点无法满足容量或吞吐需求时，可升级到分布式集群部署。集群模式引入 Raft 元数据集群、分片节点和对象存储，支持水平扩展和高可用。

详见 [Docker Compose 集群部署](docker-compose-cluster.md)。

集群模式与单节点模式的关键区别：

| 维度 | 嵌入式单节点 | 云原生集群 |
|------|-------------|------------|
| 进程数 | 1 | 多（meta + shard + coordinator） |
| 元数据 | JSON 文件 | Raft FSM 共识 |
| 数据存储 | 本地文件系统 | 本地 NVMe + S3 对象存储 |
| 高可用 | 无 | Raft 仲裁 + 副本 |
| 扩展性 | 垂直 | 水平分片 |
| 适用场景 | 开发/测试/小规模生产 | 生产环境 |
