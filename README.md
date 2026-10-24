# UniSearch

UniSearch 是一个融合全文检索与向量搜索的数据库，提供 Elasticsearch 8.x 兼容的 REST API，使用 Go 语言开发，编译为单一静态二进制文件。支持全文 BM25 检索、KNN 向量搜索、混合检索融合和聚合查询，内置自研列式存储引擎，无需外部数据库依赖。

## 核心特性

- **融合检索**：全文 BM25 + KNN 向量搜索 + 混合融合（RRF / Weighted / RelativeScore）
- **ES 兼容**：REST API 高度兼容 ES 8.x（主流查询/聚合/管理 API），未覆盖能力返回明确的 `unsupported_capability` 错误（详见 [兼容性文档](docs/user-guide/es-compatibility.md)）
- **自研列存**：列式存储引擎，支持 DICT / DELTA_BITPACK / GORILLA / ZSTD 编码，zstd 压缩
- **HNSW 索引**：CSR 邻接布局 + SQ8 量化 + SIMD 距离框架（运行时 CPU 特性探测；汇编内核规划中，当前为纯 Go 标量路径）+ Cross-Link Merge
- **多租户**：四维配额（存储/向量数/QPS/TPS）+ 准入控制 + 背压
- **两种部署模式**：embedded 单节点（开发/测试）和 cloud 分布式集群（生产）
- **零外部依赖**：不依赖 etcd / Kafka / 外部数据库，元数据层 Raft 内置（hashicorp/raft），分片层 Raft 组已实现待接线
- **可观测性**：Prometheus 指标 + OpenTelemetry 分布式追踪 + slog 结构化日志

## 快速开始

```bash
# 构建二进制
make build

# 启动嵌入式单节点
./unisearch --embedded --data-dir ./data --listen :9200

# 验证服务
curl http://localhost:9200/
curl http://localhost:9200/_cluster/health

# 创建索引并写入文档
curl -X PUT http://localhost:9200/my-index -H 'Content-Type: application/json' -d '{
  "mappings": {
    "properties": {
      "title": { "type": "text" },
      "embedding": { "type": "dense_vector", "dimensions": 128, "similarity": "cosine" }
    }
  }
}'

curl -X POST http://localhost:9200/my-index/_doc -H 'Content-Type: application/json' -d '{
  "title": "Hello UniSearch",
  "embedding": [0.1, 0.2, 0.3]
}'

# 搜索
curl -X POST http://localhost:9200/my-index/_search -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "Hello" } }
}'
```

> 详细的快速上手指南参见 [docs/user-guide/quickstart.md](docs/user-guide/quickstart.md)。

## 架构概览

```
                    ┌─────────────────────────────┐
                    │       Client / SDK          │
                    └──────────┬──────────────────┘
                               │ HTTP :9200
                    ┌──────────▼──────────────────┐
                    │     ES 兼容 REST API         │
                    │    (internal/server/esapi)   │
                    └──────────┬──────────────────┘
                               │
              ┌────────────────▼────────────────┐
              │         查询引擎                   │
              │  DSL 解析 → 计划优化 → Volcano 执行  │
              │  (internal/engine)               │
              └────┬──────────┬──────────┬───────┘
                   │          │          │
          ┌────────▼──┐ ┌────▼────┐ ┌───▼───────┐
          │ 倒排索引    │ │ HNSW   │ │ 标量索引   │
          │ (BM25/WAND)│ │ (向量) │ │ (BKD/Bitmap)│
          └────────┬──┘ └────┬──┘ └───┬───────┘
                   │         │         │
              ┌────▼─────────▼─────────▼───────┐
              │         存储引擎                 │
              │  MemTable → WAL → Segment       │
              │  (internal/storage)             │
              └────────────────────────────────┘
```

## 运行模式

| 维度 | embedded（嵌入式） | cloud（云原生） |
|------|-------------------|-----------------|
| 进程数 | 1 | 多（meta + shard + coordinator） |
| 元数据 | JSON 文件 | Raft FSM 共识 |
| 数据存储 | 本地文件系统 | 本地 NVMe + S3 对象存储 |
| 高可用 | 无 | Raft 仲裁 + 副本 |
| 扩展性 | 垂直 | 水平分片 |
| 适用场景 | 开发 / 测试 / 小规模生产 | 生产环境 |

## 项目结构

```
uni-search/
├── cmd/                    # 可执行入口
│   ├── unisearch/          # 主服务二进制
│   ├── uniadm/             # 管理工具
│   └── operator/           # K8s Operator
├── internal/               # 内部实现
│   ├── config/             # 分层配置系统
│   ├── embedded/           # 嵌入式引擎
│   ├── engine/             # 查询引擎（DSL/Plan/Exec/Fusion/Agg）
│   ├── index/              # 索引引擎（HNSW/Inverted/Scalar）
│   ├── storage/            # 存储引擎（MemTable/WAL/Segment/Merge）
│   ├── server/             # HTTP/gRPC 服务层
│   ├── meta/               # 元数据管理
│   ├── raft/               # Raft 共识
│   ├── observability/      # 可观测性（Metrics/Tracing/Logging）
│   └── ...
├── pkg/                    # 公共包
│   ├── types/              # 共享类型
│   └── unisdk/             # Go SDK
├── api/proto/              # Protobuf 定义
├── deploy/k8s/operator/    # K8s Operator
├── test/                   # 测试套件
├── docs/                   # 项目文档
└── scripts/                # 构建/CI 脚本
```

