# Proposal: 从 Qdrant 引入高级搜索与存储功能到 UniSearch

## 1. 背景与动机 (Background & Motivation)

### 1.1 当前问题

UniSearch 当前已具备融合全文检索与向量搜索的核心能力（HNSW + CSR 邻接 + 三级量化 SQ8/RaBitQ/PQ + BM25/WAND + RRF/Weighted/RelativeScore 融合 + Raft 共识 + mmap + 47 种聚合），但与 Qdrant（Rust 实现的向量搜索引擎）相比，在以下方面存在差距：

- **稀疏向量索引**：段格式已定义 `sparse_vector` 类型，但段级索引未实现（仅 memtable 暴力扫描），无法高效支持 SPLADE/BGE-M3 等神经稀疏检索
- **量化方案**：已有 RaBitQ（1-bit），但缺少 TurboQuant 这种 Hadamard 旋转 + Lloyd-Max 码本的更先进方案
- **搜索模式**：缺少 Recommend/Discover/Context 等基于正负例的推荐搜索、MMR 多样性搜索、Formula 公式重排序
- **多向量支持**：不支持 ColBERT/COIL 等 late-interaction 检索模型的 MaxSim 评分
- **查询编排**：缺少统一 Query API 管道（嵌套 prefetch + 融合 + 重评分）
- **分布式同步**：副本同步依赖全量快照，缺少 WAL Delta 增量同步
- **企业级功能**：缺少 RBAC 访问控制、集合级配额、审计日志

### 1.2 用户期望

通过深入分析 Qdrant 1.19.2 的代码库，识别出 16 个可引入 UniSearch 的功能，按优先级分为 P0/P1/P2 三个阶段。使 UniSearch 具备新一代检索模型（SPLADE/ColBERT）的基础设施，同时保持 ES 8.x REST API 兼容。

## 2. 目标 (Objective)

- 使 UniSearch 具备 SPLADE/ColBERT 等新一代检索模型的基础设施
- 统一 BM25 全文检索与神经稀疏检索在同一引擎内
- 增强 Query API 表达能力（管道式查询、Recommend、MMR、Formula 重排序）
- 提升量化精度与内存效率（TurboQuant、Float16 存储）
- 增强分布式副本同步效率（WAL Delta 增量同步）
- 补齐企业级安全与配额能力（RBAC、审计日志）

## 3. 方案设计 (Approach)

### 3.1 设计原则

1. **atomic.Pointer 量化分层**：新量化方案（TQ、Float16）必须通过 atomic.Pointer 实现热切换，与现有 SQ8/RaBitQ/PQ 一致
2. **索引注册表模式**：新索引类型通过 `index.RegisterIndex` 注册，在 `fieldRoutes` 声明式路由表声明
3. **Volcano 算子管道**：所有后处理步骤（MMR、Formula、Pipeline stage）实现 `exec.Operator` 接口
4. **Fusion 接口模式**：DBSF 实现 `fusion.Fusion` 接口，通过 `NewFusion` 工厂注册
5. **段不可变原则**：所有段级改动通过 flush/merge 生命周期驱动
6. **SIMD 分发预留**：新距离计算内核在 `internal/simd/dist.go` 声明函数指针，初期 pure Go，后续汇编接入
7. **ES 兼容优先**：所有新功能通过 ES DSL 扩展字段暴露，不破坏现有查询解析

### 3.2 依赖关系与实施顺序

```
P2 先行（支撑组件）
└── F11 P-square → 支撑 F6 TurboQuant

P0 阶段（并行实施，3-4 个月）
├── F1 稀疏向量倒排索引 [独立]
├── F2 DBSF 融合 [独立]
├── F3 MMR 多样性搜索 [独立]
├── F4 Float16 存储 [独立]
└── F5 Query API 统一管道 [独立] ← P1 功能的编排基础

P1 阶段（部分依赖 P0，4-6 个月）
├── F6 TurboQuant [依赖 F11] ← 高复杂度
├── F7 Multi-vector [独立] ← 高复杂度，段格式升级 v2
├── F8 Recommend/Discover [依赖 F5] ← 需要管道编排
├── F9 Formula 重排序 [依赖 F5] ← 需要管道编排
└── F10 WAL Delta [独立]

P2 阶段（并行实施，2-3 个月）
├── F11 P-square [先行，支撑 F6]
├── F12 HNSW 图修复 [独立]
├── F13 多语言分词 [独立]
├── F14 BM25 嵌入生成 [依赖 F1]
├── F15 RBAC [独立]
└── F16 集合配额+审计 [独立]
```

