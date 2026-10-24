# Spec: M0 引擎接线 + M1 ES 兼容 GA 全量

## 1. 概述 (Overview)

本 Spec 将 Proposal `01_proposal.md` 转化为可执行的工程细节。任务分两阶段：
- **Phase 0**（M0 引擎接线）：将 storage.Shard + index（HNSW/倒排/标量）+ engine（DSL/Plan/Exec/Fusion）接线到 HTTP 服务层，使 `_search` 走真实引擎栈
- **Phase 1-11**（M1 ES 兼容 GA）：完成 W5.1-W5.11 全部 11 个任务，达成 elasticsearch-py 测试套件通过率 ≥90%

设计评审共 3 轮，9 个技术决策点已确认（见 §1.1）。

### 1.1 设计评审决策汇总

| # | 决策点 | 选择 |
|---|--------|------|
| D1 | Memtable 搜索 | 两阶段：hash map 实时可搜索 + 冻结时复用 inverted.Build 构建临时索引 |
| D2 | 多段合并 | fan-out 算子（混合自适应：小 K 批量收集，大 K/多段降级为拉取式堆归并） |
| D3 | 字段类型路由 | Index Registry 声明式路由（每种索引 Kind 声明适用字段类型） |
| D4 | Mini 倒排实现 | 两阶段：hash map（写入时实时）+ inverted.Build（冻结时构建正式临时索引） |
| D5 | PartialState 序列化 | 自定义二进制（与 CGO_ENABLED=0 和自研策略一致） |
| D6 | PIT 与段 GC | 后台 GC + tombstone（merge 新段不立即删旧段，后台 GC 清理无引用旧段） |
| D7 | MergeSortOp 接口 | 混合自适应（小 K 批量收集，大 K/多段降级为拉取式堆归并） |
| D8 | 金标准测试范围 | 全量翻译（200-300 用例，全覆盖 elasticsearch-py 测试套件） |
| D9 | 多租户启用 | 嵌入式也支持（通过配置开启 shared-shard + 配额） |

### 1.2 变更清单

| 类型 | 文件路径 | 说明 |
|------|----------|------|
| **Phase 0 新增** | `internal/embedded/manager.go` | IndexManager：索引生命周期管理 |
| **Phase 0 新增** | `internal/embedded/docparse.go` | JSON→memtable.Row 文档解析器 |
| **Phase 0 新增** | `internal/embedded/translator.go` | DSL AST→plan.LogicalPlan 翻译器 |
| **Phase 0 新增** | `internal/embedded/planner.go` | Plan→Exec 管道构建器 + 多段合并 + 结果组装 |
| **Phase 0 新增** | `internal/embedded/textcol.go` | 文本/标量列提供器（供 flush 构建索引） |
| **Phase 0 新增** | `internal/embedded/miniinv.go` | Memtable Mini 倒排索引（hash map 阶段） |
| **Phase 0 新增** | `internal/embedded/engine_test.go` | 接线集成测试 |
| **Phase 0 修改** | `internal/embedded/engine.go` | 完全重写（核心目标） |
| **Phase 0 修改** | `internal/storage/flush.go` | 扩展倒排/标量索引构建 + 加载 |
| **Phase 0 修改** | `internal/meta/model.go` | IndexMeta 新增 FieldIDs 字段 |
| **Phase 0 修改** | `internal/index/registry.go` | 新增字段类型路由声明（D3） |
| **Phase 0 修改** | `internal/engine/exec/operators.go` | 新增 MergeSortOp（D2/D7） |
| **Phase 0 修改** | `test/e2e/embedded_test.go` | E2E 适配真实引擎栈 |
| **M1 新增** | `internal/index/inverted/fst.go` | W5.1 自研 FST 词典 |
| **M1 新增** | `internal/index/inverted/fuzzy.go` | W5.1 Levenshtein 自动机 |
| **M1 新增** | `internal/index/inverted/wand.go` | W5.2 WAND/Block-Max WAND |
| **M1 新增** | `internal/engine/agg/` | W5.3 聚合引擎（aggregator/hll/tdigest/pipeline） |
| **M1 新增** | `internal/server/esapi/pit.go` | W5.4 PIT 生命周期管理 |
| **M1 新增** | `internal/server/esapi/scroll.go` | W5.4 scroll 转译 |
| **M1 新增** | `internal/engine/highlight/` | W5.5 unified highlighter |
| **M1 新增** | `internal/engine/suggest/` | W5.5 term/phrase/completion suggester |
| **M1 新增** | `internal/engine/script/` | W5.6 painless 子集 + 向量函数 |
| **M1 新增** | `internal/engine/profile/` | W5.7 profile tracer + render |
| **M1 新增** | `internal/engine/stats/` | W5.9 BM25 全局统计表 |
| **M1 新增** | `internal/tenant/` | W5.10 资源组 + 配额 + 背压 |
| **M1 新增** | `internal/server/esapi/msearch.go` 等 | W5.8 _msearch/_field_caps/_terms_enum/_explain/by_query/_reindex/template/ILM |
| **M1 新增** | `test/golden/` | W5.11 elasticsearch-py 用例翻译 |
| **M1 新增** | `test/integration/` | W5.11 测试基础设施 |
| **M1 新增** | `test/compat/` | W5.11 兼容矩阵报告 |
| **M1 修改** | `internal/index/inverted/dict.go` | W5.1 TermDict 实现→FST |
| **M1 修改** | `internal/index/inverted/index.go` | W5.2/W5.9 SearchTerms 改用 WAND/全局统计 |
| **M1 修改** | `internal/engine/exec/operators.go` | W5.2/W5.3/W5.7 新增算子 |
| **M1 修改** | `internal/engine/plan/logical.go` | W5.3 新增 Aggregate 节点 |
| **M1 修改** | `internal/server/esapi/router.go` | W5.4/W5.8 新路由 |
| **M1 修改** | `internal/server/esapi/engine.go` | W5.8 Engine 接口新增方法 |
| **M1 修改** | `internal/storage/shard.go` | W5.4 快照 pin + 段 GC |
| **M1 修改** | `internal/config/config.go` | W5.10 多租户配置 |

---

## 2. 详细设计 (Detailed Design)

### 2.1 Phase 0: M0 引擎接线

#### 2.1.1 IndexManager（`internal/embedded/manager.go`）

**变更内容**：新建索引生命周期管理器，桥接 meta（字段名）与 Shard（FieldID）。