> 详细结构说明参见 [docs/development/project-structure.md](docs/development/project-structure.md)。

## 构建与测试

```bash
# 编译
make build

# 运行测试
make test

# 代码检查
make lint

# 覆盖率报告
make coverage

# 交叉编译
make cross-build

# 模糊测试
make fuzz

# CI 全流程（lint + test + build）
make ci
```

> 详细的构建与测试说明参见 [docs/development/build-and-test.md](docs/development/build-and-test.md)。

## 部署方式

| 方式 | 适用场景 | 文档 |
|------|----------|------|
| Docker 单节点 | 开发/测试 | [docker-single-node.md](docs/deployment/docker-single-node.md) |
| Docker Compose 集群 | 分布式测试 | [docker-compose-cluster.md](docs/deployment/docker-compose-cluster.md) |
| K8s Operator | 生产部署 | [k8s-operator.md](docs/deployment/k8s-operator.md) |
| 交叉编译二进制 | 多平台发布 | [cross-build.md](docs/deployment/cross-build.md) |

## 技术栈

| 组件 | 技术 | 说明 |
|------|------|------|
| 语言 | Go 1.26 | CGO_ENABLED=0，纯静态编译 |
| CLI 框架 | cobra | 命令行参数解析 |
| 配置 | yaml.v3 + 环境变量 | 三层配置合并 |
| 共识 | hashicorp/raft | 元数据一致性 |
| 对象存储 | minio-go/v7 | S3 兼容存储 |
| 指标 | prometheus/client_golang | Prometheus 指标 |
| 追踪 | opentelemetry-go | OTLP gRPC 导出 |
| 压缩 | klauspost/compress/zstd | 纯 Go zstd |
| 中文分词 | go-ego/gse | 中文分析器 |
| JSON | goccy/go-json | 高性能 JSON |
| K8s | controller-runtime | Operator 框架 |

## 文档导航

### 设计文档

- [概要设计](docs/vector-database-design.md) — 项目背景、竞品调研、架构目标
- [开发规划](docs/development-plan.md) — M0-M3 全量 WBS 任务分解
- [Go 详细设计总纲](docs/go-design/00-overview.md) — 技术选型、模块划分、公共规范
- [存储引擎设计](docs/go-design/01-storage-engine.md) — MemTable/WAL/Segment 详细设计
- [索引引擎设计](docs/go-design/02-index-engine.md) — HNSW/倒排/标量索引设计
- [查询引擎设计](docs/go-design/03-query-engine.md) — DSL/Plan/Exec/Fusion 设计
- [ES 兼容层设计](docs/go-design/04-es-compat-layer.md) — REST API/DSL 兼容矩阵
- [分布式架构设计](docs/go-design/05-distributed.md) — Raft/分片/多租户设计
- [质量保障与可观测性](docs/go-design/06-quality-and-observability.md) — 指标/追踪/测试策略

### 研发文档

- [开发入门](docs/development/getting-started.md) — 环境搭建与首次构建
- [构建与测试](docs/development/build-and-test.md) — 编译/测试/覆盖率/模糊测试
- [项目结构](docs/development/project-structure.md) — 目录结构与模块职责
- [代码规范](docs/development/code-style.md) — Lint/错误处理/日志/注释规范
- [CI/CD](docs/development/ci-cd.md) — GitHub Actions 流程与门禁

### 部署文档

- [Docker 单节点](docs/deployment/docker-single-node.md) — 容器化单节点部署
- [Docker Compose 集群](docs/deployment/docker-compose-cluster.md) — 分布式集群部署
- [K8s Operator](docs/deployment/k8s-operator.md) — Kubernetes 集群管理
- [交叉编译](docs/deployment/cross-build.md) — 多平台二进制构建

### 运维文档

- [Prometheus 监控](docs/operations/monitoring.md) — 指标体系与告警
- [分布式追踪](docs/operations/tracing.md) — OTel/Jaeger 配置
- [日志](docs/operations/logging.md) — slog JSON 日志与 trace 关联
- [健康检查](docs/operations/health-check.md) — 集群健康与探针集成
- [故障排查](docs/operations/troubleshooting.md) — 常见问题诊断
- [备份恢复](docs/operations/backup-restore.md) — 数据备份与恢复策略
- [性能调优](docs/operations/performance-tuning.md) — 写入/查询/向量/内存调优

### 用户指南

- [快速开始](docs/user-guide/quickstart.md) — 5 分钟上手
- [配置参考](docs/user-guide/configuration-reference.md) — 全量配置项说明
- [API 参考](docs/user-guide/api-reference.md) — REST API 端点文档
- [查询 DSL](docs/user-guide/query-dsl.md) — 查询语法详解
- [映射与设置](docs/user-guide/mappings-and-settings.md) — 索引映射管理
- [向量搜索](docs/user-guide/vector-search.md) — KNN 向量检索指南
- [混合检索](docs/user-guide/hybrid-search.md) — 全文+向量融合查询
- [聚合查询](docs/user-guide/aggregations.md) — 指标/桶/管道聚合
- [ES 兼容性](docs/user-guide/es-compatibility.md) — 兼容矩阵与迁移指南
- [Go SDK](docs/user-guide/sdk-guide.md) — Go 客户端使用

## License

See [LICENSE](LICENSE).
