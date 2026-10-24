# 项目结构

## 顶层目录

```
uni-search/
├── cmd/                    # 可执行入口
├── internal/               # 内部实现（不对外暴露）
├── pkg/                    # 公共包（可被外部引用）
├── api/                    # Protobuf 定义与生成代码
├── deploy/                 # 部署配置
├── docs/                   # 项目文档
├── scripts/                # 构建/CI/测试脚本
├── test/                    # 测试套件
├── Makefile                # 构建入口
├── Dockerfile              # 容器镜像
├── docker-compose.yml      # 集群编排
├── go.mod / go.sum         # Go 模块定义
├── .golangci.yml            # Lint 配置
└── .github/workflows/      # CI 配置
```

## cmd/ — 可执行入口

### cmd/unisearch/

主服务二进制，使用 cobra CLI 框架。

| 文件 | 职责 |
|------|------|
| `main.go` | 入口点，调用 `newRootCommand().Execute()` |
| `root.go` | 命令行参数定义（`--embedded`/`--data-dir`/`--listen`/`--config`/`--dump-config`），日志初始化 |
| `server.go` | 嵌入式模式服务组装：引擎初始化 → OTel tracing → HTTP 路由 → gRPC 服务 → 信号驱动优雅关闭 |

### cmd/uniadm/

管理工具（占位实现）。已实现 ES 兼容性扫描库 `escompat/escompat.go`，支持 `ScanMapping()` 和 `ScanQueries()` 分析 ES 迁移兼容性。

### cmd/operator/

K8s Operator 二进制，使用 controller-runtime 框架管理 `UniSearchCluster` CRD。

## internal/ — 核心实现

### config/ — 分层配置系统

- `config.go` — `Config` 结构体定义、`Default()` 默认值、`Validate()` 校验、`DumpConfig()` 导出
- `load.go` — 三层配置合并：默认值 < YAML < 环境变量 < 命令行

### embedded/ — 嵌入式引擎

嵌入式单节点模式的核心，将存储/索引/查询全栈组装为 `esapi.Engine` 接口实现。

| 文件 | 职责 |
|------|------|
| `engine.go` | `Engine` 结构体，实现 `esapi.Engine` 接口的全部方法 |
| `manager.go` | 索引管理器，管理 IndexMeta 和 Shard 生命周期 |
| `translator.go` | DSL 查询到段级逻辑计划的翻译 |
| `planner.go` | 查询计划构建与执行 |
| `docparse.go` | 文档解析与字段提取 |
| `miniinv.go` | MiniInverted 索引（MemTable 内的微型倒排索引） |

### engine/ — 查询引擎

| 子包 | 职责 |
|------|------|
| `dsl/` | ES Query DSL 解析器，JSON → 类型化 AST |
| `plan/` | 逻辑计划 + 优化器（R1-R7 规则） |
| `exec/` | Volcano 模型执行器（pull-based 算子管道） |
| `fusion/` | 混合检索融合（RRF / Weighted / RelativeScore） |
| `agg/` | 聚合引擎（sum/count/min/max/avg/stats/cardinality/terms/percentiles） |
| `highlight/` | 搜索结果高亮 |
| `percolate/` | Percolator 反向查询 |
| `rerank/` | 结果重排序 |
| `script/` | 脚本求值（painless 子集） |
| `profile/` | 查询性能追踪 |
| `stats/` | 全局统计信息 |
| `suggest/` | 搜索建议/自动补全 |

### index/ — 索引引擎

| 子包 | 职责 |
|------|------|
| `registry.go` | 统一索引插件接口 + 声明式路由表 |
| `hnsw/` | HNSW 向量图索引（CSR 邻接 + SIMD + SQ8 量化 + Cross-Link Merge + RaBitQ） |
| `inverted/` | 倒排索引（FST 词典 + Postings + BM25 + WAND + 模糊搜索） |
| `scalar/` | 标量索引（BKD 树 / Bitmap / Bloom / Geo） |
| `analyzer/` | 文本分析器（分词器 + 过滤器 + 工厂 + 热重载） |

### storage/ — 存储引擎

