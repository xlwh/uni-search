# Spec: 从 Qdrant 引入高级搜索与存储功能到 UniSearch

## 1. 概述 (Overview)

本 Spec 基于已批准的 Proposal `2026-10-23-qdrant-features-import`，将 16 个 Qdrant 功能引入 UniSearch，分 P0/P1/P2 三阶段实施。所有新功能通过 ES DSL 扩展字段暴露，保持 ES 8.x REST API 兼容。

### 设计评审决策

| 决策点 | 选择 | 影响 |
|--------|------|------|
| 稀疏向量索引架构 | 独立 `sparse_index.go`（复用 FST 词典，独立 WeightedPostingList/SparseWANDScorer） | F1 |
| 段格式 v2 兼容 | Footer 版本号 + 条件读取（format_version=2） | F7 |
| Pipeline 执行模型 | 自适应并行（根据子查询数量和系统负载动态选择并行/顺序） | F5 |
| TurboQuant SIMD 策略 | Pure Go 先行，汇编后补 | F6 |
| Multi-vector HNSW 集成 | 独立 MultiVectorGraph（与单向量 Graph 完全隔离） | F7 |
| WAL Delta 传输协议 | Raft 集成同步（通过 Raft 共识层同步 WAL） | F10 |

### 变更清单

| 类型 | 文件路径 | 说明 |
|------|----------|------|
| 新增 | `internal/index/inverted/sparse_index.go` | F1 稀疏向量倒排索引 |
| 新增 | `internal/engine/rerank/mmr.go` | F3 MMR 多样性搜索 |
| 新增 | `internal/types/f16.go` | F4 Float16↔Float32 转换 |
| 新增 | `internal/index/hnsw/turboquant.go` | F6 TurboQuant 量化状态 |
| 新增 | `internal/index/hnsw/hadamard.go` | F6 Walsh-Hadamard 变换 |
| 新增 | `internal/index/hnsw/lloyd_max.go` | F6 Lloyd-Max 量化器训练 |
| 新增 | `internal/index/hnsw/multi_graph.go` | F7 Multi-vector HNSW 图 |
| 新增 | `internal/engine/vector_arith.go` | F8 向量运算 |
| 新增 | `internal/engine/formula/expr.go` | F9 表达式引擎 |
| 新增 | `internal/engine/stats/quantile.go` | F11 P-square 分位数估计 |
| 新增 | `internal/index/hnsw/healing.go` | F12 HNSW 图修复 |
| 新增 | `internal/index/inverted/bm25_embedding.go` | F14 BM25 嵌入生成 |
| 新增 | `internal/auth/jwt.go` | F15 JWT 认证 |
| 新增 | `internal/auth/rbac.go` | F15 RBAC 权限 |
| 新增 | `internal/observability/audit.go` | F16 审计日志 |
| 修改 | `internal/index/registry.go` | F1/F7 索引注册与路由 |
| 修改 | `internal/index/hnsw/quant.go` | F6/F4/F11 量化层扩展 |
| 修改 | `internal/index/hnsw/hnsw.go` | F4 Float16 Graph 结构 |
| 修改 | `internal/index/hnsw/search.go` | F6/F7 搜索路径扩展 |
| 修改 | `internal/engine/fusion/fusion.go` | F2 DBSF 融合 |
| 修改 | `internal/engine/dsl/ast.go` | F1-F9 AST 节点 |
| 修改 | `internal/engine/dsl/parser.go` | F1-F9 解析逻辑 |
| 修改 | `internal/engine/plan/logical.go` | F3/F5/F8/F9 计划节点 |
| 修改 | `internal/engine/exec/operators.go` | F3/F5/F9 算子 |
| 修改 | `internal/embedded/translator.go` | F1-F9 翻译逻辑 |
| 修改 | `internal/embedded/planner.go` | F1-F9 管道构建 |
| 修改 | `internal/storage/segment/wire.go` | F1/F4/F7 段格式扩展 |
| 修改 | `internal/storage/segment/builder.go` | F4/F7 列编码 |
| 修改 | `internal/storage/flush.go` | F1/F4/F14 flush 集成 |
| 修改 | `internal/simd/dist.go` | F4/F6/F7 距离内核 |
| 修改 | `internal/storage/wal/wal.go` | F10 WAL Delta 读取 |
| 修改 | `internal/raft/types.go` | F10 per-replica 水位线 |
| 修改 | `internal/raft/raft_cluster.go` | F10 Raft 集成 WAL 同步 |
| 修改 | `internal/meta/mapping.go` | F4/F7/F14 mapping 扩展 |
| 修改 | `internal/tenant/tenant.go` | F16 集合级配额 |
| 修改 | `internal/server/esapi/router.go` | F15/F16 中间件 |
| 修改 | `internal/storage/merge/scheduler.go` | F12 merge 后 healing |
| 修改 | `internal/index/analyzer/tokenizers.go` | F13 CJK 分词器 |
| 修改 | `internal/index/analyzer/filters.go` | F13 Snowball stemmer |
| 修改 | `internal/index/analyzer/factory.go` | F13 分词器注册 |

---

## 2. 详细设计 (Detailed Design)

### 2.1 F1: 稀疏向量倒排索引

**变更内容**：
在 `internal/index/inverted/` 下新建 `sparse_index.go`，实现基于倒排索引的稀疏向量搜索。复用现有 FST 词典（`TermDict`），但独立实现 `WeightedPostingList`（存储 float 权重而非 uint8 TF）和 `SparseWANDScorer`（基于 per-dim 最大权重做 WAND 前缀剪枝）。

**核心数据结构**：

```go
// internal/index/inverted/sparse_index.go

// WeightedPostingList 存储稀疏向量的加权倒排链
// 每个 posting 包含 rowID 和 float 权重（替代 BM25 的整数 TF）
type WeightedPostingList struct {
    termID  uint64           // 维度 ID（通过 FST 词典映射）
    docIDs  []int32          // 有序 rowID 列表
    weights []float32        // 对应权重（或 f16/u8 量化版本）
    maxWeight float32        // 该维度最大权重（用于 WAND 上界剪枝）
    weightPrecision WeightPrecision // 权重精度模式
}

type WeightPrecision uint8
const (
    WeightF32 WeightPrecision = 0  // float32 精确
    WeightF16 WeightPrecision = 1  // float16 压缩（50% 内存）
    WeightU8  WeightPrecision = 2  // uint8 标量量化（75% 内存）
)

// SparseInvertedIndex 稀疏向量倒排索引
type SparseInvertedIndex struct {
    mu          sync.RWMutex
    termDict    *TermDict        // 复用 FST 词典
    postings    map[uint64]*WeightedPostingList
    numDocs     int32
    weightPrecision WeightPrecision
}
```

**SparseWANDScorer 设计**：

```go
// SparseWANDScorer 基于 WAND 算法的稀疏向量评分器
// 与文本 BM25 的 WANDScorer 区别：score = Σ(query_weight[i] * doc_weight[i])（dot product）
// 剪枝上界 = Σ(query_weight[i] * max_weight[i])（各维度最大权重之和）
type SparseWANDScorer struct {
    queryDimIDs   []uint64        // 查询维度 ID（有序）
    queryWeights  []float32       // 查询权重
    postings      []*WeightedPostingList // 对应倒排链
    upperBounds   []float32       // 前缀上界（upperBounds[i] = Σ_{j<=i} queryWeight[j] * maxWeight[j]）
    threshold     float32         // TopK 当前最低分数阈值
    heap          *topNHeap       // TopK 有界堆
}

func (s *SparseWANDScorer) Search(k int) []ScoredDoc {
    // WAND 算法：遍历候选文档，跳过上界 < threshold 的文档
    // 1. 按 docID 排序 postings 当前指针位置
    // 2. 从左到右累加上界，找到第一个上界 >= threshold 的 pivot
    // 3. pivot 之前的 posting 全部 advance 到 pivot 的 docID
    // 4. 对 pivot 文档计算完整 dot product 分数
    // 5. 若分数 >= threshold，加入堆，更新 threshold
}
```

**索引注册**：

```go
// internal/index/registry.go 新增

const KindSparseInverted IndexKind = "sparse_inverted"

func init() {
    RegisterIndex(KindSparseInverted, NewSparseInvertedIndex)
    // 为 TypeSparseVector 添加路由
    fieldRoutes[TypeSparseVector] = []IndexKind{KindSparseInverted}
}
```

