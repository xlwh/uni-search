# Proposal: 补齐设计文档 vs 代码审计的 9 个功能缺口

## 1. 背景与动机 (Background & Motivation)

### 1.1 当前问题

经过对 `docs/go-design/` 7 份设计文档与实际代码的全量对比审计（117 个功能点），发现：
- **88/117 完全实现** (75.2%)
- **20/117 部分实现** (17.1%)
- **9/117 未实现** (7.7%)

9 个未实现缺口横跨 M0-M2 四个里程碑，涉及可观测性、DevOps、分布式核心能力，直接影响生产就绪度。

### 1.2 用户期望

将 9 个缺口全部补齐到"端到端可用"标准，使审计覆盖率从 75.2% 提升至 ≥92%，为项目 GA 奠定基础。

## 2. 目标 (Objective)

1. **可观测性闭环**：Prometheus metrics + OTel tracing 全量接入，覆盖 HTTP→engine→storage 全链路
2. **分布式核心能力补齐**：gRPC 服务实现 + K8s Operator reconciler + M2 三个数据迁移功能端到端可用
3. **工程基础设施**：CI/CD pipeline + goccy/json 热路径加速

## 3. 方案设计 (Approach)

### 3.1 设计原则

- **最小侵入**：新增模块优先，修改现有文件仅在必要接线点
- **分层实施**：P0 基础设施 → P1 分布式核心 → P2 数据迁移 → P3 性能优化
- **测试先行**：每个缺口配套集成测试，确保端到端验证

### 3.2 核心方案

按优先级分 4 组并行/串行实施：

```mermaid
graph TD
    subgraph P0 基础设施
        A1[Prometheus metrics] --> A0[/metrics endpoint]
        A2[OTel tracing] --> A0
        A3[CI/CD scripts]
    end
    subgraph P1 分布式核心
        B1[gRPC server/client] --> B2[K8s Operator reconciler]
    end
    subgraph P2 数据迁移
        C1[Online split] --> C2[Cross-Link merge] --> C3[冷热分层]
    end
    subgraph P3 性能
        D1[goccy/json 热路径]
    end
    P0 --> P1 --> P2 --> P3
```

### 3.3 各缺口实施方案

#### G1: Prometheus Metrics (M0, 06 §1)

- 新增 `internal/observability/metrics.go`：定义五组指标（写入/查询/段/索引/租户配额）
- 在 `internal/server/esapi/router.go` 添加 `GET /_prometheus/metrics` endpoint
- 在 HTTP middleware、storage flush、index search、tenant quota 关键路径插入埋点
- 依赖：`github.com/prometheus/client_golang` v1.19+
- 指标清单（对齐设计 06 §1）：
  - 写入：`unisearch_write_docs_total`、`unisearch_write_batch_duration_seconds`、`unisearch_write_rejected_total`
  - 查询：`unisearch_query_requests_total`、`unisearch_query_duration_seconds`、`unisearch_query_rows_scanned_total`、`unisearch_query_errors_total`
  - 段：`unisearch_segments_open`、`unisearch_flush_duration_seconds`、`unisearch_merge_running`、`unisearch_merge_bytes_total`、`unisearch_deletes_pending`
  - 索引：`unisearch_hnsw_search_duration_seconds`、`unisearch_hnsw_nodes_visited`、`unisearch_bm25_wand_pruned_ratio`
  - 租户：`unisearch_tenant_quota_usage_ratio`、`unisearch_tenant_throttled_total`
  - 运行时：`go_*` 原生指标全量保留

#### G2: OTel 分布式追踪 (M1, 00 §2 + 06 §2)

- 新增 `internal/observability/tracing.go`：初始化 OTel SDK + OTLP gRPC exporter
- 四级 span 树：`http.request` → `query.plan` → `shard.search` → `segment.scan / index.hnsw / index.bm25 / agg.partial`
- 在 HTTP middleware 注入 root span，在 engine planner 注入 plan span，在 shard search 注入 shard span
- 采样策略：parent-based + 错误全采 + 慢查询 >1s 全采 + 其余 1%
- 依赖：`go.opentelemetry.io/otel`、`go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`

#### G3: CI/CD Pipeline (M0, 06 §6)