## 4. 变更范围 (Impact)

### P0 阶段：核心搜索增强（5 个功能，低中复杂度）

#### F1: 稀疏向量倒排索引

- **描述**：基于倒排索引实现稀疏向量搜索，支持 SPLADE/BGE-M3 模型输出。Postings 中存储维度 ID 作为 term、float 权重替代整数 TF，支持权重精度量化（f32/f16/u8）。WAND 剪枝使检索从 O(N) 暴力扫描降为亚线性。
- **Qdrant 参考**：`lib/sparse/` — 倒排索引 + posting list + `SearchContext` 剪枝批量搜索 + 权重量化
- **UniSearch 适配**：
  - `internal/index/registry.go`：注册 `KindSparseInverted` 索引类型，为 `TypeSparseVector` 添加 `fieldRoutes` 路由
  - `internal/index/inverted/sparse_index.go`（新增）：加权 postings 变体，复用 FST 词典，新增 `WeightedPostingList`（float 权重）+ `SparseWANDScorer`（基于 per-dim 最大权重剪枝）
  - `internal/storage/segment/wire.go`：新增 `sparse.idx` 索引文件格式
  - `internal/embedded/planner.go`：`buildSparseVector` 从暴力扫描升级为倒排索引 WAND 搜索
  - `internal/storage/flush.go`：flush 时对 sparse_vector 列触发稀疏倒排索引构建
- **复杂度**：中
- **ES 兼容**：通过 ES `dense_vector`/`sparse_vector` mapping 扩展字段配置

#### F2: DBSF 融合（Distribution-Based Score Fusion）

- **描述**：使用 Welford 在线方差归一化的分布感知评分融合，比现有 min-max 归一化更鲁棒，不受极端离群值影响。
- **Qdrant 参考**：`lib/segment/src/common/score_fusion.rs` — Welford 均值/方差 + z-score 归一化 + 加权求和
- **UniSearch 适配**：
  - `internal/engine/fusion/fusion.go`：新增 `DBSFFusion` 结构体实现 `Fusion` 接口，Welford 单遍计算均值/方差，z-score 归一化后加权求和
  - `internal/engine/dsl/ast.go`：`RankMethod` 新增 `RankMethodDBSF`
  - `internal/engine/dsl/parser.go`：解析 `dbsf` 融合方法
  - `fusion.go` 的 `NewFusion` 工厂添加 `dbsf` 分支
- **复杂度**：低
- **ES 兼容**：通过 ES `_search` 请求体中 `rank` 扩展字段指定融合方法

#### F3: MMR 多样性搜索（Maximal Marginal Relevance）

- **描述**：后处理重排序，平衡查询相关性与结果多样性，λ 参数控制平衡点。对 RAG 场景避免重复上下文有直接价值。
- **Qdrant 参考**：`lib/shard/src/query/mmr/` — `mmr_from_points_with_vector`
- **UniSearch 适配**：
  - `internal/engine/rerank/mmr.go`（新增）：MMR 后处理函数，复用 `simd.DotF32` 计算候选间相似度
  - `internal/engine/exec/operators.go`：新增 `MMROperator`（pull-based 后处理算子）
  - `internal/engine/dsl/ast.go` + `parser.go`：搜索请求体新增 `mmr` 字段（lambda 参数）
  - `internal/engine/plan/logical.go`：新增 `MMR` 计划节点
  - `internal/embedded/translator.go` + `planner.go`：翻译 MMR 配置并构建管道
- **复杂度**：低
- **ES 兼容**：通过 `_search` 请求体扩展字段 `mmr: { enabled: true, lambda: 0.5 }`

#### F4: Float16 向量存储

