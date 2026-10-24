# Proposal: ES 功能补齐 — 全量功能差距分析与实施路线图

## 1. 背景与动机 (Background & Motivation)

### 1.1 当前问题

通过系统对比 Elasticsearch 8.x 代码库（`/Users/zhb/workspace/elasticsearch`，611 个 API spec、40+ 种 Query DSL、30+ 种聚合）与 uni-search 当前实现，发现 uni-search 在 9 个功能域存在 50+ 个功能缺口：

- **Query DSL**：28 种查询类型未实现（`prefix`/`regexp`/`function_score`/`nested`/`geo_distance` 等），返回 `unsupported_capability` 错误
- **搜索增强**：`rescore`/`collapse`/`post_filter`/`search_template`/`runtime_mappings` 未接入管线（`RescoreOp`/`CollapseOp` 已实现但未接入）
- **索引管理**：缺少 `_open`/`_close`/`_stats`/`_segments`/`_shrink`/`_split`/`_rollover`/`_resolve/index` 等 14 个 API
- **运维 API**：缺少 `_cluster/state`/`_cluster/stats`/`_nodes/stats`/`_nodes/info` 等 15+ 个 API，`_cat` 仅有 4 个
- **快照恢复**：无 ES 风格 `_snapshot`/`_restore` API（仅有 Raft 快照 + WAL 回放）
- **Ingest Pipeline**：完全缺失数据摄入管道框架
- **异步搜索**：无 `_async_search` API
- **同义词管理**：无 `_synonyms` REST API
- **Term Vectors**：无 `_termvectors`/`_mtermvectors` API

### 1.2 用户期望

ES 存量用户迁移到 uni-search 时，期望 REST API 100% 兼容 ES 8.x。当前缺失的功能导致：
- 部分 ES 客户端库因查询类型不支持而报错
- 运维工具（Kibana、Cerebro 等）因 `_cluster/state`、`_nodes/stats` 等缺失无法连接
- 数据管理工具因 `_snapshot`/`_restore` 缺失无法备份恢复
- 日志接入因 Ingest Pipeline 缺失需要额外预处理层

## 2. 目标 (Objective)

1. **补齐 9 个功能域的 P0 核心功能**，使 ES 兼容 API 覆盖率从当前 ~65% 提升至 ~90%
2. **建立分域并行开发框架**，Engine 接口拆分为子接口，支持 9 个域独立并行开发
3. **输出完整 P0-P3 实施路线图**，约 87 人天工作量，分 4 个阶段交付

## 3. 方案设计 (Approach)

### 3.1 设计原则

- **ES 兼容优先**：所有 API 响应格式严格遵循 ES 8.x spec，使用 ES golden test 验证
- **分域并行**：9 个功能域可独立并行开发，每域内 P0→P3 串行
- **复用已有基础设施**：`RescoreOp`/`CollapseOp`（已实现）、FST 前缀迭代、`scalar.GeoSearcher`、Script 引擎、`objstore.S3Client`、`PITManager` 模式等
- **接口拆分**：Engine 接口拆分为 `SearchEngine`/`DocEngine`/`IndexAdminEngine`/`ClusterEngine`/`SnapshotEngine` 子接口

### 3.2 核心方案 — Engine 接口拆分

