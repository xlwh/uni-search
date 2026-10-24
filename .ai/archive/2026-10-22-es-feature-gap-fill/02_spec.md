# Spec: ES 功能补齐 — 详细工程设计

## 1. 概述 (Overview)

本 Spec 基于 Proposal `2026-10-22-es-feature-gap-fill`，定义 9 个功能域、50+ 个功能点的详细工程实现。分 P0→P3 四阶段交付，P0 核心功能为本 Spec 重点。

### 设计决策（spec-plan 评审确认）

| # | 决策点 | 选择 | 影响 |
|---|--------|------|------|
| D1 | Engine 接口组织 | 复合接口 + 分文件实现 | 不拆分子接口，新方法按域分文件添加到 `embedded.Engine` |
| D2 | rescore/collapse 接入 | operator pipeline | 修改 `RescoreOp`/`CollapseOp` 接受子 plan，在 planner 中构建 |
| D3 | Ingest Pipeline 执行 | Engine.Write 内部 | `embedded.Engine.Write` 在 `shard.Write` 前执行 Pipeline |
| D4 | RescoreOp DSL 执行 | 子 plan 传入 | `RescoreOp` 持有并执行另一个 `Operator`（rescore query 子 plan） |
| D5 | 快照段保护 | 引用计数到段 | `SegmentEntry` 新增 `refCount`，merge 删除段文件前检查引用 |
| D6 | 错误返回模式 | 返回 `*errors.APIError` | 新增 Engine 方法返回 `*errors.APIError`，handler 直接传给 `writeError` |

### 变更清单

| 类型 | 文件路径 | 说明 |
|------|----------|------|
| 修改 | `internal/server/esapi/engine.go` | Engine 接口新增 ~36 方法 + EngineNoop 空实现 |
| 修改 | `internal/server/esapi/router.go` | 注册 ~60 条新路由 |
| 修改 | `internal/engine/dsl/parser.go` | SearchExtras 扩展 + dispatchQuery + unsupportedQueries |
| 修改 | `internal/engine/dsl/ast.go` | 新增 ~20 个 AST 结构体 |
| 修改 | `internal/embedded/translator.go` | 新增 ~20 个 translate 函数 |
| 修改 | `internal/embedded/engine.go` | Search 管线扩展 + 新方法实现 |
| 修改 | `internal/embedded/planner.go` | memtable 搜索分发 + docMatchesQuery |
| 修改 | `internal/embedded/manager.go` | indexState closed + shrink/split/clone |
| 修改 | `internal/engine/exec/operators.go` | RescoreOp 改为持有子 Operator |
| 修改 | `internal/storage/segmentset.go` | SegmentEntry 新增 refCount |
| 修改 | `internal/storage/merge.go` | merge 删除段前检查 refCount |
| 修改 | `internal/server/esapi/handlers.go` | handleIndexDoc/handleBulk pipeline 参数 |
| 修改 | `internal/meta/mapping.go` | nested 类型支持 |
| 修改 | `internal/storage/segment/builder.go` | nested 子文档独立索引 |
| 修改 | `internal/index/analyzer/factory.go` | 同义词过滤器集成 |
| 修改 | `internal/errors/codes.go` | 新增错误类型 |
| 修改 | `internal/config/config.go` | ClusterSettings + SnapshotConfig |
| 新增 | `internal/server/esapi/admin_handlers.go` | 索引管理 handler |
| 新增 | `internal/server/esapi/cluster_handlers.go` | 集群运维 handler |
| 新增 | `internal/server/esapi/snapshot_handlers.go` | 快照 handler |
| 新增 | `internal/server/esapi/cat_handlers.go` | _cat 扩展 handler |
| 新增 | `internal/server/esapi/ingest_handlers.go` | Ingest handler |
| 新增 | `internal/server/esapi/async_search.go` | 异步搜索 manager + handler |
| 新增 | `internal/server/esapi/synonym_handlers.go` | 同义词 handler |
| 新增 | `internal/server/esapi/termvectors.go` | Term Vectors handler |
| 新增 | `internal/server/esapi/template_handlers.go` | 模板搜索 handler |
| 新增 | `internal/server/esapi/validate_handler.go` | _validate/query handler |
| 新增 | `internal/snapshot/repo.go` | 快照仓库接口 + 注册管理 |
| 新增 | `internal/snapshot/repo_fs.go` | FS 仓库实现 |
| 新增 | `internal/snapshot/repo_s3.go` | S3 仓库实现 |
| 新增 | `internal/snapshot/snapshot.go` | 快照创建逻辑 |
| 新增 | `internal/snapshot/restore.go` | 快照恢复逻辑 |
| 新增 | `internal/ingest/pipeline.go` | Pipeline 定义 + Processor 接口 |
| 新增 | `internal/ingest/processor.go` | Processor 注册表 + 工厂 |
| 新增 | `internal/ingest/execute.go` | Pipeline 执行引擎 |
| 新增 | `internal/ingest/store.go` | PipelineStore (sync.Map) |
| 新增 | `internal/ingest/processors_basic.go` | 基础 Processor (P1) |
| 新增 | `internal/ingest/processors_text.go` | 文本 Processor (P1) |
| 新增 | `internal/synonyms/model.go` | SynonymSet 数据模型 |
| 新增 | `internal/synonyms/store.go` | SynonymStore (sync.Map) |
| 新增 | `internal/index/analyzer/synonym_filter.go` | 同义词 TokenFilter (P1) |
| 新增 | `internal/engine/scorefunction/scorefunction.go` | 评分函数 (P1) |
| 新增 | `internal/engine/nested/nested.go` | nested 查询执行 (P2) |
| 新增 | `internal/engine/template/template.go` | Mustache 渲染 (P1) |
| 新增 | `internal/embedded/termvectors.go` | Term Vectors 提取 |
| 新增 | `internal/embedded/admin_impl.go` | 索引管理方法实现（分文件） |
| 新增 | `internal/embedded/cluster_impl.go` | 集群运维方法实现（分文件） |
| 新增 | `internal/embedded/snapshot_impl.go` | 快照方法实现（分文件） |
| 新增 | `internal/embedded/ingest_impl.go` | Ingest 方法实现（分文件） |

---

## 2. 详细设计 — P0 核心功能

### 2.1 前置：Engine 接口扩展模式（D1）