**Flush 集成**：

```go
// internal/storage/flush.go 修改
// flush 时对 sparse_vector 列触发稀疏倒排索引构建
func (f *FlushWorker) buildSparseInvertedIndex(col ColumnProvider) (*SparseInvertedIndex, error) {
    // 1. 遍历所有 sparse_vector 列值
    // 2. 对每个非零维度，将 (dimID, weight) 添加到对应 posting list
    // 3. 训练权重量化（如配置了 f16/u8）
    // 4. 计算每个维度的 maxWeight
    // 5. 序列化到 sparse.idx 文件
}
```

**段格式扩展**（`internal/storage/segment/wire.go`）：

```
sparse.idx 文件格式：
  [magic u32: "SPID"] 
  [version u8: 1]
  [num_terms u32]
  [weight_precision u8]
  for each term:
    [term_id u64]
    [num_docs u32]
    [max_weight f32]
    [doc_ids: delta-encoded + bitpacked]
    [weights: f32/f16/u8 per precision]
  [num_docs_total u32]
```

**查询路径修改**（`internal/embedded/planner.go`）：

```go
// buildSparseVector 从暴力扫描升级为倒排索引 WAND 搜索
func (p *Planner) buildSparseVector(seg *segment.Reader, q *SparseVectorQuery) (exec.Operator, error) {
    idx := seg.Index(KindSparseInverted)
    if idx == nil {
        // fallback: 暴力扫描（兼容旧段）
        return p.buildSparseVectorScan(seg, q)
    }
    scorer := NewSparseWANDScorer(q.DimIDs, q.Weights, idx.Postings())
    return &SparseSearchOperator{scorer: scorer, k: q.K}, nil
}
```

### 2.2 F2: DBSF 融合

**变更内容**：
在 `internal/engine/fusion/fusion.go` 中新增 `DBSFFusion`，实现 `Fusion` 接口。使用 Welford 单遍算法计算各路径分数的均值和标准差，z-score 归一化后加权求和。

```go
// internal/engine/fusion/fusion.go 新增

type DBSFFusion struct {
    weights []float64 // per-path 权重
    k       float64   // 归一化极值因子（默认 3.0，即 3 倍标准差）
}

func NewDBSFFusion(weights []float64) *DBSFFusion {
    return &DBSFFusion{weights: weights, k: 3.0}
}

// Merge 实现 Fusion 接口
func (f *DBSFFusion) Merge(paths [][]ScoredDoc, windowSize int) []ScoredDoc {
    // 1. Welford 单遍计算每条路径分数的均值 mean[i] 和方差 var[i]
    // 2. z-score 归一化：score' = (score - mean) / (k * sqrt(var))
    //    归一化后分数 ∈ 约 [-1, 1]，异常值被压缩
    // 3. 加权求和：final_score = Σ weight[i] * score'[i]
    // 4. 按 final_score 降序排序，截断到 windowSize
}
```

**DSL 扩展**：

```go
// internal/engine/dsl/ast.go
const RankMethodDBSF = "dbsf"

// internal/engine/dsl/parser.go
// 在 parseRank 中添加:
case "dbsf":
    method = RankMethodDBSF
    // 解析 weights 数组
```

```go
// internal/engine/fusion/fusion.go NewFusion 工厂
case RankMethodDBSF:
    return NewDBSFFusion(spec.Weights), nil
```

### 2.3 F3: MMR 多样性搜索

**变更内容**：
新建 `internal/engine/rerank/mmr.go`，实现 Maximal Marginal Relevance 后处理。在执行器中新增 `MMROperator` 作为 TopN 之后的可选算子。

```go
// internal/engine/rerank/mmr.go

type MMRConfig struct {
    Enabled bool
    Lambda  float64 // 0=纯多样性, 1=纯相关性, 默认 0.5
    K       int     // 重排序候选数（默认取 TopN 的全部）
}

// MMR 重排序：每次选择 argmax(lambda * sim(query, doc) - (1-lambda) * max(sim(doc, selected)))
func MMRRescore(queryVec []float32, candidates []ScoredDoc, docVecs [][]float32, lambda float64, k int) []ScoredDoc {
    // 1. 计算每个候选与 query 的相似度（已有分数）
    // 2. 迭代选择：每轮从未选集合中选 MMR 分数最高的文档
    //    MMR(d) = lambda * sim(q, d) - (1-lambda) * max_{s in selected} sim(d, s)
    // 3. sim(d, s) 使用 simd.DotF32
    // 4. 输出重排序后的 k 个文档
}
```

```go
// internal/engine/exec/operators.go 新增

type MMROperator struct {
    input    Operator      // 上游算子（通常是 TopN）
    queryVec []float32
    config   MMRConfig
    batch    []ColumnBatch
    done     bool
}

func (op *MMROperator) Next() (*ColumnBatch, error) {
    if op.done {
        return nil, nil
    }
    // 1. 从 input 拉取所有候选批次
    // 2. 收集候选文档 ID、分数、向量
    // 3. 调用 MMRRescore
    // 4. 输出重排序后的单个批次
    op.done = true
    return op.buildResultBatch(), nil
}
```

**DSL 扩展**：

```go
// internal/engine/dsl/ast.go
type MMRSpec struct {
    Enabled bool    `json:"enabled"`
    Lambda  float64 `json:"lambda"` // 默认 0.5
}

type SearchRequest struct {
    // ... 现有字段 ...
    MMR *MMRSpec `json:"mmr,omitempty"` // 扩展字段
}
```

### 2.4 F4: Float16 向量存储

**变更内容**：
新增 `internal/types/f16.go` 实现 IEEE 754 half precision 转换。在 Graph 结构、段编码、SIMD 距离计算中支持 f16 存储路径。

```go
// internal/types/f16.go

// F16ToF32 将 IEEE 754 half precision (uint16) 转换为 float32
func F16ToF32(h uint16) float32 {
    // IEEE 754 half: 1 sign + 5 exponent + 10 mantissa
    sign := uint32(h>>15) & 1
    exp := uint32(h>>10) & 0x1F
    mant := uint32(h) & 0x3FF

    if exp == 0 {
        if mant == 0 {
            return math.Float32frombits(sign << 31) // ±0
        }
        // Subnormal: denormalize
        for mant&0x400 == 0 {
            mant <<= 1
            exp--
        }
        exp++
        mant &= 0x3FF
    } else if exp == 0x1F {
        // Inf or NaN
        return math.Float32frombits(sign<<31 | 0x7F800000 | mant<<13)
    }

    return math.Float32frombits(sign<<31 | (exp+112)<<23 | mant<<13)
}

// F32ToF16 将 float32 转换为 IEEE 754 half precision (uint16)
func F32ToF16(f float32) uint16 {
    bits := math.Float32bits(f)
    sign := uint16(bits>>31) & 1
    exp := int32(bits>>23) & 0xFF
    mant := bits & 0x7FFFFF

    if exp == 0xFF {
        // Inf or NaN
        return sign<<15 | 0x7C00 | uint16(mant>>13)
    }
    // 舍入到最近偶数
    // ... 完整实现
}
```

```go
// internal/meta/mapping.go
const ElementFloat16 = "float16"

// internal/index/hnsw/hnsw.go Graph 结构扩展
type Graph struct {
    // ... 现有字段 ...
    vecsF16  []uint16    // f16 存储的向量（与 vecs 互斥）
    elemType ElementType  // 当前向量元素类型
}

func (g *Graph) Vector(nodeID int32) []float32 {
    if g.elemsType == ElemFloat16 {
        // 惰性转换：从 f16 解码为 f32
        // 可缓存解码结果
    }
    return g.vecs[nodeID*g.dims : (nodeID+1)*g.dims]
}
```

```go
// internal/simd/dist.go 新增
var DotF16F32 func(query []float32, docF16 []uint16, dims int) float32
// pure Go 实现（kernel_purego.go）
// 后续汇编实现（dispatch_amd64.go）
```

### 2.5 F5: Query API 统一查询管道

**变更内容**：
新增 `PipelineQuery` AST 节点、`Pipeline` 计划节点和 `PipelineOperator` 执行算子，支持嵌套 Prefetch + 融合 + 重评分的管道式查询。采用自适应并行执行策略。