```go
// SearchEngine — 搜索与查询
type SearchEngine interface {
    Search(ctx, indices, body) (*SearchResult, error)
    Count(ctx, indices, body) (int64, error)
    TermVectors(ctx, req) (*TermVectorsResponse, error)
    ValidateQuery(ctx, index, body) (*ValidateResult, error)
    SearchShards(ctx, indices) (*SearchShardsResult, error)
}

// IndexAdminEngine — 索引管理
type IndexAdminEngine interface {
    PutMapping / GetMapping / PutSettings / GetSettings
    PutAlias / RemoveAlias / GetAliases
    OpenIndex / CloseIndex / IndexStats / Segments
    ShrinkIndex / SplitIndex / CloneIndex / RolloverIndex
    ResolveIndex / ClearCache / RecoveryInfo / ShardStores
    Analyze / Refresh / Indices / CurrentSnapshot
    RunPipeline(ctx, doc, pipelineID) error
}

// ClusterEngine — 集群运维
type ClusterEngine interface {
    Health / ClusterState / ClusterStats
    NodesStats / NodesInfo / HotThreads
    GetClusterSettings / PutClusterSettings
    PendingTasks / ClusterReroute / AllocationExplain
}

// SnapshotEngine — 快照恢复
type SnapshotEngine interface {
    PutSnapshotRepo / GetSnapshotRepos / DeleteSnapshotRepo / VerifySnapshotRepo
    CreateSnapshot / GetSnapshots / DeleteSnapshot / SnapshotStatus
    RestoreSnapshot / CloneSnapshot / CleanupRepository
}

// Engine — 复合接口
type Engine interface {
    SearchEngine
    DocEngine
    IndexAdminEngine
    ClusterEngine
    SnapshotEngine
}
```

### 3.3 九域实施概要

#### 域一：Query DSL 补齐（20 个查询类型）

**实现链路**：`ast.go` 定义 → `parser.go` 解析 + `unsupportedQueries` 移除 → `translator.go` 翻译 → `planner.go` memtable 分发 → `docMatchesQuery` 匹配

| 优先级 | 查询类型 | 复杂度 | 关键依赖 |
|--------|---------|--------|---------|
| P0 | prefix / regexp / constant_score / boosting / dis_max | S/M/S/S/S | FST `Iterate(prefix)`、Go `regexp`、bool.filter 语义 |
| P1 | function_score / script_score / geo_distance / geo_bounding_box | L/M/M/S | Script 引擎、`scalar.GeoSearcher`（已就绪） |
| P2 | more_like_this / nested(原生) / simple_query_string / combined_fields / match_bool_prefix | L/XL/M/M/S | TF-IDF 特征提取、存储模型改造、递归下降解析器 |
| P3 | distance_feature / rank_feature / pinned / terms_set / intervals / wrapper | M/S/M/M/XL/S | haversine 距离、positions 数据 |

**nested 原生实现关键步骤**（用户选择原生方案）：
1. `meta/mapping.go`：ObjectMapper 新增 `nested: bool` 字段
2. `storage/segment/builder.go`：nested 字段数组元素作为子文档独立索引（带 `_parent_id`/`_nested_path`）
3. `internal/index/inverted/`：子文档共享父文档 `_id` 命名空间（`{parent_id}#{path}#{index}`）
4. 新建 `internal/engine/nested/nested.go`：在子文档空间执行 query，映射回父文档
5. 评分按 `score_mode`（avg/max/sum/none）合并

#### 域二：搜索功能增强（10 个功能点）

| 优先级 | 功能 | 复杂度 | 关键集成点 |
|--------|------|--------|-----------|
| P0 | rescore / collapse+inner_hits / post_filter | M/M/S | `RescoreOp`/`CollapseOp`（已实现）管线接入、`postFilterHits`（已实现） |
| P1 | search_template / _validate/query / docvalue_fields / stored_fields | M/S/S/S | Mustache 渲染、`ParseSearchBodyFull` 验证 |
| P2 | runtime_mappings / script_fields / _search_shards | L/M/S | Script 引擎评估注入 |

#### 域三：索引管理 API（14 个 API）

| 优先级 | API | 复杂度 |
|--------|-----|--------|
| P0 | _open/_close / _stats / _segments | S/M/M |
| P1 | _shrink / _split / _rollover / _clone / _resolve/index | L/L/M/M/S |
| P2 | _cache/clear / _recovery / _shard_stores | S/M/M |
| P3 | _reload_search_analyzers / _disk_usage / 数据流 | S/L/XL |

#### 域四：运维 API（15+ 个 API）

| 优先级 | API | 复杂度 |
|--------|-----|--------|
| P0 | _cluster/state / _cluster/stats / _nodes/stats / _nodes/info | M/M/M/S |
| P1 | _cluster/settings + _cat 扩展（aliases/segments/recovery/templates/tasks/thread_pool） | S/M |
| P2 | _cluster/pending_tasks / _nodes/hot_threads / _cluster/reroute / _cluster/allocation/explain | S/M/M/M |