- 新增 `scripts/ci.sh`：统一入口脚本（lint + test + build + coverage）
- 新增 `scripts/build.sh`：交叉编译脚本（linux/amd64, linux/arm64, darwin/arm64，CGO_ENABLED=0）
- 扩展 `Makefile`：新增 `ci`、`coverage`、`cross-build`、`fuzz` target
- 新增 `.golangci.yml`：golangci-lint 配置（errcheck/govet/staticcheck/revive/gocritic）
- 平台无关设计：CI 平台调用 `make ci`，不绑定特定 CI 平台

#### G4: gRPC 服务实现 (M2, 05 §7)

- 新增 `api/proto/gen/`：protoc 生成的 Go 代码（shard.pb.go, meta.pb.go, shard_grpc.go, meta_grpc.go）
- 新增 `internal/server/grpc/server.go`：gRPC server 实现 ShardService + MetaService
  - ShardService：Write/Search/Get 转发到 embedded.Engine，PullWAL/Snapshot 对接 WAL/段管理
  - MetaService：Apply 转发到 raft.FSM，Watch 基于路由表变更通知，Heartbeat 更新 members
- 修改 `cmd/unisearch/main.go`：同进程启动 HTTP + gRPC 双协议（不同端口）
- 依赖：`google.golang.org/grpc`（已在 go.mod indirect，提升为 direct）

#### G5: K8s Operator Reconciler (M2, 05 §8)

- 扩展 `deploy/k8s/operator/types.go`：补充 DeepCopy 方法、SchemeBuilder 注册
- 新增 `deploy/k8s/operator/controller.go`：Reconciler 逻辑
  - 创建/更新 StatefulSet（meta/coordinator/shard 三角色）
  - 创建 Headless Service（Raft 互发现）
  - 创建 ConfigMap（集群拓扑配置）
  - 状态回写：Phase/Ready/LeaderAddr
- 新增 `deploy/k8s/operator/main.go`：controller-runtime manager 启动入口
- 依赖：`sigs.k8s.io/controller-runtime`

#### G6: Online Split 数据迁移 (M2, 05 §3.2)

- 扩展 `internal/raft/split.go`：补齐 `AdvancePhase` 各阶段实际逻辑
  - Phase 2 (DualRead)：父分片冻结写入，子分片继承段引用 + DeleteBitmap 副本
  - Phase 3 (Catching)：子分片拉取父分片增量 WAL 并回放
  - Phase 4 (Complete)：原子切换路由表 gen，父分片段引用计数转移后下线
- 新增 `internal/raft/split_test.go`：端到端 split 集成测试（initiate → catchup → complete → verify routing）

#### G7: Cross-Link 增量合并 (M2, 02 §2.4.3)

- 扩展 `internal/storage/crosslink_merge.go`：补齐 `ExecuteMerge` 实际执行逻辑
  - 合并 posting lists + HNSW 图
  - 保留源段 SegmentID（crosslink 引用稳定）
  - 原子替换 SegmentSet 中的源段
  - 重建合并段的 crosslink 出边
- 新增 `internal/storage/crosslink_merge_test.go`：合并前后召回率验证

#### G8: 冷热分层迁移 (M2, 01 §9 + 02 §2.5)

- 扩展 `internal/storage/tiering/tier_manager.go`：补齐 `ExecuteMigration` 实际迁移逻辑
  - Hot→Warm：HNSW 图量化为 RaBitQ 1-bit 表示，释放内存
  - Warm→Cold：段文件上传对象存储，本地仅保留 footer 索引，按需加载
- 新增 `internal/storage/tiering/tier_test.go`：迁移端到端测试

#### G9: goccy/json 热路径加速 (M1, 00 §2)

- 依赖：`github.com/goccy/go-json` v0.10+
- 替换范围：
  - `internal/server/esapi/handlers.go`：所有 `json.Marshal`/`json.Unmarshal` → `jsoniter.Marshal`/`jsoniter.Unmarshal`（通过别名）
  - `internal/embedded/engine.go`：文档序列化/反序列化
  - `internal/embedded/docparse.go`：JSON→memtable.Row 解析
  - `internal/engine/dsl/parser.go`：DSL body 解析
- 方式：新增 `internal/jsonx/jsonx.go` 统一别名包，全局 import 替换

### 3.4 技术选型