```go
// internal/engine/dsl/ast.go 新增

type PipelineQuery struct {
    Prefetches []json.RawMessage `json:"prefetches"` // 嵌套子查询（JSON Raw 延迟解析）
    Fusion     *RankSpec          `json:"fusion"`     // prefetch 间融合策略
    Rescore    []RescoreSpec      `json:"rescore"`    // 重评分阶段
    Size       int                `json:"size"`       // 最终结果数
}

type RescoreSpec struct {
    Formula string  `json:"formula"`  // F9 公式表达式
    Weight  float64 `json:"weight"`   // 重评分权重
    Scope   string  `json:"scope"`    // "total" | "partial"
}
```

```go
// internal/engine/plan/logical.go 新增

type PipelineNode struct {
    Prefetches   []LogicalPlan  // 子查询计划
    FusionNode   *Fusion       // 融合方法
    RescoreNodes []LogicalPlan // 重评分计划
    Size         int
}

func (n *PipelineNode) String() string { return "Pipeline" }
```

```go
// internal/engine/exec/operators.go 新增

type PipelineOperator struct {
    prefetches   []Operator     // 子查询算子
    fusion        fusion.Fusion  // 融合器
    rescores     []Operator     // 重评分算子
    size         int
    adaptive     *AdaptiveParallel // 自适应并行控制器
    batches      []*ColumnBatch // 收集的子查询结果
    done         bool
}

// AdaptiveParallel 自适应并行控制器
type AdaptiveParallel struct {
    maxConcurrent int           // 最大并发数（基于 CPU 核心数和系统负载）
    threshold     int           // 并行阈值（子查询数 <= threshold 时并行）
}

func (op *PipelineOperator) Next() (*ColumnBatch, error) {
    if op.done {
        return nil, nil
    }
    op.done = true

    // 自适应并行执行 prefetch 子查询
    if len(op.prefetches) <= op.adaptive.threshold {
        // 并行执行：每个 prefetch 用 goroutine
        results := make([][]ScoredDoc, len(op.prefetches))
        var wg sync.WaitGroup
        for i, sub := range op.prefetches {
            wg.Add(1)
            go func(idx int, optr Operator) {
                defer wg.Done()
                results[idx] = drainOperator(optr)
            }(i, sub)
        }
        wg.Wait()
    } else {
        // 顺序执行：避免资源争抢
        for i, sub := range op.prefetches {
            op.batches[i] = drainOperator(sub)
        }
    }

    // 融合
    merged := op.fusion.Merge(toScoredDocSlices(op.batches), op.size)

    // 重评分
    for _, rescoreOp := range op.rescores {
        merged = applyRescore(merged, rescoreOp)
    }

    return op.buildResultBatch(merged), nil
}
```

**翻译器**（`internal/embedded/translator.go`）：

```go
func (t *Translator) translatePipeline(seg *segment.Reader, pq *dsl.PipelineQuery) (*plan.PipelineNode, error) {
    var prefetches []plan.LogicalPlan
    for _, raw := range pq.Prefetches {
        // 延迟解析每个 prefetch 子查询
        sub, err := t.parseSubQuery(raw)
        if err != nil {
            return nil, err
        }
        node, err := t.translate(seg, sub)
        if err != nil {
            return nil, err
        }
        prefetches = append(prefetches, node)
    }

    // 构建融合节点
    var fusionNode *fusion.Fusion
    if pq.Fusion != nil {
        f, err := fusion.NewFusion(pq.Fusion)
        if err != nil {
            return nil, err
        }
        fusionNode = &f
    }

    return &plan.PipelineNode{
        Prefetches: prefetches,
        FusionNode: fusionNode,
        Size:       pq.Size,
    }, nil
}
```

### 2.6 F6: TurboQuant (TQ) 量化

**变更内容**：
新增 `turboquant.go`、`hadamard.go`、`lloyd_max.go` 三个文件，实现 TurboQuant 量化方案。遵循现有 `atomic.Pointer` 热切换模式。Pure Go 先行实现，汇编 kernel 作为后续独立任务。

```go
// internal/index/hnsw/turboquant.go

type TQBits uint8
const (
    TQ1Bit   TQBits = 1
    TQ1_5Bit TQBits = 2  // 1.5-bit（3 级）
    TQ2Bit   TQBits = 3
    TQ4Bit   TQBits = 4
)

type TQMode uint8
const (
    TQNormal TQMode = 0 // 标准 TurboQuant
    TQPlus   TQMode = 1 // TQ+ 带误差校正
)

// TQState TurboQuant 量化状态（通过 atomic.Pointer 热切换）
type TQState struct {
    bits         TQBits
    mode         TQMode
    dims         int            // 原始维度
    paddedDims   int            // Hadamard 补齐后维度（2 的幂）
    rotation     []float32      // Hadamard 旋转矩阵（扁平存储，paddedDims*paddedDims）
    codebooks    [][]float32    // per-dim Lloyd-Max 重建值（每维 2^bits 个级别）
    codes        []byte         // sub-byte packed 量化码
    errorCorrection *TQErrorCorrection // TQ+ 误差校正（可选）
    distFunc     TQDistFunc     // 距离计算函数（按 bits 选择）
}

type TQErrorCorrection struct {
    shifts []float32 // per-coordinate shift
    scales []float32 // per-coordinate scale
}

// TQDistFunc TurboQuant 距离计算函数类型
type TQDistFunc func(queryRotated []float32, codes []byte, codebooks [][]float32, nodeID int32, dims int) float32
```

```go
// internal/index/hnsw/hadamard.go

// FastHadamardTransform 快速 Walsh-Hadamard 变换
// 递归蝴蝶网络实现，O(n log n)
// 输入维度补齐到 2 的幂（padding 维度置零）
func FastHadamardTransform(vec []float32, paddedDims int) []float32 {
    result := make([]float32, paddedDims)
    copy(result, vec)
    // 蝴蝶网络：逐层分组运算
    for h := 1; h < paddedDims; h *= 2 {
        for i := 0; i < paddedDims; i += 2 * h {
            for j := i; j < i+h; j++ {
                u := result[j]
                v := result[j+h]
                result[j] = u + v
                result[j+h] = u - v
            }
        }
    }
    // 归一化：除以 sqrt(paddedDims)
    norm := 1.0 / math.Sqrt(float64(paddedDims))
    for i := range result {
        result[i] *= float32(norm)
    }
    return result
}
```

```go
// internal/index/hnsw/lloyd_max.go

// LloydMaxQuantizer 最优标量量化器
// 对每个维度独立训练 2^bits 个量化级别
type LloydMaxQuantizer struct {
    bits       TQBits
    levels     int            // 2^bits
    maxIter    int            // 最大迭代次数（默认 20）
    tolerance  float64        // 收敛阈值
    // P-square 分位数估计器用于初始化（F11）
    estimators []*PSquareEstimator
}

// Train 训练 Lloyd-Max 量化器
// 输入：训练数据（旋转后的向量各维度值）
// 输出：per-dim 重建值（codebook）
func (q *LloydMaxQuantizer) Train(data [][]float32, dims int) ([][]float32, error) {
    codebooks := make([][]float32, dims)
    for d := 0; d < dims; d++ {
        // 1. 收集该维度所有值
        // 2. 用 P-square 估计初始分位数作为初始码本
        // 3. Lloyd 迭代：
        //    a. 将每个值分配到最近的量化级别
        //    b. 重新计算每个级别的中心（均值）
        //    c. 检查收敛
        codebooks[d] = q.lloydIterate(dimValues(d, data))
    }
    return codebooks, nil
}
```

**搜索路径**（`internal/index/hnsw/search.go`）：

```go
func (g *Graph) searchTQ(query []float32, k int, ef int) []ScoredDoc {
    tq := g.tqState.Load()
    if tq == nil {
        return g.searchExact(query, k, ef) // fallback
    }

    // 1. 查询向量做 Hadamard 旋转
    queryRotated := FastHadamardTransform(query, tq.paddedDims)

    // 2. widen beam 1.5×（coarse ranking 阶段）
    widenedEF := int(float64(ef) * 1.5)

    // 3. HNSW 搜索使用 TQ 距离函数
    candidates := g.hnswSearch(queryRotated, widenedEF, tq.distFunc)

    // 4. float32 rerank：对 TopK 候选用原始向量精确计算
    // 反量化候选向量，用 f32 距离重排序
    return rerank(candidates, query, g, k)
}
```