**变更内容**：保持 `Engine` 为复合接口，新增方法按域分文件实现。

**engine.go 修改**：

```go
// Engine 接口新增方法（按域分组注释）
type Engine interface {
    // === 现有方法保持不变 ===
    Write(ctx context.Context, doc IndexDoc, refresh string) (*WriteResult, error)
    // ... 其余现有方法 ...

    // === 域一: Search 增强 ===
    ValidateQuery(ctx context.Context, index string, body []byte) (*ValidateResult, error)

    // === 域三: Index Admin ===
    OpenIndex(ctx context.Context, index string) *errors.APIError
    CloseIndex(ctx context.Context, index string) *errors.APIError
    IndexStats(ctx context.Context, indices []string, metricGroups []string) (*IndexStatsResult, error)
    Segments(ctx context.Context, indices []string) (*SegmentsResult, error)
    ResolveIndex(ctx context.Context, names []string) (*ResolveIndexResult, error)

    // === 域四: Cluster ===
    ClusterState(ctx context.Context, metric, index []string) ([]byte, error)
    ClusterStats(ctx context.Context) (*ClusterStatsResult, error)
    NodesStats(ctx context.Context, nodes, metric []string) (*NodesStatsResult, error)
    NodesInfo(ctx context.Context, nodes, metric []string) (*NodesInfoResult, error)

    // === 域五: Snapshot ===
    PutSnapshotRepo(ctx context.Context, repo string, body []byte) *errors.APIError
    GetSnapshotRepos(ctx context.Context, repos []string) (*SnapshotReposResult, error)
    DeleteSnapshotRepo(ctx context.Context, repo string) *errors.APIError
    CreateSnapshot(ctx context.Context, repo, snapshot string, body []byte) (*SnapshotInfo, error)
    GetSnapshots(ctx context.Context, repo string, snapshots []string) (*GetSnapshotsResult, error)
    RestoreSnapshot(ctx context.Context, repo, snapshot string, body []byte) (*RestoreResult, error)

    // === 域六: Ingest ===
    PutPipeline(ctx context.Context, id string, body []byte) *errors.APIError
    GetPipeline(ctx context.Context, id string) (*PipelineConfig, error)
    DeletePipeline(ctx context.Context, id string) *errors.APIError
    SimulatePipeline(ctx context.Context, id string, body []byte) (*SimulateResult, error)

    // === 域九: Term Vectors ===
    TermVectors(ctx context.Context, req TermVectorsRequest) (*TermVectorsResponse, error)
}
```

**EngineNoop 扩展**：每个新方法返回 `errNotImplemented` 或 `nil, errNotImplemented`。

**分文件实现**：
- `internal/embedded/admin_impl.go` — `OpenIndex`/`CloseIndex`/`IndexStats`/`Segments`/`ResolveIndex`
- `internal/embedded/cluster_impl.go` — `ClusterState`/`ClusterStats`/`NodesStats`/`NodesInfo`
- `internal/embedded/snapshot_impl.go` — 快照仓库 + 快照 CRUD + 恢复
- `internal/embedded/ingest_impl.go` — Pipeline 管理 + 写入集成

---

### 2.2 域一 P0：Query DSL 补齐（5 个查询类型）

**实现链路**：`ast.go` → `parser.go` → `translator.go` → `planner.go`

#### 2.2.1 prefix 查询

```go
// ast.go
type PrefixQuery struct {
    Field string
    Value string
    Boost float32
}
func (*PrefixQuery) queryNode() {}

// parser.go — dispatchQuery 新增
case "prefix":
    return parsePrefixQuery(c, raw)

// parser.go — 实现
func parsePrefixQuery(c *parseCtx, raw jsonx.RawMessage) (Query, error) {
    // 支持 {"prefix": {"field": "value"}} 和 {"prefix": {"field": {"value": "pre", "boost": 2.0}}}
}
```

**translator.go**：
```go
case *dsl.PrefixQuery:
    return translatePrefix(ctx, idx, q)
// 对 text 字段：FST.Iterate(prefix) 收集 TermID → 多 term BM25 plan
// 对 keyword 字段：ScanOp + PredFilter(field LIKE prefix%)
// term 数量上限 1024，超限时截断并记录 warning
```

**planner.go**：
```go
// searchMemtableByQuery 新增
case *dsl.PrefixQuery:
    // 对 memtable 文档的 _source 检查 field 值是否以 prefix 开头
// docMatchesQuery 新增
case *dsl.PrefixQuery:
    // 从 _source 提取 field 值，strings.HasPrefix 检查
```

#### 2.2.2 regexp 查询

```go
type RegexpQuery struct {
    Field   string
    Pattern string
    Flags   string // "ALL"/"COMPLEMENT"/"INTERSECTION"/"ANYSTRING"
    Boost   float32
}
```

**翻译策略**：遍历 `TermDict.Iterate(nil)` 全量 term，用 Go `regexp.Compile` 过滤匹配的 term，收集 posting list 合并。添加 `max_determinized_states` 参数限制（默认 10000）。

#### 2.2.3 constant_score 查询

```go
type ConstantScoreQuery struct {
    Filter Query  // 内部查询
    Boost  float32 // 默认 1.0
}
```

**翻译策略**：内部 query 翻译为 filter 语义（不贡献评分），所有匹配文档得分 = Boost。复用 `translateBool` 中 filter 子句的处理逻辑。

#### 2.2.4 boosting 查询

```go
type BoostingQuery struct {
    Positive      Query
    Negative      Query
    NegativeBoost float32
}
```

**翻译策略**：positive 正常翻译评分；negative 匹配的文档得分 × NegativeBoost。在 memtable 路径中：文档匹配 positive 且不匹配 negative → 正常评分；匹配 positive 且匹配 negative → 评分 × NegativeBoost。

#### 2.2.5 dis_max 查询

```go
type DisMaxQuery struct {
    Queries    []Query
    TieBreaker float32
    Boost      float32
}
```

**翻译策略**：各子查询分别翻译为子 plan，在评分阶段取最高分 + TieBreaker × 其余分。可复用 `fusion` 包的 `ScoredDoc` 合并逻辑，使用 dis_max 策略。

---

### 2.3 域二 P0：rescore + collapse + post_filter

#### 2.3.1 RescoreOp 改造（D2 + D4）

**operators.go 修改**：

