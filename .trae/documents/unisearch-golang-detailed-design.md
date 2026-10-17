# UniSearch Golang 详细设计与开发规划 — 实施计划

## Summary

基于 `docs/vector-database-design.md`（概要设计，原 Rust 技术栈），产出 **Go 语言版全量（M0-M3）详细设计文档 + 开发规划文档**，共 8 份纯文档交付（不含代码实现，但包含关键 Go interface/struct 定义、文件格式规格、流程图与里程碑标注）。

产出物：

| # | 文件 | 内容 |
|---|------|------|
| 1 | `docs/go-design/00-overview.md` | 总体设计：Go 技术选型、模块/仓库布局、Go 特有工程策略（GC/SIMD/内存） |
| 2 | `docs/go-design/01-storage-engine.md` | 存储引擎详细设计 |
| 3 | `docs/go-design/02-index-engine.md` | 索引引擎详细设计（HNSW/倒排/BKD/位图/分析器） |
| 4 | `docs/go-design/03-query-engine.md` | 查询引擎详细设计（DSL 解析/计划/向量化执行/融合/聚合） |
| 5 | `docs/go-design/04-es-compat-layer.md` | ES REST/DSL 兼容层详细设计 |
| 6 | `docs/go-design/05-distributed.md` | 分布式架构详细设计（Raft/元数据/分片/多租户） |
| 7 | `docs/go-design/06-quality-and-observability.md` | 可观测性、测试与质量保障设计 |
| 8 | `docs/development-plan.md` | 开发规划：M0-M3 WBS 任务分解、依赖、验收标准 |

## Current State Analysis

- 仓库现状：**空项目**，仅有 `docs/vector-database-design.md` 一份概要设计（v0.1 草案），无任何代码与 Go module。
- 概要设计核心内容：融合型统一检索数据库（全文 BM25 + 稀疏/稠密向量 + 标量过滤），列存不可变段 + 内置 Raft WAL + 存算分离 + ES DSL 兼容层；四阶段路线图 M0（嵌入式单机 MVP）→ M1（ES 兼容 GA）→ M2（云原生分布式）→ M3（生态进阶）。
- 概要设计 §12 技术选型为 **Rust**，本任务将其全量转译为 **Go** 并重评估生态选型（Raft 库、FST、分词、SIMD 等在 Go 生态的对应方案）。
- 用户已确认：纯设计文档交付；覆盖全量 M0-M3；Go 生态选型由我推荐。

## Proposed Changes（逐文档规格）

所有文档使用中文撰写（技术术语保留英文），Mermaid 画图，关键接口用 Go 代码块给出定义。每份文档包含「里程碑标注」列（M0/M1/M2/M3），标明该设计点的落地阶段。

### 文档 1：`docs/go-design/00-overview.md` — 总体设计

章节规格：

1. **文档定位与阅读路径**：与概要设计的关系（概要 → 详细的映射表）、详细设计文档索引。
2. **Go 技术选型总表**（替换概要设计 §12，含理由与备选）：
   - Go 版本：1.23+；**禁用 CGO**（`CGO_ENABLED=0` 静态交叉编译，保单二进制卖点）
   - HTTP：标准库 `net/http`（Go 1.22+ ServeMux 方法+路径模式路由，ES REST 路由 `/{index}/_search` 等可原生表达）；中间件自研（限流/鉴权/trace）
   - gRPC：`google.golang.org/grpc` + protobuf（M2 内部协议）
   - JSON：M0 `encoding/json`；M1 热点（`_bulk`/`_source`）引入 `goccy/go-json` 或 sonic 评估结论写入文档
   - Raft：`hashicorp/raft` v1.3+（Consul/Nomad 生产验证，Multi-Raft 可组合；备选 dragonboat，说明取舍）
   - 位图：`github.com/RoaringBitmap/roaring`
   - 压缩：`github.com/klauspost/compress`（zstd，纯 Go 接近 C 性能）
   - 中文分词：`github.com/go-ego/gse`（纯 Go，避免 gojieba 的 CGO）；其余 20+ 分析器自研（standard/ngram/edge_ngram/whitespace/path/email 等）
   - SIMD：`github.com/mmcloughlin/avo` 生成汇编（dot/cosine/L2 距离内核，amd64 AVX2/FMA + arm64 NEON 双架构）
   - 对象存储：`aws-sdk-go-v2`（S3 协议通吃 MinIO/OSS/COS）
   - 指标 `prometheus/client_golang`、追踪 OpenTelemetry、日志 `log/slog`
   - 自研清单（Go 生态无对应生产级组件）：HNSW（Cross-Link Merge 核心创新需图结构可控）、FST 词典（M0 ART/有序数组过渡 → M1 自研 FST，参考 Lucene/tantivy 设计）、BKD 树、WAL
