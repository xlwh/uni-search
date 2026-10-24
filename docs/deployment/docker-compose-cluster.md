# Docker Compose 集群部署

## 概述

Docker Compose 部署 UniSearch 云原生分布式集群，包含 3 个 Meta 节点（Raft 集群）、2 个 Shard 节点、1 个 MinIO（S3 对象存储）和 1 个 Jaeger（分布式追踪）。该部署方式适用于分布式测试、集成验证和小规模生产环境。

## 架构

```
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│   meta-1    │  │   meta-2    │  │   meta-3    │
│  Raft 集群   │←→│  Raft 集群   │←→│  Raft 集群   │
│ :9201/:9301 │  │ :9202/:9302 │  │ :9203/:9303 │
└──────┬──────┘  └─────────────┘  └─────────────┘
       │ gRPC
┌──────▼──────┐  ┌─────────────┐
│   shard-1   │  │   shard-2   │
│  数据存储    │  │  数据存储    │
│ :9211/:9311 │  │ :9212/:9312 │
└──────┬──────┘  └──────┬──────┘
       │                │
┌──────▼────────────────▼──────┐
│         MinIO (S3)           │
│     WAL/段文件持久化           │
│      :9000 / :9001           │
└──────────────────────────────┘

┌──────────────────────────────┐
│       Jaeger (追踪)           │
│    :16686 (UI) / :4317       │
└──────────────────────────────┘
```

## 前置条件

- Docker 20.10+ 和 Docker Compose v2
- 资源建议：最低 8C 16G
- 端口 9201-9203、9211-9212、9000-9001、16686 可用

## docker-compose.yml 详解

### 基础服务模板

```yaml
x-unisearch-base: &unisearch-base
  build: .
  image: unisearch:latest
```

所有 UniSearch 节点共享同一镜像，通过 `command` 参数区分角色。

### Meta 节点（3 个）

```yaml
meta-1:
  <<: *unisearch-base
  container_name: meta-1
  command: ["--cloud", "--role", "meta",
            "--raft-peers", "meta-1@meta-1:9300,meta-2@meta-2:9300,meta-3@meta-3:9300",
            "--data-dir", "/data", "--listen", ":9200"]
  ports: ["9201:9200", "9301:9300"]
  volumes: ["meta-1-data:/data"]
  healthcheck:
    test: ["CMD", "curl", "-f", "http://localhost:9200/_cluster/health"]
    interval: 5s
    timeout: 3s
    retries: 20
```

| 配置项 | 说明 |
|--------|------|
| `--cloud` | 云原生模式 |
| `--role meta` | Meta 节点角色 |
| `--raft-peers` | Raft 集群成员列表（id@host:port 格式） |
| 端口映射 | 9201→9200（HTTP），9301→9300（gRPC） |
| healthcheck | 通过 `_cluster/health` 端点检查健康 |

meta-2 和 meta-3 配置类似，端口分别为 9202/9302 和 9203/9303。

### Shard 节点（2 个）

```yaml
shard-1:
  <<: *unisearch-base
  container_name: shard-1
  command: ["--cloud", "--role", "shard",
            "--meta-addrs", "meta-1:9200",
            "--objstore", "s3://minio:9000/unisearch",
            "--data-dir", "/data", "--listen", ":9200"]
  ports: ["9211:9200", "9311:9300"]
  volumes: ["shard-1-data:/data"]
  depends_on:
    meta-1: {condition: service_healthy}
    minio: {condition: service_healthy}
  environment:
    - UNISEARCH_OBJSTORE_ACCESS_KEY=minioadmin
    - UNISEARCH_OBJSTORE_SECRET_KEY=minioadmin
```

| 配置项 | 说明 |
|--------|------|
| `--role shard` | Shard 节点角色 |
| `--meta-addrs` | Meta 节点地址（用于注册和路由） |
| `--objstore` | S3 对象存储地址（WAL/段归档） |
| depends_on | 等待 meta-1 和 minio 健康后启动 |
| 环境变量 | S3 凭证 |

shard-2 配置类似，端口为 9212/9312。

### MinIO

```yaml
minio:
  image: minio/minio:latest
  container_name: minio
  command: server /data --console-address ":9001"
  ports: ["9000:9000", "9001:9001"]
  environment:
    - MINIO_ROOT_USER=minioadmin
    - MINIO_ROOT_PASSWORD=minioadmin
  volumes: ["minio-data:/data"]
  healthcheck:
    test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
    interval: 5s
    timeout: 3s
    retries: 10
```

| 端口 | 用途 |
|------|------|
| 9000 | S3 API |
| 9001 | MinIO Web Console |

默认凭证：`minioadmin` / `minioadmin`。

### Jaeger

```yaml
jaeger:
  image: jaegertracing/all-in-one:1.55
  container_name: jaeger
  ports: ["16686:16686", "4317:4317"]
  environment:
    - COLLECTOR_OTLP_ENABLED=true
```

| 端口 | 用途 |
|------|------|
| 16686 | Jaeger UI |
| 4317 | OTLP gRPC（接收 trace 数据） |

UniSearch 节点通过 `OTEL_EXPORTER_OTLP_ENDPOINT` 环境变量配置追踪端点。

## 启动集群

### 构建镜像

```bash
# 先编译 Linux 二进制
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o unisearch-linux ./cmd/unisearch

# 构建镜像
docker build -t unisearch:latest .
```