**量化切换**（`internal/index/hnsw/quant.go`）：

```go
func (g *Graph) Quantize(level QuantLevel, config ...QuantConfig) error {
    switch level {
    case QuantTQ:
        bits := TQ1Bit // 默认
        mode := TQNormal
        if len(config) > 0 {
            bits = config[0].TQBits
            mode = config[0].TQMode
        }
        state, err := trainTurboQuant(g.vecs, g.dims, g.numNodes, bits, mode)
        if err != nil {
            return err
        }
        g.tqState.Store(state)
    // ... 现有 SQ8/RaBitQ/PQ 分支 ...
    }
}
```

### 2.7 F7: Multi-vector / ColBERT MaxSim

**变更内容**：
新建 `internal/index/hnsw/multi_graph.go` 实现独立的 `MultiVectorGraph` 结构。段格式升级到 v2（footer 版本号 + 条件读取）。

```go
// internal/index/hnsw/multi_graph.go

// MultiVectorGraph 独立的多向量 HNSW 图
// 与单向量 Graph 完全隔离，使用 MaxSim 距离
type MultiVectorGraph struct {
    // CSR 邻接布局（与 Graph 相同结构）
    neighbors []int32
    offsets    []int32
    levels     []int8
    entryPoint int32
    maxLevel   int8

    // 多向量存储（扁平化 + 偏移量）
    multiVecsFlat []float32  // 所有 token 向量连续存储
    multiOffsets  []int32    // per-node: [start_offset, token_count]
    dims          int        // 单个 token 向量维度
    numNodes      int32

    distFunc MaxSimDistFunc  // MaxSim 距离函数
}

// MaxSim 距离：sum_a(max_b(dot(query[a], doc[b])))
// query: q_tokens × dims, doc: d_tokens × dims
// 结果：q_tokens 个最大匹配分数之和
type MaxSimDistFunc func(queryVecs [][]float32, docVecs [][]float32) float32
```

```go
// internal/simd/dist.go 新增
var MaxSimF32 func(queryVecs []float32, qTokens int, docVecs []float32, dTokens int, dims int) float32

// pure Go 实现
func maxSimF32PureGo(queryVecs []float32, qTokens int, docVecs []float32, dTokens int, dims int) float32 {
    var total float32
    for q := 0; q < qTokens; q++ {
        var maxScore float32 = -math.MaxFloat32
        for d := 0; d < dTokens; d++ {
            var dot float32
            for i := 0; i < dims; i++ {
                dot += queryVecs[q*dims+i] * docVecs[d*dims+i]
            }
            if dot > maxScore {
                maxScore = dot
            }
        }
        total += maxScore
    }
    return total
}
```

**段格式 v2**（`internal/storage/segment/wire.go`）：

```go
// Footer 新增 format_version 字段
const (
    FormatVersionV1 uint8 = 1
    FormatVersionV2 uint8 = 2
)

// 新增列类型码
const ftMultiVec uint8 = 11

// 段加载时按版本号选择读取路径
func openSegment(path string) (*Segment, error) {
    footer, err := readFooter(path)
    if err != nil {
        return nil, err
    }
    switch footer.FormatVersion {
    case FormatVersionV1:
        return openSegmentV1(path, footer)
    case FormatVersionV2:
        return openSegmentV2(path, footer) // 支持 ftMultiVec 列
    default:
        return nil, fmt.Errorf("unsupported format version: %d", footer.FormatVersion)
    }
}
```

**多向量列编码**（`internal/storage/segment/builder.go`）：

```go
// MultiVectorColumn 编码多向量列
// 每行存储 token_count * dims 个 float32 + 行内紧凑 + 偏移量表
type MultiVectorColumn struct {
    data     []float32    // 扁平化向量数据
    offsets  []int32      // per-row: [start_offset, token_count]
    dims     int
}
```

**DSL 扩展**：

```go
// internal/engine/dsl/ast.go
type MultiVectorQuery struct {
    Field        string      `json:"field"`
    QueryVectors [][]float32 `json:"query_vectors"` // 多个 token 向量
    K            int         `json:"k"`
    NumCandidates int        `json:"num_candidates"`
}
```

### 2.8 F8: Recommend/Discover/Context 搜索

**变更内容**：
新增 `vector_arith.go` 实现向量运算，新增 `RecommendQuery`/`DiscoverQuery` AST 节点。复用 HNSW 搜索基础设施，区别仅在查询向量来源（合成 vs 用户直接提供）。

```go
// internal/engine/vector_arith.go

// AvgVectors 计算向量集合的平均值
func AvgVectors(vecs [][]float32) []float32 {
    if len(vecs) == 0 {
        return nil
    }
    result := make([]float32, len(vecs[0]))
    for _, v := range vecs {
        for i, x := range v {
            result[i] += x
        }
    }
    n := float32(len(vecs))
    for i := range result {
        result[i] /= n
    }
    return result
}

// CombineRecommend 合成推荐查询向量
// query = avg(positive) - lambda * avg(negative)
func CombineRecommend(positive, negative [][]float32, lambda float64) []float32 {
    posAvg := AvgVectors(positive)
    if len(negative) == 0 {
        return posAvg
    }
    negAvg := AvgVectors(negative)
    result := make([]float32, len(posAvg))
    for i := range result {
        result[i] = posAvg[i] - float32(lambda)*negAvg[i]
    }
    return result
}
```

```go
// internal/engine/dsl/ast.go
type RecommendQuery struct {
    Positive        []string    `json:"positive"`          // 正例文档 ID
    Negative        []string    `json:"negative"`          // 负例文档 ID
    PositiveVectors [][]float32 `json:"positive_vectors"`  // 直接提供正例向量
    NegativeVectors [][]float32 `json:"negative_vectors"`  // 直接提供负例向量
    Strategy        string      `json:"strategy"`          // "average" | "sum" | "best_score"
    Field           string      `json:"field"`
    K               int         `json:"k"`
    Lambda          float64     `json:"lambda"`             // 负例权重（默认 1.0）
}
```

### 2.9 F9: Formula 公式重排序

**变更内容**：
新建 `internal/engine/formula/` 子包实现表达式解析和求值。新增 `FormulaRescoreOperator` 作为 Pipeline 管道的一个 stage。

```go
// internal/engine/formula/expr.go

// Expr 表达式 AST 节点
type Expr interface {
    Eval(ctx *EvalContext) float64
}

type EvalContext struct {
    Score   float64        // 原始检索分数
    Payload map[string]any // 文档 payload 字段值
    GeoDist float64        // 地理距离（如有 geo 查询）
    Now     time.Time      // 当前时间
}

// 表达式类型
type ConstExpr struct{ Val float64 }
type FieldExpr struct{ Name string }       // payload.field_name
type ScoreExpr struct{}                    // score
type BinopExpr struct { Op string; L, R Expr } // +, -, *, /
type FuncExpr struct { Name string; Args []Expr } // log, exp, sqrt, abs, geo_distance
type CondExpr struct { Cond Expr; Then, Else Expr } // if/else

// Parse 解析表达式字符串为 AST
func Parse(expr string) (Expr, error) {
    // 递归下降解析器
    // 支持: +, -, *, /, (), 函数调用, 字段引用, 条件表达式
    // 示例: "score * log(1 + payload.likes) + payload.freshness * 0.3"
}

// EvalStr 求值
func (e *ConstExpr) Eval(ctx *EvalContext) float64 { return e.Val }
func (e *FieldExpr) Eval(ctx *EvalContext) float64 {
    v, ok := ctx.Payload[e.Name]
    if !ok { return 0 }
    // 类型断言转 float64
}
func (e *BinopExpr) Eval(ctx *EvalContext) float64 {
    l, r := e.L.Eval(ctx), e.R.Eval(ctx)
    switch e.Op {
    case "+": return l + r
    case "-": return l - r
    case "*": return l * r
    case "/": return l / r
    }
}
```