| 子包 | 职责 |
|------|------|
| `shard.go` | Shard 结构体，管理 MemTable/WAL/SegmentSet/DeleteTracker |
| `flush.go` | MemTable flush 流程 |
| `merge.go` | 段合并 |
| `segmentset.go` | 段集合管理 |
| `deletes.go` | 删除跟踪（segment bitmap + memtable tombstone） |
| `memtable/` | 列式内存写缓冲区 |
| `wal/` | 预写日志（group commit + CRC32C） |
| `segment/` | 不可变段格式（列存编解码、字典、ZoneMap、Footer） |
| `merge/` | 后台段合并调度器 |
| `tiering/` | 冷热分层管理（Hot/Warm/Cold） |

### server/ — 服务层

| 子包 | 职责 |
|------|------|
| `esapi/` | ES 兼容 REST API（路由 + 处理器 + Engine 接口 + PIT/Scroll 管理） |
| `grpc/` | gRPC 服务端（ShardService + MetaService + 拦截器） |

### 其他内部包

| 包 | 职责 |
|------|------|
| `meta/` | 索引元数据模型（Mapping/IndexMeta/JSON 持久化/动态推断） |
| `raft/` | Raft 共识层（MetaState FSM/集群管理/分片分裂） |
| `coordinator/` | 协调层（查询扇出合并/摄入攒批背压） |
| `objstore/` | S3 兼容对象存储客户端 + WAL 日志存储 |
| `tenant/` | 多租户资源管理（四维配额/准入控制/再均衡） |
| `observability/` | 可观测性（Prometheus 指标/OTel 追踪/慢查询/HTTP 中间件） |
| `simd/` | SIMD 向量距离计算（AVX2+FMA/NEON 双架构） |
| `errors/` | ES 对齐错误码体系 |
| `jsonx/` | JSON 工具 |
| `plugin/` | 插件系统（进程内注册 + gRPC sidecar） |
| `deps/` | 第三方依赖锚定 |

## pkg/ — 公共包

| 包 | 职责 |
|------|------|
| `types/` | 全局标识符类型（TenantID/IndexID/ShardID/SegmentID/RowID/FieldID/SeqNo）和 MVCC Snapshot |
| `unisdk/` | Go SDK 客户端（Index/Search/Bulk/Get/Delete/CreateIndex/Refresh/Health） |

## api/ — Protobuf 定义

| 文件 | 说明 |
|------|------|
| `shard.proto` | ShardService：Write/Search/SearchStream/Get/PullWAL/Snapshot |
| `meta.proto` | MetaService：Apply/Watch/Heartbeat |
| `segment.proto` | 段格式定义（列存编码规范） |
| `gen/` | protoc 生成的 Go 代码 |

生成命令：`make proto`

## test/ — 测试套件

| 目录 | 说明 |
|------|------|
| `bench/` | 性能基准 B1-B10 + 性能回归测试 |
| `chaos/` | 混沌测试框架 C1-C6 |
| `e2e/` | 端到端测试（18 个场景） |
| `golden/` | ES 兼容性金标准测试（27 个测试 + ES rest-api-spec 加载器） |
| `integration/` | 测试基础设施（TestServer + TestClient） |
| `python/` | Python 测试套件（34 个 pytest 文件，Docker 集成） |

## 模块依赖关系

```
cmd/unisearch
  → internal/config        (配置加载)
  → internal/embedded       (引擎组装)
  → internal/server/esapi   (REST 路由)
  → internal/server/grpc    (gRPC 服务)
  → internal/observability  (追踪初始化)

internal/embedded
  → internal/storage        (存储引擎)
  → internal/index           (索引引擎)
  → internal/engine          (查询引擎)
  → internal/meta             (元数据)
  → internal/simd             (向量距离)

internal/server/esapi
  → internal/embedded.Engine  (接口依赖，解耦)

internal/engine
  → internal/engine/dsl       (DSL 解析)
  → internal/engine/plan      (计划优化)
  → internal/engine/exec      (执行器)
  → internal/engine/fusion    (融合排序)
  → internal/engine/agg       (聚合)
```

核心解耦点：`esapi.Engine` 接口。REST 层只依赖接口，`embedded.Engine` 提供实现。`grpc.ShardServer` 同样委托给 `esapi.Engine`，HTTP 和 gRPC 共享同一引擎。
