# Spec: 深度审计差距修复 — 详细设计

> @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md

## 概述

对照 `docs/go-design/` 7 篇设计文档，修复 7 个确认 Bug 和 67 项差距，按模块分 4 批交付。本文档定义每项的工程细节：接口签名、数据结构、修改点、测试场景。

## 关键架构决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| Memtable 搜索 | 方案C：Mini倒排+暴力KNN | 复用已有 miniinv.go 做 BM25 搜索，KNN 对小数据量暴力扫描足够，不侵入执行管道 |
| 聚合框架 | 方案B：扩展现有 GroupAggregator | 基于现有 Registry 扩展，改动最小，复用 map[string]*groupBucket 哈希表 |
| LogStore raft 接口 | 方案A：实现 raft LogStore/StableStore 替换 BoltDB | 完全对齐设计 05§2.2，NVMe 主存储 + 对象存储异步归档 |

---

## 第 1 批：存储引擎 + 索引引擎

### 1.1 Bug 修复

#### B1: CmdUnregisterNode 复用 applyDeleteIndex

**文件**: `internal/raft/types.go`, `internal/raft/fsm.go`

```go
// types.go — 新增方法
func (s *MetaState) applyUnregisterNode(data []byte) error {
    var cmd UnregisterNodeCmd
    if err := jsonx.Unmarshal(data, &cmd); err != nil {
        return err
    }
    for i, m := range s.Members {
        if m.NodeID == cmd.NodeID {
            s.Members = append(s.Members[:i], s.Members[i+1:]...)
            s.Version++
            return nil
        }
    }
    return nil // 幂等：节点不存在时静默成功
}

// fsm.go — 修改 apply
case CmdUnregisterNode:
    return s.applyUnregisterNode(log.Data)  // 替换 applyDeleteIndex
```

**测试**: `raft/fsm_test.go` 新增 `TestApplyUnregisterNode`：注册2节点 → UnregisterNode1 → 验证 Members 仅剩1节点。

#### B2: uploadCh 满时静默丢日志段

**文件**: `internal/objstore/logstore.go`

```go
// 修改 Append 方法（logstore.go:60-69）
func (ls *LogStore) Append(ctx context.Context, data []byte) (uint64, error) {
    ls.mu.Lock()
    ls.seq++
    seq := ls.seq
    seg := logSegment{key: ls.s3Key(seq), data: data, localPath: ls.localPath(seq)}
    ls.mu.Unlock()
    select {
    case ls.uploadCh <- seg:
    default:
        // channel 满：同步上传，不丢弃
        if err := ls.s3.PutObject(ctx, seg.key, seg.data); err != nil {
            ls.mu.Lock()
            ls.seq-- // 回滚 seq
            ls.mu.Unlock()
            return 0, fmt.Errorf("logstore: upload failed and channel full: %w", err)
        }
    }
    // NVMe 写入（见 3.1）
    return seq, nil
}
```

**测试**: `objstore/logstore_test.go`（新建）`TestAppendChannelFull`：填满 channel → 追加 → 验证不丢失。

#### B3: highlight 放置在响应顶层

**文件**: `internal/server/esapi/handlers.go`

```go
// handlers.go:500-506 — 移入 hit 内部
// 修改前：resp["highlight"] = hlMap  // 顶层
// 修改后：
for i := range resp.Hits {
    if hl, ok := hlMap[resp.Hits[i].ID]; ok {
        resp.Hits[i].Highlight = hl  // hit 内部
    }
}
```

**SearchHit 结构体**（engine.go）新增 `Highlight map[string][]string` 字段。

**测试**: `golden_test.go` 修改 `TestGolden_Highlight` 验证 highlight 在 hit 内部。

#### B4: 别名 Remove 动作未处理

**文件**: `internal/server/esapi/handlers.go`, `internal/server/esapi/engine.go`

```go
// engine.go — Engine 接口新增
RemoveAlias(ctx context.Context, index, alias string) error

// handlers.go:710-716 — 处理 Remove
for _, a := range req.Actions {
    if a.Add != nil {
        // 现有逻辑
    }
    if a.Remove != nil {
        if err := s.engine.RemoveAlias(ctxFromReq(r), a.Remove.Index, a.Remove.Alias); err != nil {
            // 错误处理
        }
    }
}
```

**embedded/engine.go** 实现 `RemoveAlias`：从 `IndexMeta.Aliases` 移除别名。

#### B5: 花括号展开误拆

**文件**: `internal/server/esapi/router.go`

```go
// router.go — 新增 expandBraces
func expandBraces(s string) []string {
    start := strings.IndexByte(s, '{')
    if start < 0 { return []string{s} }
    end := strings.IndexByte(s, '}')
    if end < 0 || end <= start { return []string{s} }
    prefix := s[:start]
    suffix := s[end+1:]
    options := strings.Split(s[start+1:end], ",")
    var result []string
    for _, opt := range options {
        result = append(result, prefix+strings.TrimSpace(opt)+suffix)
    }
    return result
}

// parseIndices 中先 expandBraces 再 Split
func (s *Server) parseIndices(r *http.Request) []string {
    raw := r.PathValue("index")
    if raw == "" || raw == "_all" || raw == "*" { return nil }
    var indices []string
    for _, part := range strings.Split(raw, ",") {
        indices = append(indices, expandBraces(part)...)
    }
    return indices
}
```

**测试**: `router_test.go`（新建）`TestExpandBraces`：`logs-{a,b}-*` → `["logs-a-*", "logs-b-*"]`。

#### B6: ctxFromReq 丢弃 cancel

**文件**: `internal/server/esapi/handlers.go`

```go
// 修改 ctxFromReq — 返回 cancel 供调用方 defer
func ctxFromReq(r *http.Request) (context.Context, context.CancelFunc) {
    ctx, cancel := context.WithTimeout(r.Context(), requestTimeout(r))
    return ctx, cancel
}

// 各 handler 改为：
ctx, cancel := ctxFromReq(r)
defer cancel()
```

`requestTimeout` 新增函数：解析 `?timeout` 参数覆盖默认值（_search=30s, _bulk=60s）。

#### B7: Makefile fuzz target 名称

**文件**: `Makefile`

```makefile
# 修改
fuzz: ## Run fuzz tests
	go test -fuzz=FuzzParseQuery -fuzztime=60s ./internal/engine/dsl/
	go test -fuzz=FuzzWALReplay -fuzztime=60s ./internal/storage/wal/
	go test -fuzz=FuzzSegmentFormat -fuzztime=60s ./internal/storage/segment/
	go test -fuzz=FuzzQueryString -fuzztime=60s ./internal/engine/dsl/
```

