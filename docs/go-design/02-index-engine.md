# UniSearch 详细设计 02 —— 索引引擎

> 版本：v0.1
> 日期：2026-10-17
> 上游：[概要设计 §6](../vector-database-design.md) / [00-overview.md](00-overview.md) / [01-storage-engine.md](01-storage-engine.md)
> 范围：统一索引接口、HNSW（含 Cross-Link Merge）、倒排、分析器、稀疏向量、标量索引、suggester
> 代码位置：`internal/index/`、`internal/simd/`

---

## 1. 统一索引插件接口 `[M0 接口 / M3 插件化]`

### 1.1 Index trait

```go
// internal/index/registry.go
// 所有索引（向量/倒排/标量）实现此接口；生命周期由存储引擎 flush/merge 驱动。
type Index interface {
    Kind() IndexKind   // KindHNSW | KindInverted | KindBKD | KindBitmap | KindBloom | KindFST

    // 构建：从段列数据构建索引文件（写入 seg_xxx/ 对应子目录，01 §4.1）
    Build(ctx context.Context, col ColumnProvider, w IndexOutput) error
    // 加载：mmap 只读打开（返回的句柄不可变、并发安全）
    Load(r IndexInput) (LoadedIndex, error)

    // 合并：两个同字段索引合并（语义各异：HNSW=Cross-Link、倒排=posting 归并、BKD=重建）
    Merge(ctx context.Context, others []LoadedIndex, w IndexOutput) error

    // 量化降级（仅向量索引实现，其余返回 ErrUnsupported）
    Quantize(ctx context.Context, to QuantLevel) error

    // 内存预算上报（TierManager 与合并调度消费，00 §3.1）
    MemEstimate() int64
}

type LoadedIndex interface {
    Search(ctx context.Context, q Query, budget SearchBudget) (*SearchResult, error)
    Close() error
}
```

`ColumnProvider` 由存储层实现（读 `col_*.bin` 流式块迭代），`IndexInput/Output` 封装段目录文件句柄——**索引引擎与存储引擎仅通过段文件与这两个窄接口耦合**（00 §4 依赖规则）。

### 1.2 注册机制（M3 插件 SDK 的现实方案）

```go
// 进程内注册（M0-M2 唯一形态）
func RegisterIndex(kind IndexKind, factory IndexFactory)
```

M3 「动态库插件」在 Go 的约束（`plugin` 包仅 Linux、ABI 不稳）下采取**双路径**：
1. **进程内注册**：第三方 fork 编译或 `//go:build` 标签（默认路径）；
2. **sidecar 进程插件**：索引引擎跑在独立进程，gRPC `IndexService` 协议通信（对 GPU CAGRA 等重依赖天然合适）——协议定义随 M3 细化，接口预留 `RemoteIndex` 实现位。

---

## 2. HNSW 向量索引（自研核心）`[M0]`

对标痛点：ES 每 segment 一棵图 + merge 重建图（概要设计 §2.3.2 / §6.1）。

### 2.1 图存储布局：CSR + mmap `[M0]`

```
seg_xxx/vec.idx/
├── hnsw.graph    # CSR 邻接表（mmap 只读）
│    neighbors: []int32    # 全部邻接点 id 顺序展开（节点内按层分段）
│    offsets:   []int32    # node_id*MAX_LEVEL + level → neighbors 起始偏移
├── hnsw.levels  # []uint8  每节点层级
├── entry.bin    # 入口点 node_id + 全局 max_level（Cross-Link 路由层指针，§2.4.3）
└── quant.state  # 量化状态（§2.5）
```

- 节点 id = 段内 RowID（零转换）；
- `MAX_LEVEL = 32`；层内度上限：`M=16`（默认），第 0 层 `M*2=32`；
- **零指针布局**：三个扁平 `[]int32/[]uint8`（经 `unsafe.Slice` 挂 mmap），GC 成本 O(3)（00 §3.1）；
- 向量本体在 `vec_{field_id}.bin`（01 §4.3），图只存 id——距离计算按 id 回表（mmap page cache 命中）。

### 2.2 参数与距离度量 `[M0]`

| 参数 | 默认 | 说明 |
|------|------|------|
| M | 16 | 每层最大邻接 |
| ef_construction | 200 | 构建搜索宽度 |
| ef_search | 64（可按查询覆盖） | 查询搜索宽度 |

距离度量（mapping `similarity` 决定，构建期定死）：