| 组件 | 选型 | 版本基线 | 理由 |
|------|------|---------|------|
| Prometheus | `prometheus/client_golang` | v1.19+ | 设计 00 §2.1 指定 |
| OTel | `go.opentelemetry.io/otel` | v1.x | 设计 00 §2.1 指定，OTLP gRPC exporter |
| gRPC | `google.golang.org/grpc` | v1.84 (已有) | proto 已定义，生成 Go 代码 |
| K8s Operator | `sigs.k8s.io/controller-runtime` | v0.18+ | K8s 官方 controller 框架 |
| JSON | `github.com/goccy/go-json` | v0.10+ | 设计 00 §2.1 指定 M1 评估替换 |
| CI lint | `golangci-lint` | v1.60+ | 设计 06 §6 指定 |

## 4. 变更范围 (Impact)

### 4.1 新增文件

| 文件路径 | 说明 |
|----------|------|
| `internal/observability/metrics.go` | Prometheus 指标定义 + 注册 |
| `internal/observability/tracing.go` | OTel tracing 初始化 + span 工具 |
| `internal/observability/middleware.go` | HTTP metrics + tracing middleware |
| `internal/server/grpc/server.go` | gRPC server 实现 (ShardService + MetaService) |
| `internal/server/grpc/interceptor.go` | gRPC interceptor (tracing + metrics) |
| `api/proto/gen/*.go` | protoc 生成的 Go 代码 |
| `deploy/k8s/operator/controller.go` | Reconciler 逻辑 |
| `deploy/k8s/operator/main.go` | Operator 启动入口 |
| `deploy/k8s/operator/scheme.go` | SchemeBuilder + CRD 注册 |
| `internal/jsonx/jsonx.go` | goccy/json 统一别名包 |
| `scripts/ci.sh` | CI 统一入口 |
| `scripts/build.sh` | 交叉编译脚本 |
| `.golangci.yml` | golangci-lint 配置 |
| `internal/raft/split_test.go` | Online split 端到端测试 |
| `internal/storage/crosslink_merge_test.go` | Cross-Link 合并测试 |
| `internal/storage/tiering/tier_test.go` | 冷热分层迁移测试 |

### 4.2 修改文件

| 文件路径 | 修改内容 |
|----------|----------|
| `go.mod` | 新增 prometheus/client_golang、otel、goccy/go-json、controller-runtime 直接依赖 |
| `internal/server/esapi/router.go` | Server 结构体新增 metrics/tracing 字段，注册 /_prometheus/metrics 路由，middleware 链增加 observability |
| `internal/server/esapi/handlers.go` | json 包替换为 jsonx |
| `internal/embedded/engine.go` | json 包替换 + tracing span 注入 |
| `internal/embedded/docparse.go` | json 包替换 |
| `internal/engine/dsl/parser.go` | json 包替换 |
| `internal/storage/flush.go` | flush 指标埋点 |
| `internal/index/inverted/wand.go` | WAND pruned ratio 指标 |
| `internal/index/hnsw/` | hnsw search 指标 |
| `internal/tenant/tenant.go` | quota usage 指标 |
| `cmd/unisearch/main.go` | gRPC server 启动 + OTel 初始化 |
| `internal/raft/split.go` | 补齐 AdvancePhase 各阶段实际逻辑 |
| `internal/storage/crosslink_merge.go` | 补齐 ExecuteMerge 执行逻辑 |
| `internal/storage/tiering/tier_manager.go` | 补齐 ExecuteMigration 迁移逻辑 |
| `deploy/k8s/operator/types.go` | 补充 DeepCopy 方法 |
| `Makefile` | 新增 ci/coverage/cross-build/fuzz target |
| `.ai/02_ROADMAP.md` | In Progress 新增任务条目 |

### 4.3 不涉及

- 20 个"部分实现"功能点的补齐（本次仅聚焦 9 个明确缺口）
- M2 DoD 基础设施验证（混沌测试、10 亿基准报告）
- M3 生态功能
- GPU 加速 (W7.1)

## 5. 接口定义 (Interface Definition)

### 5.1 Prometheus Metrics Endpoint

```
GET /_prometheus/metrics
Content-Type: text/plain; version=0.0.4; charset=utf-8
```

返回 Prometheus exposition format，包含 `unisearch_*` 前缀五组指标 + `go_*` 运行时指标。

### 5.2 OTel Tracing Span 树