---

### 1.2 存储引擎 P0 缺失

#### 1.2.1 幂等写入 client_id+sequence

**文件**: `internal/storage/memtable/memtable.go`, `internal/storage/shard.go`

```go
// memtable.go — 新增
type DedupWindow struct {
    maxLen   int
    entries  map[string]uint64  // client_id → max_seq
    order    []string            // LRU 顺序
}

func NewDedupWindow(capacity int) *DedupWindow {
    return &DedupWindow{
        maxLen: capacity,  // 1_000_000
        entries: make(map[string]uint64),
    }
}

func (w *DedupWindow) Check(clientID string, seq uint64) (duplicate bool) {
    if clientID == "" { return false }
    if prev, ok := w.entries[clientID]; ok && seq <= prev {
        return true  // 重复
    }
    w.entries[clientID] = seq
    w.order = append(w.order, clientID)
    if len(w.order) > w.maxLen {
        old := w.order[0]
        w.order = w.order[1:]
        delete(w.entries, old)
    }
    return false
}

// MemTable 结构体新增 dedup *DedupWindow
// AppendBatch 接口新增 ClientID/Sequence 可选字段
```

`WriteBatch` 结构体（`shard.go`）新增 `ClientID string` + `Sequence uint64` 字段。`Shard.Write` 传入 MemTable 前检查去重窗口。

**测试**: `memtable_test.go` 新增 `TestDedupWindow`：相同 client_id+sequence → 跳过；不同 sequence → 写入；LRU 淘汰。

#### 1.2.2 合并后索引重建

**文件**: `internal/storage/merge.go`

```go
// MergeSegments 中（merge.go:75-84 之后）增加
// 现有：列数据写入 SegmentBuilder
// 新增：倒排索引构建
func (m *MergeRunner) buildInvertedIndexes(builder *segment.SegmentBuilder, sources []mergeSource) error {
    // 复用 flush.go 的 buildInvertedIndexes 逻辑
    // 遍历 text 字段 → analyzer 分词 → posting list → 写入 inv.idx/
}
// 新增：标量索引构建
func (m *MergeRunner) buildScalarIndexes(builder *segment.SegmentBuilder, sources []mergeSource) error {
    // 复用 flush.go 的 buildScalarIndexes 逻辑
    // BKD/Bitmap/Bloom 索引写入 scalar.idx/
}
```

将 `flush.go` 中的 `buildVectorIndexes`/`buildInvertedIndexes`/`buildScalarIndexes` 提取为可复用函数，flush 和 merge 共用。

**测试**: `merge_test.go` 新增 `TestMergePreservesIndexes`：写入含 text/keyword/long 字段文档 → flush 2 段 → 合并 → 验证新段可搜索/可过滤。

#### 1.2.3 多 shard 全局 flush 并发限制

**文件**: `internal/storage/flush.go`

```go
// flush.go — 新增全局限制器
var flushLimiter = semaphore.NewWeighted(int64(min(4, runtime.NumCPU()/2)))

// FlushWorker.flushTable 中获取许可
func (w *FlushWorker) flushTable(ctx context.Context, t *memtable.Table) error {
    if err := flushLimiter.Acquire(ctx, 1); err != nil {
        return err
    }
    defer flushLimiter.Release(1)
    // 现有 flush 逻辑
}
```

**测试**: `flush_test.go` 新增 `TestGlobalFlushConcurrency`：启动 >4 个 shard 并行 flush → 验证同时执行 ≤4。

#### 1.2.4 WAL async 模式（refresh=false）

**文件**: `internal/storage/wal/local.go`

```go
// LocalWAL 新增 asyncMode bool 字段
// Append 方法增加 async 参数

func (w *LocalWAL) Append(ctx context.Context, entries ...Entry) (SeqNo, error) {
    // 现有逻辑：追加到 pending → commitLoop 批量 fsync
    // async 模式：追加后立即返回，不等 fsync
    // sync 模式（wait_for）：阻塞到 commitSeq >= mySeq
}

// 新增 AppendAsync：追加后不等 fsync，100ms 后台兜底
func (w *LocalWAL) AppendAsync(ctx context.Context, entries ...Entry) (SeqNo, error)
```

`Shard.Write` 根据 refresh 参数选择 `Append`（sync）或 `AppendAsync`。

#### 1.2.5 checksum 递归子目录

**文件**: `internal/storage/segment/builder.go`

```go
// builder.go:292-323 — 修改 checksumManifest 递归遍历
func (b *SegmentBuilder) checksumManifest() ([]byte, error) {
    entries := make(map[string]uint32)
    err := filepath.Walk(b.tmpDir, func(path string, info os.FileInfo, err error) error {
        if err != nil { return err }
        if info.IsDir() { return nil }  // 递归进入子目录
        rel, _ := filepath.Rel(b.tmpDir, path)
        crc, err := fileCRC32c(path)
        if err != nil { return err }
        entries[rel] = crc
        return nil
    })
    // 序列化 entries 到 footer
}
```

---

### 1.3 索引引擎差距

#### 1.3.1 AVX2/NEON SIMD 汇编核

**新增文件**: `internal/simd/asm/gen.go`（avo 生成器）, `internal/simd/dist_amd64.s`, `internal/simd/dist_arm64.s`, `internal/simd/dist_asm.go`

```go
// asm/gen.go — avo 生成器（编译时运行生成 .s）
//go:build ignore

package main

import (
    "github.com/mmcloughlin/avo/build"
    "github.com/mmcloughlin/avo/operand"
)

func main() {
    // 生成 DotF32 AVX2+FMA 实现
    // 16 float/batch：VMOVUPS + VFMADD231PS + VADDPS
    // 生成 L2F32：VSUBPS + VMULPS + VADDPS
    build.Generate()
}

// dist_asm.go — simd_asm build tag
//go:build simd_asm

package simd

func init() {
    DotF32 = dotF32Asm
    L2F32 = l2F32Asm
    DotF32Batch16 = dotF32Batch16Asm
}
```

单测 `dist_test.go` 增加对齐验证：随机数据 + 边界 0/1/15/16/17。

#### 1.3.2 Iterative 过滤策略

**文件**: `internal/index/hnsw/filter.go`