#### 域五：快照/恢复 API（ES 风格全量）

| 优先级 | API | 复杂度 | 后端 |
|--------|-----|--------|------|
| P0 | 仓库管理 + 快照创建/恢复 | L/XL/XL | FS + S3（复用 `objstore.S3Client`） |
| P1 | 删除快照 / 状态 / 验证仓库 | S/S/S | — |
| P2 | 克隆快照 / 清理仓库 / _features | M/S/S | — |

**快照核心机制**：段引用计数（快照期间 merge 跳过被引用段）、`wait_for_completion` 异步支持、索引重命名。

#### 域六：Ingest Pipeline

| 优先级 | 功能 | 复杂度 |
|--------|------|--------|
| P0 | Pipeline 框架 + REST API + 写入集成 | L |
| P1 | 基础 Processor（Set/Convert/Remove/Rename/Append/Grok/Dissect/Json/Split/Join/Upper/Lower/Trim）+ 条件处理 | L/M |
| P2 | 高级 Processor（Date/Script/Fingerprint/ForEach/Sort/HtmlStrip/URLDecode） | M |
| P3 | 插件机制（sidecar gRPC） | M |

**写入集成**：`IndexDoc` 新增 `Pipeline string`；`handleIndexDoc`/`handleBulk` 读取 `?pipeline=` 参数；`embedded.Engine.Write` 在 `shard.Write` 前执行 Pipeline。

#### 域七：异步搜索

复用 `PITManager` 模式（mutex + map + 过期清理 goroutine + LRU）。

| 优先级 | 功能 | 复杂度 |
|--------|------|--------|
| P0 | Submit/Get/Status/Delete API | M |
| P1 | 结果存储 + keep_alive 清理 + 进度跟踪 | S |
| P2 | 并发控制（信号量，默认 50） | S |

#### 域八：同义词管理

| 优先级 | 功能 | 复杂度 |
|--------|------|--------|
| P0 | 同义词集 CRUD API（`PUT/GET/DELETE /_synonyms/{id}`） | S |
| P1 | 单条规则管理 + `SynonymFilter`（`TokenFilter` 接口）+ analyzer 集成 + 热更新 | M |

#### 域九：Term Vectors

| 优先级 | 功能 | 复杂度 |
|--------|------|--------|
| P0 | 单文档 + 批量 API（`GET/POST /{index}/_termvectors/{id}`、`GET/POST /_mtermvectors`） | L |
| P1 | 参数支持（fields/offsets/positions/term_statistics/field_statistics） | S |

**实现策略**：优先重新分析 _source 获取 term vectors（与 ES 未存储 term_vector 时行为一致），仅在 `term_statistics=true` 时查询倒排索引获取 doc_freq。

### 3.4 技术选型

| 决策 | 选择 | 理由 |
|------|------|------|
| Engine 接口组织 | 拆分子接口 | 避免单接口 56+ 方法爆炸，handler 按需接收子接口 |
| nested 查询 | 原生文档索引 | 性能最优，支持复杂 nested 查询语义 |
| 快照后端 | FS + S3 双后端 | 覆盖本地开发和云原生部署场景 |
| Grok 模式匹配 | Go `regexp` + 预定义模式库 | 无需引入 ANTLR，预编译缓存优化性能 |
| 模板渲染 | Mustache 子集 | 覆盖 95% 用例（变量/section/循环） |
| 异步搜索存储 | 内存（sync.Map） | 与 PIT 一致，P0 阶段可接受重启丢失 |

## 4. 变更范围 (Impact)

### 4.1 新增文件（35 个）