```go
// RescoreOp 改为持有子 Operator（rescore query 的 plan）
type RescoreOp struct {
    input         Operator       // 主查询 pipeline
    rescorePlan   Operator       // rescore query 子 plan（D4: 子 plan 传入）
    queryWeight   float32
    rescoreWeight float32
    window        int
    out           *ColumnBatch
    done          bool
    collected     []topNCand
}

// NewRescore 修改签名：接受 Operator 替代回调函数
func NewRescore(input, rescorePlan Operator, queryWeight, rescoreWeight float32, window int) *RescoreOp

// Next 方法中：对 collected 前 window 个 hits，执行 rescorePlan.Next() 获取 rescore 评分
// final = queryWeight * primary + rescoreWeight * rescore
```

**parser.go 修改**：

```go
// SearchExtras 新增字段
type SearchExtras struct {
    // ... 现有字段 ...
    Rescore     jsonx.RawMessage // P0: rescore JSON
    Collapse    jsonx.RawMessage // P0: collapse JSON
    PostFilter  jsonx.RawMessage // P0: post_filter JSON
}

// ParseSearchBodyFull switch 新增
case "rescore":
    extras.Rescore = vraw
case "collapse":
    extras.Collapse = vraw
case "post_filter":
    extras.PostFilter = vraw
```

**planner.go 修改**：

```go
// buildAndExecute 新增 rescore/collapse 接入逻辑
// 1. 构建主查询 pipeline（现有逻辑）
// 2. 如果 extras.Rescore 非空：
//    a. 解析 rescore JSON 为 []RescoreSpec
//    b. 对每个 RescoreSpec：解析内部 query → translateForSegment → 构建 rescorePlan Operator
//    c. pipeline = NewRescore(pipeline, rescorePlan, queryWeight, rescoreWeight, window)
// 3. 如果 extras.Collapse 非空：
//    a. 解析 collapse JSON 为 CollapseSpec{Field, InnerHits}
//    b. pipeline = NewCollapse(pipeline, groupKeyFunc, maxPerGroup)
// 4. 如果 extras.PostFilter 非空：
//    a. 解析为 DSL query
//    b. 在 pipeline 输出后用 postFilterHits 过滤
```

**RescoreSpec 数据结构**：
```go
type RescoreSpec struct {
    Query              dsl.Query
    WindowSize         int
    QueryWeight        float32
    RescoreQueryWeight float32
    ScoreMode          string // "total"/"max"/"avg"/"multiply"
}
```

#### 2.3.2 CollapseOp 接入

CollapseOp 当前签名 `NewCollapse(input, groupKey func(rowID int32) string, maxPerGroup int)` 已可用。需从 `CollapseSpec.Field` 构建 `groupKey` 闭包：从 hit 的 _source 提取 field 值作为 group key。

**CollapseSpec 数据结构**：
```go
type CollapseSpec struct {
    Field     string
    InnerHits *InnerHitsSpec // 可选
}
type InnerHitsSpec struct {
    Name string
    Size int    // 默认 3
    Sort jsonx.RawMessage
}
```

#### 2.3.3 post_filter

复用现有 `postFilterHits` 函数。从 `extras.PostFilter` 解析 DSL query，在 pipeline 输出后对 hits 列表过滤。注意：post_filter 在聚合后应用，不影响 total count 和 aggs。

---

### 2.4 域三 P0：索引管理 API

#### 2.4.1 _open / _close

**admin_handlers.go**：
```go
func (s *Server) handleOpenIndex(w http.ResponseWriter, r *http.Request) {
    index := r.PathValue("index")
    if apiErr := s.engine.OpenIndex(r.Context(), index); apiErr != nil {
        writeError(w, apiErr)
        return
    }
    writeJSON(w, 0, map[string]any{"acknowledged": true})
}
```

**embedded/manager.go 修改**：
```go
type indexState struct {
    // ... 现有字段 ...
    closed bool // P0: 索引关闭标志
}

// OpenIndex/CloseIndex 实现
func (e *Engine) OpenIndex(ctx context.Context, index string) *errors.APIError {
    idx, err := e.manager.get(index)
    if err != nil { return errors.NewNotFound("index", index) }
    idx.mu.Lock()
    idx.closed = false
    idx.mu.Unlock()
    return nil
}

func (e *Engine) CloseIndex(ctx context.Context, index string) *errors.APIError {
    idx, err := e.manager.get(index)
    if err != nil { return errors.NewNotFound("index", index) }
    idx.mu.Lock()
    idx.closed = true
    idx.mu.Unlock()
    return nil
}
```

**写入路径检查**：`Engine.Write` 中 `ensureIndex` 后检查 `idx.closed`，若关闭则返回 `errors.NewIndexClosed(index)`。

#### 2.4.2 _stats

**IndexStatsResult 数据结构**：
```go
type IndexStatsResult struct {
    Shards  ShardStats
    Indices map[string]IndexStatsData
}
type IndexStatsData struct {
    Primaries IndexStatsMetric
    Total     IndexStatsMetric
}
type IndexStatsMetric struct {
    Docs     DocStats    // {count, deleted, total, seq_no}
    Store    StoreStats  // {size_in_bytes}
    Segments SegStats    // {count, memory_in_bytes}
    Search   SearchStats // {query_total, query_time_in_millis}
}
```

**实现**：遍历目标索引的 `Shard.ListMergeCandidates()` 获取段统计（`NumRows`/`DeletedRows`/`Bytes`），从 `Shard.VisibleSeqNo()` 获取 seq_no，从 `segmentDirSize()` 累加存储大小。

#### 2.4.3 _segments

**SegmentsResult 数据结构**：
```go
type SegmentsResult struct {
    Shards  ShardStats
    Indices map[string]IndexSegments
}
type IndexSegments struct {
    Shards map[string]ShardSegments
}
type ShardSegments struct {
    Segments map[string]SegmentInfo
}
type SegmentInfo struct {
    Generation   string
    NumDocs      int
    DeletedDocs  int
    SizeInBytes  int64
    Committed    bool // always true
    Search       bool // always true
    Version      int  // format version
}
```

**实现**：遍历 `SegmentSet.Items()`，从 `Reader.Footer()` 获取 `MinSeqNo`/`MaxSeqNo`/`NumRows`，从 `DeleteTracker.SegmentBitmap()` 获取删除数。