```go
// filter.go:106-109 — 10-30% 区间改为 Iterative
case sel.Rate >= 0.10 && sel.Rate <= 0.30:
    plan.Strategy = StrategyIterative
    plan.CandFactor = 3

// ExecuteFilterPlan 新增 Iterative 路径
func ExecuteFilterPlan(...) {
    case StrategyIterative:
        // 1. WAND 产出候选位图（复用 inverted.WANDScorer）
        // 2. 图遍历仅入候选位图命中节点
        // 3. 不足 k → 回退 post-filter（ef×2 重试）
        return iterativeFilter(ctx, li, con, q, k, plan)
}

func iterativeFilter(...) (*SearchResult, error) {
    // WAND 剪枝产出候选集 → 转为 roaring.Bitmap
    // 图遍历 filter=候选位图.Contains(node)
    // 不足 k 时 post-filter 兜底
}
```

#### 1.3.3 Cross-Link 单堆跳段

**文件**: `internal/index/hnsw/multi_search.go`

```go
// MultiSegmentSearcher.Search 改为统一候选堆
func (m *MultiSegmentSearcher) Search(ctx context.Context, q Query, budget SearchBudget) (*SearchResult, error) {
    heap := newGlobalTopKHeap(q.K)
    // 1. 从最佳入口段开始
    // 2. 段内贪心下降至 layer 1
    // 3. 顶层遍历时经 crosslink 跳段：对端节点入堆（携带 peerSegID）
    // 4. 弹出堆顶候选 → 精排 → 加入结果
    // 5. 堆空或预算耗尽 → 返回 TopK
}
```

#### 1.3.4 稀疏向量索引/查询

**新增文件**: `internal/engine/dsl/ast_sparse.go`, 修改 `internal/index/inverted/index.go`

```go
// ast_sparse.go
type SparseVectorQuery struct {
    Field    string
    Embedding map[uint32]float32  // {dim: weight}
    Boost    float32
}

// parser.go — 新增 parseSparseVectorQuery

// 倒排复用：写入时 term = fmt.Sprintf("%s\x00%d", field, dim)
// posting tf 槽存 uint16(weight * 256)
// 查询时：编码为带权词项集 → 倒排求交 → score = Σ w_q * w_d
```

#### 1.3.5 FST Fingerprint

**文件**: `internal/index/inverted/fst.go`

```go
func (f *FST) Fingerprint(term []byte) uint64 {
    // min-hash：对 FST 路径上的转移做哈希累积
    // 用于跨段词典快速比较
    h := uint64(1469598103934665603) // FNV offset
    node := f.root
    for _, b := range term {
        trans, ok := node.transition(b)
        if !ok { return h }
        h = (h ^ uint64(trans.output)) * 1099511628211
        node = trans.target
    }
    return h
}
```

#### 1.3.6 分析器词典热加载

**文件**: `internal/index/analyzer/factory.go`

```go
// 新增 gse 词典 mtime 轮询
var dictMutex sync.RWMutex
var dictMtime time.Time

func reloadDictIfChanged(dictPath string) {
    fi, err := os.Stat(dictPath)
    if err != nil { return }
    if fi.ModTime().After(dictMtime) {
        dictMutex.Lock()
        defer dictMutex.Unlock()
        chineseSegmenter = sync.OnceValue(func() *gse.Segmenter {
            seg, _ := gse.NewDict(dictPath)  // 重新加载
            dictMtime = fi.ModTime()
            return seg
        })
    }
}

// SIGHUP 立即重载
signal.Notify(sighupCh, syscall.SIGHUP)
go func() {
    for range sighupCh {
        chineseSegmenter = sync.OnceValue(func() *gse.Segmenter { ... })
    }
}()
```

#### 1.3.7 Bloom 块级

**文件**: `internal/index/scalar/bloom.go`

```go
// 改为每 Block 一个 bloom
type BlockBloom struct {
    blooms []*bloom.Bloom  // 每 1024 条目一个
}

func NewBlockBloom(numBlocks int, fpp float64) *BlockBloom {
    blooms := make([]*bloom.Bloom, numBlocks)
    for i := range blooms {
        blooms[i] = NewBloom(1024, fpp)
    }
    return &BlockBloom
}
```

#### 1.3.8 段格式模糊测试

**新增文件**: `internal/storage/segment/encoding_fuzz_test.go`

```go
func FuzzSegmentFormat(f *testing.F) {
    f.Add(validFooter())  // 种子
    f.Fuzz(func(t *testing.T, data []byte) {
        // 随机破坏 footer → 验证明确报错（不 panic）
        _, err := segment.ReadFooter(data)
        if err == nil { t.Error("expected error for corrupt footer") }
    })
}
```

#### 1.3.9 query_string 模糊测试

**新增文件**: `internal/engine/dsl/querystring_fuzz_test.go`

```go
func FuzzQueryString(f *testing.F) {
    f.Add([]byte("title:hello AND body:world"))
    f.Fuzz(func(t *testing.T, data []byte) {
        _, err := ParseSearchBody(data)
        // 不 panic 即可
    })
}
```

---

## 第 2 批：查询引擎 + ES 兼容

### 2.1 聚合引擎全量扩展

#### 2.1.1 新增 9 种 Metric 聚合

**文件**: `internal/engine/agg/aggregator.go`

| 类型 | 实现要点 |
|------|---------|
| extended_stats | 扩展 stats：sum_of_squares/variance/std_deviation |
| percentiles | 注册 TDigest 为 Aggregator，Collect→AddWeighted，Result→Quantile |
| percentile_ranks | 反向百分位：值→排名百分比 |
| top_hits | 收集 TopK 文档（复用 TopN 逻辑），返回 _source/sort/fields |
| weighted_avg | 加权平均：Σ(value×weight)/Σweight |
| median_absolute_deviation | 中位数绝对偏差：median(\|x_i - median(x)\|) |
| geo_centroid | 地理质心：加权平均 lat/lon |
| geo_bounds | 地理边界框：min/max lat/lon |
| boxplot | 箱线图：min/q1/median/q3/max + upper/lower fence |
| string_stats | 字符串统计：count/min_length/max_length/avg_length |

每个类型注册到 `aggRegistry`（`init()` 函数）。

#### 2.1.2 新增 17 种 Bucket 聚合

**新增文件**: `internal/engine/agg/bucket/buckets.go`

```go
// 统一 BucketCollector 接口（基于现有 GroupAggregator 扩展）
type BucketCollector interface {
    Collect(docID uint32, value any) error
    Result() (*BucketResult, error)
    Merge(other BucketCollector) error
}

// 每种 bucket 类型实现此接口
```