| 文件路径 | 域 | 优先级 |
|---------|---|--------|
| `internal/server/esapi/admin_handlers.go` | 三 | P0 |
| `internal/server/esapi/cluster_handlers.go` | 四 | P0 |
| `internal/server/esapi/snapshot_handlers.go` | 五 | P0 |
| `internal/server/esapi/cat_handlers.go` | 四 | P1 |
| `internal/server/esapi/ingest_handlers.go` | 六 | P0 |
| `internal/server/esapi/async_search.go` | 七 | P0 |
| `internal/server/esapi/synonym_handlers.go` | 八 | P0 |
| `internal/server/esapi/termvectors.go` | 九 | P0 |
| `internal/server/esapi/template_handlers.go` | 二 | P1 |
| `internal/server/esapi/validate_handler.go` | 二 | P1 |
| `internal/server/esapi/datastream_handlers.go` | 三 | P3 |
| `internal/snapshot/repo.go` | 五 | P0 |
| `internal/snapshot/repo_fs.go` | 五 | P0 |
| `internal/snapshot/repo_s3.go` | 五 | P0 |
| `internal/snapshot/snapshot.go` | 五 | P0 |
| `internal/snapshot/restore.go` | 五 | P0 |
| `internal/ingest/pipeline.go` | 六 | P0 |
| `internal/ingest/processor.go` | 六 | P0 |
| `internal/ingest/execute.go` | 六 | P0 |
| `internal/ingest/store.go` | 六 | P0 |
| `internal/ingest/processors_basic.go` | 六 | P1 |
| `internal/ingest/processors_text.go` | 六 | P1 |
| `internal/ingest/processors_advanced.go` | 六 | P2 |
| `internal/synonyms/model.go` | 八 | P0 |
| `internal/synonyms/store.go` | 八 | P0 |
| `internal/index/analyzer/synonym_filter.go` | 八 | P1 |
| `internal/engine/scorefunction/scorefunction.go` | 一 | P1 |
| `internal/engine/mlt/mlt.go` | 一 | P2 |
| `internal/engine/nested/nested.go` | 一 | P2 |
| `internal/engine/template/template.go` | 二 | P1 |
| `internal/engine/runtime/runtime.go` | 二 | P2 |
| `internal/engine/dsl/sqs_parser.go` | 一 | P2 |
| `internal/embedded/termvectors.go` | 九 | P0 |
| `internal/meta/datastream.go` | 三 | P3 |
| `internal/embedded/datastream.go` | 三 | P3 |

### 4.2 修改文件（14 个）

| 文件路径 | 修改内容 |
|---------|----------|
| `internal/server/esapi/engine.go` | Engine 接口拆分为子接口 + 新增 ~36 个方法声明 + EngineNoop 更新 |
| `internal/server/esapi/router.go` | 注册 ~60 条新路由 |
| `internal/engine/dsl/parser.go` | SearchExtras 扩展（Rescore/Collapse/PostFilter/DocvalueFields/RuntimeMappings）+ dispatchQuery 注册 + unsupportedQueries 移除 |
| `internal/engine/dsl/ast.go` | 新增 ~20 个 AST 结构体 |
| `internal/embedded/translator.go` | 新增 ~20 个 translate 函数 |
| `internal/embedded/engine.go` | Search 方法管线扩展 + 新方法实现 |
| `internal/embedded/planner.go` | memtable 搜索分发 + docMatchesQuery 扩展 |
| `internal/embedded/manager.go` | indexState closed 标志 + shrink/split/clone/rollover |
| `internal/server/esapi/handlers.go` | handleIndexDoc/handleBulk pipeline 参数 |
| `internal/meta/mapping.go` | nested 类型支持 |
| `internal/storage/segment/builder.go` | nested 子文档独立索引 |
| `internal/index/analyzer/factory.go` | 同义词过滤器集成 |
| `internal/errors/codes.go` | 新增错误类型（IndexClosed/SnapshotFailed 等） |
| `internal/config/config.go` | ClusterSettings + SnapshotConfig 字段 |

### 4.3 不涉及

- **Security/认证授权**：不实现 ES x-pack security 模块（410 文件），由外部网关认证
- **机器学习/Watch/SQL/EQL**：不实现 ES x-pack ML/watcher/sql/eql 插件
- **Span 查询**：span_term/span_near 等 Lucene span 查询不在本次范围
- **Cross-cluster search/replication**：跨集群搜索和复制不在本次范围
- **ES|QL 查询语言**：不实现新的 ES|QL 查询语言