- **描述**：使用 IEEE 754 半精度浮点存储向量，节省 50% 内存，精度损失 <1% recall。段格式已预留 `element_type` 字段。
- **Qdrant 参考**：`VectorStorageDatatype::Float16`
- **UniSearch 适配**：
  - `internal/meta/mapping.go`：`VectorElement` 新增 `ElementFloat16 = "float16"`
  - `internal/types/f16.go`（新增）：f16↔f32 转换函数（IEEE 754 half precision 位操作）
  - `internal/index/hnsw/hnsw.go`：`Graph` 结构新增 `vecsF16 []uint16` 字段，`Vector(int32)` 按 element type 返回 f32 视图
  - `internal/simd/dist.go`：新增 `DotF16F32` 函数指针（pure Go 实现，后续汇编接入）
  - `internal/storage/segment/builder.go`：新增 f16 列编码路径
  - `internal/storage/flush.go` + `column_provider.go`：根据 `FieldDef.Element` 选择 f16 存储路径
  - `internal/index/hnsw/quant.go`：SQ8/RaBitQ/PQ 训练时从 f16 读取原始向量
- **复杂度**：低-中
- **ES 兼容**：通过 `dense_vector` mapping 的 `element_type: float16` 扩展字段配置

#### F5: Query API 统一查询管道

- **描述**：嵌套 Prefetch + 融合 + 重评分的管道式查询，统一所有检索类型（向量/BM25/稀疏向量）的编排。是 Recommend/Discover、Formula Rescoring 等功能的编排基础。
- **Qdrant 参考**：`lib/collection/src/operations/universal_query/` + `lib/shard/src/query/planned_query.rs`
- **UniSearch 适配**：
  - `internal/engine/dsl/ast.go`：新增 `PipelineQuery` 结构体（`Prefetches []Query` + `Fusion RankSpec` + `Rescore []RescoreSpec`）
  - `internal/engine/dsl/parser.go`：新增 `parsePipelineQuery` 函数
  - `internal/engine/plan/logical.go`：新增 `Pipeline` 计划节点（`Prefetches []LogicalPlan` + `FusionNode *Fusion` + `RescoreNodes []LogicalPlan`）
  - `internal/engine/exec/operators.go`：新增 `PipelineOperator`（驱动多个子查询 → 融合 → 重评分）
  - `internal/embedded/translator.go`：新增 `translatePipeline` 函数
  - `internal/embedded/planner.go`：新增 `buildPipeline` 函数
  - 现有 `HybridQuery` 作为 Pipeline 的简化语法糖保留向后兼容
- **复杂度**：中
- **ES 兼容**：通过 `_search` 请求体扩展字段 `_pipeline: { prefetches: [...], fusion: {...}, rescore: [...] }`

### P1 阶段：高级检索能力（5 个功能，中高复杂度）

#### F6: TurboQuant (TQ) 量化

- **描述**：Hadamard 旋转 + Lloyd-Max 码本 + 误差校正的先进量化方案，支持 1/1.5/2/4-bit，在相同比特率下比 RaBitQ 提升 3-5% recall。TQ+ 模式带 per-coordinate 误差校正。
- **Qdrant 参考**：`lib/quantization/src/turboquant/` — Hadamard 旋转 + Lloyd-Max 码本 + ErrorCorrection + 专用 SIMD 内核
- **UniSearch 适配**：
  - `internal/index/hnsw/turboquant.go`（新增）：`TQState` 结构体 + `tqState atomic.Pointer[TQState]`，遵循现有 SQ8/RaBitQ/PQ 的 atomic.Pointer 热切换模式
  - `internal/index/hnsw/hadamard.go`（新增）：快速 Walsh-Hadamard 变换（递归蝴蝶网络，O(n log n)），维度补齐到 2 的幂
  - `internal/index/hnsw/lloyd_max.go`（新增）：Lloyd-Max 最优标量量化器训练（迭代 10-20 次收敛）
  - `internal/index/hnsw/search.go`：新增 `searchTQ` 方法（查询 Hadamard 旋转 → 码本查表 → widen beam 1.5× → float32 rerank）
  - `internal/simd/dist.go`：新增 sub-byte 解码 + 距离计算函数指针（pure Go 初期，汇编后续接入 `dispatch_amd64.go`）
  - `internal/index/hnsw/quant.go`：`Index.Quantize` 新增 `QuantTQ` 分支（指定位宽 1/1.5/2/4-bit）
  - `internal/storage/segment/wire.go`：`quant.state` 文件格式新增 TQ tier 序列化布局