---

### 2.5 域四 P0：集群运维 API

#### 2.5.1 _cluster/state

**实现**：构造 `map[string]any` 返回 ES 格式集群状态：
- `cluster_name`/`cluster_uuid`（复用 `versionInfo`）
- `nodes`：单节点 `node-1`
- `routing_table`：每索引 → shards.[0] = `{state: "STARTED", primary: true, node: "node-1"}`
- `metadata`：indices → `{settings, mappings, aliases}`（从 `meta.Store.Load()` 获取）
- 支持 metric 过滤（`_cluster/state/{metric}`）和 index 过滤

#### 2.5.2 _cluster/stats

**实现**：汇总 `IndexStats` + Go runtime 信息：
- `indices`：count/docs/shards/store_size/segments（汇总所有索引）
- `nodes`：count/versions/os/cpu/mem（`runtime.NumCPU()`/`runtime.ReadMemStats()`）
- `status`：从 `Health()` 获取

#### 2.5.3 _nodes/stats

**NodesStatsResult 数据结构**：
```go
type NodesStatsResult struct {
    Nodes       map[string]NodeStats
    ClusterName string
}
type NodeStats struct {
    Timestamp  int64
    Name       string
    TransportAddress string
    IP         string
    Roles      []string
    Indices    NodeIndicesStats
    OS         NodeOSStats
    Process    NodeProcessStats
    ThreadPool map[string]ThreadPoolStats
}
```

**实现**：单节点统计。`indices` 汇总所有索引文档数/搜索次数（从 `observability` 指标读取）。`os` 通过 `runtime.ReadMemStats()` 获取。`thread_pool` 返回 goroutine 统计（`runtime.NumGoroutine()`）。

#### 2.5.4 _nodes/info

**实现**：构造单节点信息。复用 `versionInfo` 常量。`os` 通过 `runtime.GOOS`/`runtime.GOARCH`/`runtime.NumCPU()` 获取。`process` 通过 `os.Getpid()` 获取。JVM 字段返回占位值 `{"version":"go-runtime","vm_name":"go"}`。

---

### 2.6 域五 P0：快照/恢复 API

#### 2.6.1 快照仓库接口

**internal/snapshot/repo.go**：
```go
type SnapshotRepo interface {
    Name() string
    Type() string
    Verify(ctx context.Context) error
    CreateSnapshot(ctx context.Context, name string, indices []IndexSnapshotData) (*SnapshotInfo, error)
    ListSnapshots(ctx context.Context) ([]SnapshotInfo, error)
    GetSnapshot(ctx context.Context, name string) (*SnapshotInfo, error)
    DeleteSnapshot(ctx context.Context, name string) error
    RestoreSnapshot(ctx context.Context, name string, target string) (*RestoreResult, error)
    Cleanup(ctx context.Context) error
}

type SnapshotRepoConfig struct {
    Type     string          `json:"type"`     // "fs" | "s3"
    Settings json.RawMessage `json:"settings"` // 类型特定配置
}

type SnapshotRepoRegistry struct {
    mu      sync.RWMutex
    repos   map[string]SnapshotRepo
    dataDir string
}
func (r *SnapshotRepoRegistry) Put(name string, config SnapshotRepoConfig) *errors.APIError
func (r *SnapshotRepoRegistry) Get(name string) (SnapshotRepo, *errors.APIError)
func (r *SnapshotRepoRegistry) Delete(name string) *errors.APIError
func (r *SnapshotRepoRegistry) List() []SnapshotRepo
func (r *SnapshotRepoRegistry) Load() error  // 从 {dataDir}/snapshot-repos.json 恢复
func (r *SnapshotRepoRegistry) Save() error  // 持久化到 {dataDir}/snapshot-repos.json
```

#### 2.6.2 FS 仓库实现

**internal/snapshot/repo_fs.go**：
```go
type FSRepo struct {
    name     string
    location string // 快照根目录
    compress bool
}
```

**快照创建**：对每目标索引，遍历 `Shard.SegmentSet.Items()`，将段目录硬链接到 `{location}/snap-{name}/indices/{index}/segments/{segID}/`。记录索引元数据（mapping/settings/aliases）到 `{location}/snap-{name}/index-{N}` 文件。生成快照 UUID + 时间戳 + 状态。

#### 2.6.3 S3 仓库实现

**internal/snapshot/repo_s3.go**：
```go
type S3Repo struct {
    name   string
    client objstore.S3Client // 复用 objstore.S3Client 接口
    bucket string
    prefix string
}
```

快照存储到 S3 对象，路径格式 `{prefix}/snap-{name}/indices/{index}/segments/{segID}/col_*.bin`。

#### 2.6.4 段引用计数（D5）

**storage/segmentset.go 修改**：
```go
type SegmentEntry struct {
    Reader  *segment.Segment
    Indexes map[types.FieldID]index.LoadedIndex
    refCount int32         // P0: 快照引用计数（atomic）
}

func (e *SegmentEntry) Acquire() { atomic.AddInt32(&e.refCount, 1) }
func (e *SegmentEntry) Release() { atomic.AddInt32(&e.refCount, -1) }
func (e *SegmentEntry) RefCount() int32 { return atomic.LoadInt32(&e.refCount) }
```

**storage/merge.go 修改**：merge 完成后删除旧段文件前，检查 `SegmentEntry.RefCount() > 0`。若被引用，将段标记为 `pendingDelete`（不删除文件），引用释放后再清理。

**快照创建流程**：
1. 获取目标索引的 `SegmentSet`
2. 对每个 `SegmentEntry` 调用 `Acquire()`
3. 拷贝/硬链接段文件到快照目录
4. 完成后调用 `Release()`

---

### 2.7 域六 P0：Ingest Pipeline 框架

#### 2.7.1 Pipeline 数据模型

**internal/ingest/pipeline.go**：
```go
type Pipeline struct {
    ID          string
    Description string
    Version     int
    Processors  []ProcessorConfig
    OnFailure   []ProcessorConfig // 可选
}

type ProcessorConfig struct {
    Type      string                 `json:"type"`       // "set"/"convert"/"remove" 等
    If        string                 `json:"if"`         // 条件表达式（Painless 子集）
    Config    map[string]any         `json:"config"`     // processor 特定参数
    OnFailure string                 `json:"on_failure"` // "skip"/"mark"/"ignore"
    IgnoreFailure bool               `json:"ignore_failure"`
}

type Processor interface {
    Process(doc map[string]any, meta IngestMetadata) error
    Type() string
}

type IngestMetadata struct {
    Index string
    ID    string
}

type CompoundProcessor struct {
    Processors []Processor
    OnFailure  []Processor
}
func (cp *CompoundProcessor) Execute(doc map[string]any, meta IngestMetadata) error
```