```go
// indexState 是单个索引的运行时状态。
type indexState struct {
    name      string
    shard     *storage.Shard
    fieldIDs  map[string]types.FieldID   // 字段名→FieldID（持久化到 meta）
    fieldDefs map[types.FieldID]*meta.FieldDef  // FieldID→字段定义
    cancel    context.CancelFunc         // 停止 shard 后台 goroutine
    mu        sync.RWMutex               // 保护 fieldIDs/fieldDefs 的并发读写
}

// indexManager 管理所有索引的运行时状态。
type indexManager struct {
    store   *meta.Store           // 元数据持久化
    indices map[string]*indexState
    dataDir string
    mu      sync.RWMutex
}
```

**核心方法**：

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `newIndexManager` | `(store *meta.Store, dataDir string) *indexManager` | 构造函数 |
| `createIndex` | `(ctx, name string, mapping json.RawMessage) (*indexState, error)` | 创建索引：解析 mapping → 分配 FieldID → OpenShard → 持久化 meta |
| `ensureIndex` | `(ctx, name string) (*indexState, error)` | 确保索引存在，不存在则用空 mapping 创建 |
| `deleteIndex` | `(ctx, name string) error` | 删除索引：关闭 shard → 删除数据目录 → 删除 meta |
| `getIndex` | `(name string) (*indexState, bool)` | 获取索引状态 |
| `recoverAll` | `(ctx) error` | 重启恢复：列出所有 meta → 重建 fieldIDs/fieldDefs → OpenShard |
| `closeAll` | `()` | 关闭所有 shard |

**FieldID 分配策略**：
- `_id` 固定为 `FieldID(1)`
- 其余字段按 mapping Properties 的 key 排序依次分配 `FieldID(2)`, `FieldID(3)...`
- 动态新增字段：在 `ApplyInsert` 中如遇未知字段名，加写锁分配下一个可用 FieldID，更新 fieldIDs/fieldDefs 并持久化到 meta

**依赖关系**：
- 依赖 `meta.Store` 用于元数据持久化
- 依赖 `storage.OpenShard` 用于创建/恢复 shard
- 被 `embedded.Engine` 的所有方法调用

#### 2.1.2 文档解析器（`internal/embedded/docparse.go`）

**变更内容**：实现 `storage.ShardConfig.ApplyInsert` 回调，将 JSON payload 解析为 `memtable.Row`。

```go
// applyInsertFn 返回一个符合 storage.ShardConfig.ApplyInsert 签名的函数。
// 闭包捕获 indexState 引用以访问/更新 fieldIDs 和 fieldDefs。
func applyInsertFn(idx *indexState) func(payload []byte, seq types.SeqNo) (memtable.Row, error) {
    return func(payload []byte, seq types.SeqNo) (memtable.Row, error) {
        var doc map[string]any
        if err := json.Unmarshal(payload, &doc); err != nil {
            return memtable.Row{}, err
        }
        // 1. 动态映射推断：遇未知字段时分配 FieldID
        idx.maybeApplyDynamic(doc)
        // 2. 遍历字段，按 FieldDef.Type 转换值
        values := make(map[types.FieldID]any)
        for field, val := range doc {
            fid, def := idx.resolveField(field) // 查 fieldIDs + fieldDefs
            converted, err := convertValue(val, def.Type)
            if err != nil {
                return memtable.Row{}, err
            }
            values[fid] = converted
        }
        return memtable.Row{
            ID:     extractID(doc),  // _id 字段或自动生成
            SeqNo:  seq,
            Source: payload,         // 原始 JSON 保留为 _source
            Values: values,
        }, nil
    }
}
```

**字段类型转换表**：

| JSON 类型 | meta.FieldType | Go 类型 | 说明 |
|-----------|---------------|---------|------|
| string | keyword | string | 精确匹配 |
| string | text | string | 全文检索（分析器处理） |
| number (int) | long | int64 | 整数 |
| number (float) | double | float64 | 浮点 |
| bool | boolean | bool | 布尔 |
| []float32 | dense_vector | []float32 | 稠密向量 |
| []number | <multi> | []any | 多值字段扁平化 |

**依赖关系**：
- 依赖 `indexState` 的 `fieldIDs`/`fieldDefs` 进行字段解析
- 依赖 `meta.ApplyDynamic` 进行动态映射推断
- 被 `storage.OpenShard` 通过 `ShardConfig.ApplyInsert` 回调调用

#### 2.1.3 Mini 倒排索引（`internal/embedded/miniinv.go`，D1/D4）

**变更内容**：在 memtable 中维护轻量倒排，使未 flush 数据实时可搜索。两阶段策略：

**阶段 1 — hash map（写入时实时）**：
```go
// miniInvertedIndex 是 memtable 内的轻量倒排索引。
type miniInvertedIndex struct {
    // term -> set of rowIDs（使用 map[string]map[int32]struct{} 避免依赖 roaring）
    postings map[string]map[int32]struct{}
    // term -> document frequency（用于 BM25 IDF 近似）
    docFreq map[string]int
    mu      sync.RWMutex
}

// add 在写入文档时实时更新倒排。
func (m *miniInvertedIndex) add(fieldID types.FieldID, text string, rowID int32, analyzer analyzer.Analyzer) {
    tokens := analyzer.Analyze(text)
    m.mu.Lock()
    defer m.mu.Unlock()
    for _, tok := range tokens {
        term := termKey(fieldID, tok)
        if m.postings[term] == nil {
            m.postings[term] = make(map[int32]struct{})
        }
        m.postings[term][rowID] = struct{}{}
        m.docFreq[term]++
    }
}

// search 在 hash map 中查找匹配文档。
func (m *miniInvertedIndex) search(fieldID types.FieldID, text string, analyzer analyzer.Analyzer) []int32 {
    // 返回匹配的 rowIDs，评分用 docFreq 近似 BM25
}
```

**阶段 2 — 冻结构建（memtable 冻结时）**：
```go
// buildOnFreeze 在 memtable 冻结时复用 inverted.Index.Build 构建正式临时索引。
// 该临时索引与段级倒排索引格式一致，搜索时可统一查询。
func (m *miniInvertedIndex) buildOnFreeze(mt *memtable.MemTable, idx *indexState) (inverted.LoadedIndex, error) {
    // 用 textcol.go 列提供器调用 inverted.Index.Build
    // 构建结果作为临时 LoadedIndex 返回，供 planner 查询
}
```

**依赖关系**：
- 依赖 `analyzer.Analyzer` 进行分词
- 依赖 `inverted.Index.Build` 构建冻结后的正式索引
- 被 `planner.go` 在搜索未 flush 数据时查询

#### 2.1.4 DSL→Plan 翻译器（`internal/embedded/translator.go`，D3）

**变更内容**：将 DSL AST 翻译为 plan.LogicalPlan 树，使用 Index Registry 声明式路由。