- **依赖**：F11（P-square 分位数估计）用于 Lloyd-Max 码本初始化加速
- **复杂度**：高
- **ES 兼容**：通过 `dense_vector` mapping 的 `quantization: { type: "turboquant", bits: 2 }` 扩展字段配置

#### F7: Multi-vector / ColBERT MaxSim

- **描述**：每个文档存储多个 token 级向量，检索时通过 MaxSim 后期交互评分（sum_a(max_b(sim(a, b)))）。支持 ColBERT/COIL 等 late-interaction 检索模型。
- **Qdrant 参考**：`lib/segment/src/vector_storage/multi_dense/` + `MultiVectorComparator::MaxSim`
- **UniSearch 适配**：
  - `internal/meta/mapping.go`：`FieldDef` 新增 `MultiVector bool` + `TokenCount int`，或新增 `TypeMultiVector FieldType = "multivector"`
  - `internal/storage/segment/wire.go`：新增 `ftMultiVec` 类型码，`FormatVersion` 升级到 2（保留 v1 向后兼容）
  - `internal/storage/segment/builder.go`：multi-vector 列编码（每行 `token_count * dims` 个 float32，行内紧凑存储 + offsets）
  - `internal/index/registry.go`：`ColumnProvider` 接口新增 `ForEachMultiVector(fn func(rowID int32, vecs [][]float32) error) error`
  - `internal/index/hnsw/hnsw.go`：`Graph` 结构新增 `multiVecsFlat []float32` + `multiOffsets []int32`，`Vector(int32)` 扩展为 `MultiVector(int32) [][]float32`
  - `internal/simd/dist.go`：新增 `MaxSimF32` 函数指针（O(q_tokens * d_tokens * dims)）
  - `internal/index/hnsw/core.go`：`distFunc` 新增 MaxSim 距离变体
  - `internal/index/hnsw/search.go`：新增 `searchMultiVector` 方法
  - `internal/engine/dsl/ast.go`：扩展 `KnnQuery` 或新增 `MultiVectorQuery`（携带 `[][]float32`）
  - `internal/embedded/translator.go` + `planner.go`：multi-vector 查询翻译和计划构建
- **复杂度**：高
- **ES 兼容**：通过 `dense_vector` mapping 的 `multivector: true` 扩展字段配置；查询通过 `_search` 扩展字段

#### F8: Recommend/Discover/Context 搜索

- **描述**：基于正例/负例的推荐搜索。Recommend：正例 - 负例向量运算生成合成查询向量。Discover：约束搜索空间。Context：上下文调整查询。
- **Qdrant 参考**：`lib/shard/src/query/query_enum.rs` — `RecommendBestScore`/`RecommendSumScores`/`Discover`/`Context`
- **UniSearch 适配**：
  - `internal/engine/dsl/ast.go`：新增 `RecommendQuery`/`DiscoverQuery` 结构体（`Positive []string` + `Negative []string` + `PositiveVectors` + `NegativeVectors` + `Strategy`）
  - `internal/engine/dsl/parser.go`：解析 `recommend`/`discover` 子句
  - `internal/embedded/engine.go`：新增 `fetchVectorsByIDs` 工具函数（通过文档 ID 获取向量）
  - `internal/engine/vector_arith.go`（新增）：向量平均、加权求和、差运算（`query = avg(positive) - lambda * avg(negative)`）
  - `internal/engine/plan/logical.go`：新增 `Recommend` 计划节点（包装 ANN 查询 + 合成查询向量）
  - `internal/embedded/translator.go`：`translateRecommend` 获取正负例向量 → 计算合成向量 → 复用 `translateKnn` 路径
- **复杂度**：中
- **ES 兼容**：通过 `_search` 请求体扩展字段 `recommend: { positive: [...], negative: [...], strategy: "average" }`

#### F9: Formula 公式重排序