#### 2.7.2 PipelineStore

**internal/ingest/store.go**：
```go
type PipelineStore struct {
    sync.Map // id → *Pipeline
    dataDir  string
}
func (s *PipelineStore) Put(id string, p *Pipeline)
func (s *PipelineStore) Get(id string) (*Pipeline, bool)
func (s *PipelineStore) Delete(id string)
func (s *PipelineStore) List() []*Pipeline
```

#### 2.7.3 写入集成（D3）

**embedded/ingest_impl.go**：
```go
// Engine.Write 修改
func (e *Engine) Write(ctx context.Context, doc IndexDoc, refresh string) (*WriteResult, error) {
    idx, err := e.manager.get(doc.Index)
    if err != nil { return nil, err }
    if idx.closed { return nil, errors.NewIndexClosed(doc.Index) }

    // P0: Ingest Pipeline 执行（D3: Engine.Write 内部）
    if doc.Pipeline != "" {
        pipeline, ok := e.pipelineStore.Get(doc.Pipeline)
        if !ok {
            return nil, errors.NewNotFound("pipeline", doc.Pipeline)
        }
        var sourceMap map[string]any
        if err := jsonx.Unmarshal(doc.Source, &sourceMap); err != nil {
            return nil, errors.NewIllegalArgument("invalid document source JSON")
        }
        if err := pipeline.Execute(sourceMap, IngestMetadata{Index: doc.Index, ID: doc.ID}); err != nil {
            return nil, errors.NewIllegalArgument("pipeline execution failed: " + err.Error())
        }
        // 重新序列化处理后的文档
        doc.Source, _ = jsonx.Marshal(sourceMap)
    }

    // 现有写入逻辑
    return idx.shard.Write(ctx, doc)
}
```

**IndexDoc 结构体修改**：
```go
type IndexDoc struct {
    // ... 现有字段 ...
    Pipeline string // P0: ingest pipeline ID
}
```

**handlers.go 修改**：
```go
// handleIndexDoc 新增 pipeline 参数读取
func (s *Server) handleIndexDoc(w http.ResponseWriter, r *http.Request) {
    // ... 现有逻辑 ...
    doc.Pipeline = r.URL.Query().Get("pipeline") // ?pipeline=<id>
    // ... 调用 engine.Write ...
}

// handleBulk 新增 pipeline 参数
func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
    pipeline := r.URL.Query().Get("pipeline")
    for i := range docs {
        docs[i].Pipeline = pipeline
    }
    // ... 调用 engine.Bulk ...
}
```

---

### 2.8 域七 P0：异步搜索

**internal/server/esapi/async_search.go**：
```go
type AsyncSearchManager struct {
    mu      sync.Mutex
    entries map[string]*asyncSearchEntry
    sem     chan struct{} // P2: 并发控制（默认 50）
    engine  SearchEngine
}

type asyncSearchEntry struct {
    id              string
    result          *SearchResult
    isRunning       bool
    isPartial       bool
    error           *errors.APIError
    startTime       time.Time
    expirationTime  time.Time
    indices         []string
    body            []byte
}

func NewAsyncSearchManager(engine SearchEngine, maxConcurrent int) *AsyncSearchManager
func (m *AsyncSearchManager) Submit(indices []string, body []byte, keepAlive time.Duration) (string, error)
func (m *AsyncSearchManager) Get(id string) (*asyncSearchEntry, bool)
func (m *AsyncSearchManager) Delete(id string)
func (m *AsyncSearchManager) Cleanup() // 后台 goroutine 定期清理
```

**Submit 流程**：
1. 生成随机 ID（复用 `PITManager` 的 `crypto/rand` + `base64.URLEncoding` 模式）
2. 创建 entry 并存入 map
3. 启动后台 goroutine 执行 `engine.Search`
4. `wait_for_completion_timeout`（默认 1s）内完成则直接返回结果，否则返回 id + `is_running: true`

**Server 结构体新增**：
```go
type Server struct {
    // ... 现有字段 ...
    asyncSearchMgr *AsyncSearchManager
}
```

**路由注册**：
```go
s.mux.HandleFunc("POST /_async_search", mw(s.handleSubmitAsyncSearch))
s.mux.HandleFunc("GET /_async_search/{id}", mw(s.handleGetAsyncSearch))
s.mux.HandleFunc("GET /_async_search/status/{id}", mw(s.handleGetAsyncSearchStatus))
s.mux.HandleFunc("DELETE /_async_search/{id}", mw(s.handleDeleteAsyncSearch))
```

---

### 2.9 域八 P0：同义词管理 API

**internal/synonyms/model.go**：
```go
type SynonymSet struct {
    ID        string    `json:"id"`
    Synonyms  []string  `json:"synonyms"` // 如 ["happy, joy, glad => elated", "ipod, i-pod, i pod"]
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

**internal/synonyms/store.go**：
```go
type SynonymStore struct {
    sync.Map // id → *SynonymSet
}
func (s *SynonymStore) Put(id string, set *SynonymSet)
func (s *SynonymStore) Get(id string) (*SynonymSet, bool)
func (s *SynonymStore) Delete(id string)
func (s *SynonymStore) List() []*SynonymSet
```

**synonym_handlers.go**：实现 `PUT/GET/DELETE /_synonyms/{id}` 和 `GET /_synonyms`，复用 `handleIndexTemplate`/`handleILM` 的 sync.Map 存储模式。

---

### 2.10 域九 P0：Term Vectors API

**internal/embedded/termvectors.go**：
```go
type TermVectorsRequest struct {
    Index          string
    ID             string
    Fields         []string // 空时返回所有 text/keyword 字段
    Offsets        bool     // 默认 true
    Positions      bool     // 默认 true
    TermStatistics bool     // 默认 false（需要查询倒排索引获取 doc_freq）
    FieldStatistics bool    // 默认 false
    Doc            json.RawMessage // 可选：直接提供文档（不从索引查找）
}

