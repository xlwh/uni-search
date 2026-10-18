# Tasks — implement-m0-mvp

> 每个任务必须交付：实现代码 + 单元测试（测试名与 spec.md Requirement 场景对应）+ `go test ./...` 该包全绿。
> 接口签名/文件格式严格遵循 docs/go-design/ 对应章节（引用见各任务）。

## Track A 基础（无依赖，先行）

- [x] T1: 工程骨架与公共基建（R1；00-overview.md §4/§6）
  - [x] T1.1: go module 初始化（github.com/unisearch-db/unisearch，Go 1.23）、目录骨架（cmd/ internal/ pkg/ api/ test/）、.gitignore、Makefile（build/test/lint）
  - [x] T1.2: internal/errors：APIError/UnsupportedInfo/错误码注册表 + 序列化单测（R1 错误体结构场景）
  - [x] T1.3: internal/config：三层合并（默认<YAML<env UNISEARCH_ 前缀）+ --dump-config + 单测（R1 配置场景）
  - [x] T1.4: cmd/unisearch：cobra 骨架（--embedded/--data-dir/--listen 占位）+ slog 初始化
- [x] T2: SIMD 距离内核纯 Go 基线（R2；00-overview.md §3.2）
  - [x] T2.1: internal/simd：DotF32/L2F32 unrolled 纯 Go + 函数指针绑定（预留 asm 替换位）
  - [x] T2.2: 随机数据 + 边界长度（0/1/15/16/17）与朴素实现对齐单测（误差 <1e-6）
- [x] T3: 类型系统与动态 mapping（R3；01-storage-engine.md §1）
  - [x] T3.1: pkg/types：全部 ID 类型 + SeqNo/Snapshot（01 §8）
  - [x] T3.2: internal/meta：FieldDef/FieldType 全集/IndexMeta（单机 JSON 持久化 + fsync）
  - [x] T3.3: 动态推断引擎（推断表/冲突拒绝/dynamic:false|strict）+ 单测（R3 两场景）

## Track B 存储（依赖 T1/T3）

- [x] T4: 段格式与编码矩阵（R4；01-storage-engine.md §4）
  - [x] T4.1: api/proto/segment.proto（ColumnHeader/BlockIndex/ZoneMap/SegmentFooter）+ 生成
  - [x] T4.2: internal/storage/segment：块编码器族（字典/delta+bitpack/gorilla/RLE/zstd）+ 自动选择矩阵
  - [x] T4.3: ColumnWriter/ColumnReader（块流式迭代）+ src.zst + id.dict/id.hash
  - [x] T4.4: 编码往返 + ZoneMap 一致性 + 跳块计数器单测（R4 两场景）
- [x] T5: MemTable 与本地 WAL（R5；01-storage-engine.md §2/§3）
  - [x] T5.1: internal/storage/memtable：列式 Table/pkIndex/冻结双缓冲/512MB/30s 触发
  - [x] T5.2: internal/storage/wal：记录格式（len/crc32c/seq_no/type/zstd）、256MB 滚动、SegmentFlushed 标记
  - [x] T5.3: group commit（2ms/4MB 攒批 fsync）
  - [x] T5.4: 崩溃恢复（CRC 校验截断回放）+ fuzz 种子（随机字节流不 panic）
  - [x] T5.5: 单测：写入即可见/崩溃恢复/并发 group commit（R5 三场景）
- [x] T6: DeleteBitmap 与 Flush（R6+R7 前半；01-storage-engine.md §5/§6）
  - [x] T6.1: DeleteTracker（segmentDeletes/memDeletes）+ 查询过滤接口
  - [x] T6.2: FlushWorker：冻结→并行编码→footer→fsync→rename 原子可见（SegmentSet atomic.Pointer）
  - [x] T6.3: UPDATE=DELETE+INSERT 同 seq_no 批次原子
  - [x] T6.4: 单测：删除立即生效/UPDATE 原子性/flush 期并发查询零错误（R6 两场景 + R7 Flush 场景）
- [x] T7: 段合并（R7 后半；01-storage-engine.md §7）
  - [x] T7.1: size-tiered 调度器（删除率>30% 优先/段数>10/_forcemerge 目标段数，并发≤2）
  - [x] T7.2: 多路归并（跳过位图命中行、保序、索引重建挂接点）
  - [x] T7.3: 单测：合并物理清除/查询结果不变/合并期查询零错误（R7 合并场景）

## Track C 索引（依赖 T2/T4，与 Track B 的 T5-T7 并行）

- [x] T8: HNSW 核心与 SQ8（R8；02-index-engine.md §2.1-2.3/§2.5）
  - [x] T8.1: CSR 图结构（neighbors/offsets/levels/entry）+ 构建流程（贪心下降+启发式选邻）
  - [x] T8.2: 搜索（ef 搜索、cosine 归一化+dot）复用 internal/simd
  - [x] T8.3: SQ8 量化状态（逐维 min/max）+ int8 内核 + TopK 回表精排（ef×1.5）
  - [x] T8.4: 基准单测：10 万×768d 召回 ≥0.95（ef64）；SQ8 相对召回 ≥0.98（R8 两场景）
- [x] T9: 过滤规划器（R9；02-index-engine.md §2.6）
  - [x] T9.1: 选择率估算（位图基数/zone map/保守默认）
  - [x] T9.2: pre-filter（位图受限遍历）/post-filter（5×k+ef 扩展重试 ≤2）/迭代档
  - [x] T9.3: 单测：与「先过滤后暴力 TopK」对拍 + 补偿满 k（R9 两场景）
