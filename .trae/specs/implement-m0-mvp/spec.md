# UniSearch M0 嵌入式 MVP 研发 Spec

> change-id: implement-m0-mvp
> 权威设计依据（实现不得偏离其中的接口签名与格式规格）：
> - [00-overview.md](../../docs/go-design/00-overview.md)（仓库布局/公共规范/SIMD 策略）
> - [01-storage-engine.md](../../docs/go-design/01-storage-engine.md)（存储引擎）
> - [02-index-engine.md](../../docs/go-design/02-index-engine.md)（索引引擎）
> - [03-query-engine.md](../../docs/go-design/03-query-engine.md)（查询引擎）
> - [04-es-compat-layer.md](../../docs/go-design/04-es-compat-layer.md)（REST 基础 API 子集）
> - [development-plan.md](../../docs/development-plan.md) §2（M0 WBS 与 DoD）

## Why

docs/ 下的概要设计与 Go 详细设计已完成，需要按 development-plan.md 的 M0 里程碑落地**可运行的单机嵌入式实现**，并以完善的单元测试保障每个组件符合设计文档定义的行为规格（development-plan.md §9：设计文档是契约，实现偏离需先改设计）。

## What Changes

- 新建 Go module `github.com/unisearch-db/unisearch`（Go 1.23+，`CGO_ENABLED=0`，零 CGO）
- 实现 M0 全量能力（对齐 development-plan.md §2 W1-W4）：
  - **存储引擎**：类型系统与动态 mapping、段格式（编码矩阵/zone map/footer）、MemTable、本地 WAL（group commit/崩溃恢复）、DeleteBitmap、Flush、size-tiered 合并
  - **索引引擎**：HNSW（CSR + 构建/搜索）、SQ8 量化 + 精排回表、过滤规划器（pre/post）、倒排（排序数组词典 + roaring posting + 位置块 + BM25）、8 个分析器、BKD/位图索引/_id 哈希、Cross-Link 段间跳段
  - **查询引擎**：DSL 子集解析（bool/term/terms/range/exists/match/match_phrase/knn/hybrid）、优化器 R1-R4/R6、向量化算子（ColumnBatch/Scan/Filter/ANN/BM25Score/TopN）、Fusion（rrf/weighted）、from/size 分页
  - **REST 基础 API**：`GET /`、_doc、_bulk（NDJSON 流式）、_search、_count、_mget、mapping/alias/settings 基础、_analyze、_refresh、_cat(health/indices)
  - **嵌入式集成**：`unisearch --embedded` 单二进制全链路
- **每个任务交付配套单元测试**；`go test ./...` 全绿为总验收门禁
- 明确不在本 spec 范围（后续 spec 立项）：M1 兼容层全量（FST/WAND/聚合/PIT/highlight 等）、M2 分布式（Raft/对象存储/分裂）、M3 生态；avo 汇编优化（M0 先交付纯 Go 内核 + 函数指针绑定，性能优化不阻塞功能验收）

## Impact

- Affected specs: 无（首个 spec）
- Affected code: 全新代码库 `cmd/ internal/ pkg/ api/ test/`（布局严格按 [00-overview.md §4](../../docs/go-design/00-overview.md)）
- 交付物：可编译单二进制 + 全量单测 + e2e 冒烟测试

## ADDED Requirements

以下每条 Requirement 的验收均以单元测试表达；接口签名以设计文档为准（本文不重复）。

### Requirement: R1 工程骨架与公共基建

系统 SHALL 按 00-overview.md §4/§6 建立 monorepo：`internal/errors`（APIError/UnsupportedInfo，错误码注册表）、`internal/config`（默认值 < YAML < 环境变量三层）、`internal/server/middleware`（panic recovery 占位）、`cmd/unisearch`（cobra）。

#### Scenario: 配置三层合并
- **WHEN** 同时存在默认值、YAML 文件与环境变量 `UNISEARCH_LISTEN`
- **THEN** 环境变量生效；仅 YAML 时 YAML 生效；`--dump-config` 输出全量最终配置