type TermVectorsResponse struct {
    Index       string                          `json:"_index"`
    ID          string                          `json:"_id"`
    Found       bool                            `json:"found"`
    TermVectors map[string]FieldTermVector      `json:"term_vectors"`
}

type FieldTermVector struct {
    FieldStatistics *FieldStatistics `json:"field_statistics,omitempty"`
    Terms            map[string]TermInfo `json:"terms"`
}

type TermInfo struct {
    TermFreq   int   `json:"term_freq"`
    DocFreq    int   `json:"doc_freq,omitempty"`     // term_statistics=true 时
    TTF        int   `json:"ttf,omitempty"`          // total term frequency
    Positions  []int `json:"positions,omitempty"`
    StartOffset []int `json:"start_offset,omitempty"`
    EndOffset  []int `json:"end_offset,omitempty"`
}
```

**实现策略**：
1. 通过 `shard.GetDocSource` 获取文档 _source
2. 对每个 text 字段用 analyzer 重新分析获取 tokens（含 term/position/start_offset/end_offset）
3. 统计 term_freq（token 出现次数）
4. 如果 `term_statistics=true`：遍历倒排索引 `TermDict` 查找 doc_freq
5. 如果 `field_statistics=true`：从 `SegmentStats` 获取 doc_count、sum_doc_freq

**handleMTermVectors**：解析 body 中的 `docs` 数组（每个含 `_index`/`_id` 或 `doc`），逐条调用 `TermVectors`，返回数组。

---

## 3. 接口设计 — 新增 REST API（P0 路由）

### 3.1 索引管理路由

```go
// router.go 新增
s.mux.HandleFunc("POST /{index}/_open", mw(s.handleOpenIndex))
s.mux.HandleFunc("POST /{index}/_close", mw(s.handleCloseIndex))
s.mux.HandleFunc("GET /{index}/_stats", mw(s.handleIndexStats))
s.mux.HandleFunc("GET /_stats", mw(s.handleIndexStats))
s.mux.HandleFunc("GET /{index}/_segments", mw(s.handleSegments))
s.mux.HandleFunc("GET /_segments", mw(s.handleSegments))
```

### 3.2 集群运维路由

```go
s.mux.HandleFunc("GET /_cluster/state", mw(s.handleClusterState))
s.mux.HandleFunc("GET /_cluster/state/{metric}", mw(s.handleClusterState))
s.mux.HandleFunc("GET /_cluster/stats", mw(s.handleClusterStats))
s.mux.HandleFunc("GET /_nodes/stats", mw(s.handleNodesStats))
s.mux.HandleFunc("GET /_nodes/info", mw(s.handleNodesInfo))
s.mux.HandleFunc("GET /_nodes", mw(s.handleNodesInfo))
```

### 3.3 快照恢复路由

```go
s.mux.HandleFunc("PUT /_snapshot/{repo}", mw(s.handlePutSnapshotRepo))
s.mux.HandleFunc("GET /_snapshot", mw(s.handleGetSnapshotRepos))
s.mux.HandleFunc("DELETE /_snapshot/{repo}", mw(s.handleDeleteSnapshotRepo))
s.mux.HandleFunc("PUT /_snapshot/{repo}/{snapshot}", mw(s.handleCreateSnapshot))
s.mux.HandleFunc("GET /_snapshot/{repo}", mw(s.handleGetSnapshots))
s.mux.HandleFunc("POST /_snapshot/{repo}/{snapshot}/_restore", mw(s.handleRestoreSnapshot))
```

### 3.4 Ingest Pipeline 路由

```go
s.mux.HandleFunc("PUT /_ingest/pipeline/{id}", mw(s.handlePutPipeline))
s.mux.HandleFunc("GET /_ingest/pipeline", mw(s.handleGetPipelines))
s.mux.HandleFunc("GET /_ingest/pipeline/{id}", mw(s.handleGetPipeline))
s.mux.HandleFunc("DELETE /_ingest/pipeline/{id}", mw(s.handleDeletePipeline))
s.mux.HandleFunc("POST /_ingest/pipeline/{id}/_simulate", mw(s.handleSimulatePipeline))
s.mux.HandleFunc("POST /_ingest/pipeline/_simulate", mw(s.handleSimulatePipeline))
```

### 3.5 异步搜索 + 同义词 + Term Vectors 路由

```go
// 异步搜索
s.mux.HandleFunc("POST /_async_search", mw(s.handleSubmitAsyncSearch))
s.mux.HandleFunc("GET /_async_search/{id}", mw(s.handleGetAsyncSearch))
s.mux.HandleFunc("GET /_async_search/status/{id}", mw(s.handleGetAsyncSearchStatus))
s.mux.HandleFunc("DELETE /_async_search/{id}", mw(s.handleDeleteAsyncSearch))

// 同义词
s.mux.HandleFunc("PUT /_synonyms/{id}", mw(s.handlePutSynonyms))
s.mux.HandleFunc("GET /_synonyms", mw(s.handleGetSynonyms))
s.mux.HandleFunc("GET /_synonyms/{id}", mw(s.handleGetSynonyms))
s.mux.HandleFunc("DELETE /_synonyms/{id}", mw(s.handleDeleteSynonyms))

// Term Vectors
s.mux.HandleFunc("GET /{index}/_termvectors/{id}", mw(s.handleTermVectors))
s.mux.HandleFunc("POST /{index}/_termvectors/{id}", mw(s.handleTermVectors))
s.mux.HandleFunc("GET /_mtermvectors", mw(s.handleMTermVectors))
s.mux.HandleFunc("POST /_mtermvectors", mw(s.handleMTermVectors))
```

---

## 4. 数据模型 — 关键新增结构

### 4.1 RescoreSpec / CollapseSpec

```go
// internal/engine/dsl/parser.go
type RescoreSpec struct {
    Query              Query
    WindowSize         int
    QueryWeight        float32
    RescoreQueryWeight float32
    ScoreMode          string
}

type CollapseSpec struct {
    Field     string
    InnerHits  *InnerHitsSpec
}

type InnerHitsSpec struct {
    Name string
    Size int
    Sort jsonx.RawMessage
}
```

### 4.2 快照相关结构

```go
// internal/snapshot/repo.go
type IndexSnapshotData struct {
    IndexName  string
    Mapping    []byte
    Settings   []byte
    Aliases    []byte
    Segments   []SegmentSnapshotData
}