- [x] T10: 倒排与 BM25（R10；02-index-engine.md §3）
  - [x] T10.1: 排序数组词典（前缀共享+zstd+mmap 读）+ TermID 分配
  - [x] T10.2: posting（roaring docid + FOR tf + fldLen bucket）+ 位置块（delta+bitpack）
  - [x] T10.3: BM25 打分（k1/b 参数化、段级统计）
  - [x] T10.4: 单测：BM25 数值手算对齐/短语位置序/边界 slop（R10 两场景）
- [x] T11: 分析器（R11；02-index-engine.md §4）
  - [x] T11.1: Analyzer 接口 + Token 结构 + 组合链（tokenizer→filters）
  - [x] T11.2: 8 个实现：standard(UAX#29)/simple/whitespace/keyword/chinese(gse)/ngram/edge_ngram/lowercase+stop
  - [x] T11.3: 单测：偏移/Pos 连续/ngram 边界（R11 两场景）
- [x] T12: 标量索引（R12；02-index-engine.md §6）
  - [x] T12.1: BKD 1D（排序数组 leaf 优化）+ 范围求交位图输出
  - [x] T12.2: BKD 2D（geo_point 定点 + 距离过滤）
  - [x] T12.3: 低基数位图索引（基数<4096 自动启用）+ 布隆（可选）+ _id 哈希接入（T4.3）
  - [x] T12.4: 单测：范围/geo 与线性扫描对拍（R12 两场景）
- [x] T13: Cross-Link 段间跳段（R13；02-index-engine.md §2.4.2）
  - [x] T13.1: crosslinks 构建（顶层入口集 √N、互近邻 k=4）+ entry.bin 扩展格式
  - [x] T13.2: 多段查询协议（入口选择→段内遍历→跨段跳跃→全局 TopK）
  - [x] T13.3: 单测：1 段 vs 5 段 top-10 重合率 ≥0.98（R13 场景）

## Track D 查询（依赖 T4/T8/T10/T11；T15 可与 T12/T13 并行）

- [x] T14: DSL 解析子集（R14；03-query-engine.md §2 + 04 §3）
  - [x] T14.1: AST 节点全集（bool/term/terms/range/exists/match/match_phrase/knn/hybrid）
  - [x] T14.2: 手工逐层解析（JSON 路径错误、unsupported 抛 APIError）
  - [x] T14.3: 单测：嵌套 bool/错误路径/unsupported + fuzz 种子（R14 两场景）
- [x] T15: 执行引擎（R15；03-query-engine.md §3）
  - [x] T15.1: ColumnBatch(4096) + Operator 接口 + sync.Pool 复用
  - [x] T15.2: 算子：Scan（跳块+MVCC）/BitmapFilter/PredFilter/TopN
  - [x] T15.3: 算子：ANN（挂 T8/T9）/BM25Score（挂 T10）
  - [x] T15.4: 优化器 R1-R4/R6（下推/跳块/投影/pre-post 决策/段上界剪枝）+ 计划打印
  - [x] T15.5: 单测：下推跳块/MVCC 快照（R15 两场景）
- [x] T16: Fusion 与分页（R16；03-query-engine.md §5/§8）
  - [x] T16.1: RRF/weighted 融合器
  - [x] T16.2: from/size（上限 10000、400 错误）
  - [x] T16.3: 单测：RRF 手算对齐/分页上限（R16 两场景）

## Track E 服务集成（依赖全部）

- [x] T17: REST 基础 API（R17；04-es-compat-layer.md §2.2/§6）
  - [x] T17.1: 路由表（Go 1.22 ServeMux）+ 中间件（panic recovery/请求日志）
  - [x] T17.2: `GET /` 版本探测 + _cluster/health + _cat/health + _cat/indices
  - [x] T17.3: _doc CRUD（含 if_seq_no 409、_create 409）+ _mget + _count
  - [x] T17.4: _bulk NDJSON 流式 + 逐条错误聚合 + refresh 三态
  - [x] T17.5: _search（8.x 响应骨架：hits/_shards/took）+ _mapping PUT/GET + _alias + _settings + _analyze + _refresh
  - [x] T17.6: handler 单测：R17 两场景（mock 引擎层）
- [x] T18: 嵌入式集成与 e2e（R18；00-overview.md §5）
  - [x] T18.1: --embedded 装配：meta+storage+index+engine+server 全链路
  - [x] T18.2: graceful shutdown（停接入→排空→flush→关 WAL）
  - [x] T18.3: e2e 测试（test/e2e）：RAG hybrid 检索全流程 + kill 重启零丢失（R18 两场景，真实进程）
  - [x] T18.4: `go build ./... && go test ./...` 全绿 + go vet 零报告

# Task Dependencies

- T1、T2 无依赖（立即可并行）
- T3 依赖 T1
- T4 依赖 T1/T3；T5 依赖 T1/T3；T6 依赖 T4/T5；T7 依赖 T6
- T8 依赖 T2；T9 依赖 T8；T10 依赖 T4；T11 无引擎依赖（仅 T1）；T12 依赖 T4；T13 依赖 T8
- T14 依赖 T1；T15 依赖 T4/T8/T10/T11；T16 依赖 T15
- T17 依赖 T14-T16；T18 依赖 T17 与全部
- 并行分组的建议批次：①T1+T2+T11 ②T3+T4+T5+T8+T10+T14 ③T6+T7+T9+T12+T13+T15 ④T16+T17 ⑤T18