| 类型 | 实现要点 |
|------|---------|
| range | 预定义区间桶，二分查找归属 |
| date_range | 日期区间桶（支持 date math） |
| filter | 单查询桶，bool 匹配 |
| filters | 多查询桶，{filter_name: query} |
| missing | 缺失值桶 |
| nested | 嵌套文档桶（扁平化展开路径） |
| histogram | 等宽直方图桶，interval 参数 |
| date_histogram | 日期直方图，calendar_interval/fixed_interval |
| auto_date_histogram | 自动区间数 date_histogram |
| significant_terms | 显著词项桶（p-value 计算） |
| sampler | 采样桶（随机/前 N） |
| global | 全局桶（忽略查询条件） |
| composite | 复合桶（多字段笛卡尔积 + 游标分页） |
| multi_terms | 多字段 terms 桶 |
| rare_terms | 稀有词项桶（低频值） |
| ip_range | IP 区间桶（CIDR/范围） |
| geo_distance | 地理距离桶（同心圆环） |

#### 2.1.3 新增 12 种 Pipeline 聚合

**新增文件**: `internal/engine/agg/pipeline/pipeline.go`

```go
// Pipeline 聚合基于父桶结果二次计算
type PipelineAgg interface {
    Compute(parentBuckets []BucketResult) ([]PipelineResult, error)
}
```

| 类型 | 实现要点 |
|------|---------|
| bucket_sort | 对桶排序 + 分页 |
| bucket_selector | 条件过滤桶（script 表达式） |
| bucket_script | 桶间计算（script 表达式） |
| derivative | 导数：bucket[i] - bucket[i-1] |
| cumulative_sum | 累计求和 |
| moving_avg | 移动平均（window 参数） |
| avg_bucket | 桶平均值 |
| max_bucket | 最大桶值 |
| sum_bucket | 桶值求和 |
| stats_bucket | 桶统计 |
| serial_diff | 序列差分（lag 参数） |
| normalize | 归一化（0-1 范围） |

#### 2.1.4 PartialState 协议

**新增文件**: `internal/engine/agg/partial.go`

```go
// PartialState 跨段聚合中间态
type PartialState struct {
    AggID uint32
    State PartialStateOneof
}

type PartialStateOneof interface {
    partialStateMarker()
}

type HLLSketch struct { Data []byte }  // HLL.Bytes()
type TDigestSketch struct { Centroids []Centroid }
type TopState struct { Items []TopItem }
type SumState struct { Sum, Count float64 }

// Marshal/Unmarshal
func MarshalPartial(s *PartialState) ([]byte, error)
func UnmarshalPartial(data []byte) (*PartialState, error)

// Merge：同 agg_id 的 partial state 合并
// HLL: 寄存器 max; TDigest: centroid 归并重压缩; Top: 合并排序; Sum: 加和
```

#### 2.1.5 HashAggregate 算子化

**文件**: `internal/engine/exec/operators.go`

```go
type HashAggregateOp struct {
    child      Operator
    collector  agg.BucketCollector
    batchPool  *sync.Pool
}

func (op *HashAggregateOp) Next(ctx context.Context) (*ColumnBatch, error) {
    // 流式消费 child batch → collector.Collect
    // child 返回 io.EOF → collector.Result() → 输出聚合结果 batch
}
```

---

### 2.2 深分页链路

#### 2.2.1 PIT 真实快照

**文件**: `internal/server/esapi/pit_handlers.go`

```go
// pit_handlers.go:25 — 获取真实快照
func (s *Server) handleCreatePIT(w http.ResponseWriter, r *http.Request) {
    // 修改前：snap := types.NewSnapshot(0)
    // 修改后：从 shard 获取当前可见性水位
    snap := s.engine.CurrentSnapshot(ctx, indices)  // 新增 Engine 方法
    pit := s.pitMgr.Create(snap)
}
```

`Engine.CurrentSnapshot` 返回 `max(shard.VisibleSeqNo())` 作为 ReadUpTo。

#### 2.2.2 scroll 推进

**文件**: `internal/server/esapi/pit_handlers.go`

```go
func (s *Server) handleScroll(w http.ResponseWriter, r *http.Request) {
    // 解码 scroll_id → PIT id + cursor
    // 用 cursor 的 search_after 执行下一批搜索
    result, newCursor, err := s.scrollMgr.AdvanceScroll(ctx, scrollID)
    // 返回真实结果（非空页）
}

// scroll.go — AdvanceScroll 实现
func (m *ScrollManager) AdvanceScroll(ctx context.Context, scrollID string) (*SearchResult, string, error) {
    // 1. 解码 scrollID → pitID + sortValues cursor
    // 2. 用 PIT 快照 + search_after 执行搜索（size = scroll batch size）
    // 3. 返回结果 + 新 cursor 编码为 scroll_id
}
```

#### 2.2.3 search_after 游标比较

**文件**: `internal/embedded/planner.go`

```go
// buildAndExecute 增加 searchAfter 参数
func (p *Planner) buildAndExecute(ctx context.Context, plans []LogicalPlan, segSet, snap, from, size int, searchAfter []interface{}) ([]*SearchHit, error) {
    // 收集所有段 + memtable 结果
    // 合并排序
    // search_after 过滤：丢弃 sortValues <= searchAfter 的文档
    // 分页 [from:from+size]
}

// sortValues 逐 key 二进制比较
func compareSortValues(a, b []interface{}) int {
    for i := range a {
        c := compareValue(a[i], b[i])
        if c != 0 { return c }
    }
    return 0
}
```

#### 2.2.4 relative_score 融合

**新增文件**: `internal/engine/fusion/relative_score.go`

```go
type RelativeScoreFusion struct {
    Baseline string  // "bm25f"
}

func (f *RelativeScoreFusion) Merge(cands [][]ScoredDoc) ([]ScoredDoc, error) {
    // 1. BM25 路作为基线
    // 2. 向量路分数映射到 BM25 分布同尺度
    //    percentile_rank(vector_score, vector_scores) → 映射到 BM25 分数范围
    // 3. 合并：score = bm25_score + mapped_vector_score
}
```

#### 2.2.5 rerank 外部 HTTP

**新增文件**: `internal/engine/rerank/http_reranker.go`

```go
type HTTPReranker struct {
    url     string
    timeout time.Duration  // 2s
    client  *http.Client
}

func (r *HTTPReranker) Rerank(ctx context.Context, query string, docs [][]byte) ([]float32, error) {
    // POST {query, docs[]} → {scores[]}
    // 超时 2s → 返回 ErrRerankTimeout，调用方降级为 rrf
}

// fusion.go — rerank 方法
func (f *Fusion) rerankMerge(cands [][]ScoredDoc, reranker rerank.Reranker) ([]ScoredDoc, error) {
    // 1. 各路 min-max 归一
    // 2. 合并 TopN（window）
    // 3. 调用 reranker.Rerank → 重排
    // 4. 超时降级 rrf
}
```