### 启动全部服务

```bash
docker-compose up -d
```

### 查看服务状态

```bash
docker-compose ps
# 预期: 所有服务状态为 healthy/running
```

### 查看日志

```bash
# 全部服务日志
docker-compose logs -f

# 指定服务
docker-compose logs -f meta-1
docker-compose logs -f shard-1
```

## 集群验证

```bash
# 1. 集群健康（通过 meta-1）
curl http://localhost:9201/_cluster/health
# 预期: {"status":"green",...}

# 2. 节点列表
curl http://localhost:9201/_cat/nodes?v

# 3. 分片信息
curl http://localhost:9201/_cat/shards?v

# 4. 索引列表
curl http://localhost:9201/_cat/indices?v

# 5. 创建测试索引
curl -X PUT http://localhost:9201/test-cluster -H 'Content-Type: application/json' -d '{
  "mappings": {
    "properties": {
      "title": { "type": "text" },
      "embedding": { "type": "dense_vector", "dimensions": 128, "similarity": "cosine" }
    }
  }
}'

# 6. 写入并搜索
curl -X POST http://localhost:9201/test-cluster/_doc -H 'Content-Type: application/json' -d '{
  "title": "cluster test", "embedding": [0.1, 0.2]
}'

curl -X POST http://localhost:9201/test-cluster/_search -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "cluster" } }
}'
```

## 端口映射总表

| 服务 | HTTP 端口 | gRPC 端口 | 说明 |
|------|----------|----------|------|
| meta-1 | 9201 | 9301 | Meta 节点 1（主入口） |
| meta-2 | 9202 | 9302 | Meta 节点 2 |
| meta-3 | 9203 | 9303 | Meta 节点 3 |
| shard-1 | 9211 | 9311 | Shard 节点 1 |
| shard-2 | 9212 | 9312 | Shard 节点 2 |
| MinIO | 9000 | — | S3 API |
| MinIO Console | 9001 | — | Web 管理界面 |
| Jaeger UI | 16686 | — | 追踪可视化 |
| OTLP gRPC | — | 4317 | OTel Collector |

## 配置调整

### 修改副本数

编辑 `docker-compose.yml`，添加更多 Shard 节点：

```yaml
shard-3:
  <<: *unisearch-base
  container_name: shard-3
  command: ["--cloud", "--role", "shard", "--meta-addrs", "meta-1:9200",
            "--objstore", "s3://minio:9000/unisearch", "--data-dir", "/data", "--listen", ":9200"]
  ports: ["9213:9200", "9313:9300"]
  volumes: ["shard-3-data:/data"]
  depends_on:
    meta-1: {condition: service_healthy}
    minio: {condition: service_healthy}
  environment:
    - UNISEARCH_OBJSTORE_ACCESS_KEY=minioadmin
    - UNISEARCH_OBJSTORE_SECRET_KEY=minioadmin
```

### 修改资源限制

```yaml
shard-1:
  <<: *unisearch-base
  deploy:
    resources:
      limits:
        memory: 4G
        cpus: "2"
```

### 自定义对象存储凭证

```yaml
environment:
  - MINIO_ROOT_USER=my-access-key
  - MINIO_ROOT_PASSWORD=my-secret-key
```

同时更新 Shard 节点的环境变量：

```yaml
environment:
  - UNISEARCH_OBJSTORE_ACCESS_KEY=my-access-key
  - UNISEARCH_OBJSTORE_SECRET_KEY=my-secret-key
```

## 数据持久化

Volume 管理：

```bash
# 查看 Volume
docker volume ls | grep unisearch

# 备份 Volume
docker run --rm -v meta-1-data:/data -v $(pwd):/backup alpine tar czf /backup/meta-1-data.tar.gz /data

# 恢复 Volume
docker run --rm -v meta-1-data:/data -v $(pwd):/backup alpine tar xzf /backup/meta-1-data.tar.gz -C /
```

## 集群运维操作

### 滚动重启

```bash
docker-compose restart meta-1
# 等待健康后继续
docker-compose restart meta-2
docker-compose restart meta-3
docker-compose restart shard-1 shard-2
```

### 扩缩容

```bash
# 扩容：添加新节点并重新启动
docker-compose up -d shard-3

# 缩容：停止并移除节点
docker-compose stop shard-2
docker-compose rm -f shard-2
```

### 查看追踪

访问 Jaeger UI：`http://localhost:16686`

在 Jaeger UI 中搜索 trace，通过 Service 选择 `unisearch` 查看请求链路。详见 [分布式追踪](../operations/tracing.md)。

## 清理与重置

```bash
# 停止全部服务
docker-compose down

# 停止并删除 Volume（数据将丢失）
docker-compose down -v

# 清理构建镜像
docker rmi unisearch:latest
```

## 网络配置

Docker Compose 创建默认网络，服务间通过容器名 DNS 解析互联：

- Meta 节点间：`meta-1:9300`、`meta-2:9300`、`meta-3:9300`
- Shard → Meta：`meta-1:9200`
- Shard → MinIO：`minio:9000`
- UniSearch → Jaeger：`jaeger:4317`

外部访问通过端口映射连接 `localhost:9201`（Meta）或 `localhost:9211`（Shard）。

## 故障排查

详见 [故障排查](../operations/troubleshooting.md)。