3. **Go vs Rust 差异与对策**（重点章节，概要设计无）：
   - GC 延迟对策：段不可变 → mmap 按需读取不驻堆；向量/图数据用 file-backed `unsafe.Slice` 扁平数组 + 索引寻址（避免指针密集）；`sync.Pool` 复用 ColumnBatch/查询中间态；`GOMEMLIMIT` 指导 + 后台任务（merge/索引构建）分片 yield 控制 GC 压力
   - SIMD 对策：avo 代码生成 + 运行时 CPU 特性探测（`internal/cpu`）降级路径
   - 并发模型：goroutine/errgroup/channel 管道（写路径、flush、merge）；per-shard 单写多读模型
   - 延迟预算表：GC pause 目标 < 5ms（GOGC 调优 + 堆稳态控制）
4. **模块划分与仓库布局**（monorepo）：`cmd/unisearch`（含 `--embedded` 单机模式）、`cmd/uniadm`、`internal/server|engine|index|storage|meta|raft|tenant`、`pkg/`（SDK/公共类型）、`api/proto/`，逐目录职责说明
5. **进程模型**：嵌入式（单进程全含）/ 云原生（Coordinator 无状态 + Shard Node + Meta Raft），同一二进制两种启动模式
6. **公共规范**：错误码体系（对齐 ES 错误体 `error.reason` + `unsupported_capability`）、context 传递与超时、配置加载（分层：默认值/文件/环境变量）、日志与 trace 字段规范
7. **横切关注点里程碑映射表**：概要设计 §2.4 痛点对策 → 本详细设计章节索引（确保每条对策有着落）

### 文档 2：`docs/go-design/01-storage-engine.md` — 存储引擎

1. **数据模型 Go 定义**：`Cluster/Tenant/Index/Shard/Segment` 结构体；mapping 动态推断规则（类型推断表、冲突拒绝、`dynamic:false`）；字段类型全集（keyword/text/long/double/date/boolean/ip/geo_point/dense_vector/sparse_vector/nested v1 扁平化 `parent__child__` 列名编码）
2. **MemTable**：列式（`map[FieldID]*ColumnBuilder`）+ `_source` 行副本；读写并发（RWMutex + 双缓冲冻结语义）；容量/时限双阈值触发 flush（默认 512MB/30s）
3. **WAL（M0 本地版 → M2 Raft 化）**：日志段文件格式（`[len|crc32c|entry]`，硬件 CRC32 Castagnoli）；group commit 批量 fsync 策略与延迟权衡；崩溃恢复回放流程；幂等写入（client_id+sequence 去重窗口）
4. **段格式规格**（逐文件字节级）：`col.*.bin`（编码方案选择矩阵：字典/RLE/delta/gorilla/zstd，按类型×基数自动选择）、`vec.*.bin`（float32/SQ8/RaBitQ 布局）、`source.zst`（zstd-19）、`meta.footer`（protobuf schema 给出：行数/列 min-max/null 计数/TTL/版本）；**Zone map**：块级（默认 8192 行/块）min/max 结构
5. **Flush 流程**：冻结 → 并行编码各列 → footer 写入 → 原子 rename → 可见性发布；时序图
6. **Delete Bitmap 与更新语义**：全局 `map[SegmentID]*roaring.Bitmap`；(segment_id, row_id) 定位；UPDATE=DELETE+INSERT 同 seq_no 原子提交；查询侧向量化位图过滤算子
7. **段合并**：size-tiered 选择策略（借鉴 Lucene/CH）；合并时物理清除删除行、重建索引；Cross-Link Merge 在合并中的挂接点（详见 02 文档）
8. **MVCC 与可见性水位**：seq_no 分配（Raft 提交序号复用）；watermark 推进协议；`?consistency=eventual` 读路径
9. **冷热分层接口（M2）**：`TierManager` 接口（NVMe 热 → RaBitQ 温 → DiskANN 冷），段级降级任务 + 原子指针切换
10. **Go 接口定义**：`type SegmentWriter/SegmentReader/WAL/FlushScheduler interface` 完整签名
11. **里程碑标注**：每个子模块标注 M0（MemTable/本地 WAL/段格式/flush）或 M2（Raft WAL/分层/在线分裂位图继承——含概要设计开放问题 2 的细化方案）