- **描述**：基于 payload 字段值的动态评分公式表达式，支持算术运算、常用数学函数、字段引用、条件表达式。对电商搜索和个性化排序不可或缺。
- **Qdrant 参考**：`lib/shard/src/query/formula.rs` — 表达式引擎（常量、变量、条件、GeoDistance、DateTime）
- **UniSearch 适配**：
  - `internal/engine/formula/`（新增子包）：表达式解析器 + 求值器，支持四则运算、`log`/`exp`/`sqrt`/`abs`/`geo_distance`、字段引用（`payload.field_name`）、条件表达式（`if/else`）
  - `internal/engine/exec/operators.go`：新增 `FormulaRescoreOperator`（TopN 之后后处理，读取 payload 字段 → 求值 → 更新分数 → 重新排序）
  - `internal/engine/dsl/ast.go`：新增 `FormulaRescore` 结构体（表达式字符串 + 权重 + scope）
  - `internal/engine/dsl/parser.go`：解析 `rescore` 子句中的公式表达式
  - `internal/engine/plan/logical.go`：新增 `FormulaRescore` 计划节点
  - 作为 Pipeline 查询管道（F5）的一个 stage，在融合后执行
- **复杂度**：中
- **ES 兼容**：通过 `_search` 请求体 `rescore` 扩展，ES 本身有 `function_score`/`script_score`，Formula 作为增强替代

#### F10: WAL Delta 增量同步

- **描述**：基于 LSN/seq_no 的增量日志同步，副本只同步自上次同步点之后的 WAL 记录，避免全量快照传输。对大段（GB 级）副本恢复从分钟级降到秒级。
- **Qdrant 参考**：`lib/collection/src/shards/transfer/wal_delta.rs` + `clock_set.rs`
- **UniSearch 适配**：
  - `internal/storage/wal/`：新增 `ReadSince(lsn int64) ([]Record, error)` 方法，支持从指定 LSN 扫描日志段
  - `internal/raft/types.go`：`Watermarks` 扩展为 per-replica 水位线
  - `internal/coordinator/` 或 `internal/raft/`：新增 `DeltaSync` 协议（`SyncSince(myLSN)` → leader 返回增量记录 → apply → 更新水位线）
  - `internal/server/grpc/`：新增 `DeltaSync` RPC 方法
  - Fallback 机制：增量同步失败或追不上时自动回退全量快照
  - 复用现有 `SplitCoordinator.replayWAL` hook 的泛化
- **复杂度**：中
- **ES 兼容**：内部分布式机制，不影响 API

### P2 阶段：支撑性增强与企业功能（6 个功能，低中复杂度）

#### F11: P-square 在线分位数估计

- **描述**：P² 算法流式分位数估计，无需存储全部数据。用于 SQ8 量化参数抗离群值、TQ Lloyd-Max 码本初始化加速、DBSF 融合分数分布估计。
- **Qdrant 参考**：`lib/quantization/src/turboquant/quantization.rs` 中的 P-square 使用
- **UniSearch 适配**：
  - `internal/engine/stats/quantile.go`（新增）：P² 算法在线分位数估计器（纯算法，无外部依赖）
  - `internal/index/hnsw/quant.go`：`trainSQ8` 中 per-dim min/max 改为 P1/P99 分位数
  - `internal/engine/fusion/fusion.go`：DBSF 中可选用 P² 替代 Welford 做分数分布估计
- **复杂度**：低
- **依赖关系**：F6（TurboQuant）依赖此功能做码本初始化，建议在 P1 之前或同期实施

#### F12: HNSW 图修复（Healing）

- **描述**：自动检测并修复 HNSW 图中的断连节点和低度数节点，改善 edge case 下的 recall。
- **Qdrant 参考**：`lib/segment/src/index/hnsw_index/graph_layers_healer.rs`
- **UniSearch 适配**：
  - `internal/index/hnsw/healing.go`（新增）：BFS 从 entry point 检测不可达节点 + 低度数节点，暴力 k-NN 搜索找邻居重建连接
  - `internal/storage/merge/scheduler.go`：merge 完成后、写入文件前触发 healing 步骤（段不可变，healing 在 merge/rebuild 阶段执行）
  - 可用于修复 Cross-Link 中的段间连接缺失
- **复杂度**：低

#### F13: 多语言分词器

- **描述**：多语言分词支持，包括 Snowball stemmer（英/法/德/西等欧洲语言）、多语言停用词、日语分词、ASCII folding。纯 Go 实现，无 CGO 依赖。
- **Qdrant 参考**：`lib/segment/src/index/field_index/full_text_index/tokenizers/`
- **UniSearch 适配**：
  - `internal/index/analyzer/tokenizers.go`：新增 CJK 分词器（二元组/字典分词）
  - `internal/index/analyzer/filters.go`：新增 Snowball stemmer filter（纯 Go 移植）
  - `internal/index/analyzer/factory.go`：注册新分词器和 filter，analyzer 名称与 ES 兼容（`kuromoji`→纯 Go 替代、`icu_analyzer`→纯 Go 替代）