#### Scenario: 错误体结构
- **WHEN** 返回 unsupported 参数错误
- **THEN** 序列化后含 `error.type`/`error.reason`/`error.unsupported_capability`（HTTP 400）

### Requirement: R2 SIMD 距离内核（纯 Go 基线）

`internal/simd` SHALL 提供 `DotF32/L2F32`（unrolled 纯 Go）并通过函数指针暴露，M0 绑定纯 Go 实现；单测：随机数据（含边界长度 0/1/15/16/17）与朴素实现误差 < 1e-6。

#### Scenario: 距离正确性
- **WHEN** 任意两等长 float32 向量输入
- **THEN** Dot/L2 结果与逐元素朴素计算一致（相对误差 < 1e-6）

### Requirement: R3 类型系统与动态 mapping

按 01-storage-engine.md §1：`pkg/types`（TenantID/IndexID/ShardID/SegmentID/RowID/FieldID/SeqNo）+ `internal/meta`（FieldDef/IndexMeta 单机形态 + 动态推断表 + 冲突拒绝 + dynamic:false/strict）。

#### Scenario: 动态类型推断
- **WHEN** 写入 bool/int/float/ISO 日期/普通字符串/对象/数组
- **THEN** 依次推断 boolean/long/double/date/text+keyword multi-field/object 递归/取首元素类型

#### Scenario: mapping 冲突
- **WHEN** 字段已声明 long，写入字符串 "abc"
- **THEN** 拒绝并返回 400 风格错误；`dynamic:false` 时未知字段仅入 _source 不建索引

### Requirement: R4 段格式与编码矩阵

按 01-storage-engine.md §4：段目录布局、ColumnHeader/BlockIndex/ZoneMap/SegmentFooter（protobuf）、8192 行块、编码选择矩阵（字典/delta+bitpack/gorilla/RLE/zstd 按类型×基数自动选择）、`src.zst`、`id.dict+id.hash`。

#### Scenario: 编码往返
- **WHEN** 任意类型列数据写入段并读回
- **THEN** 数据逐值相等（浮点按位相等），块 ZoneMap 与实际 min/max 一致

#### Scenario: 跳块
- **WHEN** 查询谓词与某块 ZoneMap 无交集
- **THEN** 该块零解码（以解码计数器验证）

### Requirement: R5 MemTable 与本地 WAL

按 01-storage-engine.md §2/§3：列式 MemTable（冻结双缓冲、512MB/30s 阈值、_id pkIndex）、本地 WAL（`[len|crc32c|seq_no|type|zstd payload]` 记录、256MB 滚动、group commit 攒批 2ms/4MB、SegmentFlushed 标记、崩溃恢复 CRC 校验截断）。

#### Scenario: 写入即可见
- **WHEN** 文档 append 到 active MemTable
- **THEN** 同分片查询立即读到（无需 flush）

#### Scenario: 崩溃恢复
- **WHEN** 写入 N 条后模拟进程崩溃（WAL 尾部注入坏 CRC）
- **THEN** 重启回放：坏记录前的数据全部恢复，坏记录及之后丢弃，无 panic

#### Scenario: group commit
- **WHEN** 并发 100 goroutine 各写 1 条
- **THEN** 每条的返回 seq_no 单调且 WAL 文件中记录连续

### Requirement: R6 DeleteBitmap 与更新语义

按 01-storage-engine.md §6：`map[SegmentID]*roaring.Bitmap` + memDeletes；DELETE 立即生效；UPDATE=DELETE+INSERT 同 seq_no 原子（同一 group commit 批次）。

#### Scenario: 删除立即生效
- **WHEN** 删除段内某 _id 后立即查询
- **THEN** 该文档不出现在结果中

#### Scenario: UPDATE 原子性
- **WHEN** UPDATE 文档（字段值变更）
- **THEN** 任意时刻查询只见旧值或新值之一，不见二者同存或皆失

### Requirement: R7 Flush 与段合并

按 01-storage-engine.md §5/§7：冻结→并行编码→footer→fsync→rename 原子可见；合并 size-tiered（删除率>30% 优先、段数>10 合并、_forcemerge 目标段数）、归并保序、物理清除删除行、合并期查询零错误。