```go
// translateQuery 将 DSL 查询翻译为逻辑计划。
func translateQuery(q dsl.Query, idx *indexState, snap types.Snapshot, segSet *storage.SegmentSet, from, size int) (plan.LogicalPlan, error) {
    switch typed := q.(type) {
    case nil:
        return translateMatchAll(idx, snap, segSet, from, size)
    case *dsl.MatchQuery:
        return translateMatch(typed, idx, snap, segSet)
    case *dsl.KnnQuery:
        return translateKnn(typed, idx, snap, segSet)
    case *dsl.TermQuery:
        return translateTerm(typed, idx, snap, segSet)
    case *dsl.RangeQuery:
        return translateRange(typed, idx, snap, segSet)
    case *dsl.BoolQuery:
        return translateBool(typed, idx, snap, segSet)
    case *dsl.HybridQuery:
        return translateHybrid(typed, idx, snap, segSet, from, size)
    // ... other query types
    }
}
```

**Index Registry 声明式路由（D3）**：

在 `internal/index/registry.go` 新增字段类型路由声明：

```go
// FieldTypeRoute 声明一种索引 Kind 适用的 ES 字段类型。
type FieldTypeRoute struct {
    Kind     IndexKind
    ESFieldTypes []string  // e.g. ["text"], ["dense_vector"], ["keyword"], ["long","date"]
}

var fieldRoutes = []FieldTypeRoute{
    {KindHNSW, []string{"dense_vector"}},
    {KindInverted, []string{"text"}},
    {KindBKD, []string{"long", "date", "double"}},
    {KindBitmap, []string{"keyword", "boolean"}},
}

// RouteForField 返回字段类型适用的索引 Kind 列表。
func RouteForField(esType string) []IndexKind {
    var kinds []IndexKind
    for _, r := range fieldRoutes {
        for _, t := range r.ESFieldTypes {
            if t == esType {
                kinds = append(kinds, r.Kind)
            }
        }
    }
    return kinds
}
```

翻译器使用 `RouteForField` 决定每个字段应走哪种索引路径：
- `text` → `KindInverted` → `plan.BM25` 节点
- `dense_vector` → `KindHNSW` → `plan.ANN` 节点
- `keyword` → `KindBitmap` → `plan.Scan` + `plan.Filter` 节点
- `long`/`date` → `KindBKD` → `plan.Scan` + zone map 谓词

**依赖关系**：
- 依赖 `dsl.Query` AST 类型
- 依赖 `index.RouteForField` 进行字段类型路由
- 依赖 `indexState` 解析字段名→FieldID
- 被 `engine.go` 的 Search 方法调用

#### 2.1.5 Plan→Exec 管道构建器（`internal/embedded/planner.go`，D2/D7）

**变更内容**：将优化后的 plan 树构建为 exec.Operator 管道，处理多段扇出和 memtable 搜索。

```go
// buildAndExecute 构建并执行查询管道，返回搜索结果。
func buildAndExecute(ctx context.Context, p plan.LogicalPlan, idx *indexState,
    snap types.Snapshot, segSet *storage.SegmentSet, from, size int) ([]esapi.SearchHit, int, error) {

    var allHits []scoredHit
    var total int

    // 1. 段级执行：为每个 segment 构造 operator 链
    for _, entry := range segSet.Items() {
        ops := buildSegmentPipeline(p, entry, snap, idx)
        hits, cnt := executePipeline(ctx, ops)
        allHits = append(allHits, hits...)
        total += cnt
    }

    // 2. Memtable 搜索（D1/D4：两阶段 mini 倒排）
    mtHits := searchMemtable(ctx, p, idx, snap)
    allHits = append(allHits, mtHits...)

    // 3. 多段合并（D2/D7：fan-out 算子混合自适应）
    merged := mergeSortHits(allHits, from, size)

    // 4. 结果组装：SplitDocID → Segment.DocID/Source
    hits := assembleHits(merged, segSet, idx)
    return hits, total, nil
}
```

**MergeSortOp（D2/D7 混合自适应）**：

在 `internal/engine/exec/operators.go` 新增：

```go
// MergeSortOp 管理多段扇出和归并排序。
// 混合自适应策略（D7）：
//   - 小 K（K ≤ 200）且段数 ≤ 4：批量收集所有子算子结果到内存排序
//   - 大 K 或段数 > 4：降级为拉取式堆归并（Volcano 模型）
type MergeSortOp struct {
    inputs   []Operator    // 每段的 operator 链
    from, size int
    // 自适应策略选择
    strategy mergeStrategy
}

type mergeStrategy int
const (
    mergeBatch    mergeStrategy = iota  // 批量收集
    mergeHeap                            // 堆归并
)

func NewMergeSort(inputs []Operator, from, size int) *MergeSortOp {
    // 自适应选择策略
    totalCap := len(inputs) * (from + size)
    if totalCap <= 200*4 {
        return &MergeSortOp{inputs: inputs, from: from, size: size, strategy: mergeBatch}
    }
    return &MergeSortOp{inputs: inputs, from: from, size: size, strategy: mergeHeap}
}
```

**Memtable 搜索路径**：

```go
// searchMemtable 在活跃 memtable 中搜索未 flush 数据。
// 使用 mini 倒排索引（D1/D4 两阶段策略）：
//   - 文本查询：查询 miniInvertedIndex 的 hash map（实时阶段）
//   - 向量查询：暴力距离计算 + TopN
//   - 标量查询：遍历 memtable 行 + 谓词过滤
func searchMemtable(ctx context.Context, p plan.LogicalPlan, idx *indexState, snap types.Snapshot) []scoredHit {
    mt := idx.shard.MemTable()
    switch p.(type) {
    case *plan.BM25:
        return searchMemtableBM25(mt, idx.miniInv, p.(*plan.BM25))
    case *plan.ANN:
        return searchMemtableKNN(mt, idx, p.(*plan.ANN))
    case *plan.Scan, *plan.Filter:
        return searchMemtableScan(mt, idx, p, snap)
    }
}
```

**依赖关系**：
- 依赖 `exec.Operator` 算子链构建
- 依赖 `miniInvertedIndex` 搜索 memtable 数据
- 依赖 `segment.Segment` 的 DocID/Source 取回
- 被 `engine.go` 的 Search 方法调用

#### 2.1.6 Flush 扩展索引构建（`internal/storage/flush.go`）

**变更内容**：在 `flushTable` 中新增倒排和标量索引构建步骤。