- **复杂度**：中

#### F14: BM25 嵌入生成

- **描述**：将 BM25 倒排索引的 term-doc 频率转换为稀疏向量嵌入，使 BM25 检索和 SPLADE 稀疏向量检索统一在同一稀疏向量索引上。
- **Qdrant 参考**：`lib/bm25/` — 独立 BM25 embedding crate（Murmur3 哈希 + TF 权重公式）
- **UniSearch 适配**：
  - `internal/index/inverted/bm25_embedding.go`（新增）：从倒排索引 postings 到稀疏向量格式的转换（dim = termID, weight = BM25 score 或 raw TF）
  - 统一索引路径：文本 → 分词 → BM25 嵌入生成 → 稀疏向量索引构建（复用 F1 稀疏向量倒排索引）
  - `internal/meta/mapping.go`：`FieldDef` 新增 `Bm25Embedding bool` 标记
  - `internal/storage/flush.go`：text 字段启用此选项时，flush 时同时构建倒排索引和 BM25 稀疏向量索引
- **复杂度**：中
- **依赖**：F1（稀疏向量倒排索引）需先落地

#### F15: RBAC 基于角色的访问控制

- **描述**：JWT 认证 + 角色定义（admin/writer/reader）+ 集合级权限检查。
- **Qdrant 参考**：`lib/storage/src/rbac/` — JWT 认证 + 操作权限检查
- **UniSearch 适配**：
  - `internal/auth/`（新增模块）：JWT 签发/验证（`golang-jwt/jwt`，无 CGO）、角色定义、权限检查
  - `internal/server/esapi/router.go`：挂载认证中间件
  - `internal/server/esapi/`：新增 `_security/role`/`_security/user` CRUD handler（兼容 ES security API）
- **复杂度**：中

#### F16: 集合级配额 + 审计日志

- **描述**：集合级配额管理（max_docs/max_storage/write_rate/query_rate）+ 严格模式 + 操作审计日志。
- **Qdrant 参考**：`lib/shard/src/quota/` + `lib/storage/src/audit.rs`
- **UniSearch 适配**：
  - `internal/tenant/tenant.go`：`TenantQuota` 扩展 per-index 配额，令牌桶速率限制
  - `internal/storage/flush.go`：写入配额检查
  - `internal/observability/audit.go`（新增）：审计日志模块，`esapi/router.go` 新增审计中间件
- **复杂度**：中

### 4.1 新增文件

| 文件路径 | 说明 |
|----------|------|
| `internal/index/inverted/sparse_index.go` | F1 稀疏向量倒排索引（加权 postings + SparseWANDScorer） |
| `internal/engine/rerank/mmr.go` | F3 MMR 多样性搜索 |
| `internal/types/f16.go` | F4 Float16↔Float32 转换 |
| `internal/index/hnsw/turboquant.go` | F6 TurboQuant 量化状态 |
| `internal/index/hnsw/hadamard.go` | F6 Walsh-Hadamard 变换 |
| `internal/index/hnsw/lloyd_max.go` | F6 Lloyd-Max 量化器训练 |
| `internal/engine/vector_arith.go` | F8 向量运算（平均/加权/差） |
| `internal/engine/formula/` | F9 表达式解析器 + 求值器 |
| `internal/engine/stats/quantile.go` | F11 P-square 在线分位数估计 |
| `internal/index/hnsw/healing.go` | F12 HNSW 图修复 |
| `internal/index/inverted/bm25_embedding.go` | F14 BM25 嵌入生成 |
| `internal/auth/` | F15 JWT 认证 + RBAC |
| `internal/observability/audit.go` | F16 审计日志 |

### 4.2 修改文件