## 5. 接口定义 (Interface Definition)

### 5.1 新增 REST API 路由（~60 条）

#### 索引管理
```
POST /{index}/_open          POST /{index}/_close
GET  /{index}/_stats         GET  /_stats
GET  /{index}/_segments       GET  /_segments
POST /{index}/_shrink/{target}  POST /{index}/_split/{target}
POST /{alias}/_rollover/{target}  POST /{index}/_clone/{target}
GET  /_resolve/index/{name}   POST /{index}/_cache/clear
GET  /{index}/_recovery       GET  /{index}/_shard_stores
```

#### 集群运维
```
GET  /_cluster/state          GET  /_cluster/state/{metric}
GET  /_cluster/stats         GET  /_nodes/stats  GET  /_nodes/info
GET  /_cluster/settings       PUT  /_cluster/settings
GET  /_cluster/pending_tasks  GET  /_nodes/hot_threads
POST /_cluster/reroute        GET  /_cluster/allocation/explain
GET  /_cat/aliases  GET  /_cat/segments  GET  /_cat/recovery
GET  /_cat/templates  GET  /_cat/tasks  GET  /_cat/thread_pool
```

#### 快照恢复
```
PUT  /_snapshot/{repo}        GET  /_snapshot   DELETE /_snapshot/{repo}
PUT  /_snapshot/{repo}/{snapshot}  GET  /_snapshot/{repo}  DELETE /_snapshot/{repo}/{snapshot}
POST /_snapshot/{repo}/{snapshot}/_restore  GET  /_snapshot/{repo}/{snapshot}/_status
POST /_snapshot/{repo}/_verify  POST /_snapshot/{repo}/{snapshot}/_clone
POST /_snapshot/{repo}/_cleanup  GET  /_features
```

#### Ingest Pipeline
```
PUT  /_ingest/pipeline/{id}   GET  /_ingest/pipeline  GET  /_ingest/pipeline/{id}
DELETE /_ingest/pipeline/{id}  POST /_ingest/pipeline/{id}/_simulate  POST /_ingest/pipeline/_simulate
```

#### 异步搜索
```
POST /_async_search           GET  /_async_search/{id}
GET  /_async_search/status/{id}  DELETE /_async_search/{id}
```

#### 同义词管理
```
PUT  /_synonyms/{id}          GET  /_synonyms     GET  /_synonyms/{id}
DELETE /_synonyms/{id}        PUT  /_synonyms/{id}/{rule_id}
GET  /_synonyms/{id}/{rule_id}  DELETE /_synonyms/{id}/{rule_id}
```

#### Term Vectors
```
GET  /{index}/_termvectors/{id}  POST /{index}/_termvectors/{id}
GET  /_mtermvectors           POST /_mtermvectors
```

#### 搜索增强
```
POST /_search/template        POST /{index}/_search/template
POST /_msearch/template       POST /_render/template
GET  /{index}/_validate/query  POST /{index}/_validate/query
GET  /{index}/_search_shards  POST /{index}/_search_shards
PUT  /_scripts/{id}           GET  /_scripts/{id}  DELETE /_scripts/{id}
```

## 6. 稳定性架构 (Stability Architecture)

### 6.1 限流与熔断

- **异步搜索**：信号量限制并发数（默认 50），超限返回 429
- **Ingest Pipeline**：Pipeline 执行 step budget 限制（100k 步），防止条件表达式无限循环
- **快照创建**：`wait_for_completion=false` 异步模式避免长查询阻塞

### 6.2 资源隔离与限制

- **Pipeline 执行**：单文档处理超时（默认 60s），`ignore_failure` 机制防止单条失败阻塞批量写入
- **快照段引用**：引用计数机制，快照期间 merge 调度器跳过被引用段
- **异步搜索结果**：LRU 驱逐（最大 10000）+ keep_alive 过期清理

### 6.3 灾难恢复

- **快照仓库注册**：持久化到 `{dataDir}/snapshot-repos.json`，重启后自动恢复
- **Pipeline/同义词**：M0 阶段内存存储（sync.Map），M2 扩展为 Raft FSM 命令持久化
- **异步搜索结果**：内存存储，重启丢失（与 PIT 一致），M2 可持久化到内部索引