```go
// flushTable 扩展后的流程：
//   1. 冻结 memtable
//   2. 编码列存（col_*.bin）→ 已有
//   3. 构建 HNSW 索引 → 已有 (buildVectorIndexes)
//   4. 构建倒排索引 (buildInvertedIndexes)  ← 新增
//   5. 构建标量索引 (buildScalarIndexes)   ← 新增
//   6. 写段 footer → fsync → rename 原子发布 → 已有
//   7. 加载所有索引到 SegmentEntry.Indexes   ← 修改
```

**buildInvertedIndexes**：
```go
func (f *FlushWorker) buildInvertedIndexes(mt *memtable.MemTable, tmpDir string,
    fieldDefs map[types.FieldID]*meta.FieldDef, out *segmentSetHolder) error {
    for fid, def := range fieldDefs {
        if def.Type != "text" && def.Type != "keyword" {
            continue
        }
        provider := newMemtableTextColumnProvider(mt, fid, def)
        idx, err := index.NewIndex(index.KindInverted)
        if err != nil { return err }
        idxDir := filepath.Join(tmpDir, "inv.idx", fmt.Sprintf("%d", fid))
        output := newDirIndexOutput(idxDir)
        if err := idx.Build(provider, output, nil); err != nil { return err }
    }
    return nil
}
```

**buildScalarIndexes**：
```go
func (f *FlushWorker) buildScalarIndexes(mt *memtable.MemTable, tmpDir string,
    fieldDefs map[types.FieldID]*meta.FieldDef) error {
    for fid, def := range fieldDefs {
        switch def.Type {
        case "long", "date":
            provider := newMemtableLongColumnProvider(mt, fid, def)
            idx, _ := index.NewIndex(index.KindBKD)
            idx.Build(provider, newDirIndexOutput(filepath.Join(tmpDir, "scalar.idx", fmt.Sprintf("%d", fid))), nil)
        case "keyword":
            provider := newMemtableBytesColumnProvider(mt, fid, def)
            idx, _ := index.NewIndex(index.KindBitmap)
            idx.Build(provider, newDirIndexOutput(filepath.Join(tmpDir, "scalar.idx", fmt.Sprintf("%d", fid))), nil)
        }
    }
    return nil
}
```

#### 2.1.7 Engine 重写（`internal/embedded/engine.go`）

**变更内容**：完全重写 Engine 结构体和所有方法，替换极简内存引擎为真实引擎栈。

```go
type Engine struct {
    manager *indexManager
    store   *meta.Store
    dataDir string
    cfg     *config.Config
    mu      sync.RWMutex
}

func New(dataDir string) (*Engine, error) {
    store, err := meta.OpenStore(filepath.Join(dataDir, "meta"))
    if err != nil { return nil, err }
    mgr := newIndexManager(store, dataDir)
    if err := mgr.recoverAll(context.Background()); err != nil {
        return nil, err
    }
    return &Engine{manager: mgr, store: store, dataDir: dataDir}, nil
}

func (e *Engine) Search(ctx context.Context, indices []string, body []byte) (*esapi.SearchResult, error) {
    q, from, size, err := dsl.ParseSearchBody(body)
    if err != nil { return nil, err }

    // 跨多索引搜索：合并各索引结果
    var allHits []esapi.SearchHit
    var total int
    for _, idxName := range indices {
        idx, ok := e.manager.getIndex(idxName)
        if !ok { continue }
        snap, segSet := idx.shard.Snapshot()
        plan := translateQuery(q, idx, snap, segSet, from, size)
        optPlan, _ := plan_optimize.Optimize(plan, 0, size)
        hits, cnt, err := buildAndExecute(ctx, optPlan, idx, snap, segSet, from, size)
        if err != nil { return nil, err }
        allHits = append(allHits, hits...)
        total += cnt
    }
    // 全局 TopN + 组装响应
    return assembleSearchResult(allHits, total, from, size), nil
}
```

**空白导入**（确保 init() 注册生效）：
```go
import (
    _ "github.com/unisearch-db/unisearch/internal/index/hnsw"
    _ "github.com/unisearch-db/unisearch/internal/index/inverted"
    _ "github.com/unisearch-db/unisearch/internal/index/scalar"
)
```

### 2.2 Phase 1-11: M1 ES 兼容 GA

#### 2.2.1 W5.1 FST 词典

**设计引用**：`docs/go-design/02-index-engine.md §3.1`

**变更内容**：
- 新建 `internal/index/inverted/fst.go`：自研 FST（字节序构建 + 输出共享前缀 + mmap 只读）
- 新建 `internal/index/inverted/fuzzy.go`：Levenshtein 自动机（状态数上限 32）
- 修改 `internal/index/inverted/dict.go`：`TermDict` 从排序数组替换为 FST 实现（接口不变）

**关键接口**（接口不变，实现替换）：
```go
type TermDict interface {
    Get(term []byte) (TermID, bool)
    Fingerprint(term []byte) uint64
    Iterate(prefix []byte) TermIterator
}
```

**DoD**：词典内存较数组版降 ≥60%；fuzz 查询用例通过

#### 2.2.2 W5.2 WAND/Block-Max WAND

**设计引用**：`docs/go-design/02-index-engine.md §3.4`

**变更内容**：
- 新建 `internal/index/inverted/wand.go`：词项剪枝 + 段上界剪枝
- 修改 `internal/index/inverted/index.go`：`SearchTerms` 改用 WAND 而非全量遍历
- 修改 `internal/engine/exec/operators.go`：`BM25ScoreOp` 集成段上界剪枝

**DoD**：B4 P99 <80ms（1 亿文档 BM25 检索）

#### 2.2.3 W5.3 聚合全量

**设计引用**：`docs/go-design/03-query-engine.md §6`

**变更内容**：
- 新建 `internal/engine/agg/aggregator.go`：向量化分组聚合（flat 开放寻址哈希表 + 列批 SIMD 归约）
- 新建 `internal/engine/agg/hll.go`：HLL++（sparse/dense 自适应，`precision_threshold` 语义与 ES 一致）
- 新建 `internal/engine/agg/tdigest.go`：t-digest（centroid 压缩序列化）
- 新建 `internal/engine/agg/pipeline.go`：PartialState 协议（自定义二进制序列化，D5）+ pipeline 聚合
- 修改 `internal/engine/exec/operators.go`：新增 `AggregateOp`
- 修改 `internal/engine/plan/logical.go` + `optimizer.go`：新增 `Aggregate` 节点 + 优化规则
- 修改 `embedded/translator.go` + `planner.go`：DSL aggs 翻译 + 管道构建

**PartialState 协议（D5 自定义二进制）**：
```go
type PartialState interface {
    Encode() []byte          // 自定义二进制编码
    Decode([]byte) error     // 自定义二进制解码
    Merge(other PartialState) PartialState
    Finalize() (json.RawMessage, error)
}
```