#### Scenario: Flush 原子性
- **WHEN** flush 进行中并发查询
- **THEN** 查询零错误且数据不重不漏（MemTable 与新段恰好交接一次）

#### Scenario: 合并物理清除
- **WHEN** 段内 50% 行已删除位图标记后触发合并
- **THEN** 新段行数 = 原行数-删除数，位图归零，查询结果不变

### Requirement: R8 HNSW 与 SQ8 量化

按 02-index-engine.md §2.1-2.3/§2.5：CSR 邻接（扁平 []int32 + offsets）、M=16/efc=200/efs=64、cosine 归一化预处理 + dot 内核、构建/搜索；SQ8（逐维 min/max 量化 + TopK 回表精排 + ef×1.5 补偿）。

#### Scenario: ANN 召回率
- **WHEN** 10 万条 768d 随机单位向量，k=10 查询 100 次
- **THEN** 相对暴力 TopK 召回率 ≥ 0.95（ef=64 无过滤）

#### Scenario: SQ8 量化
- **WHEN** SQ8 索引查询同上
- **THEN** 相对 float32 索引召回率 ≥ 0.98（含精排回表），内存占用 < float32 的 1/3

### Requirement: R9 过滤规划器

按 02-index-engine.md §2.6：选择率 <10% pre-filter（位图受限遍历）、>30% post-filter（5×k 过采样 + ef 扩展重试 ≤2 次）、中间迭代档；选择率估算来源：位图索引/zone map/保守默认 post。

#### Scenario: pre-filter 正确性
- **WHEN** 选择率 ~5% 的标量过滤 + ANN 查询
- **THEN** 结果与「先过滤后暴力 TopK」完全一致

#### Scenario: post-filter 补偿
- **WHEN** 选择率 ~50% 过滤、k=10
- **THEN** 返回满 10 条（过采样重试生效）

### Requirement: R10 倒排索引与 BM25

按 02-index-engine.md §3：排序数组词典（前缀共享 + zstd）、roaring posting + FOR 词频 + fldLen bucket、位置块、BM25（k1=1.2/b=0.75，IDF=ln(1+(N-df+0.5)/(df+0.5))）、段级统计。

#### Scenario: BM25 数值正确性
- **WHEN** 构造已知 tf/df/fldLen 的小语料
- **THEN** 打分与公式手算值一致（相对误差 < 1e-9）

#### Scenario: 短语查询
- **WHEN** match_phrase "hello world" 查询，文档含 "world hello"
- **THEN** 不命中（位置序校验）；含 "hello beautiful world" 且 slop≥1 时命中

### Requirement: R11 分析器（8 个）

按 02-index-engine.md §4：standard（UAX#29 + lowercase）、simple、whitespace、keyword、chinese（gse）、ngram、edge_ngram、lowercase+stop 组合链；Token{Term,Start,End,Pos,Type}。

#### Scenario: 分析输出结构
- **WHEN** `_analyze` 输入多语言文本
- **THEN** Token 的 Start/End 指向原文正确字节偏移，Pos 连续

#### Scenario: ngram 参数
- **WHEN** ngram(min=2,max=3) 输入 "abcd"
- **THEN** 产出 ab/abc/bc/bcd/cd（边界截断正确）

### Requirement: R12 标量索引

按 02-index-engine.md §6：BKD（1D 排序数组 leaf 优化 + 2D geo 距离）、低基数位图（基数<4096 自动启用）、布隆（可选）、_id 稀疏哈希 + 列存。

#### Scenario: 范围过滤
- **WHEN** long 字段 range 查询 [100,200)
- **THEN** 返回行与线性扫描结果一致；BKD 剪枝后解码行数 ≤ 线性扫描

#### Scenario: geo 距离
- **WHEN** geo_point 字段 distance 查询（中心点+半径）
- **THEN** 结果与 haversine 逐行计算一致

### Requirement: R13 Cross-Link 段间跳段