type SegmentSnapshotData struct {
    SegmentID  types.SegmentID
    DirPath    string // 段目录路径
}

type SnapshotInfo struct {
    Snapshot       string    `json:"snapshot"`
    UUID           string    `json:"uuid"`
    Version        string    `json:"version"`
    Indices        []string  `json:"indices"`
    State          string    `json:"state"` // SUCCESS/IN_PROGRESS/FAILED/PARTIAL
    StartTime      time.Time `json:"start_time"`
    EndTime        time.Time `json:"end_time"`
    DurationMillis int64     `json:"duration_in_millis"`
    Failures       []string  `json:"failures,omitempty"`
}
```

### 4.3 Pipeline 相关结构

```go
// internal/ingest/pipeline.go
type PipelineConfig struct {
    Description string              `json:"description"`
    Version     int                 `json:"version,omitempty"`
    Processors  []jsonx.RawMessage  `json:"processors"`
    OnFailure   []jsonx.RawMessage  `json:"on_failure,omitempty"`
}
```

---

## 5. 错误处理

### 5.1 新增错误类型

| 错误类型 | 触发条件 | HTTP 状态码 | 用户提示 |
|---------|---------|------------|---------|
| `IndexClosed` | 对 closed 索引执行写入 | 400 | `index_closed_exception: [index] closed` |
| `PipelineNotFound` | pipeline ID 不存在 | 404 | `pipeline [id] not found` |
| `PipelineExecutionFailed` | Pipeline 执行出错 | 400 | `pipeline_execution_exception: [error]` |
| `SnapshotInProgress` | 快照正在进行 | 409 | `snapshot_in_progress_exception` |
| `RepositoryMissing` | 快照仓库不存在 | 404 | `repository_missing_exception` |
| `SnapshotNotFound` | 快照不存在 | 404 | `snapshot_missing_exception` |
| `IndexAlreadyExists` | 恢复时目标索引已存在 | 400 | `resource_already_exists_exception` |
| `TooManyAsyncSearches` | 异步搜索并发超限 | 429 | `too_many_requests` |

### 5.2 errors/codes.go 新增

```go
func NewIndexClosed(index string) *APIError
func NewPipelineNotFound(id string) *APIError
func NewPipelineExecutionFailed(err error) *APIError
func NewSnapshotInProgress(repo, snapshot string) *APIError
func NewRepositoryMissing(repo string) *APIError
func NewSnapshotNotFound(repo, snapshot string) *APIError
func NewIndexAlreadyExists(index string) *APIError
func NewTooManyAsyncSearches() *APIError
```

---

## 6. 测试规划 (Test Plan)

### 6.1 测试策略表

| 测试类型 | 覆盖范围 | 目标覆盖率 | 说明 |
|----------|----------|------------|------|
| 单元测试 | DSL 解析器（prefix/regexp/constant_score/boosting/dis_max） | ≥ 90% | 每个新 DSL 的解析和翻译 |
| 单元测试 | Processor（Set/Convert/Remove 等） | ≥ 90% | 每个 processor 独立测试 |
| 单元测试 | SnapshotRepo（FS/S3） | ≥ 80% | 仓库 CRUD + 快照创建/恢复 |
| 单元测试 | AsyncSearchManager | ≥ 85% | Submit/Get/Delete/Cleanup |
| 集成测试 | REST API 端点 | 每端点 ≥ 3 用例 | 正常/边界/错误 |
| 集成测试 | ES golden test YAML | 关键 API | 响应格式兼容性验证 |
| 回归测试 | `go build/test/vet` | 100% 全绿 | 现有 163 Python + 46 Go 测试不退化 |

### 6.2 关键测试场景清单

#### 正向用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| prefix 查询命中 | `{"prefix": {"title": "hel"}}` | 返回 title 以 "hel" 开头的文档 | P0 |
| regexp 查询命中 | `{"regexp": {"title": "hel.*"}}` | 返回 title 匹配正则的文档 | P0 |
| rescore 调整排序 | match 查询 + rescore query | rescore 后排序与原始不同 | P0 |
| collapse 去重 | collapse by category | 每组仅返回 top-1 | P0 |
| post_filter 过滤 | query + post_filter | hits 被过滤但 total 不变 | P0 |
| 索引关闭后写入 | POST /{index}/_close 后写入 | 返回 index_closed_exception | P0 |
| _stats 返回统计 | GET /{index}/_stats | 返回 docs.count/store.size | P0 |
| 快照创建+恢复 | PUT /_snapshot/repo/snap1 → POST restore | 恢复后索引可搜索 | P0 |
| Pipeline 处理 | ?pipeline=set-field 写入 | 文档字段被 Pipeline 修改 | P0 |
| 异步搜索 | POST /_async_search + GET /{id} | 获取搜索结果 | P0 |
| 同义词 CRUD | PUT/GET/DELETE /_synonyms/test | 创建/获取/删除成功 | P0 |
| Term Vectors | GET /{index}/_termvectors/{id} | 返回 term_freq/positions | P0 |

#### 边界用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| prefix 空 value | `{"prefix": {"field": ""}}` | 匹配所有有该字段的文档 | P0 |
| regexp 复杂正则 | `{"regexp": {"field": "(a\|b)*c"}}` | 正确匹配，无 ReDoS | P1 |
| rescore window > hits | window_size=100 但仅 5 条结果 | 对 5 条全部 rescore | P0 |
| collapse 无 inner_hits | collapse by field, 无 inner_hits | 每组仅 top-1 | P0 |
| 快照空索引 | 对空索引创建快照 | 快照成功，indices 为空列表 | P0 |
| Pipeline 不存在 | ?pipeline=nonexistent | 返回 404 | P0 |
| 异步搜索超时 | 搜索 > wait_for_completion_timeout | 返回 id + is_running | P0 |
| Term Vectors 不存在文档 | GET /index/_termvectors/nonexistent | found: false | P0 |

#### 异常用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| regexp 非法正则 | `{"regexp": {"field": "["}}` | 400 illegal_argument | P0 |
| 对 closed 索引搜索 | POST /closed-index/_search | 返回 index_closed_exception 或空结果 | P0 |
| 快照仓库不存在 | PUT /_snapshot/nonexist/snap1 | 返回 repository_missing | P0 |
| 恢复时索引已存在 | POST restore 到已存在索引 | 返回 resource_already_exists | P0 |
| Pipeline 条件表达式错误 | if: "doc.invalid.field" | ignore_failure 或 400 | P1 |
| 异步搜索并发超限 | 超过 50 个并发 | 返回 429 | P2 |

### 6.3 Mock 清单

| 依赖项 | Mock 方式 | 模拟行为 | 使用场景 |
|--------|-----------|----------|----------|
| `objstore.S3Client` | `MemS3Client`（已存在） | 内存模拟 S3 PutObject/GetObject | S3 仓库单元测试 |
| `meta.Store` | 临时目录 | 临时文件系统 | 索引管理/快照测试 |
| `Engine` | Mock Engine | 实现 Engine 接口返回固定数据 | handler 单元测试 |
| `SearchEngine` | Mock SearchEngine | 返回固定 SearchResult | 异步搜索单元测试 |

### 6.4 测试数据设计

- **DSL 测试数据**：每索引 100 条文档，覆盖 text/keyword/long/double/boolean/geo_point 字段类型
- **快照测试数据**：3 个索引各 1000 条文档，含 mapping/settings/aliases
- **Pipeline 测试数据**：模拟日志文档（含 message/timestamp/host 字段）
- **Term Vectors 测试数据**：含已知 term frequencies 的文档，可验证 term_freq/positions/offets

---

## 7. 验收标准 (Acceptance Criteria)

- [ ] **接口签名**: Engine 接口新增 ~36 方法声明 + EngineNoop 空实现
- [ ] **路由注册**: ~60 条新路由在 `router.go` 中注册
- [ ] **DSL 补齐**: prefix/regexp/constant_score/boosting/dis_max 从 `unsupportedQueries` 移除并可用
- [ ] **搜索增强**: rescore/collapse/post_filter 可在 search body 中使用
- [ ] **索引管理**: _open/_close/_stats/_segments API 返回 ES 格式响应
- [ ] **集群运维**: _cluster/state/_cluster/stats/_nodes/stats/_nodes/info API 可用
- [ ] **快照恢复**: FS 仓库可创建/列出/恢复快照
- [ ] **Ingest Pipeline**: Pipeline CRUD + ?pipeline= 写入集成 + _simulate
- [ ] **异步搜索**: Submit/Get/Delete API 可用
- [ ] **同义词管理**: CRUD API 可用
- [ ] **Term Vectors**: 单文档 + 批量 API 返回 term_freq/positions/offsets
- [ ] **段引用计数**: 快照期间 merge 不删除被引用的段文件
- [ ] **错误处理**: 新增 8 个错误类型返回正确 HTTP 状态码
- [ ] **测试**: `go build ./...` + `go test ./...` + `go vet ./...` 全绿
- [ ] **回归**: 现有 163 Python 集成测试 + 46 Go 测试全部通过

---

## 8. 实现清单 (Implementation Checklist)

### 前置（1.5 天）
- [ ] Engine 接口扩展 + EngineNoop 空实现
- [ ] errors/codes.go 新增 8 个错误类型
- [ ] config.go 新增 ClusterSettings/SnapshotConfig 字段

### 域一 P0（8 天）
- [ ] prefix: ast.go + parser.go + translator.go + planner.go
- [ ] regexp: ast.go + parser.go + translator.go + planner.go
- [ ] constant_score: ast.go + parser.go + translator.go + planner.go
- [ ] boosting: ast.go + parser.go + translator.go + planner.go
- [ ] dis_max: ast.go + parser.go + translator.go + planner.go

### 域二 P0（5 天）
- [ ] RescoreOp 改造为持有子 Operator
- [ ] SearchExtras 扩展（Rescore/Collapse/PostFilter 字段）
- [ ] ParseSearchBodyFull 新增 case 分支
- [ ] planner.go rescore/collapse/post_filter 接入
- [ ] docMatchesQuery 新增 prefix/regexp/constant_score/boosting/dis_max 分支

### 域三 P0（5 天）
- [ ] admin_handlers.go: handleOpenIndex/handleCloseIndex/handleIndexStats/handleSegments
- [ ] manager.go: indexState.closed 标志
- [ ] embedded/admin_impl.go: OpenIndex/CloseIndex/IndexStats/Segments 实现
- [ ] router.go 路由注册

### 域四 P0（7 天）
- [ ] cluster_handlers.go: handleClusterState/handleClusterStats/handleNodesStats/handleNodesInfo
- [ ] embedded/cluster_impl.go: 实现
- [ ] router.go 路由注册

### 域五 P0（10 天）
- [ ] snapshot/repo.go: SnapshotRepo 接口 + Registry
- [ ] snapshot/repo_fs.go: FS 仓库实现
- [ ] snapshot/repo_s3.go: S3 仓库实现
- [ ] snapshot/snapshot.go: 快照创建
- [ ] snapshot/restore.go: 快照恢复
- [ ] segmentset.go: SegmentEntry.refCount
- [ ] merge.go: 段引用检查
- [ ] snapshot_handlers.go: handler
- [ ] embedded/snapshot_impl.go: 实现
- [ ] router.go 路由注册

### 域六 P0（4 天）
- [ ] ingest/pipeline.go: Pipeline + Processor 接口 + CompoundProcessor
- [ ] ingest/processor.go: 注册表 + 工厂
- [ ] ingest/execute.go: 执行引擎
- [ ] ingest/store.go: PipelineStore
- [ ] ingest_handlers.go: handler
- [ ] embedded/ingest_impl.go: 写入集成
- [ ] handlers.go: IndexDoc.Pipeline + handleIndexDoc/handleBulk 修改
- [ ] router.go 路由注册

### 域七 P0（2 天）
- [ ] async_search.go: AsyncSearchManager + handler
- [ ] Server 结构体新增 asyncSearchMgr
- [ ] router.go 路由注册

### 域八 P0（1 天）
- [ ] synonyms/model.go: SynonymSet
- [ ] synonyms/store.go: SynonymStore
- [ ] synonym_handlers.go: handler
- [ ] router.go 路由注册

### 域九 P0（3 天）
- [ ] embedded/termvectors.go: computeTermVectors
- [ ] termvectors.go: handleTermVectors/handleMTermVectors
- [ ] router.go 路由注册