```go
// internal/engine/exec/operators.go 新增

type FormulaRescoreOperator struct {
    input   Operator
    formula Expr
    weight  float64
    seg     *segment.Reader // 用于读取 payload 字段
}

func (op *FormulaRescoreOperator) Next() (*ColumnBatch, error) {
    batch, err := op.input.Next()
    if err != nil || batch == nil {
        return batch, err
    }
    // 1. 对 batch 中每个文档读取 payload 字段
    // 2. 构建 EvalContext
    // 3. 求值 formula
    // 4. 更新分数：new_score = old_score * (1 - weight) + formula_score * weight
    // 5. 重新排序
    return rescoredBatch, nil
}
```

### 2.10 F10: WAL Delta 增量同步

**变更内容**：
通过 Raft 共识层同步 WAL。副本作为 Raft follower 自动接收 log entries。新增 `ReadSince` 方法支持从指定 LSN 扫描 WAL。

```go
// internal/storage/wal/wal.go 新增

// ReadSince 从指定 LSN 开始读取 WAL 记录
func (w *WAL) ReadSince(lsn int64) ([]Record, error) {
    // 1. 扫描 WAL 段文件，找到包含 lsn 的段
    // 2. 从该位置开始顺序读取，直到 WAL 末尾
    // 3. CRC 校验每条记录
    // 4. 返回 [lsn, currentLSN) 范围的记录
}
```

```go
// internal/raft/types.go 扩展

// Watermarks 扩展为 per-replica
type ReplicaWatermark struct {
    ReplicaID  string
    SyncedLSN   int64  // 该副本已同步的 LSN
    LastSyncAt time.Time
}

type MetaState struct {
    // ... 现有字段 ...
    ReplicaWatermarks map[string]*ReplicaWatermark // per-shard per-replica
}
```

```go
// internal/raft/raft_cluster.go 新增

// SyncWALDelta 通过 Raft 共识层同步 WAL Delta
// 副本作为 Raft follower 自动接收 log entries
// 1. Leader 将 WAL 记录通过 Raft log 复制到 followers
// 2. Follower apply 后将 WAL 记录写入本地 WAL
// 3. 更新 ReplicaWatermark
func (rc *RaftCluster) SyncWALDelta(shardID string, sinceLSN int64) error {
    // 1. 从 WAL 读取 [sinceLSN, currentLSN) 的记录
    records, err := rc.wal.ReadSince(sinceLSN)
    if err != nil {
        return err
    }

    // 2. 将记录封装为 Raft log entry
    cmd := FSMCommand{
        Type:    CmdWALDelta,
        ShardID: shardID,
        Payload: encodeWALRecords(records),
    }

    // 3. 通过 Raft 提交
    return rc.raft.Apply(encodeCmd(cmd), raftTimeout).Error()
}
```

**Fallback 机制**：
```go
// 增量同步失败或追不上时自动回退全量快照
func (rc *RaftCluster) SyncWithFallback(shardID, replicaID string) error {
    watermark := rc.getWatermark(shardID, replicaID)
    gap := rc.wal.CurrentLSN() - watermark.SyncedLSN

    if gap > maxDeltaGap {
        // Delta 过大，fallback 全量快照
        return rc.snapshotSync(shardID, replicaID)
    }

    if err := rc.SyncWALDelta(shardID, watermark.SyncedLSN); err != nil {
        // 同步失败，fallback 全量快照
        return rc.snapshotSync(shardID, replicaID)
    }
    return nil
}
```

### 2.11-2.16 P2 功能简要设计

#### F11: P-square 在线分位数估计

```go
// internal/engine/stats/quantile.go

// PSquareEstimator P² 算法在线分位数估计器
// 无需存储全部数据，O(1) 空间复杂度
type PSquareEstimator struct {
    p       float64    // 目标分位数 (0-1)
    q       [5]float64 // 5 个标记点
    n       [5]int     // 标记点位置
    np      [5]int     // 期望位置
    dn      [5]float64 // 位置增量
    count   int        // 观测总数
}

func (e *PSquareEstimator) Add(val float64)  // 添加观测值
func (e *PSquareEstimator) Quantile() float64 // 返回当前分位数估计
```

#### F12: HNSW 图修复

```go
// internal/index/hnsw/healing.go

// HealGraph 检测并修复 HNSW 图中的断连节点
func HealGraph(g *Graph) int {
    // 1. BFS 从 entry point 遍历可达节点集合
    // 2. 找出不可达节点和度数 < minDegree 的节点
    // 3. 对问题节点暴力 k-NN 搜索找邻居
    // 4. 重建连接
    // 5. 返回修复的节点数
}
```

#### F13: 多语言分词器

```go
// internal/index/analyzer/tokenizers.go 新增
// CJKTokenizer: 二元组分词（中文）
// JapaneseTokenizer: 纯 Go 字典分词（替代 kuromoji）

// internal/index/analyzer/filters.go 新增
// SnowballStemmerFilter: 纯 Go Snowball 词干提取（英/法/德/西/意/葡/俄等）
```

#### F14: BM25 嵌入生成

```go
// internal/index/inverted/bm25_embedding.go

// BM25Embedding 将文本转换为 BM25 稀疏向量嵌入
// dim = termID (Murmur3 hash), weight = BM25 TF
func BM25Embedding(tokens []string, params Bm25Params) SparseVector {
    // 1. 对每个 token 计算 Murmur3 hash 作为 dim ID
    // 2. 计算 BM25 TF 权重: tf = n * (k1+1) / (k1*(1-b+b*doc_len/avgdl) + n)
    // 3. 返回稀疏向量 {dimIDs, weights}
}
```

#### F15: RBAC

```go
// internal/auth/jwt.go
// JWT 签发/验证（golang-jwt/jwt v5，无 CGO）

// internal/auth/rbac.go
type Role string
const (
    RoleAdmin  Role = "admin"
    RoleWriter Role = "writer"
    RoleReader Role = "reader"
)

type Permission struct {
    Role       Role
    Index      string // "*" = 所有索引
    Operations []string // ["read", "write", "manage"]
}
```

#### F16: 集合级配额 + 审计日志

```go
// internal/tenant/tenant.go 扩展
type IndexQuota struct {
    MaxDocs        int64
    MaxStorageBytes int64
    WriteRatePerSec int     // 令牌桶
    QueryRatePerSec int     // 令牌桶
}

// internal/observability/audit.go
type AuditEntry struct {
    Timestamp time.Time
    User      string
    Operation string
    Index     string
    Result    string
    Duration  time.Duration
}
```

---

## 3. 接口设计 (Interface Design)

### 3.1 ES 兼容 API 扩展

所有新功能通过 ES DSL 扩展字段暴露：

#### 稀疏向量 Mapping

```json
PUT /my-index
{
  "mappings": {
    "properties": {
      "sparse_embedding": {
        "type": "sparse_vector",
        "indexed": true
      }
    }
  }
}
```

#### Float16 / TurboQuant / Multi-vector Mapping

```json
PUT /my-index
{
  "mappings": {
    "properties": {
      "embedding": {
        "type": "dense_vector",
        "dimensions": 768,
        "element_type": "float16"
      },
      "colbert_vec": {
        "type": "dense_vector",
        "dimensions": 128,
        "multivector": true
      },
      "quantized_embedding": {
        "type": "dense_vector",
        "dimensions": 768,
        "quantization": { "type": "turboquant", "bits": 2 }
      }
    }
  }
}
```

#### Query API 管道

```json
POST /my-index/_search
{
  "_pipeline": {
    "prefetches": [
      { "knn": { "field": "embedding", "query_vector": [0.1, 0.2, ...], "k": 100 } },
      { "query": { "match": { "title": "hello" } } }
    ],
    "fusion": { "method": "dbsf", "weights": [0.7, 0.3] },
    "rescore": [
      { "formula": "score * log(1 + payload.likes)", "weight": 0.3 }
    ]
  }
}
```

#### Recommend 搜索

```json
POST /my-index/_search
{
  "recommend": {
    "positive": ["doc1", "doc2"],
    "negative": ["doc3"],
    "strategy": "average",
    "field": "embedding",
    "k": 10
  }
}
```

#### MMR 多样性搜索

```json
POST /my-index/_search
{
  "knn": { "field": "embedding", "query_vector": [...], "k": 50 },
  "mmr": { "enabled": true, "lambda": 0.5 }
}
```

### 3.2 Go 内部接口