### 文档 3：`docs/go-design/02-index-engine.md` — 索引引擎

1. **统一索引插件接口**：`type Index interface { Build/Load/Search/Merge/Quantize/MemBudget }`，注册机制（M3 动态库 → Go plugin 的限制说明与进程内注册的现实方案）
2. **HNSW（自研，核心章节）**：
   - 图存储布局：CSR 邻接表（`[]int32` 扁平数组 + 偏移，无指针 GC 友好）、层级表、入口点
   - 参数：M=16/ef_construction=200 默认；距离度量 cosine/dot/l2（含归一化预处理决策）
   - SIMD 距离内核：avo 生成 AVX2+FMA/NEON，批量 8/16 float 一次比较，运行时特性探测降级
   - **Cross-Link Merge 详细算法**：合并两段不重建全图——大段图为主、小段节点按层级逐层插入 O(N_small×logN)；跨段顶层邻接构建（每段 O(√N) 距离计算）；原子切换与回滚快照（小段图保留）；伪代码
   - 量化：SQ8（标量量化边界计算、召回补偿 ef 提升）、RaBitQ 1-bit（v1.5/M2，随机旋转 + 逐行标定）
   - 过滤策略统一规划器：选择率 <10% pre-filter（位图受限图遍历）、>30% post-filter（5×k 过采样 + ef 扩展重试）、中间态迭代 pre-filter；选择率统计来源（zone map/cardinality 估算）
3. **倒排索引**：词典演进路线（M0 排序数组+二分/ART → M1 FST，自研 FST 设计要点：字节序、输出共享、mmap 只读）；posting list（roaring + FOR 压缩 + 位置块）；BM25/BM25F 打分公式与 per-segment 统计；**WAND/Block-Max WAND** 剪枝实现；高亮 offsets 记录格式
4. **分析器**：`type Analyzer interface { Analyze(text string) []Token }`；内置 20+ 清单及实现要点（standard Unicode 分词、gse 中文、ngram/edge_ngram 参数、path/email 层级切分）；自定义词典热加载；`_analyze` API 挂接点
5. **稀疏向量**：KV 张量格式（dimension→weight），复用倒排 posting 存储；与稠密联合融合的接口
6. **标量索引**：BKD 树（点数/区间、geo_point 距离排序；Lucene 论文级细节：leaf 1024 点、1D 退化排序数组优化）；zone map（01 文档定义，此处消费）；低基数位图索引（基数 <4096 自动启用规则）；布隆过滤器（可选，`bloom_filter` mapping 参数）；主键 `_id` 稀疏哈希 + 列存
7. **completion suggester FST**（M1）
8. **里程碑标注**：M0（HNSW-SQ8/倒排基础/zone map/standard+gse 分析器）→ M1（FST/WAND/highlight/suggester）→ M2（RaBitQ/Cross-Link 全量/DiskANN 冷层）→ M3（GPU CAGRA/插件 SDK）

### 文档 4：`docs/go-design/03-query-engine.md` — 查询引擎