| similarity | 内核 | 预处理 |
|---|---|---|
| cosine | dot（归一化后） | **构建期归一化并落盘归一化副本**，查询向量同变换（一次归一化，终生 dot） |
| dot_product | dot | 无（可负分，TopK 语义保留） |
| l2_norm | L2 | 无 |
| max_inner_product | dot | 无（MIPS，v1 直接 ANN，弱保证记录在兼容差异清单） |

### 2.3 SIMD 距离内核 `[M0]`

```go
// internal/simd/dist.go —— 函数指针 init 期按 CPU 特性绑定
// 纯 Go 参考实现 + avo 生成实现（amd64 AVX2+FMA / arm64 NEON）双份，
// 单测强制对齐（00 §3.2）。
var DotF32 func(a, b []float32) float32
var L2F32  func(a, b []float32) float32
// 批量版本：一次跑 16 个候选 vs 1 个查询向量（缓存友好，HNSW 遍历主热点）
var DotF32Batch16 func(q []float32, cands []float32, dims int, out []float32)
```

构建/查询的邻接展开循环内使用 `Batch16`；SQ8 量化后对应 `DotI8F32`（int8 × float32 混合内核）。

### 2.4 Cross-Link Merge（核心创新）`[M0 基础 / M2 全量]`

#### 2.4.1 问题定义

ES 痛点：两段合并 = 在 merge 线程全量重建大图（索引耗时 +9%~26%，概要设计 §2.3.2）。UniSearch 目标：合并复杂度从 O((N1+N2)·logN) 全量构建降为 **O(N_small × log N_big)** 增量插入，且**段间查询不需要遍历所有段图**。

#### 2.4.2 段间 Cross-Link（查询侧）`[M0]`

同一 shard 的多个段图通过**顶层邻接**互连（概要设计 §6.1 图示）：