```go
// SparseInvertedIndex 接口
type SparseInvertedIndexer interface {
    AddVector(rowID int32, dimIDs []uint64, weights []float32) error
    Search(queryDimIDs []uint64, queryWeights []float32, k int) ([]ScoredDoc, error)
    Flush(path string) error
}

// Fusion 接口（现有，DBSF 新增实现）
type Fusion interface {
    Merge(paths [][]ScoredDoc, windowSize int) []ScoredDoc
}

// Formula 表达式接口
type FormulaExpr interface {
    Eval(ctx *EvalContext) float64
    String() string
}
```

---

## 4. 内部设计 (Internal Design)

### 4.1 稀疏向量搜索路径

**职责**: 从 sparse_vector 字段查询到返回结果

**核心方法**:

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `NewSparseInvertedIndex` | `(precision WeightPrecision) *SparseInvertedIndex` | 创建索引 |
| `AddVector` | `(idx *SparseInvertedIndex, rowID int32, dimIDs []uint64, weights []float32) error` | 添加稀疏向量 |
| `Search` | `(idx *SparseInvertedIndex, qDimIDs []uint64, qWeights []float32, k int) ([]ScoredDoc, error)` | WAND 搜索 |
| `NewSparseWANDScorer` | `(qDimIDs []uint64, qWeights []float32, postings []*WeightedPostingList) *SparseWANDScorer` | 创建评分器 |

**依赖关系**:
- 依赖 `inverted.TermDict`（FST 词典）用于维度 ID 映射
- 依赖 `exec.Operator` 接口集成到执行管道
- 被 `flush.FlushWorker` 调用构建段级索引

### 4.2 Pipeline 执行器

**职责**: 编排嵌套子查询的并行执行、融合和重评分

**核心方法**:

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `NewPipelineOperator` | `(prefetches []Operator, f Fusion, rescores []Operator, size int) *PipelineOperator` | 构建管道 |
| `Next` | `() (*ColumnBatch, error)` | Volcano pull 接口 |
| `shouldParallel` | `(n int) bool` | 自适应并行决策 |

**依赖关系**:
- 依赖 `fusion.Fusion` 接口
- 依赖 `exec.Operator` 接口（子查询和重评分都是算子）
- 被 `Translator.translatePipeline` 创建

### 4.3 TurboQuant 量化器

**职责**: 训练和存储 TurboQuant 量化状态

**核心方法**:

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `trainTurboQuant` | `(vecs []float32, dims, numNodes int, bits TQBits, mode TQMode) (*TQState, error)` | 训练量化器 |
| `FastHadamardTransform` | `(vec []float32, paddedDims int) []float32` | Hadamard 旋转 |
| `LloydMaxQuantizer.Train` | `(data [][]float32, dims int) ([][]float32, error)` | 训练码本 |
| `searchTQ` | `(g *Graph, query []float32, k, ef int) []ScoredDoc` | TQ 搜索路径 |

**依赖关系**:
- 依赖 `stats.PSquareEstimator`（F11）用于码本初始化
- 依赖 `atomic.Pointer` 模式实现热切换
- 被 `Graph.Quantize` 调用

### 4.4 Multi-vector 图

**职责**: 多向量 HNSW 图构建和 MaxSim 搜索

**核心方法**:

| 方法名 | 签名 | 说明 |
|--------|------|------|
| `NewMultiVectorGraph` | `(dims, m, efConstruct int) *MultiVectorGraph` | 创建图 |
| `Insert` | `(g *MultiVectorGraph, nodeID int32, vecs [][]float32) error` | 插入多向量节点 |
| `Search` | `(g *MultiVectorGraph, queryVecs [][]float32, k, ef int) []ScoredDoc` | MaxSim 搜索 |

**依赖关系**:
- 依赖 `simd.MaxSimF32` 距离函数
- 独立于单向量 `Graph`，通过 `index.RegisterIndex` 注册

---

## 5. 数据模型 (Data Model)

### 5.1 段格式 v2

```go
// Segment Footer v2
type SegmentFooterV2 struct {
    Magic        [8]byte // "USSEGFT1"
    FormatVersion uint8  // 2
    NumColumns   uint32
    ColumnEntries []ColumnEntryV2
    ZoneMaps      map[string]*ZoneMap
    FieldMeta     map[string]*FieldMeta
}

type ColumnEntryV2 struct {
    FieldName   string
    FieldType   uint8     // ftMultiVec=11 新增
    Offset      int64
    Length      int64
    ElementType uint8     // 新增: 0=f32, 1=f16, 2=i8, 3=bf16
    // Multi-vector 特有
    TokenCount  int32     // 每文档 token 数（固定）
    Dims        int32     // 单 token 维度
}
```

### 5.2 稀疏向量存储格式

```go
// sparse.idx 文件结构
type SparseIndexFile struct {
    Magic          uint32 // "SPID"
    Version        uint8
    NumTerms       uint32
    WeightPrecision uint8  // 0=f32, 1=f16, 2=u8
    Terms          []TermEntry
    NumDocsTotal   uint32
}

type TermEntry struct {
    TermID     uint64
    NumDocs    uint32
    MaxWeight  float32
    DocIDs     []int32   // delta-encoded + bitpacked
    Weights    []byte    // 按 WeightPrecision 编码
}
```

### 5.3 TurboQuant 量化状态文件

```go
// quant.state 文件 TQ tier 布局
type TQStateFile struct {
    Tier        uint8   // QuantTQ
    Bits        uint8
    Mode        uint8
    Dims        int32
    PaddedDims  int32
    Rotation    []float32  // paddedDims * paddedDims
    Codebooks   []float32  // dims * (2^bits)
    Codes       []byte     // sub-byte packed
    ErrorCorrection *TQECFile // TQ+ 可选
}
```

---

## 6. 错误处理 (Error Handling)

### 6.1 错误类型定义

| 错误码/类型 | 触发条件 | 处理方式 | 用户提示 |
|-------------|----------|----------|----------|
| `ErrSparseIndexNotFound` | 旧段无稀疏倒排索引 | Fallback 到暴力扫描 | 200（透明降级） |
| `ErrTQTrainingFailed` | TurboQuant 训练数据不足 | Fallback 到 float32 精确搜索 | 500 + warning log |
| `ErrTQSearchFallback` | TQ 搜索路径异常 | Fallback 到 SQ8 或 float32 | 200 + warning log |
| `ErrMultiVecFormatV1` | v1 段遇到 multi-vector 查询 | 返回错误提示升级索引 | 400 "index format v1 does not support multivector" |
| `ErrWALDeltaGap` | WAL Delta 间距过大 | Fallback 到全量快照 | 200（透明降级 + log） |
| `ErrWALDeltaCorrupt` | WAL Delta 记录 CRC 校验失败 | Fallback 到全量快照 | 200（透明降级 + log） |
| `ErrFormulaParse` | 公式表达式语法错误 | 返回 400 | 400 "invalid formula expression: {detail}" |
| `ErrFormulaFieldNotFound` | 公式引用的 payload 字段不存在 | 该字段值为 0 | 200（静默处理） |
| `ErrRBACUnauthorized` | JWT 无效或权限不足 | 返回 401/403 | 401 "unauthorized" / 403 "forbidden" |
| `ErrQuotaExceeded` | 集合级配额超限 | 返回 429 | 429 "quota exceeded: {detail}" |

### 6.2 异常传播策略

- **量化异常**：TQ/F16 量化搜索失败时自动 fallback 到下一级精度（TQ → SQ8 → float32），记录 warning 日志但不中断搜索
- **WAL 同步异常**：Delta 同步失败时 fallback 全量快照，通过 Raft 共识保证一致性
- **Pipeline 子查询异常**：单个 prefetch 子查询失败时，该路径返回空结果，不阻断其他子查询；最终融合时跳过失败路径
- **Formula 求值异常**：单个文档公式求值失败时保留原始分数，不阻断重评分流程

### 6.3 边界情况处理

| 场景 | 输入示例 | 期望行为 |
|------|----------|----------|
| 空稀疏向量 | dimIDs=[] | 返回空结果，不报错 |
| 单文档稀疏搜索 | numDocs=1 | 跳过 WAND，直接返回该文档 |
| TQ 训练数据 < 100 条 | numNodes < 100 | Fallback 到 float32（TQ 训练需足够样本） |
| Multi-vector token_count=0 | 空 token 向量列表 | 返回错误 "empty multivector" |
| Pipeline 无 prefetch | prefetches=[] | 返回 400 "pipeline requires at least one prefetch" |
| Recommend 正负例均为空 | positive=[], negative=[] | 返回 400 "recommend requires at least one positive or negative example" |
| Formula 除零 | payload.denominator=0 | 返回 0（不 panic） |
| WAL Delta LSN > current | sinceLSN > currentLSN | 返回空记录列表（副本已超前） |
| F16 向量 NaN/Inf | NaN in input | 拒绝写入，返回 400 |