## 7. 可测性设计 (Testability Design)

### 7.1 本地手动验证

以 rescore + collapse 为例：
```bash
# 写入测试数据
curl -X POST http://localhost:9200/test/_bulk -H 'Content-Type: application/x-ndjson' -d '
{"index": {"_id": "1"}}
{"category": "A", "title": "hello world", "score": 1.0}
{"index": {"_id": "2"}}
{"category": "A", "title": "hello uni", "score": 2.0}
{"index": {"_id": "3"}}
{"category": "B", "title": "hello search", "score": 3.0}
'

# 验证 rescore
curl -X POST http://localhost:9200/test/_search -H 'Content-Type: application/json' -d '{
  "query": {"match": {"title": "hello"}},
  "rescore": [{"query": {"match": {"title": "uni"}}, "window_size": 10, "query_weight": 0.3, "rescore_query_weight": 0.7}]
}'

# 验证 collapse
curl -X POST http://localhost:9200/test/_search -H 'Content-Type: application/json' -d '{
  "query": {"match": {"title": "hello"}},
  "collapse": {"field": "category"}
}'
```

### 7.2 自动化测试策略

- **Unit Tests**：每个新 DSL 解析器、Processor、SnapshotRepo 方法都有单元测试
- **Integration Tests**：使用 ES golden test YAML 验证 API 兼容性；每个新 API 端点至少 3 个集成测试（正常/边界/错误）
- **回归测试**：`go build ./...` + `go test ./...` + `go vet ./...` 全绿

## 8. 风险与缓解 (Risks & Mitigation)

| 风险 | 缓解措施 |
|------|----------|
| Engine 接口爆炸（~56 方法） | 拆分为 5 个子接口，handler 按需接收子接口 |
| nested 原生实现需修改存储模型 | 独立阶段实施，先实现 nested 文档写入再实现查询，充分回归测试 |
| 快照创建期间段被 merge 删除 | 引入段引用计数，merge 调度器跳过被引用段（类似 PIT 机制） |
| JVM 相关字段无 Go 对等概念 | 返回合理占位值，在响应中标注 Go runtime 信息 |
| Grok 正则 ReDoS | 预编译正则缓存 + 匹配超时 + 正则长度限制 |
| 大索引统计/快照耗时 | 支持 `wait_for_completion=false` 异步模式 + 统计结果缓存 |
| 数据流引入写入路径变更 | 仅匹配数据流名时额外检查，不影响普通索引写入路径 |
| Pipeline 执行失败导致写入中断 | `ignore_failure` + `on_failure` 机制，默认标记失败但继续处理 |
| EngineNoop 需实现 ~36 个新方法 | 按子接口分组生成空实现，使用代码生成减少手工编写 |

## 9. 验收标准 (Acceptance Criteria)

- [ ] **业务**: Engine 接口拆分为 5 个子接口，~60 条新路由注册，9 个功能域 P0 功能全部实现
- [ ] **兼容性**: 新增 API 响应格式通过 ES 8.x golden test 验证
- [ ] **稳定性**: 异步搜索并发控制（信号量 50）、Pipeline step budget 限制、快照段引用计数
- [ ] **运维**: 新增 Prometheus 指标覆盖快照/恢复/Pipeline/异步搜索
- [ ] **可测性**: 每个新 API 至少 3 个集成测试，`go build/test/vet` 全绿
- [ ] **回归**: 现有 163 Python 集成测试 + 46 Go 测试全部通过

## 10. 分阶段实施路线图

| 阶段 | 范围 | 估时 | 可并行数 |
|------|------|------|---------|
| 前置 | Engine 接口重构 | 1.5 天 | — |
| P0 | 9 域核心功能 | ~25 天 | 3-4 人 |
| P1 | 9 域重要功能 | ~22 天 | 3-4 人 |
| P2 | 9 域增强功能 | ~18 天 | 2-3 人 |
| P3 | 9 域高级功能 | ~22 天 | 2-3 人 |
| **合计** | | **~87 天** | |