1. **DSL 解析层**：ES Query JSON → 内部 AST（Go 结构体全集：BoolQuery/MatchQuery/KnnQuery/HybridQuery/Agg...）；递归下降+serde 式 `UnmarshalJSON` 定制（精细错误信息：路径+原因+unsupported 标注）
2. **逻辑计划 → 物理计划**：算子树 IR；优化器规则集（谓词下推、分片裁剪、投影裁剪、pre/post-filter 决策注入、zone map 跳块）
3. **向量化执行引擎**：ColumnBatch 模型（固定 4096 行批，`[]float64/[]int64/[]string` 列 + 有效性位图）；`type Operator interface { Next(ctx) (*ColumnBatch, error) }`；算子清单（Scan/Filter/HashAggregate/TopK/Merge/BitmapFilter/ANN/BM25Score/Fusion...）；SIMD 过滤与聚合内核复用 02 文档距离内核基建
4. **两阶段 MPP 执行（M2）**：分片子计划序列化（ protobuf schema）、局部 TopK+partial agg 状态、协调器 O(shards×k) 归并；M0 单机退化为本地全 pipeline
5. **混合检索融合**：`type Fusion interface`；RRF（rank_constant 语义）、weighted（min-max 归一）、relative_score（Weaviate 式）、rerank（外部 HTTP 调用协议）；window_size 预算控制
6. **聚合引擎**：向量化 group by（哈希表聚合，flat 数组开放寻址）；可序列化 partial state（HLL++/t-digest/top 数组的 protobuf）；terms/histogram/date_histogram/cardinality/percentiles/top_hits 实现要点；`precision_threshold` 语义
7. **BM25 全局统计**：词频统计表（meta 集群维护、定期刷新协议）；`bm25.global_stats` 开关回退局部 IDF
8. **分页与游标**：from/size（默认 10000 上限对齐 ES）、search_after（排序键序列化、tie-breaker `_shard_doc`）、PIT（MVCC 快照水位 pin 与过期回收）
9. **脚本引擎**：painless 子集 → 自研表达式 AST 直接求值（M0）；v2 字节码 VM 演进（预留接口）；`cosineSimilarity` 等向量函数一等支持；安全限制（无 IO/无限时长）
10. **rescore/collapse/suggester/runtime_mappings** 执行挂接点
11. **profile API**：执行计划树输出结构（对齐 ES profile JSON 字段）
12. **里程碑标注**

### 文档 5：`docs/go-design/04-es-compat-layer.md` — ES 兼容层

1. **路由表全清单**：概要设计 §8.2 逐 API → Go handler 映射表（方法/路径/参数/兼容级别），`net/http` ServeMux pattern 写法示例
2. **请求/响应规范**：`GET /` 版本探测（`version.number` 报 8.x 兼容、`tagline` 等 JSON 结构对齐）；错误体格式（`400` + `error.reason` + `unsupported_capability` 字段）；NDJSON `_bulk` 流式解析（避免整包驻内存）
3. **DSL 兼容矩阵落地**：§8.3/§8.4 矩阵逐条 → 03 文档 AST 节点的映射表；🟡 partial 项的差异清单写法（如 intervals 支持子集、script painless 子集）
4. **mapping/settings 语义映射实现**：§8.5 逐行 Go 实现（doc_values 恒开+告警、`_source:false` 行为、nested 扁平化、copy_to/multi-fields、coerce/similarity/null_value/ignore_above、`hash(routing)%n` 路由兼容 + 在线分裂差异）
5. **并发控制**：`if_seq_no/if_primary_term` 乐观锁实现（seq_no 即 MVCC 版本复用）
6. **_refresh/_flush/_forcemerge 语义映射**（到内部 flush/merge 任务的触发器）；scroll → search_after+PIT 内部转译协议
7. **_cat API**：indices/health/shards/nodes/allocation 五索引输出列规格
8. **_msearch/_mget/_field_caps/_terms_enum/_count/_explain/_analyze** 实现要点
9. **_reindex/_delete_by_query/_update_by_query**：批处理任务框架（throttling、取消、进度查询）
10. **ILM 子集**：rollover/delete/forcemerge/shrink 动作状态机（M1）
11. **兼容性测试金标准**：elasticsearch-py / go-elasticsearch 官方 test suite 的运行策略、通过率 ≥95% 门禁（对应概要设计 §11）、`uniadm es-compat` 扫描工具设计（M3）
12. **里程碑标注**（本层大部分为 M1 重点，M0 仅 _doc/_bulk/_search 基础子集）