按 02-index-engine.md §2.4.2：段 flush 后后台构建 crosslinks（顶层入口互近邻 k=4），查询从最优入口段出发可跨段跳跃，全局 TopK 归并。

#### Scenario: 多段 vs 单段
- **WHEN** 同一批向量分别写成 1 段与 5 段（含 crosslink）
- **THEN** 两者 ANN 查询结果 top-10 重合率 ≥ 0.98

### Requirement: R14 DSL 解析子集

按 03-query-engine.md §2 + 04-es-compat-layer.md §3 M0 子集：bool(嵌套)/term/terms/range/exists/match/match_phrase/knn/hybrid(+rrf/weighted) 解析到 AST；错误信息带 JSON 路径；不支持的 DSL 返回 unsupported_capability。

#### Scenario: 嵌套 bool 解析
- **WHEN** 三层嵌套 bool + filter + must_not 的 JSON
- **THEN** AST 结构与字段一一对应

#### Scenario: 错误路径
- **WHEN** `query.bool.must[2].match.foo` 缺少必填 text
- **THEN** 错误信息含该路径；未知 DSL（如 intervals）返回 unsupported 错误

### Requirement: R15 查询执行引擎

按 03-query-engine.md §3：ColumnBatch(4096)/算子（Scan 跳块+MVCC 过滤/BitmapFilter/PredFilter/ANN/BM25Score/TopN）/优化器 R1-R4、R6（谓词下推、跳块注入、投影裁剪、pre/post 决策、BM25 段上界剪枝）；MVCC 快照读（seq_no ≤ ReadUpTo）。

#### Scenario: 谓词下推跳块
- **WHEN** range 过滤命中少量块
- **THEN** 计划含下推谓词，扫描块数 < 总块数，结果与后过滤一致

#### Scenario: MVCC 快照
- **WHEN** 查询持有快照 ReadUpTo=S 后继续写入
- **THEN** 查询只见 seq_no ≤ S 的数据（PIT 视图稳定性）

### Requirement: R16 Fusion 与分页

按 03-query-engine.md §5/§8：RRF（`Σ 1/(k+rank)`，k=60）、weighted（路内 min-max 归一）、from/size（from+size ≤ 10000，超限 400）。

#### Scenario: RRF 正确性
- **WHEN** 两路各返回有序文档列表
- **THEN** 融合分数与公式手算一致，排序正确

#### Scenario: 分页上限
- **WHEN** from=9991,size=20
- **THEN** 返回 400（同 ES 文案）；from+size=10000 合法

### Requirement: R17 REST 基础 API

按 04-es-compat-layer.md §2.2/§6 M0 行：`GET /`（版本探测 8.12 兼容 + distribution:unisearch）、_doc（含 if_seq_no 乐观锁 409）、_bulk（NDJSON 流式、逐条错误聚合、refresh 三态）、_search（8.x 响应骨架）、_count、_mget、_mapping、_alias、_settings 基础、_analyze、_refresh（no-op 200）、_cat/health、_cat/indices。

#### Scenario: ES 客户端连通
- **WHEN** elasticsearch-py 8.x 指向 127.0.0.1:9200 执行 info/index/search
- **THEN** 请求成功且响应结构与 8.x 一致（e2e 冒烟）

#### Scenario: _bulk 逐条错误
- **WHEN** bulk 含合法与非法（mapping 冲突）混合条目
- **THEN** HTTP 200 + items 逐条 errors 标记，合法条目已写入

### Requirement: R18 嵌入式集成

`unisearch --embedded --data-dir ... --listen :9200` 单进程全链路：启动加载/恢复、graceful shutdown（停接入→排空→flush→关 WAL）、重启数据零丢失。

#### Scenario: 端到端 RAG 检索
- **WHEN** _bulk 写入含向量与文本的文档，_search 发起 hybrid（match+knn+rrf）查询
- **THEN** 返回融合排序 hits，含 _source 与 _score

#### Scenario: 重启零丢失
- **WHEN** 写入 N 条 → kill 进程 → 重启
- **THEN** 全部 N 条可查（WAL 恢复），_cat/health 正常