---

### 2.3 ES 兼容补齐

#### 2.3.1 缺失路由注册

**文件**: `internal/server/esapi/router.go`

```go
// 新增注册
s.mux.HandleFunc("POST /{index}/_update/{id}", mw(s.handleUpdateDoc))
s.mux.HandleFunc("POST /{index}/_flush", mw(s.handleFlush))
s.mux.HandleFunc("POST /_flush", mw(s.handleFlush))
s.mux.HandleFunc("POST /{index}/_forcemerge", mw(s.handleForceMerge))
s.mux.HandleFunc("POST /_forcemerge", mw(s.handleForceMerge))
s.mux.HandleFunc("GET /_cat/shards", mw(s.handleCatShards))
s.mux.HandleFunc("GET /_cat/nodes", mw(s.handleCatNodes))
s.mux.HandleFunc("GET /_cat/allocation", mw(s.handleCatAllocation))
s.mux.HandleFunc("PUT/GET /_index_template/{name}", mw(s.handleIndexTemplate))
s.mux.HandleFunc("PUT/GET /_component_template", mw(s.handleComponentTemplate))
s.mux.HandleFunc("PUT /{index}/_ilm/start", mw(s.handleILMStart))
```

#### 2.3.2 _terms_enum 实现

**文件**: `internal/server/esapi/m1_handlers.go`

```go
func (s *Server) handleTermsEnum(w http.ResponseWriter, r *http.Request) {
    // 1. 解析 field 参数
    // 2. 获取字段的 Bitmap 索引或 FST 词典
    // 3. 前缀遍历返回 terms 列表
    // 修改前：固定返回空数组
}
```

#### 2.3.3 _update_by_query 实现

**文件**: `internal/server/esapi/m1_handlers.go`

```go
func (s *Server) handleUpdateByQuery(w http.ResponseWriter, r *http.Request) {
    // 1. 解析 query
    // 2. 搜索匹配文档（search_after + PIT 分页）
    // 3. 逐批更新（painless script 执行）
    // 4. 返回 updated/deleted/failures 计数
    // 修改前：固定返回 updated=0
}
```

#### 2.3.4 _index_template/_ilm 持久化

**新增文件**: `internal/server/esapi/template_store.go`, `internal/server/esapi/ilm_store.go`

```go
// template_store.go
type TemplateStore struct {
    store *meta.Store  // 复用 meta.Store 的 JSON 持久化
}

func (s *TemplateStore) Put(name string, template *IndexTemplate) error
func (s *TemplateStore) Get(name string) (*IndexTemplate, error)
func (s *TemplateStore) List() ([]string, error)

// ilm_store.go — 类似，ILM 策略状态机 hot→warm→cold→delete
```

#### 2.3.5 deprecation Warning header

**文件**: `internal/server/esapi/router.go`

```go
// 新增工具函数
func setDeprecationWarning(w http.ResponseWriter, msg string) {
    w.Header().Add("Warning", fmt.Sprintf(`299 unisearch-8.12 "%s"`, msg))
}

// 在以下场景调用：
// - doc_values:false → "doc_values is always enabled; false is accepted but deprecated"
// - norms → "norms are not stored separately; field length is always tracked"
// - scroll → "scroll is deprecated; use PIT + search_after instead"
```

#### 2.3.6 通配索引展开

**文件**: `internal/server/esapi/router.go`

```go
// parseIndices 增加 glob 匹配
func (s *Server) parseIndices(r *http.Request) []string {
    raw := r.PathValue("index")
    if raw == "" || raw == "_all" || raw == "*" { return nil }
    var indices []string
    for _, part := range strings.Split(raw, ",") {
        for _, expanded := range expandBraces(part) {
            if strings.ContainsAny(expanded, "*?") {
                // glob 匹配：从 meta.Store.List() 获取索引名
                all := s.engine.Indices(ctx, "")
                for _, name := range all {
                    if matched, _ := filepath.Match(expanded, name); matched {
                        indices = append(indices, name)
                    }
                }
            } else {
                indices = append(indices, expanded)
            }
        }
    }
    return indices
}
```

#### 2.3.7 ES 官方 yaml 金标准

**新增文件**: `test/golden/es_spec/loader.go`

```go
// 解析 ES rest-api-spec yaml 文件
type SpecTest struct {
    Setup  map[string]any  // 测试前准备
    Test   []TestCase      // 测试用例
}

func LoadSpecYAML(dir string) ([]SpecTest, error) {
    // 读取 test/golden/es_spec/*.yaml
    // 解析 setup + test sections
}

func RunSpecTest(t *testing.T, spec SpecTest, client *integration.TestClient) {
    // 执行 setup（创建索引、写入文档）
    // 逐条执行 test assertions
    // 断言 HTTP 状态 + JSON 结构
}
```

`test/golden/es_spec/` 目录放入 ES 官方 rest-api-spec yaml 子集（10_percolate, 20_terms, 30_search 等核心用例）。

#### 2.3.8 _field_caps 结构修正

**文件**: `internal/server/esapi/m1_handlers.go`

```go
// 修改二级 key：索引名 → 类型名
// 修改前：fields[field] = map[string]any{indexName: {...}}
// 修改后：fields[field] = map[string]any{typeName: {...}}  // e.g. "text": {"type":"text",...}
```

#### 2.3.9 _bulk 单条 100MB 限制

**文件**: `internal/server/esapi/handlers.go`

```go
// handleBulk 中 Scanner 增加 per-line 限制
scanner := bufio.NewScanner(r.Body)
scanner.Buffer(make([]byte, 0, 1024*1024), 100*1024*1024) // max 100MB per line
for scanner.Scan() {
    line := scanner.Bytes()
    if len(line) > 100*1024*1024 {
        writeError(w, apierr.NewIllegalArgument("bulk item exceeds 100MB limit"))
        return
    }
}
```

#### 2.3.10 指标标签补全

**文件**: `internal/observability/metrics.go`

```go
// 6 个指标补全缺失标签维度
WriteBatchDuration = prometheus.NewHistogramVec(..., prometheus.Labels{"tenant", "index", "shard"})  // +shard
WriteRejectedTotal = prometheus.NewCounterVec(..., prometheus.Labels{"tenant", "index", "shard"})  // +shard
QueryRequestsTotal = prometheus.NewCounterVec(..., prometheus.Labels{"tenant", "index", "phase"})  // +phase
QueryRowsScanned = prometheus.NewHistogramVec(..., prometheus.Labels{"tenant", "index", "phase"})  // +phase
QueryErrorsTotal = prometheus.NewCounterVec(..., prometheus.Labels{"tenant", "index", "phase"})  // +phase
FlushDuration = prometheus.NewHistogramVec(..., prometheus.Labels{"index", "shard", "tier"})  // +tier
// MergeRunning/MergeBytesTotal/DeletesPending 同理 +tier
```