- 新段 flush 后，后台任务 `crosslink(segNew, segNeighbors[])`：取 segNew 顶层入口节点集 `E_new`（≈ 前 `2×√N` 个高度数节点）与每邻段顶层节点集 `E_nb`，做**互近邻连接**（mutual k-NN, k=4）；
- 跨段邻接存储：各段 `entry.bin` 末尾的 `crosslinks: []int32`（对端 `SegmentID<<32|node_id` 编码）；
- 查询协议（03 §3.4 ANN 算子消费）：
  1. 路由层（shard 级，驻内存）从各段 entry 集中选**离查询向量最近的入口**（段 footer 质心对比，O(#segments)）；
  2. 段内贪心下降至 layer 1；
  3. 顶层遍历时可经 crosslink **跳段**（对端节点入候选堆，等效单图）；
  4. 全局 TopK 归并。
- 复杂度：连接构建每段 O(√N × √N × dims) 距离计算（√N 量级 ≈ 5000 万段仅 7000 节点）。

#### 2.4.3 合并算法（Cross-Link Merge）`[M2]`

两段 A（大，N_A）+ B（小，N_B）合并为 C，**不重建 A 的图**：

```
mergeHNSW(A, B):
  C = A 的 CSR 拷贝（写时复制展开为可变结构）
  for each node b in B（按层级从高到低排序）:
      // HNSW 标准插入流程，但插入目标是 C（含 A 的既有邻接）
      ep  = C 的入口（或 B 中已插入节点里最近的）
      idx = greedy descent in C from ep to layer(b)+1
      for l in [layer(b) .. 0]:
          W = searchLayer(q=b.vec, ep=idx, ef=max(ef_construction, M), layer=l, G=C)
          neighbors = selectNeighborsHeuristic(W, M)   // 启发式剪枝（保多样性）
          C.addEdges(b, neighbors, l)                  // 双向边，溢出走剪枝
      入口更新: 若 layer(b) > C.maxLevel → C.entry = b
  C.crosslinks = A.crosslinks ∪ B.crosslinks（去重，重定向到 C）
  return C   // 原子切换前，B 图保留为回滚快照（01 §7.2）
```

- 复杂度：`N_B × (ef_construction 局部搜索 × log N_A)` —— 与全量构建 `(N_A+N_B)` 线性因子相比，大段越大收益越大；
- **召回率保障**：小段节点重插入等价于「这些点在 C 上重新执行了一次标准 HNSW insert」，与全量构建的差异仅在于 A 的既有邻接未因 B 的加入而调整——ann-benchmarks 上持续验证（[06 §5](06-quality-and-observability.md)），回滚快照兜底；
- M0 阶段简化：合并时直接全量重建（数据量小可接受），但**接口与文件格式按增量设计**，M2 切换无格式迁移。

#### 2.4.4 删除节点处理

Delete Bitmap 命中的行（01 §6）在图遍历中**运行时跳过**（候选入堆前查位图）；合并时物理剪除（CSR 紧凑化）。不做在线图修补（复杂度不可控）。

### 2.5 量化分层 `[M0 SQ8 / M2 RaBitQ+DiskANN]`

| 层级 | 格式 | 内存 | 召回目标 | 触发 |
|------|------|------|---------|------|
| 热 | float32 原始 | 4B/dim | 100% | 新鲜段 < 24h |
| 温 | **SQ8**（M0 默认） | 1B/dim | ≈98% @ ef64 | 默认（构建即量化，原始数据仍在 `vec_*.bin`） |
| 温+ | **RaBitQ 1-bit**（M2） | ~0.125B/dim | ≈95% + 精排回表 | 段龄/读写比（01 §9） |
| 冷 | **DiskANN**（M2） | PQ 码本+磁盘图 | 按配置 | ILM |

**SQ8**（标量量化）：
- 训练：段内该向量列的逐维 min/max（footer `column_stats` 已有，零额外扫描）；
- 存储：`quant.state` 存 min/max 数组，量化数据追加在 `vec_*.bin` 的 SQ8 区；
- 查询：查询向量同维量化 → int8 点积内核 → TopK 后**回表 float32 精排**（mmap 原始列）；
- 召回补偿：`ef_search` 自动 ×1.5（量化召回损失的默认补偿，[概要设计 §11] 内存目标 ≤3.5GB/千万 768d 的依据）。

**RaBitQ**（M2，设计要点）：随机正交旋转矩阵 R（段级共享，落 `quant.state`）→ 逐向量 1-bit 符号化 + 范数标定 → 距离上界估计排序 → 精排回表。

### 2.6 标量过滤 × 向量统一规划器 `[M0]`

概要设计 §6.1 的三档策略，决策点在**查询计划期**（03 §2 优化器调用本节 API）：

```go
// internal/index/hnsw/filter.go
type FilterPlan struct {
    Strategy  FilterStrategy  // PreFilter | PostFilter | Iterative
    CandFactor int            // post-filter 过采样倍数（默认 5×k）
    Budget    SearchBudget    // ef 扩展上限
}

// 选择率估算：zone map（01 §4.2）+ 位图索引基数 + BKD 统计
func PlanVectorFilter(sel EstimatedSelectivity, k int) FilterPlan
```

| 档位 | 条件 | 执行 |
|------|------|------|
| pre-filter | 选择率 < 10% | 标量索引求交得候选位图 → 图遍历仅入候选堆中位图命中的节点（受限遍历，对标 Milvus bitmask） |
| post-filter | 选择率 > 30% | ANN 取 `5×k`（ef 同步扩）→ 过滤 → 不足 k 自动 `ef×2` 重试（≤2 次） |
| 迭代 pre-filter | 10%~30% | WAND 剪枝与图遍历交替（词项型过滤） |

选择率估算来源（按优先级）：位图索引精确基数 → BKD/zone map 区间估算 → mapping 静态基数提示（`cardinality_hint`）→ 保守默认 post-filter。

---

## 3. 倒排索引 `[M0]`

### 3.1 词典演进 `[M0 数组 → M1 FST]`

**M0：排序数组 + 二分**

```
seg_xxx/inv/terms.sorted
  ┌─────────────┬──────────────┬────────────────┐
  │ termCount   │ offsets[]    │ termsBlob(zstd)│
  │ u32         │ u32[]        │ 拼接 + 前缀共享 │
  └─────────────┴──────────────┴────────────────┘
  查找：二分 O(log n)，1 亿词项 ≈ 27 次比较，mmap 冷启动友好
```

**M1：自研 FST**（替换数组，接口不变 `TermDict`）：

```go
// internal/index/inverted/fst.go
type TermDict interface {
    Get(term []byte) (TermID, bool)
    Fingerprint(term []byte) uint64   // min-hash 用于跨段词典比较（§3.4 归并加速）
    Iterate(prefix []byte) TermIterator   // prefix/regexp/wildcard 遍历
}
```

FST 设计要点（参考 Lucene/tantivy，Go 生态无生产级实现故自研，00 §2.2）：
- 字节序构建，输出（TermID）共享前缀最大化压缩；只读 mmap；
- 构建流式：term 按 sorted 顺序喂入（MemTable 冻结表天然有序）；
- fuzzy 查询（`fuzziness` DSL）：FST 上 Levenshtein 自动机遍历（自动机状态数上限 32，防状态爆炸——对齐 ES 限制）。

### 3.2 Posting List `[M0]`

```
seg_xxx/inv/post.{field}.bin
  TermID → [docFreq(u32) | blockNav | blocks...]
  block（每 512 docid）:
    docids: roaring 容器（array/bitmap 自适应）
    tf:     FOR 压缩 u8[]（词频，饱和上限 255）
    fldLen bucket: u16（字段长度分桶，BM25 的 B 项）
  位置块 pos.{field}.bin（phrase/highlight 字段才建）:
    每 docid: positions[] delta+bitpack; offsets[]（高亮：start/end u16，超长截断）
```

- BM25 打分统计按段维护：`avgFieldLen`、`docCount`（段 footer）+ `docFreq`（posting 头）——跨段 IDF 合并协议见 03 §7；
- `norms` 不单独存（fldLen bucket 即其来源，打分时内联展开）。

### 3.3 BM25 / BM25F 打分 `[M0]`

标准 BM25（k1=1.2, b=0.75，字段级可调）：

```
score(q,d) = Σ_t IDF(t) · (tf · (k1+1)) / (tf + k1·(1-b+b·fldLen/avgFL))
IDF(t) = ln(1 + (N - df + 0.5)/(df + 0.5))     // ES 8.x 同式
```

BM25F（multi_match 跨字段加权，对齐 Weaviate hybrid 行为）：字段权重 `fieldBoost` 折入 `tf'` 与 `fldLen'` 的线性组合，公式实现于打分算子（03 §3.4），统计来源同上。

### 3.4 WAND / Block-Max WAND `[M1]`

- 经典 WAND：词项 posting 游标 + 上界分数（`maxTF × IDF`）堆剪枝；
- Block-Max 增强：块内最大 tf 上界（posting 块头顺手记录）进一步收紧；
- 触发：纯词项查询（无 filter）且 `track_total_hits=false` + TopK 排序——纯 BM25 长尾查询 P99 <80ms 目标（概要设计 §11）的主要抓手；
- 跨段：段间以「段级 maxScore 上界」（构建期 footer 记录）再做一层剪枝。

### 3.5 高亮 offsets `[M1]`

posting `pos.*` 的 offsets 块（§3.2）为数据源；unified highlighter 策略：TopK 文档 → 按分析器重切分原文 → 匹配词项 offsets → 片段评分（density × coverage）选 top fragments。规格与 ES `highlight` 参数对齐（04 §8）。

---

## 4. 分析器 `[M0]`

```go
// internal/index/analyzer/analyzer.go
type Token struct {
    Term   string  // 归一化后的词项
    Start  int     // 原文偏移（高亮）
    End    int
    Pos    int     // 位置（phrase）
    Type   string  // "<ALPHANUM>" 等（与 ES _analyze 输出对齐）
}

type Analyzer interface {
    Analyze(text []byte) []Token
}
```

内置清单（对齐概要设计 §6.2 的 20+，分阶段）：

| 分析器 | 实现 | 里程碑 |
|---|---|---|
| standard | Unicode 文本分割（`golang.org/x/text/segment` UAX#29）+ asciifolding + lowercase | M0 |
| simple / whitespace | 正则分词 + lowercase | M0 |
| chinese（ik/jieba 语义） | **gse**（HMM 模式）+ 停用词；ik 兼容模式 = gse + 自定义词典（词典格式转换工具入 uniadm） | M0 |
| ngram / edge_ngram | 定长滑窗（min/max 参数） | M0 |
| keyword（不分词） | 恒等 | M0 |
| lowercase / stop / trim / asciifolding / reverse | token filter 形态，可组合 | M0 |
| pattern | 正则 | M1 |
| path / email / url | 层级/边界切分 | M1 |
| kuromoji（日） | kakasi 类纯 Go（评估 `ikawaha/kakasi`） | M1 |
| 各语言 minimal（德/法/西/俄/泰…） | snowball 算法移植（`snowball` stemmer 纯 Go 有现成 `kljensen/snowball`，允许引入） | M1 |
| 自定义 | `settings.analysis` 组合 tokenizer + filter 链（04 §4.4） | M1 |

- 词典热加载：gse 词典文件 `mtime` 轮询（5s）+ SIGHUP 立即重载（ES `_reload_search_analyzers` 兼容 API 挂接，04 §8）；
- 分析器实例 per-index 缓存（构建期 + 查询期同一实例，保证 index/search analyzer 一致性语义）。

---

## 5. 稀疏向量 `[M1]`

`sparse_vector` 字段（概要设计 §6.2，本系统一等公民，ES 为废弃实验态）：

- **存储复用倒排**：`{dim: weight}` 张量 → 词典 term = `field\x00dim`（uint32 编码），posting 的 tf 槽位存 **weight × 2^8 量化 u16**（保留 1/256 精度，分数差 < 0.4%，[设计估算]）；
- 打分：查询稀疏向量同样编码为「带权词项集」→ 倒排求交 → `score = Σ w_q · w_d`（内积语义，对标 SPLADE 检索）；
- 与稠密联合：Fusion 算子（03 §5）将稀疏路输出与 HNSW 路、BM25 路同归一融合；
- KV 张量格式与模型解耦（BGE-M3 → 后继模型可平滑，[概要设计 §14]）。

---

## 6. 标量索引 `[M0]`

### 6.1 BKD 树（数值/日期/ip/geo_point）

对标 Lucene BKD（概要设计 §8.5 同构设计）：

- 维度：1D（long/double/date/ip 定点化）与 2D（geo_point interleave 定点）；
- 构建：递归中位数切分（按维度轮换），**leaf = 1024 点**；leaf 内 1D 情形退化为排序数组 + 二分（Lucene 同优化）；
- 存储：`bkd_{field_id}.bin`：树节点（min/max 边界框）+ leaf 指针 + docid 列（roaring 压缩）；
- 查询：范围/geo 距离 → 边界框裁剪下推 → 候选 docid 位图 → 交给过滤算子（03 §3.4）或 pre-filter 规划器（§2.6）；
- geo 距离排序：2D 距离在 leaf 精算，`_geo_distance sort` 直接复用（04 §8）。

### 6.2 Zone Map

01 §4.2 已定义（块级 min/max），本引擎不重复建设——**Scan 算子跳块**的唯一数据源（03 §2）。

### 6.3 位图索引（低基数 keyword）

- 启用规则：flush 时统计块基数（footer `column_stats`），**全局 cardinality < 4096 自动启用**（概要设计 §6.3）；
- 格式：`bitmap_{field_id}.bin` = 值字典 + `map[valueID]roaring.Bitmap`（行号）；
- 消费方：term/terms 过滤（等值求交，替代扫描）+ 选择率估算（§2.6 的首选来源）。

### 6.4 布隆过滤器（可选）

- mapping 参数 `bloom_filter: true` 启用（高基数 keyword/uuid 等值查询）；
- 每 Block 一个 bloom（1024 条目，fpp 1%），块级预过滤减少解码。

### 6.5 主键索引（`_id`）

01 §4.4 定义（字典 + 开放寻址哈希）；本节补充：GET/UPDATE 定位流程 = `id.hash` 命中 → DeleteBitmap 检查 → 返回最新版本（01 §6）。

---

## 7. Completion Suggester（FST 前缀）`[M1]`

- 独立小 FST：`suggest.{field}.fst`，输入 = suggest 文本（分析后拼接）+ weight（文档权重/频率）+ payload（docid）；
- 前缀查询：FST 前缀遍历 + top-weight 剪枝；
- 与 Lucene 同构（概要设计 §8.3 对齐），M1 随 FST 基建一并交付（§3.1）。

---

## 8. 里程碑汇总

| 能力 | M0 | M1 | M2 | M3 |
|------|----|----|----|----|
| HNSW（CSR + SIMD + SQ8 + 过滤规划器） | ✅ | | | |
| Cross-Link 段间跳段查询 | ✅（单机多段） | | ✅（分布式全量） | |
| Cross-Link Merge（增量合并） | 接口预留（格式兼容） | | ✅ | |
| RaBitQ / DiskANN 冷层 | | | ✅ | |
| 倒排（数组词典 + roaring + BM25） | ✅ | | | |
| FST 词典 / WAND / 高亮 / suggester | | ✅ | | |
| 稀疏向量 | | ✅ | | |
| 分析器（standard/gse/ngram 族 8 个） | ✅ | ✅ 补齐 20+ | | |
| GPU CAGRA / 插件 SDK（sidecar） | | | | ✅ |

验收基准（ann-benchmarks 接入与召回率门禁）见 [06-quality-and-observability.md](06-quality-and-observability.md) §5。

---

*本文件与 [03-query-engine.md](03-query-engine.md) 的契约：查询引擎通过 `LoadedIndex.Search(ctx, Query, budget)` 消费索引，`SearchResult` 返回 `(docid, score)` 对与必要统计（BM25 maxScore 等）；索引引擎不理解 DSL。修订 Cross-Link 格式需同步 03 §3.4 ANN 算子与 [05-distributed.md](05-distributed.md) §3（分裂段的 crosslinks 重定向）。*