### 文档 6：`docs/go-design/05-distributed.md` — 分布式架构

1. **元数据集群**：hashicorp/raft 封装（状态机内容：schema/路由表/租户表/水位/成员；快照格式与恢复）；**数据永不进元数据**约束；单 Raft 组容量估算（万级路由 <1KB/条）；超阈值自动分组（Multi-Raft by index namespace，M2+）；嵌入式单点 Raft → 三副本化升级路径
2. **WAL 即 Raft 日志（Woodpecker 式对象存储 WAL）**：Raft log storage 自定义实现（实现 hashicorp/raft `LogStore`/`StableStore` 接口落到对象存储）；日志段格式、follower 从对象存储补拉协议；本地 NVMe 双写缓解对象存储延迟抖动（概要设计 §14 风险缓解落地）；断点续传与 GC（已快照日志段回收）
3. **分片与路由**：逻辑分片哈希路由；**在线分裂协议**（概要设计 §9.4 细化）：父分片 dual-read → 路由表原子双版本切换 → 子分片接管 → 父下线；**Delete Bitmap 父子继承规则**（开放问题 2 的设计决策：子分片按 row_id 区间切分位图，段文件共享不重写）
4. **副本与故障恢复**：每分片 Raft 组；leader 读默认 / `preference=_replica` follower 读；新 leader 秒级接管流程（回放对象存储日志段，RTO<30s 演练标准）；再均衡调度器（磁盘/内存/QPS 水位打分、拉快照+增量日志+原子切换三阶段）
5. **多租户（Weaviate 式）**：租户=路由表一级实体；三模式实现（dedicated 独占 Shard Node / shared-shard 资源组限额：cgroup v2 + 查询并发预算 semaphore / cold 租户暂停唤醒）；四维配额（存储/向量数/QPS/TPS）计数与限流挂接点
6. **Coordinator 层**：无状态 Query/Ingest Coordinator；_bulk 攒批路由、背压（队列水位+信用流控）；幂等去重窗口
7. **内部 gRPC 协议**：proto 文件组织与关键 service 定义（ShardService: Read/Write/Scan；MetaService: Watch；AdminService），partial state 传输的流式设计
8. **部署拓扑**：单二进制两模式启动参数；K8s Operator 需求清单（M2 后期）；配置示例（3 节点 meta + N 数据节点）
9. **混沌工程场景清单**（对应 M2 验收：kill -9、网络分区、对象存储慢 IO、磁盘满）
10. **里程碑标注**（本章整体 M2，嵌入式退化路径 M0）

### 文档 7：`docs/go-design/06-quality-and-observability.md` — 质量与可观测

1. **指标体系**：Prometheus 指标命名规范（`unisearch_` 前缀；写入/查询/段/合并/租户配额五组核心指标清单）；`/_prometheus/metrics` 端点（对齐 ES 监控习惯的指标名映射表）
2. **追踪**：OpenTelemetry span 树规范（请求→分片→段→算子四级）；采样策略
3. **日志**：slog 结构化字段规范（tenant/index/shard/segment_id/trace_id 必带字段）
4. **健康探针**：`GET /_cluster/health` 状态计算（green/yellow/red 语义映射）
5. **测试策略**：单元（表格驱动+testify）、集成（docker-compose 起集群）、**模糊测试**（DSL 解析器/WAL 回放/段格式 go fuzz targets）、兼容金标准（ES 官方 client test suite 门禁 ≥95%）、基准（`uni-bench` 套件设计：写入吞吐/P99 延迟/召回率 ann-benchmarks 接入/聚合吞吐 vs CH 对比方法）、混沌（M2）
6. **CI/CD**：lint（golangci-lint 规则集）、静态检查（govet/staticcheck）、交叉编译矩阵（linux/amd64,linux/arm64,darwin/arm64）、发布流程（单二进制+容器镜像）
7. **性能预算跟踪**：概要设计 §11 指标 → 基准用例 ID 映射表，CI 性能回归看护

### 文档 8：`docs/development-plan.md` — 开发规划