---

## 第 3 批：分布式架构

### 3.1 LogStore NVMe 写入 + raft 接口

**新增文件**: `internal/objstore/raft_logstore.go`

```go
type RaftLogStore struct {
    nvmeDir   string
    s3        S3Client
    shardID   uint64
    mu        sync.RWMutex
    cache     map[uint64]*raft.Log  // NVMe 读取缓存
}

// 实现 raft.LogStore
func (s *RaftLogStore) FirstIndex() (uint64, error)
func (s *RaftLogStore) LastIndex() (uint64, error)
func (s *RaftLogStore) GetLog(index uint64, log *raft.Log) error
func (s *RaftLogStore) StoreLog(log *raft.Log) error {
    // 1. 序列化 log → NVMe 写入 + fsync
    path := filepath.Join(s.nvmeDir, fmt.Sprintf("%020d.log", log.Index))
    data := encodeRaftLog(log)
    os.WriteFile(path, data, 0644)
    os.Sync()  // fsync 父目录
    // 2. 异步上传对象存储
    go s.s3.PutObject(ctx, s3Key(log.Index), data)
}
func (s *RaftLogStore) StoreLogs(logs []*raft.Log) error
func (s *RaftLogStore) DeleteRange(min, max uint64) error

// 实现 raft.StableStore
func (s *RaftLogStore) Set(key []byte, value []byte) error
func (s *RaftLogStore) Get(key []byte) ([]byte, error)
func (s *RaftLogStore) SetUint64(key []byte, val uint64) error
func (s *RaftLogStore) GetUint64(key []byte) (uint64, error)
```

**修改 `raft_cluster.go`**: 替换 `raftboltdb.NewBoltStore` 为 `NewRaftLogStore`。

### 3.2 Coordinator gRPC client

**新增文件**: `internal/coordinator/grpc_client.go`

```go
type GRPCShardClient struct {
    conn   *grpc.ClientConn
    client pb.ShardServiceClient
}

func NewGRPCShardClient(addr string) (*GRPCShardClient, error) {
    conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
    return &GRPCShardClient{conn: conn, client: pb.NewShardServiceClient(conn)}
}

func (c *GRPCShardClient) Search(ctx context.Context, body []byte) (*ShardSearchResult, error) {
    resp, err := c.client.Search(ctx, &pb.SearchRequest{Body: body})
    // 转换 SearchResult
}

func (c *GRPCShardClient) Get(ctx context.Context, index, id string) ([]byte, bool, error) {
    resp, err := c.client.Get(ctx, &pb.GetRequest{Index: index, Id: id})
    return resp.Source, resp.Found, nil
}

func (c *GRPCShardClient) Close() error { return c.conn.Close() }
```

`QueryCoordinator.RegisterShardClient` 改为创建 `GRPCShardClient`。

### 3.3 Raft ReadIndex 线性一致读

**文件**: `internal/raft/raft_cluster.go`

```go
func (c *RaftCluster) GetIndex(ctx context.Context, id types.IndexID) (*meta.IndexMeta, error) {
    // 修改前：直接读 FSM（eventual consistency）
    // 修改后：ReadIndex 保证线性一致
    fut := c.raft.ReadIndex(ctx, nil)
    if err := fut.Error(); err != nil {
        return nil, err  // leader 变更时返回 error
    }
    // 等待 ReadIndex 确认后读 FSM
    return c.fsm.State().GetIndex(id), nil
}
```

### 3.4 ShardService PullWAL/Snapshot

**文件**: `internal/server/grpc/server.go`

```go
func (s *ShardServer) PullWAL(req *pb.WALCursor, stream pb.ShardService_PullWALServer) error {
    // 从 req.AfterSeq 开始流式发送 WAL 条目
    // 每 1MB flush 一次 stream
    wal := s.engine.GetWAL(req.ShardID)
    return wal.Replay(stream.Context(), req.AfterSeq, func(entry wal.Entry) error {
        return stream.Send(&pb.LogChunk{Data: entry.Payload, SeqNo: entry.SeqNo})
    })
}

func (s *ShardServer) Snapshot(ctx context.Context, req *pb.SnapshotRequest) (*pb.SnapshotMeta, error) {
    // 返回段文件列表 + 元数据
    segs := s.engine.GetSegments(req.ShardID)
    return &pb.SnapshotMeta{Segments: convertSegs(segs)}, nil
}
```

### 3.5 MetaServer 实现

**文件**: `internal/server/grpc/server.go`

```go
func (s *MetaServer) Apply(ctx context.Context, req *pb.MetaCommand) (*pb.ApplyResult, error) {
    // 委托 RaftCluster.Apply
    data := EncodeCommand(req.Type, req.Payload)
    if err := s.cluster.Apply(ctx, data); err != nil {
        return nil, err
    }
    return &pb.ApplyResult{Success: true}, nil
}

func (s *MetaServer) Watch(req *pb.WatchRequest, stream pb.MetaService_WatchServer) error {
    // 路由表变更事件流
    ch := s.cluster.Watch()
    for event := range ch {
        if err := stream.Send(convertEvent(event)); err != nil {
            return err
        }
    }
}

func (s *MetaServer) Heartbeat(ctx context.Context, req *pb.NodeInfo) (*pb.HeartbeatAck, error) {
    // 更新 Members + 返回 leader 地址
    s.cluster.UpdateHeartbeat(req)
    return &pb.HeartbeatAck{Acknowledged: true, LeaderAddr: s.cluster.LeaderAddr()}, nil
}
```

### 3.6 Rebalance 数据迁移

**文件**: `internal/tenant/rebalance.go`

```go
// RebalanceCoordinator 增加 hook 机制（类似 SplitCoordinator）
type RebalanceCoordinator struct {
    hooks RebalanceHooks
}

type RebalanceHooks struct {
    PullSnapshot  func(shardID, targetNode) error
    SyncIncremental func(shardID, targetNode, fromSeq) error
    SwitchRoute   func(shardID, newGen) error
}

func (rc *RebalanceCoordinator) AdvancePhase(shardID uint64) error {
    switch state.Phase {
    case Snapshot:
        rc.hooks.PullSnapshot(shardID, state.TargetNode)
    case Incremental:
        rc.hooks.SyncIncremental(shardID, state.TargetNode, state.LastSeq)
    case Complete:
        rc.hooks.SwitchRoute(shardID, state.NewGen)
    }
}
```