---

## 7. 测试规划 (Test Plan)

### 7.1 测试策略表

| 测试类型 | 覆盖范围 | 目标覆盖率 | 说明 |
|----------|----------|------------|------|
| 单元测试 | 稀疏向量索引、WAND 评分、DBSF 融合、MMR、F16 转换、Formula 表达式、P-square、Hadamard 变换、Lloyd-Max 训练、图修复、BM25 嵌入 | ≥ 90% | 核心算法和数学逻辑 |
| 单元测试 | RBAC 权限检查、配额令牌桶、审计日志 | ≥ 80% | 企业级功能 |
| 单元测试 | DSL 解析器、AST 构造、计划节点翻译 | ≥ 85% | 查询管道 |
| 集成测试 | 段格式 v2 读写、稀疏向量 flush→search、TQ 量化→搜索→rerank、Multi-vector 索引→MaxSim 搜索、Pipeline 多级查询、Recommend 搜索、WAL Delta 同步 | ≥ 80% | 端到端功能验证 |
| E2E 测试 | ES DSL 扩展字段 API、Python pytest | - | API 兼容性验证 |

### 7.2 关键测试场景清单

#### 正向用例（主成功场景）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| SPLADE 稀疏向量索引+搜索 | 1000 条稀疏向量（每条 ~50 非零维度） | recall@10 ≥ 0.95 vs 暴力扫描 | P0 |
| DBSF 融合两路搜索 | BM25 + ANN 各 100 条结果 | 融合后结果合理排序，无极端值偏斜 | P0 |
| MMR 重排序 50 条结果 | lambda=0.5 | 结果多样性提升，相似文档去重 | P0 |
| F16 向量存储+搜索 | 768 维 float32 → f16 存储 | recall@10 ≥ 0.99 vs float32，内存减半 | P0 |
| Pipeline 三级查询 | knn → match → sparse 三路 prefetch + RRF + Formula | 返回融合+重排序后的结果 | P0 |
| TQ 1-bit 量化搜索 | 768 维向量，TQ 1-bit | recall@10 ≥ 0.90 vs float32，32x 压缩 | P1 |
| TQ 2-bit 量化搜索 | 768 维向量，TQ 2-bit | recall@10 ≥ 0.97 vs float32，16x 压缩 | P1 |
| TQ+ 误差校正 | TQ 2-bit + ErrorCorrection | recall@10 ≥ 0.99 vs float32 | P1 |
| ColBERT Multi-vector 搜索 | 128 维 × 20 tokens/doc | MaxSim 评分正确，recall@10 ≥ 0.90 | P1 |
| Recommend 搜索 | 2 正例 + 1 负例 | 返回与正例相似、与负例不同的文档 | P1 |
| Formula 重排序 | "score * log(1 + payload.likes)" | 按公式重排序，likes 高的优先 | P1 |
| WAL Delta 同步 | 100 条新写入 | 副本只同步增量，耗时 < 全量快照 | P1 |

#### 边界用例（输入边界、状态边界、并发边界）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| 稀疏向量全零维度 | dimIDs=[] | 返回空结果 | P0 |
| 稀疏向量单维度 | dimIDs=[42], weights=[1.0] | 返回该维度权重最大的文档 | P0 |
| WAND TopK=1 | k=1 | 正确返回最高分文档 | P0 |
| DBSF 单路径融合 | 只有 1 条搜索路径 | 等效于该路径 z-score 归一化 | P0 |
| MMR K=1 | k=1 | 返回最高分文档（无多样性处理） | P0 |
| F16 NaN 输入 | vec=[NaN, 0.1, ...] | 拒绝写入，400 | P0 |
| TQ dims 非 2 的幂 | dims=768 | 补齐到 1024，padding 置零 | P1 |
| Multi-vector token_count=1 | 单 token 向量 | MaxSim 退化为普通 dot product | P1 |
| Pipeline 单个 prefetch | prefetches=[single] | 等效于普通搜索，无融合 | P0 |
| Formula 空表达式 | formula="" | 返回原始分数 | P1 |
| WAL Delta LSN=0 | sinceLSN=0 | 返回全量 WAL 记录 | P1 |
| 并发 Pipeline 搜索 | 10 个并行 pipeline 查询 | 无 deadlock，结果正确 | P0 |
| 并发 TQ 量化切换 | 搜索中切换量化层 | 通过 atomic.Pointer 无锁切换 | P1 |

#### 异常用例（错误输入、依赖失败、权限拒绝）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| 稀疏向量索引缺失 | 旧段无 sparse.idx | Fallback 暴力扫描，结果正确 | P0 |
| TQ 训练数据不足 | numNodes < 100 | Fallback float32，warning log | P1 |
| Multi-vector v1 段查询 | v1 段 + multivector query | 400 "index format v1 does not support multivector" | P1 |
| WAL Delta CRC 失败 | 损坏的 WAL 段 | Fallback 全量快照，warning log | P1 |
| Formula 语法错误 | "score * * 2" | 400 "invalid formula: unexpected operator" | P1 |
| Formula 除零 | "score / payload.zero" | 该文档分数为 0，不 panic | P1 |
| RBAC 无 token | 无 Authorization header | 401 "unauthorized" | P2 |
| RBAC 权限不足 | reader 尝试写入 | 403 "forbidden" | P2 |
| 配额超限 | 写入超过 max_docs | 429 "quota exceeded: max_docs" | P2 |
| Pipeline 子查询失败 | 一个 prefetch 返回错误 | 跳过该路径，其他路径正常融合 | P0 |
| F16 段格式不兼容 | v1 加载器遇到 f16 列 | 跳过该列或报错（按版本号判断） | P0 |

#### 兼容用例（与现有模块的交互）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| 现有 E2E 全量回归 | 294 个现有 Python E2E | 全绿，无回归 | P0 |
| ES Golden 测试 | 27 个 Golden test | 全绿，100% 兼容 | P0 |
| v1 段 + v2 段混合 | 同一 shard 中混合段 | 均可正常搜索 | P1 |
| 现有 Hybrid 查询 | 原有 hybrid query | 不受 Pipeline 影响，正常工作 | P0 |
| 现有量化切换 | SQ8/RaBitQ/PQ | 不受 TQ 新增影响 | P1 |
| 现有租户配额 | 现有 tenant.AcquireQuery | 不受集合级配额影响 | P2 |

### 7.3 Mock 清单

| 依赖项 | Mock 方式 | 模拟行为 | 使用场景 |
|--------|-----------|----------|----------|
| `segment.Reader` | Fake 实现 `ColumnProvider`/`IndexProvider` | 返回预构造的列数据和索引 | 稀疏向量搜索、TQ 搜索、Multi-vector 搜索单元测试 |
| `fusion.Fusion` | Stub 实现 `Fusion` 接口 | 返回预定义的融合结果 | Pipeline 执行器单元测试 |
| `exec.Operator` | Mock `Next()` 方法 | 返回预定义的 ColumnBatch | MMR、Formula 算子单元测试 |
| `wal.WAL` | Fake WAL | 预填充记录，支持 ReadSince | WAL Delta 同步测试 |
| `raft.RaftCluster` | Mock Raft | 模拟 Apply/Follower 状态 | WAL Delta 集成测试 |
| `auth.JWT` | 测试用 signing key | 签发测试 token | RBAC 测试 |
| HTTP Response | httptest.NewRecorder | 捕获 HTTP 响应 | ES DSL 扩展字段 API 测试 |

### 7.4 测试数据设计

- **SPLADE 稀疏向量数据集**: 1000 条 768 维稀疏向量（每条 ~50 非零维度），通过 SPLADE 模型预计算
- **ColBERT 多向量数据集**: 500 条文档，每条 20 个 128 维 token 向量
- **Recall 基准数据集**: 10,000 条 768 维 float32 向量 + 100 条查询，带 ground truth top-100
- **Formula 表达式集**: 20 个合法表达式 + 10 个非法表达式
- **段格式 v1 数据**: 现有测试段，验证 v2 加载器兼容性
- **多语言文本集**: 英/中/日/法/德 各 100 条文档