**DoD**：`04 §8.4` 兼容矩阵 full 项全过；先实现核心 6 类（sum/avg/min/max/count/cardinality）

#### 2.2.4 W5.4 search_after/PIT/scroll

**设计引用**：`docs/go-design/03-query-engine.md §8`、`04-es-compat-layer.md §6.4`

**变更内容**：
- 新建 `internal/server/esapi/pit.go`：PIT 生命周期（TTL 5 分钟 + LRU 上限 10000）
- 新建 `internal/server/esapi/scroll.go`：scroll 转译 PIT + search_after
- 修改 `internal/storage/shard.go`：快照 pin 机制（D6 后台 GC + tombstone）
- 修改 `internal/server/esapi/router.go`：新增 PIT/scroll 路由
- 修改 `embedded/engine.go`：Search 增加 PIT 支持

**段 GC 机制（D6）**：
```go
// shard.go 新增：
type segmentGC struct {
    // tombstoned 段：merge 产出新段后旧段标记为 tombstone
    // 后台 GC 定期扫描：无 PIT 引用且无活跃读快照引用时物理删除
    tombstoned map[types.SegmentID]time.Time  // segID → tombstone 时间
    pitRefs    map[types.SegmentID]int        // segID → PIT 引用计数
}
```

**DoD**：深分页 100 万行稳定

#### 2.2.5 W5.5 highlight/suggester

**设计引用**：`docs/go-design/02-index-engine.md §3.5/§7`、`03-query-engine.md §10`

**变更内容**：
- 新建 `internal/engine/highlight/unified.go`：unified highlighter（基于 posting positions + 查询词项重匹配）
- 新建 `internal/engine/suggest/term.go`：term suggester（编辑距离自动机，复用 FST fuzzy 基建）
- 新建 `internal/engine/suggest/phrase.go`：phrase suggester
- 新建 `internal/engine/suggest/completion.go`：completion suggester（FST 前缀查找）
- 修改 `embedded/engine.go`：Search 响应增加 highlight/suggest

**DoD**：ES 输出结构 diff 通过

#### 2.2.6 W5.6 脚本引擎

**设计引用**：`docs/go-design/03-query-engine.md §9`

**变更内容**：
- 新建 `internal/engine/script/painless.go`：AST 直译子集（算术/逻辑/比较/字符串/`doc['f'].value`/`_score`/三目/`emit`）
- 新建 `internal/engine/script/vector_funcs.go`：`cosineSimilarity`/`dotProduct`/`l1norm`/`l2norm`（复用 `internal/simd`）
- 新建 `internal/engine/script/budget.go`：步数预算 10 万步 + 静态语法白名单
- 修改 `internal/engine/dsl/ast.go` + `parser.go`：新增 `ScriptScoreQuery`/`ScriptQuery`
- 修改 `embedded/translator.go`：script 查询翻译

**DoD**：步数预算注入 fuzz 测试通过

#### 2.2.7 W5.7 rescore/collapse/runtime_mappings/profile

**设计引用**：`docs/go-design/03-query-engine.md §10/§11`

**变更内容**：
- 修改 `internal/engine/exec/operators.go`：`RescoreOp`（窗口内二次打分）+ `CollapseOp`（按字段去重 + inner_hits）
- 新建 `internal/engine/profile/tracer.go` + `render.go`：对齐 ES profile 结构
- 修改 `internal/server/esapi/handlers.go`：profile 响应

**DoD**：profile 与 ES 字段对齐

#### 2.2.8 W5.8 API 全量

**设计引用**：`docs/go-design/04-es-compat-layer.md §2.2/§6`

**变更内容**：
- 新建处理器：`msearch.go`、`fieldcaps.go`、`termsenum.go`、`explain.go`、`byquery.go`（_delete_by_query/_update_by_query）、`reindex.go`、`template.go`、`ilm.go`
- 修改 `router.go`：大量新路由
- 修改 `engine.go`：Engine 接口新增方法
- 修改 `handlers.go`：新 API 处理器

**新增 Engine 接口方法**：
```go
// W5.8 新增（追加到现有 17 个方法之后）
MSearch(ctx, indices []string, bodies [][]byte) ([]*SearchResult, error)
FieldCaps(ctx, indices []string) (*FieldCapsResult, error)
TermsEnum(ctx, index, field, prefix string) ([]string, error)
Explain(ctx, index, id string, body []byte) (*ExplainResult, error)
DeleteByQuery(ctx, index string, body []byte) (*ByQueryResult, error)
UpdateByQuery(ctx, index string, body []byte) (*ByQueryResult, error)
Reindex(ctx, body []byte) (*ReindexResult, error)
PutIndexTemplate(ctx, name string, body []byte) error
GetIndexTemplate(ctx, name string) ([]byte, error)
PutILMPolicy(ctx, name string, body []byte) error
```

**DoD**：金标准用例逐 API 通过

#### 2.2.9 W5.9 BM25 全局统计

**设计引用**：`docs/go-design/03-query-engine.md §7`

**变更内容**：
- 新建 `internal/engine/stats/global_stats.go`：跨段 docFreq 聚合 + 统计表维护
- 新建 `internal/engine/stats/collector.go`：段 footer term_stats 上报
- 修改 `internal/index/inverted/index.go`：`SearchTerms` 使用全局统计（`global_stats=true` 时 IDF 用全局表）
- 修改 `internal/storage/flush.go`：flush 时上报 term_stats

**DoD**：跨分片分数一致（模拟多分片单机）

#### 2.2.10 W5.10 多租户 shared-shard

**设计引用**：`docs/go-design/05-distributed.md §5`

**变更内容**：
- 新建 `internal/tenant/resource_group.go`：资源组管理
- 新建 `internal/tenant/quota.go`：四维配额（存储字节/向量条数/查询 QPS/写入 TPS）+ 429 背压
- 新建 `internal/tenant/admission.go`：查询并发 semaphore + 写入令牌桶
- 修改 `embedded/engine.go`：Write/Search 增加 tenant 隔离（D9 嵌入式也支持）
- 修改 `config/config.go`：多租户配置

**DoD**：配额超限注入测试通过（429 + 租户标识错误体）

#### 2.2.11 W5.11 兼容收口

**设计引用**：`docs/go-design/04-es-compat-layer.md §3/§9`

**变更内容**：
- 新建 `test/golden/`：全量翻译 elasticsearch-py 测试用例为 Go 测试（D8，约 200-300 个），按 API 分组
- 新建 `test/integration/`：TestServer（嵌入式引擎 + httptest）+ TestClient（HTTP 调用封装）
- 新建 `test/compat/`：兼容矩阵报告生成器 + 差异清单
- 引入 `github.com/elastic/go-elasticsearch/v8` 测试依赖