### 3.7 Cold 模式 mmap 操作

**文件**: `internal/tenant/rebalance.go`

```go
func (m *ColdManager) Pause(tenantID types.TenantID) error {
    // 实际 mmap close
    for _, seg := range m.getTenantSegments(tenantID) {
        seg.Reader.Close()  // munmap
    }
    m.coldTenants[tenantID] = &ColdState{SegmentIDs: segIDs}
}

func (m *ColdManager) Wakeup(tenantID types.TenantID) error {
    state := m.coldTenants[tenantID]
    for _, segID := range state.SegmentIDs {
        m.reopenSegment(segID)  // mmap 重新映射
    }
    delete(m.coldTenants, tenantID)
}
```

### 3.8 混沌编排引擎

**新增文件**: `test/chaos/orchestrator.go`

```go
type Orchestrator struct {
    composeFile string
    toxiproxyAddr string
}

func (o *Orchestrator) RunScenario(ctx context.Context, scenario Scenario) (ScenarioReport, error) {
    // 1. 启动 Docker Compose 集群
    exec.Command("docker-compose", "-f", o.composeFile, "up", "-d").Run()
    defer exec.Command("docker-compose", "-f", o.composeFile, "down").Run()
    // 2. 注入故障（toxiproxy CLI）
    switch scenario.FaultType {
    case FaultKill:
        // docker kill leader 容器
    case FaultPartition:
        // toxiproxy toxic add --type limit_data
    case FaultSlowIO:
        // toxiproxy toxic add --type latency --downstream
    }
    // 3. 采集指标 + 判定 RTO
    startTime := time.Now()
    // 轮询健康检查直到集群恢复
    for !isHealthy() {
        if time.Since(startTime) > 60*time.Second {
            return ScenarioReport{Passed: false, Details: "RTO > 60s"}, nil
        }
        time.Sleep(time.Second)
    }
    return ScenarioReport{Passed: time.Since(startTime) < 30*time.Second, Duration: time.Since(startTime)}, nil
}
```

### 3.9 SearchStream gRPC

**文件**: `api/proto/shard.proto`

```protobuf
// 新增流式搜索 RPC
rpc SearchStream(SubPlan) returns (stream LocalResult);

message SubPlan {
    bytes plan_data = 1;  // 序列化的物理子计划
    uint64 shard_id = 2;
    types.Snapshot snapshot = 3;
}

message LocalResult {
    repeated SearchHit hits = 1;
    bytes partial_state = 2;  // PartialState 序列化
    bool final = 3;
}
```

### 3.10 tenant/objstore 测试

**新增文件**: `internal/tenant/tenant_test.go`, `internal/objstore/objstore_test.go`

覆盖：Quota 四维检查/信号量/令牌桶/Cold pause-wakeup/S3 put-get-list-delete。

---

## 第 4 批：质量保障 / CI

### 4.1 日志规范

**文件**: `cmd/unisearch/root.go`

```go
// slog handler 增加 trace_id 提取
type traceIDHandler struct {
    inner slog.Handler
}

func (h *traceIDHandler) Handle(ctx context.Context, r slog.Record) error {
    if span := trace.SpanFromContext(ctx); span.SpanContext().HasTraceID() {
        r.AddAttrs(slog.String("trace_id", span.SpanContext().TraceID().String()))
        r.AddAttrs(slog.String("span_id", span.SpanContext().SpanID().String()))
    }
    // 提取 tenant/index/shard_id/segment_id 从 context
    return h.inner.Handle(ctx, r)
}
```

### 4.2 慢查询日志

**新增文件**: `internal/observability/slow_query.go`

```go
func LogSlowQuery(ctx context.Context, took time.Duration, shardsScanned int, rowsRead int64, planSummary string) {
    if took < time.Second { return }
    slog.WarnContext(ctx, "slow query",
        "took_ms", took.Milliseconds(),
        "shards_scanned", shardsScanned,
        "rows_read", rowsRead,
        "plan_summary", planSummary,
    )
}

// 在 engine.go Search 末尾调用
defer func() {
    LogSlowQuery(ctx, time.Since(start), len(shards), rowsRead, planSummary)
}()
```

### 4.3 尾采样

**文件**: `internal/observability/tracing.go`

```go
// 替换 ParentBased(AlwaysSample()) 为 TailSampler
func InitTracing() {
    // 使用 span processor + tail sampling
    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter),
        sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.01))),  // 1% 基础采样
    )
    // 尾采样：错误全采 + 慢查询>1s全采
    tailProcessor := NewTailSamplingProcessor(exporter, TailConfig{
        ErrorSample:   1.0,    // 错误全采
        SlowQuerySecs:  1.0,    // >1s 全采
        DefaultSample:  0.01,   // 其余 1%
    })
}
```

### 4.4 健康探针

**文件**: `internal/embedded/engine.go`

```go
func (e *Engine) Health(ctx context.Context) (*ClusterHealth, error) {
    // 修改前：硬编码 green
    // 修改后：根据分片副本状态计算
    shards := e.manager.listShards()
    activeShards := 0
    for _, s := range shards {
        if s.HasQuorum() { activeShards++ }
    }
    if activeShards == len(shards) { status = "green" }
    else if activeShards > 0 { status = "yellow" }
    else { status = "red" }
}

// handlers.go — wait_for_status 支持
func (s *Server) handleClusterHealth(w http.ResponseWriter, r *http.Request) {
    waitFor := r.URL.Query().Get("wait_for_status")
    timeout := r.URL.Query().Get("timeout")
    // 轮询直到达到 waitFor 状态或 timeout
}
```

### 4.5 uni-bench B1-B10

**新增文件**: `test/bench/bench_test.go`

```go
func BenchmarkB1WriteThroughput(b *testing.B) { ... }  // ≥20k docs/s
func BenchmarkB2VectorP99(b *testing.B) { ... }       // P99 <50ms
func BenchmarkB3FilteredVectorP99(b *testing.B) { ... } // P99 <80ms
func BenchmarkB4BM25P99(b *testing.B) { ... }          // P99 <80ms
func BenchmarkB5HybridP99(b *testing.B) { ... }        // P99 <120ms
func BenchmarkB6Visibility(b *testing.B) { ... }      // P99 <200ms
// B7-B10 类似
```

### 4.6 GitHub Actions CI