```
http.request (method, path, status, duration)
└─ query.plan (rules_hit, indices_count)
   └─ shard.search (shard_id, generation)
      └─ segment.scan / index.hnsw / index.bm25 / agg.partial
```

### 5.3 gRPC Service

proto 文件已定义（`api/proto/shard.proto` + `api/proto/meta.proto`），生成 Go 代码后实现 server 端。

### 5.4 CI/CD 入口

```bash
make ci          # lint + test + build（CI 阻塞门禁）
make coverage    # 生成覆盖率报告
make cross-build # 交叉编译 linux/amd64, linux/arm64, darwin/arm64
make fuzz         # 模糊测试种子运行
```

## 6. 稳定性架构 (Stability Architecture)

### 6.1 限流与熔断

- **Metrics endpoint**：`/_prometheus/metrics` 无限流，但仅绑定 management 端口（可选）
- **OTel exporter**：背压策略——exporter 队列满时丢弃 span，不阻塞业务路径
- **gRPC server**：复用现有 tenant 限流 + 背压机制

### 6.2 资源隔离与限制

- OTel exporter 异步批量发送，默认 30s flush，队列上限 2048 span
- Prometheus client 默认 registry，无额外内存开销
- gRPC server 独立端口，与 HTTP server 资源隔离

### 6.3 灾难恢复

- OTel collector 不可用时：exporter 自动降级，span 丢弃不影响业务
- Prometheus scrape 失败：仅影响监控，不影响业务
- gRPC server 崩溃：HTTP server 独立运行，嵌入式模式不受影响

## 7. 可测性设计 (Testability Design)

### 7.1 本地手动验证

- **Prometheus**：`make build && ./unisearch --embedded --listen :9200 &` → `curl localhost:9200/_prometheus/metrics` 验证指标输出
- **OTel**：启动 Jaeger（`docker run -p 4317:4317 jaegertracing/all-in-one`）→ 发送查询请求 → Jaeger UI 验证 span 树
- **gRPC**：`grpcurl localhost:9300 unisearch.ShardService/Get` 验证 gRPC 连通
- **CI**：`make ci` 本地完整运行 CI 流程
- **goccy/json**：`go test -bench BenchmarkSearch ./internal/server/esapi/` 验证性能提升

### 7.2 自动化测试策略

| 测试类型 | 覆盖范围 |
|----------|---------|
| Unit Tests | 指标注册、span 创建、goccy/json 往返、gRPC handler 逻辑 |
| Integration Tests | Online split 全流程、Cross-Link 合并 + 召回率验证、冷热分层迁移、gRPC 端到端 |
| Benchmarks | goccy/json vs encoding/json 对比基准 |

## 8. 风险与缓解 (Risks & Mitigation)

| 风险 | 缓解措施 |
|------|----------|
| goccy/json 兼容性问题（边缘 case 行为差异） | 通过 `internal/jsonx/jsonx.go` 统一别名，可一行回退到标准库 |
| OTel SDK 依赖引入增加二进制体积 | CGO_ENABLED=0 静态编译，体积增量 <5MB 可接受 |
| controller-runtime 依赖较重 | K8s Operator 独立二进制（cmd/operator/），不影响主二进制 |
| Online split WAL 回放复杂度高 | 先实现单分片测试场景，多分片场景逐步验证 |
| Cross-Link 合并召回率回退 | 合并后召回率 ≥ baseline - 2% 验证门禁 |
| gRPC 与 HTTP 并发端口管理 | 独立 listener，优雅关闭顺序：gRPC → HTTP |

## 9. 验收标准 (Acceptance Criteria)

- [ ] **业务**: 9 个缺口均有对应代码实现 + 测试覆盖
- [ ] **可观测性**: `/_prometheus/metrics` 返回五组指标；OTel trace 在 Jaeger 可见四级 span 树
- [ ] **分布式**: gRPC server 可接受 Write/Search/Get 请求；K8s Operator 可创建 StatefulSet
- [ ] **数据迁移**: Online split 完成后子分片可独立服务；Cross-Link 合并后召回率 ≥ baseline-2%；冷热分层可执行实际段迁移
- [ ] **工程**: `make ci` 全绿（lint+test+build）；`make cross-build` 产出三平台二进制
- [ ] **性能**: goccy/json 替换后 `_search` benchmark 无回退
- [ ] **审计覆盖率**: 117 功能点中完全实现占比 ≥92%（从 75.2% 提升）