**金标准测试结构（D8 全量翻译）**：
```
test/golden/
├── golden_test.go           # 测试入口（TestServer 初始化 + 用例加载）
├── search_golden_test.go     # _search 用例组
├── bulk_golden_test.go       # _bulk 用例组
├── doc_golden_test.go        # _doc CRUD 用例组
├── mapping_golden_test.go    # _mapping 用例组
├── count_golden_test.go      # _count 用例组
├── mget_golden_test.go       # _mget 用例组
├── msearch_golden_test.go    # _msearch 用例组
├── aggregations_golden_test.go  # aggs 用例组
├── pit_scroll_golden_test.go  # PIT/scroll 用例组
├── highlight_suggest_golden_test.go  # highlight/suggester 用例组
├── template_ilm_golden_test.go  # template/ILM 用例组
└── cat_golden_test.go       # _cat 用例组
```

**DoD**：**elasticsearch-py/go-elasticsearch 套件通过率 ≥90%** + 兼容矩阵报告（✅ 占比 ≥80%）

---

## 3. 接口设计 (Interface Design)

### 3.1 esapi.Engine 接口（Phase 0 不变，M1 扩展）

现有 17 个方法签名不变（接线后实现变更），W5.8 新增 10+ 个方法（见 §2.2.8）。

### 3.2 index.RouteForField（D3 新增）

```go
func RouteForField(esFieldType string) []IndexKind
```

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| esFieldType | string | 是 | ES 字段类型（text/dense_vector/keyword/long/date/boolean/double） |

**返回值**：

| 类型 | 说明 |
|------|------|
| []IndexKind | 适用的索引类型列表（如 text→[KindInverted]，dense_vector→[KindHNSW]） |

### 3.3 exec.MergeSortOp（D2/D7 新增）

```go
func NewMergeSort(inputs []Operator, from, size int) *MergeSortOp
func (m *MergeSortOp) Next(ctx context.Context) (*ColumnBatch, error)
func (m *MergeSortOp) Close()
```

### 3.4 agg.PartialState（D5 新增）

```go
type PartialState interface {
    Encode() []byte
    Decode([]byte) error
    Merge(other PartialState) PartialState
    Finalize() (json.RawMessage, error)
}
```

### 3.5 pit.PITManager（W5.4 新增）

```go
type PITManager interface {
    Create(ctx context.Context, index string, ttl time.Duration) (pitID string, err error)
    Search(ctx context.Context, pitID string) (snap types.Snapshot, err error)
    Delete(ctx context.Context, pitID string) error
}
```

### 3.6 script.Evaluator（W5.6 新增）

```go
type Evaluator interface {
    Eval(ctx *ScriptCtx) (any, error)
}
```

---

## 4. 内部设计 (Internal Design)

### 4.1 embedded.Engine

**职责**：HTTP 服务层与引擎栈之间的适配层，实现 `esapi.Engine` 接口

**核心方法**：

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `New` | `(dataDir string) (*Engine, error)` | 创建引擎，恢复已有索引 |
| `Write` | `(ctx, IndexDoc, refresh) (*WriteResult, error)` | 单文档写入：ensureIndex → Shard.Write |
| `Search` | `(ctx, indices, body) (*SearchResult, error)` | 搜索：DSL解析 → translateQuery → Optimize → buildAndExecute |
| `Get` | `(ctx, index, id) (source, seqNo, primary, notFound, err)` | 点查：Shard.Snapshot → LookupID |
| `Refresh` | `(ctx, index) error` | 刷新：Shard.FlushAll |
| `Close` | `() error` | 关闭：manager.closeAll + store.Close |

**依赖关系**：
- 依赖 `indexManager` 管理索引生命周期
- 依赖 `translator` + `planner` 执行查询
- 依赖 `meta.Store` 持久化元数据

### 4.2 embedded.indexState

**职责**：单个索引的运行时状态，桥接 meta 与 Shard

**核心方法**：

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `maybeApplyDynamic` | `(doc map[string]any)` | 动态映射推断，新字段分配 FieldID |
| `resolveField` | `(name string) (FieldID, *FieldDef)` | 字段名→FieldID + 字段定义 |
| `mappingFn` | `() map[FieldID]*FieldDef` | 返回当前映射（供 ShardConfig.MappingFn） |

### 4.3 storage.segmentGC（D6 新增）

**职责**：段生命周期管理，与 PIT 快照 pin 协同

**核心方法**：

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `tombstone` | `(segID SegmentID)` | 标记段为 tombstone（merge 后调用） |
| `pinSegment` | `(segID SegmentID)` | 增加段引用计数（PIT 创建时调用） |
| `unpinSegment` | `(segID SegmentID)` | 减少段引用计数（PIT 删除时调用） |
| `gc` | `()` | 后台清理：tombstoned 且无引用的段物理删除 |

---

## 5. 数据模型 (Data Model)

### 5.1 indexState

```go
type indexState struct {
    name      string
    shard     *storage.Shard
    fieldIDs  map[string]types.FieldID
    fieldDefs map[types.FieldID]*meta.FieldDef
    miniInv   *miniInvertedIndex   // D1/D4 memtable mini 倒排
    cancel    context.CancelFunc
    mu        sync.RWMutex
}
```

| 字段名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| name | string | 是 | 索引名 |
| shard | *storage.Shard | 是 | 分片聚合根 |
| fieldIDs | map[string]types.FieldID | 是 | 字段名→FieldID 映射 |
| fieldDefs | map[types.FieldID]*meta.FieldDef | 是 | FieldID→字段定义映射 |
| miniInv | *miniInvertedIndex | 是 | memtable mini 倒排索引（D1/D4） |
| cancel | context.CancelFunc | 是 | 停止 shard 后台 goroutine |

### 5.2 IndexMeta 扩展

```go
// meta/model.go 新增字段
type IndexMeta struct {
    // ... 现有字段 ...
    FieldIDs map[string]types.FieldID `json:"field_ids,omitempty"` // 新增：持久化 FieldID 映射
}
```

**约束条件**：
- `_id` 固定为 FieldID(1)
- FieldID 从 2 开始递增，不复用已删除字段的 ID
- 向后兼容：旧 meta 文件无 FieldIDs 字段时，恢复时按 Properties 排序重建

### 5.3 miniInvertedIndex（D1/D4）