**新增文件**: `.github/workflows/ci.yml`

```yaml
name: CI
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.23' }
      - run: golangci-lint run ./...
      - run: CGO_ENABLED=0 go test -race ./...
      - run: CGO_ENABLED=0 go build ./...
      - run: go test ./test/golden/...
  cross-build:
    runs-on: ubuntu-latest
    steps:
      - run: make cross-build
  bench-regression:
    runs-on: ubuntu-latest
    steps:
      - run: go test -bench=BenchmarkB1,BenchmarkB2,BenchmarkB6 -benchtime=10x ./test/bench/
```

### 4.7 Distroless Dockerfile

**文件**: `Dockerfile`

```dockerfile
# 阶段1: 构建
FROM golang:1.23-alpine AS builder
COPY . /src
WORKDIR /src
RUN CGO_ENABLED=0 go build -o /unisearch ./cmd/unisearch/

# 阶段2: 运行
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /unisearch /unisearch
ENTRYPOINT ["/unisearch"]
```

### 4.8 性能回归测试

**新增文件**: `test/bench/regression_test.go`

```go
func TestPerformanceRegression(t *testing.T) {
    // B1 微缩版：写入 10k 文档，验证吞吐 ≥ 18k docs/s (±10%)
    // B2 微缩版：搜索 1000 次，验证 P99 < 55ms (±10%)
    // B6 微缩版：写入+读取 1000 次，验证 P99 < 220ms (±10%)
}
```

---

## 测试规划

### 测试金字塔

| 层级 | 比例 | 覆盖范围 |
|------|------|---------|
| 单元测试 | 70% | 编码往返、SIMD 对齐、幂等去重、聚合数值正确性、FST Fingerprint、Bloom 块级 |
| 集成测试 | 20% | 合并后索引可搜索、PIT/scroll/search_after 端到端、ES yaml spec、多节点 Raft |
| E2E/混沌 | 10% | 混沌 C1-C6 场景、RTO<30s、distroless 镜像构建 |

### 关键测试场景清单

| 批次 | 测试函数 | 类型 | 验证点 |
|------|---------|------|--------|
| 1 | TestApplyUnregisterNode | Unit | B1: 节点移除正确 |
| 1 | TestAppendChannelFull | Unit | B2: channel 满不丢段 |
| 1 | TestDedupWindow | Unit | 幂等去重窗口 LRU 淘汰 |
| 1 | TestMergePreservesIndexes | Integration | 合并后倒排/BKD 可搜索 |
| 1 | TestGlobalFlushConcurrency | Unit | 全局并发 ≤4 |
| 1 | FuzzSegmentFormat | Fuzz | 段格式破坏不 panic |
| 1 | FuzzQueryString | Fuzz | query_string 随机输入不 panic |
| 1 | TestSIMDAlignment | Unit | AVX2/NEON vs Pure Go 对齐 |
| 1 | TestSparseVectorSearch | Integration | 稀疏向量端到端搜索 |
| 1 | TestCrossLinkSingleHeap | Integration | 跨段单堆跳段结果正确 |
| 2 | TestAll47Aggregations | Unit | 47 种聚合数值正确性 |
| 2 | TestPartialStateMerge | Unit | PartialState 跨段合并 |
| 2 | TestPITRealSnapshot | Integration | PIT 看到真实数据 |
| 2 | TestScrollAdvance | Integration | scroll 返回真实结果 |
| 2 | TestSearchAfter | Integration | search_after 游标比较 |
| 2 | TestRelativeScoreFusion | Unit | relative_score 融合正确 |
| 2 | TestHTTPReranker | Unit | 外部 reranker + 超时降级 |
| 2 | TestESSpecYAML | Golden | ES 官方 yaml 通过率 ≥90% |
| 2 | TestTermsEnum | Integration | _terms_enum 返回真实结果 |
| 2 | TestUpdateByQuery | Integration | _update_by_query 真实更新 |
| 3 | TestRaftLogStore | Unit | raft LogStore/StableStore 接口 |
| 3 | TestGRPCShardClient | Integration | gRPC client 连接+搜索 |
| 3 | TestReadIndexLinear | Integration | Raft ReadIndex 线性一致 |
| 3 | TestPullWALStream | Integration | WAL 流式拉取 |
| 3 | TestRebalanceMigration | Integration | 三阶段数据迁移 |
| 3 | TestColdPauseWakeup | Integration | Cold mmap close/reopen |
| 3 | TestChaosC1LeaderKill | Chaos | RTO <30s |
| 4 | TestSlowQueryLog | Unit | 慢查询日志输出 |
| 4 | TestTailSampling | Unit | 错误全采/慢查询全采/其余1% |
| 4 | TestHealthCalculation | Unit | green/yellow/red 计算 |
| 4 | BenchmarkB1-B10 | Benchmark | 性能指标达标 |
| 4 | TestPerformanceRegression | Regression | ±10% 阈值看护 |

### Mock 策略

| 外部依赖 | Mock 方式 |
|---------|----------|
| S3 对象存储 | MemS3Client（已有，内存实现） |
| 外部 reranker | httptest.Server 返回固定 scores |
| Docker/toxiproxy | 测试中跳过（CI 环境运行） |
| gRPC shard 节点 | MockShardClient（已有） |

### 覆盖率期望

| 模块 | 目标覆盖率 |
|------|-----------|
| 存储引擎核心（memtable/wal/merge/deletes） | ≥90% |
| 索引引擎核心（hnsw/inverted/scalar） | ≥90% |
| 聚合引擎 | ≥85% |
| ES 兼容层 handler | ≥80% |
| 分布式（raft/coordinator/tenant） | ≥80% |
| 可观测性 | ≥70% |

---

## 验收标准

- [ ] 7 个 Bug 全部修复，有对应回归测试
- [ ] 第 1 批：`go test -race ./internal/storage/... ./internal/index/...` 全绿；4 个 fuzz target 运行通过
- [ ] 第 2 批：47 种聚合全部实现 + ES 官方 yaml 金标准通过率 ≥90% + PIT/scroll/search_after 端到端可用
- [ ] 第 3 批：分布式 14 项全部生产可用 + 混沌 C1 leader kill RTO<30s + Raft ReadIndex 线性一致
- [ ] 第 4 批：uni-bench B1-B10 全部可运行 + GitHub Actions CI 全绿 + distroless 镜像 + 性能回归 ±10% 看护
- [ ] 全局：`go build ./... && go test -race ./... && go vet ./...` 全绿
- [ ] 审计覆盖：117 功能点中完全实现占比 ≥95%