1. **总览**：M0-M3 甘特式总表（阶段/范围/里程碑验收/依赖关系）
2. **M0 嵌入式 MVP（0-3 月）WBS 分解**：按周拆任务包（存储引擎列存段 → WAL/MemTable → HNSW-SQ8 → 倒排+BM25 → DSL 子集（bool/term/range/match/knn/hybrid）→ REST 基础 API（_doc/_bulk/_search）→ RAG demo + 1 亿文档基准），每个任务包含：交付物、依赖、DoD 验收标准、涉及设计文档章节引用
3. **M1 ES 兼容 GA（3-6 月）WBS**：兼容矩阵 80% full、聚合全量、search_after/PIT/highlight/suggester/msearch、template/alias/ILM 子集、多租户 shared-shard、客户端套件通过率 ≥90% 门禁
4. **M2 云原生 GA（6-10 月）WBS**：Raft 元数据集群、对象存储 WAL、存算分离、分片在线分裂、Cross-Link 全量、DiskANN 冷层、10 亿向量基准、混沌演练报告
5. **M3 生态与进阶（10-16 月）WBS**：GPU CAGRA、内置 rerank、geo_shape、percolate 完整化、插件 SDK、uniadm es-compat、首个迁移案例
6. **团队与角色**：建议人员配置（引擎/检索/分布式/兼容层分组）、Rust→Go 人才风险缓解（概要设计 §14 对应项更新）
7. **风险登记册**：概要设计 §14 风险表逐条 → 详细设计对策章节引用 + 新增 Go 特有风险（GC 延迟、SIMD 维护成本、无 tantivy 等价物自研量）
8. **开放问题跟踪**：概要设计 3 个开放问题的详细设计结论（nested 深度 PoC 计划、位图继承已定、GPU 隔离 M3 决策）

## Assumptions & Decisions

1. **语言与编译**：Go 1.23+，全项目禁 CGO（保单二进制交叉编译）；性能关键路径用纯 Go 优化 + avo 生成汇编。
2. **选型决策（已定，写入 00 文档）**：Raft=hashicorp/raft、位图=roaring、压缩=klauspost/compress、中文分词=gse、HTTP=标准库、对象存储=aws-sdk-go-v2、自研=HNSW/FST/BKD/WAL（理由：Cross-Link Merge 需图结构可控 + Go 生态无 tantivy 等价物）。
3. **文档语言**：中文为主，术语/代码/文件名英文。
4. **不修改**现有 `docs/vector-database-design.md`（概要设计保持原样，详细设计引用其章节号）。
5. 设计文档中的 Go 代码仅为接口/结构体定义级（设计规格），不是可运行实现。
6. mermaid 图用于架构/流程/时序，保持语法可渲染。

## Verification

1. **覆盖检查**：对照概要设计目录逐章映射（§4 架构→00、§5 存储→01、§6 索引→02、§7 查询→03、§8 兼容→04、§9 分布式→05、§10 API→04、§11 性能→06/08、§13 路线图→08），确认无遗漏；§2.4 痛点对策表逐条在详细设计中有对应小节。
2. **一致性检查**：跨文档术语统一（Segment/MemTable/Watermark/seq_no/ColumnBatch 等命名表）；接口签名在引用处与定义处一致；里程碑标注与开发规划 WBS 对齐。
3. **兼容矩阵对齐**：04 文档的 API/DSL 清单与概要设计 §8.2/8.3/8.4 逐条对上，无凭空增删。
4. **可读性**：所有内部链接使用相对路径并逐一验证存在；mermaid 代码块语法人工校验。
5. 完成后向用户提交 8 份文档的索引摘要与阅读路径说明。

## 执行顺序

1. `00-overview.md`（技术栈与规范是其余文档的基础）
2. `01-storage-engine.md` → `02-index-engine.md`（存储先于索引，索引消费段格式）
3. `03-query-engine.md`（消费 01/02 接口）
4. `04-es-compat-layer.md`（消费 03 AST）
5. `05-distributed.md`、`06-quality-and-observability.md`
6. `development-plan.md`（最后汇总，引用全部设计章节）