```go
type miniInvertedIndex struct {
    postings map[string]map[int32]struct{}  // term→rowIDs
    docFreq  map[string]int                  // term→doc frequency
    frozen   inverted.LoadedIndex            // 冻结后构建的正式索引（nil 表示活跃中）
    mu       sync.RWMutex
}
```

### 5.4 scoredHit（内部传输结构）

```go
type scoredHit struct {
    segID  uint64
    rowID  uint32
    score  float32
    source []byte  // 仅 memtable 命中时填充
}
```

---

## 6. 错误处理 (Error Handling)

### 6.1 错误类型定义

| 错误码/类型 | 触发条件 | 处理方式 | 用户提示 |
|-------------|----------|----------|----------|
| `ErrIndexNotFound` | 操作不存在的索引 | 404 | `index_not_found_exception` |
| `ErrFieldNotFound` | 查询引用不存在的字段 | 400 | `field_not_found_exception` |
| `ErrFieldTypeMismatch` | 查询类型与字段类型不匹配（如 match 查询 numeric 字段） | 400 | `query_shard_exception` |
| `ErrFromSizeTooLarge` | from+size > 10000 | 400 | `from + size must be <= 10000` |
| `ErrMappingConflict` | 动态映射推断与已有 mapping 冲突 | 400 | `mapper_parsing_exception` |
| `ErrScriptBudgetExceeded` | painless 步数超 10 万步 | 400 | `script_exception: exceeded step budget` |
| `ErrQuotaExceeded` | 多租户配额超限 | 429 | `quota_exceeded_exception` + 租户标识 |
| `ErrPITNotFound` | 查询不存在的 PIT | 404 | `search_context_missing_exception` |
| `ErrFSTBuildFailed` | FST 构建失败（内存不足等） | 500 | `internal server error` |

### 6.2 异常传播策略

- 引擎层错误通过 `APIError` 包装为 ES 风格错误体，由 handler 统一序列化
- panic 通过 `withRecovery` 中间件捕获，返回 500 不崩进程
- WAL/Flush 层错误通过 error 返回值传播到 Engine，再映射为适当的 HTTP 状态码

### 6.3 边界情况处理

| 场景 | 输入示例 | 期望行为 |
|------|----------|----------|
| 空索引搜索 | 搜索刚创建无文档的索引 | 返回空 hits，total=0 |
| 全部删除后搜索 | 所有文档已 DELETE | 返回空 hits |
| memtable 满期间搜索 | 512MB memtable 冻结中 | 搜索冻结的 memtable + 已有 segment |
| 并发写入 + 搜索 | 多 goroutine 同时 Write 和 Search | Snapshot 隔离，无脏读 |
| 重启后 FieldID 恢复 | meta 有 FieldIDs 字段 | 重建 fieldIDs/fieldDefs，FieldID 不变 |
| 旧 meta 无 FieldIDs | 升级后打开旧索引 | 按 Properties 排序重建 FieldIDs，持久化 |
| PIT 期间 merge | PIT 活跃时触发段合并 | 旧段 tombstone 不删除，PIT 释放后 GC |
| 多租户配额超限 | 写入超 TPS 限制 | 429 + 租户标识错误体 |

---

## 7. 测试规划 (Test Plan)

### 7.1 测试策略表

| 测试类型 | 覆盖范围 | 目标覆盖率 | 说明 |
|----------|----------|------------|------|
| 单元测试 | Phase 0 核心逻辑（translator/planner/docparse/manager/miniinv） | ≥ 90% | 每个翻译路径、管道构建路径有单测 |
| 单元测试 | M1 新包（fst/wand/agg/highlight/suggest/script/profile/stats/tenant） | ≥ 90% | 各包核心函数单测 |
| 单元测试 | 工具/辅助代码（textcol/docparse 类型转换） | ≥ 70% | 类型转换覆盖主要 JSON 类型 |
| 集成测试 | Phase 0 接线（engine_test：Write/Search/Delete/Restart 全链路） | - | 引擎栈端到端验证 |
| 集成测试 | M1 API 全量（每处理器 HTTP→Engine→响应形状） | - | 逐 API 验证 ES 兼容性 |
| E2E 测试 | R18 适配真实引擎栈 | - | BM25/ANN 融合检索 + 重启零丢失 |
| 金标准 | elasticsearch-py 全量用例翻译（D8，200-300 个） | ≥ 90% 通过率 | HTTP 状态码 + JSON 结构 diff |

### 7.2 关键测试场景清单

#### 正向用例（主成功场景）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| 写入+点查 | 写入 1 条文档 → GET | _source 完整，SeqNo 递增 | P0 |
| match 搜索 | 写入含 text 字段文档，flush 后 match 查询 | BM25 评分排序，非子串匹配 | P0 |
| knn 搜索 | 写入含 dense_vector 文档，flush 后 knn 查询 | ANN 召回，按距离排序 | P0 |
| hybrid 搜索 | knn + match 混合查询 | RRF/weighted 融合排序 | P0 |
| 重启恢复 | 写入 → Close → 重新 New → GET/Search | 全部数据可见（WAL + segment 恢复） | P0 |
| bool 查询 | must/should/filter/must_not 组合 | 正确的布尔逻辑 | P1 |
| term/range 过滤 | keyword/long 字段过滤 | 精确匹配/范围匹配 | P1 |
| refresh 后可搜索 | 写入不 flush → 搜索不可见 → refresh → 可见 | memtable flush 到段后索引可用 | P0 |
| memtable 实时搜索 | 写入后不 flush → match 查询 | D1/D4 mini 倒排实时可搜索 | P0 |

#### 边界用例（输入边界、状态边界、并发边界）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| 空索引搜索 | 搜索无文档索引 | 空 hits，total=0 | P0 |
| from+size 上限 | from+size=10001 | 400 错误 | P0 |
| 全部删除后搜索 | DELETE 所有文档后搜索 | 空 hits | P1 |
| 大 K 多段合并 | 5 个 segment，K=500 | D7 降级为堆归并 | P1 |
| 并发写入+搜索 | 10 goroutine 并发 | Snapshot 隔离无脏读 | P0 |
| memtable 冻结期间搜索 | 512MB 冻结中搜索 | 搜索冻结 memtable + 已有段 | P1 |
| PIT 期间 merge | PIT 活跃时合并 | 旧段 tombstone 保留 | P1 |
| 旧 meta 无 FieldIDs | 升级后打开旧索引 | 按 Properties 重建 FieldIDs | P1 |