| 文件路径 | 修改内容 |
|----------|----------|
| `internal/index/registry.go` | F1 稀疏向量索引注册、F7 Multi-vector ColumnProvider |
| `internal/index/hnsw/quant.go` | F6 TurboQuant 量化层、F4 Float16 存储、F11 SQ8 分位数改进 |
| `internal/index/hnsw/hnsw.go` | F4 Float16 Graph 结构、F7 Multi-vector Graph 结构 |
| `internal/index/hnsw/search.go` | F6 TQ 搜索路径、F7 Multi-vector 搜索路径 |
| `internal/engine/fusion/fusion.go` | F2 DBSF 融合 |
| `internal/engine/dsl/ast.go` | F1-F9 所有新查询类型的 AST 节点 |
| `internal/engine/dsl/parser.go` | F1-F9 所有新查询类型的解析逻辑 |
| `internal/engine/plan/logical.go` | F3 MMR、F5 Pipeline、F8 Recommend、F9 Formula 计划节点 |
| `internal/engine/exec/operators.go` | F3 MMR、F5 Pipeline、F9 Formula 算子 |
| `internal/embedded/translator.go` | F1-F9 所有新查询类型的翻译逻辑 |
| `internal/embedded/planner.go` | F1-F9 所有新查询类型的执行管道构建 |
| `internal/storage/segment/wire.go` | F1 sparse.idx 格式、F4 f16 编码、F7 ftMultiVec + v2 |
| `internal/storage/segment/builder.go` | F4 f16 列编码、F7 multi-vector 列编码 |
| `internal/storage/flush.go` | F1 稀疏索引构建、F4 f16 存储路径、F14 BM25 嵌入 |
| `internal/simd/dist.go` | F4 DotF16F32、F6 TQ sub-byte、F7 MaxSimF32 |
| `internal/storage/wal/` | F10 WAL Delta ReadSince |
| `internal/raft/types.go` | F10 per-replica 水位线 |
| `internal/meta/mapping.go` | F4 ElementFloat16、F7 MultiVector、F14 Bm25Embedding |
| `internal/tenant/tenant.go` | F16 集合级配额 |
| `internal/server/esapi/router.go` | F15 RBAC 中间件、F16 审计中间件 |
| `internal/storage/merge/scheduler.go` | F12 merge 后触发 healing |

### 4.3 不涉及

- io_uring 异步 I/O：Go 无标准 io_uring 支持，CGO_ENABLED=0 禁止 C 库调用，Go runtime netpoller 已处理异步 I/O
- GPU 加速索引构建：依赖 Vulkan/CUDA，与 CGO_ENABLED=0 约束冲突
- Qdrant Edge 边缘设备版本：UniSearch 已有 embedded 模式覆盖此场景
- 图链接 Bitpacking 压缩：UniSearch 已有 Elias-Fano 编码，Bitpacking 为同类替代，增益有限
- 动态重分片（scale down）：SplitCoordinator 已支持在线分裂（scale up），合并为全新逻辑但当前需求不强
- 副本状态机 11 种状态：现有 split phase 状态机已覆盖核心场景，过度设计

## 5. 接口定义 (Interface Definition)

### 5.1 ES 兼容 API 扩展字段

所有新功能通过 ES DSL 扩展字段暴露，不破坏现有查询解析：

```
# 稀疏向量 mapping
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

# Float16 向量 mapping
{
  "mappings": {
    "properties": {
      "embedding": {
        "type": "dense_vector",
        "dimensions": 768,
        "element_type": "float16"
      }
    }
  }
}

# TurboQuant 量化 mapping
{
  "mappings": {
    "properties": {
      "embedding": {
        "type": "dense_vector",
        "dimensions": 768,
        "quantization": { "type": "turboquant", "bits": 2 }
      }
    }
  }
}

# Multi-vector mapping
{
  "mappings": {
    "properties": {
      "colbert_embedding": {
        "type": "dense_vector",
        "dimensions": 128,
        "multivector": true
      }
    }
  }
}

# Query API 管道
POST /my-index/_search
{
  "_pipeline": {
    "prefetches": [
      { "knn": { "field": "embedding", "query_vector": [...], "k": 100 } },
      { "query": { "match": { "title": "hello" } } }
    ],
    "fusion": { "method": "rrf", "k": 60 },
    "rescore": [
      { "formula": "score * log(1 + payload.likes)", "weight": 0.3 }
    ]
  }
}

# Recommend 搜索
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

# MMR 多样性搜索
POST /my-index/_search
{
  "knn": { "field": "embedding", "query_vector": [...], "k": 50 },
  "mmr": { "enabled": true, "lambda": 0.5 }
}

# DBSF 融合
POST /my-index/_search
{
  "knn": [...],
  "query": {...},
  "rank": { "method": "dbsf", "weights": [0.7, 0.3] }
}
```