### 7.5 验证方式

- **CI 集成**: GitHub Workflow 执行 `go build ./... && go test ./... && go vet ./...` + Python E2E
- **量化精度基准**: `uni-bench` 工具对比 TQ/F16/SQ8/RaBitQ/float32 的 recall@k
- **融合质量基准**: 人工标注集计算 NDCG，对比 DBSF vs RRF vs Weighted
- **段格式兼容性**: 编写 v1 段 → v2 加载器 → 搜索 → 验证结果正确
- **WAL Delta**: 双节点环境故障注入（kill follower → 重启 → 验证增量同步）

---

## 8. 验收标准 (Acceptance Criteria)

- [ ] **编译**: `make build` 全绿（`CGO_ENABLED=0` 纯静态编译）
- [ ] **质量**: `go build ./... && go test ./... && go vet ./...` 全绿
- [ ] **回归**: 现有 294 Python E2E 全绿，27 Golden test 全绿，不引入回归
- [ ] **ES 兼容**: ES 兼容 API 覆盖率不降低（Golden 100%），所有新功能通过 DSL 扩展字段暴露
- [ ] **F1 稀疏向量**: SPLADE 稀疏向量 recall@10 ≥ 0.95 vs 暴力扫描，WAND 剪枝生效
- [ ] **F2 DBSF**: DBSF 融合 NDCG 不低于 RRF
- [ ] **F3 MMR**: lambda=0.5 时结果多样性提升可量化
- [ ] **F4 Float16**: recall@10 ≥ 0.99 vs float32，内存减少 50%
- [ ] **F5 Pipeline**: 多级 prefetch + 融合 + 重评分端到端可用
- [ ] **F6 TurboQuant**: 1-bit recall@10 ≥ 0.90，2-bit recall@10 ≥ 0.97 vs float32
- [ ] **F7 Multi-vector**: ColBERT MaxSim 搜索端到端可用，recall@10 ≥ 0.90
- [ ] **F8 Recommend**: 正负例推荐搜索端到端可用
- [ ] **F9 Formula**: 表达式解析+求值正确，10 个合法 + 10 个非法表达式测试通过
- [ ] **F10 WAL Delta**: 增量同步带宽 < 全量快照，故障注入测试通过
- [ ] **F11 P-square**: 分位数估计误差 < 1%
- [ ] **F12 图修复**: 断连节点修复后 BFS 可达率 100%
- [ ] **F13 多语言**: 英/中/日/法/德 分词器各 100 条文档测试通过
- [ ] **F14 BM25 嵌入**: BM25 嵌入 + 稀疏向量索引搜索结果与倒排索引一致
- [ ] **F15 RBAC**: JWT 认证 + 3 种角色权限检查通过
- [ ] **F16 配额+审计**: 配额超限返回 429，审计日志记录正确
- [ ] **段格式**: v2 向后兼容 v1，混合段搜索正确

---

## 9. 实现清单 (Implementation Checklist)

### P0 阶段（并行实施）

- [ ] F1: 新建 `sparse_index.go`，实现 `WeightedPostingList` + `SparseWANDScorer`
- [ ] F1: 注册 `KindSparseInverted`，添加 `fieldRoutes` 路由
- [ ] F1: 新增 `sparse.idx` 段格式，flush 集成
- [ ] F1: `planner.go` `buildSparseVector` 升级为 WAND 搜索
- [ ] F1: 单元测试 + 集成测试
- [ ] F2: 实现 `DBSFFusion`，注册 `NewFusion` 工厂
- [ ] F2: DSL 解析 `dbsf` 方法
- [ ] F2: 单元测试（Welford 方差、z-score 归一化）
- [ ] F3: 新建 `mmr.go`，实现 `MMRRescore`
- [ ] F3: 新增 `MMROperator`，DSL 解析 `mmr` 字段
- [ ] F3: 单元测试 + 集成测试
- [ ] F4: 新建 `f16.go`，实现 F16↔F32 转换
- [ ] F4: `mapping.go` 新增 `ElementFloat16`
- [ ] F4: `Graph` 结构扩展 `vecsF16`，`Vector()` 方法适配
- [ ] F4: `simd/dist.go` 新增 `DotF16F32`，段编码 f16 路径
- [ ] F4: 单元测试（精度、NaN 处理）
- [ ] F5: 新增 `PipelineQuery` AST + `parsePipelineQuery`
- [ ] F5: 新增 `PipelineNode` 计划节点 + `PipelineOperator`
- [ ] F5: 自适应并行控制器实现
- [ ] F5: `translator.go` + `planner.go` 集成
- [ ] F5: 单元测试 + 集成测试

### P2 先行

- [ ] F11: 新建 `quantile.go`，实现 P² 算法
- [ ] F11: 集成到 `trainSQ8` 分位数估计
- [ ] F11: 单元测试

### P1 阶段

- [ ] F6: 新建 `hadamard.go`，实现快速 Walsh-Hadamard 变换
- [ ] F6: 新建 `lloyd_max.go`，实现 Lloyd-Max 量化器训练
- [ ] F6: 新建 `turboquant.go`，实现 `TQState` + atomic.Pointer 切换
- [ ] F6: `search.go` 新增 `searchTQ` 方法
- [ ] F6: `simd/dist.go` 新增 sub-byte 距离内核（pure Go）
- [ ] F6: 段格式 `quant.state` TQ tier 序列化
- [ ] F6: 单元测试（Hadamard 正确性、Lloyd-Max 收敛、recall 基准）
- [ ] F7: 段格式 v2 footer + 条件读取
- [ ] F7: 新建 `multi_graph.go`，实现 `MultiVectorGraph` + MaxSim 距离
- [ ] F7: `simd/dist.go` 新增 `MaxSimF32`
- [ ] F7: `mapping.go` 新增 `MultiVector` 字段
- [ ] F7: DSL 扩展 + 翻译器 + 计划节点
- [ ] F7: 单元测试 + 集成测试 + recall 基准
- [ ] F8: 新建 `vector_arith.go`，实现向量运算
- [ ] F8: DSL 新增 `RecommendQuery`/`DiscoverQuery`
- [ ] F8: `translator.go` 实现 `translateRecommend`
- [ ] F8: 单元测试 + 集成测试
- [ ] F9: 新建 `formula/expr.go`，实现表达式解析+求值
- [ ] F9: 新增 `FormulaRescoreOperator`
- [ ] F9: DSL 新增 `FormulaRescore` + 计划节点
- [ ] F9: 单元测试（表达式解析、求值、边界）
- [ ] F10: `wal.go` 新增 `ReadSince` 方法
- [ ] F10: `raft/types.go` 扩展 per-replica 水位线
- [ ] F10: `raft_cluster.go` 新增 `SyncWALDelta` + Fallback
- [ ] F10: gRPC `DeltaSync` RPC
- [ ] F10: 集成测试（增量同步 + 故障注入）

### P2 阶段

- [ ] F12: 新建 `healing.go`，实现 BFS 检测 + k-NN 修复
- [ ] F12: `merge/scheduler.go` 集成 healing 步骤
- [ ] F12: 单元测试
- [ ] F13: `tokenizers.go` 新增 CJK 分词器
- [ ] F13: `filters.go` 新增 Snowball stemmer
- [ ] F13: `factory.go` 注册新分词器
- [ ] F13: 单元测试（多语言分词）
- [ ] F14: 新建 `bm25_embedding.go`，实现 BM25 → 稀疏向量转换
- [ ] F14: `mapping.go` 新增 `Bm25Embedding` 标记
- [ ] F14: `flush.go` 集成 BM25 嵌入构建
- [ ] F14: 单元测试（BM25 嵌入正确性）
- [ ] F15: 新建 `internal/auth/jwt.go` + `rbac.go`
- [ ] F15: `router.go` 挂载认证中间件
- [ ] F15: 新增 `_security/role` / `_security/user` handler
- [ ] F15: 单元测试 + 集成测试
- [ ] F16: `tenant.go` 扩展 per-index 配额
- [ ] F16: 新建 `audit.go` 审计日志
- [ ] F16: `router.go` 审计中间件
- [ ] F16: 单元测试