#### 异常用例（错误输入、依赖失败、权限拒绝）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| 搜索不存在索引 | GET /nonexistent/_search | 404 index_not_found | P0 |
| 字段类型不匹配 | match 查询 numeric 字段 | 400 query_shard_exception | P1 |
| mapping 冲突 | 已有 long 字段写入 string | 400 mapper_parsing_exception | P0 |
| painless 步数超限 | 脚本死循环 | 400 script_exception | P1 |
| 多租户配额超限 | 写入超 TPS | 429 quota_exceeded + 租户标识 | P1 |
| panic 恢复 | handler 内部 panic | 500 不崩进程 | P0 |

#### 兼容用例（与现有模块的交互）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| ES 8.x 响应形状 | _search 响应 | _shards/total/relation/max_score/hits 结构正确 | P0 |
| elasticsearch-py 连通 | elasticsearch-py 客户端调用 | GET / 版本探测 + 基础 CRUD | P0 |
| go-elasticsearch 连通 | go-elasticsearch v8 客户端调用 | 全部已实现端点可调用 | P1 |
| if_seq_no 乐观锁 | 并发更新同一文档 | 409 version_conflict_engine_exception | P0 |
| _bulk 混合批次 | 混合合法/非法条目 | HTTP 200 + 逐条错误 | P0 |

### 7.3 Mock 清单

| 依赖项 | Mock 方式 | 模拟行为 | 使用场景 |
|--------|-----------|----------|----------|
| `storage.Shard` | interface mock | 模拟 Write/Snapshot/LookupID/Iterate 返回 | translator/planner 单测 |
| `segment.Segment` | interface mock | 模拟 DocID/Source/ScanColumn 返回 | planner 单测结果组装测试 |
| `index.LoadedIndex` | interface mock | 模拟 Search 返回预设 SearchResult | planner 单测 ANN/BM25 路径 |
| `analyzer.Analyzer` | stub | 返回固定分词结果 | miniinv 单测 |
| HTTP Server | httptest.NewServer | 启动真实嵌入式引擎 + HTTP 层 | 金标准/集成测试 |

### 7.4 测试数据设计

- **文档 fixtures**：构造含 text/keyword/long/dense_vector 多字段类型的合成文档集（10-100 条），覆盖所有字段类型
- **向量 fixtures**：768 维合成向量（sift-128-euclidean 风格），用于 ANN 召回率验证
- **elasticsearch-py 用例**：从 elasticsearch-py GitHub 仓库提取核心测试 YAML，翻译为 Go 测试数据
- **基准数据集**：ann-benchmarks glove/sift（用于 Cross-Link 召回率验证，已有）

### 7.5 验证方式

- **CI 增量**：PR 按路由变更筛选用例，快速反馈
- **夜间全量**：全部金标准用例 + 集成测试 + E2E
- **本地手动**：`make test` 全绿 + `./unisearch --embedded` 手动 curl 验证（见 Proposal §7.1）
- **兼容矩阵**：`test/compat/` 自动生成通过率报告 + 差异清单

---

## 8. 验收标准 (Acceptance Criteria)

### Phase 0 验收
- [ ] `go build ./...` / `go test ./...` / `go vet ./...` 全绿
- [ ] 嵌入式模式 `_search` 走真实引擎栈（BM25 + HNSW + Fusion），非子串匹配
- [ ] 写入→flush→搜索→删除→重启恢复全链路通过
- [ ] memtable 未 flush 数据可通过 mini 倒排实时搜索（D1/D4）
- [ ] 多段搜索通过 MergeSortOp 混合自适应合并（D2/D7）
- [ ] 字段类型通过 Index Registry 声明式路由（D3）
- [ ] HTTP API 响应形状不变（ES 8.x 兼容）

### M1 出口验收
- [ ] **elasticsearch-py/go-elasticsearch 测试套件通过率 ≥90%**
- [ ] 兼容矩阵报告（✅ 占比 ≥80%）
- [ ] W5.1-W5.11 全部 DoD 勾选
- [ ] `go vet ./...` 零报告
- [ ] 所有新包有单测
- [ ] 实现与 `docs/go-design/` 接口签名无偏离
- [ ] PartialState 自定义二进制序列化（D5）
- [ ] PIT 段 GC 后台 tombstone 机制（D6）
- [ ] 嵌入式模式多租户可配置启用（D9）

### Spec 完整性
- [ ] 类与函数签名是否完整？
- [ ] 核心算法步骤是否清晰可执行？
- [ ] 数据结构是否定义完毕？
- [ ] 测试计划是否覆盖边界情况？
- [ ] 可测性：是否包含本地运行的 E2E 测试方案？

---

## 9. 实现清单 (Implementation Checklist)

### Phase 0: M0 引擎接线

- [ ] P0-1 修改 `meta/model.go`：IndexMeta 新增 FieldIDs 字段
- [ ] P0-2 新建 `embedded/textcol.go`：文本/标量列提供器
- [ ] P0-3 修改 `index/registry.go`：新增 RouteForField 声明式路由（D3）
- [ ] P0-4 修改 `storage/flush.go`：新增 buildInvertedIndexes / buildScalarIndexes + 加载
- [ ] P0-5 新建 `embedded/manager.go`：IndexManager + indexState
- [ ] P0-6 新建 `embedded/docparse.go`：ApplyInsert 回调 + 类型转换
- [ ] P0-7 新建 `embedded/miniinv.go`：两阶段 mini 倒排（D1/D4）
- [ ] P0-8 修改 `engine/exec/operators.go`：新增 MergeSortOp（D2/D7）
- [ ] P0-9 新建 `embedded/translator.go`：DSL→Plan 翻译器 + RouteForField 集成
- [ ] P0-10 新建 `embedded/planner.go`：Plan→Exec 管道 + 多段合并 + memtable 搜索
- [ ] P0-11 重写 `embedded/engine.go`：真实引擎栈接线 + 空白导入
- [ ] P0-12 修改 `test/e2e/embedded_test.go`：R18 适配
- [ ] P0-13 新建 `embedded/engine_test.go`：接线集成测试
- [ ] P0-14 `go build` + `go test` + `go vet` 全绿验证

### Phase 1-11: M1 ES 兼容 GA

- [ ] W5.1 FST 词典 + fuzzy 自动机
- [ ] W5.2 WAND/Block-Max WAND
- [ ] W5.3 聚合全量（agg 包 + PartialState D5）
- [ ] W5.4 search_after/PIT/scroll（段 GC D6）
- [ ] W5.5 highlight/suggester
- [ ] W5.6 脚本引擎 painless 子集
- [ ] W5.7 rescore/collapse/profile
- [ ] W5.8 API 全量（10+ 新处理器）
- [ ] W5.9 BM25 全局统计
- [ ] W5.10 多租户 shared-shard（D9 嵌入式支持）
- [ ] W5.11 兼容收口（金标准 D8 全量翻译 + go-elasticsearch）