## 6. 稳定性架构 (Stability Architecture)

### 6.1 限流与熔断

- **Rate Limiting**: 复用现有租户级令牌桶（`tenant.AcquireQuery`/`AcquireWrite`），F16 新增集合级令牌桶
- **Circuit Breaking**: WAL Delta 同步失败时自动 fallback 到全量快照

### 6.2 资源隔离与限制

- TurboQuant 量化训练使用后台 worker，限制 CPU 核心数（复用现有 merge scheduler 线程池）
- Multi-vector MaxSim 计算复杂度 O(q*d*dims)，通过 SIMD 加速 + 批量处理控制延迟
- MMR 后处理限制候选集大小（默认 TopK ≤ 500）

### 6.3 灾难恢复

- 段格式 v2 向后兼容 v1：加载时按版本号选择读取路径
- WAL Delta 增量同步失败时自动 fallback 全量快照
- TurboQuant/Float16 量化异常时 fallback 到 float32 精确搜索

## 7. 可测性设计 (Testability Design)

### 7.1 本地手动验证

- Step 1: `make build` 确认纯静态编译通过
- Step 2: 创建 sparse_vector mapping 索引，写入 SPLADE 格式稀疏向量，执行搜索验证召回
- Step 3: 创建 float16 dense_vector 索引，写入向量，搜索验证 recall@k 对比 float32
- Step 4: 使用 `_pipeline` 扩展字段执行多级 prefetch + DBSF 融合 + MMR 重排序
- Step 5: 创建 multi-vector 索引，写入 ColBERT 格式数据，执行 MaxSim 搜索

### 7.2 自动化测试策略

- **Unit Tests**: 每个功能新增 Go 单元测试（量化精度、融合正确性、表达式求值、f16 转换等）
- **Integration Tests**: Python E2E 测试覆盖新 API 扩展字段
- **Regression Tests**: 现有 294 Python E2E 全绿，不引入回归
- **Benchmark Tests**: TQ/Float16 recall@k 基准对比 float32 baseline；DBSF 对比 RRF/Weighted NDCG

## 8. 风险与缓解 (Risks & Mitigation)

| 风险 | 缓解措施 |
|------|---------|
| TurboQuant SIMD 性能不达预期 | pure Go 先保证正确性，汇编 kernel 作为后续独立交付；TQ rerank 机制保证最终精度不受量化影响 |
| Multi-vector 段格式不兼容 | `FormatVersion` 升级到 2，保留 v1 向后兼容；加载时按版本号选择读取路径 |
| 稀疏向量倒排索引与文本倒排索引代码重复 | 抽取共享 postings 基础设施，通过接口参数化（weight 类型、scorer 类型） |
| Query API 管道 DSL 与 ES 兼容冲突 | 管道语法作为 ES 扩展字段（非标准 ES DSL），不破坏现有查询解析 |
| WAL Delta 一致性问题 | 增量同步失败时自动 fallback 全量快照；水位线通过 Raft 共识保证一致 |
| 16 个功能交付周期长 | 按依赖关系分阶段交付，每阶段独立可用；P0 五个功能可并行开发，快速见效 |

## 9. 验收标准 (Acceptance Criteria)

- [ ] **编译**: `make build` 全绿（`CGO_ENABLED=0` 纯静态编译）
- [ ] **质量**: `go build ./... && go test ./... && go vet ./...` 全绿
- [ ] **回归**: 现有 Python E2E 测试全绿（不引入回归）
- [ ] **测试**: 每个功能新增单元测试 + 集成测试
- [ ] **ES 兼容**: ES 兼容 API 覆盖率不降低（现有 Golden 100%）
- [ ] **量化精度**: TQ/Float16 recall@k 基准测试对比 float32 baseline
- [ ] **融合质量**: DBSF 对比 RRF/Weighted 的 NDCG 不低于现有水平
- [ ] **段格式**: 段格式 v2 向后兼容 v1
