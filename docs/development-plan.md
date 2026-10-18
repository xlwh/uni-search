  # UniSearch 开发规划（M0-M3）

> 版本：v0.1
> 日期：2026-10-17
> 上游：[概要设计 §13 路线图](vector-database-design.md)
> 详细设计索引：[go-design/00-overview.md](go-design/00-overview.md)（总纲，含全套文档链接）
> 语言/技术栈：Go（选型决策见 [00-overview.md §2](go-design/00-overview.md)）

---

## 1. 总览

| 阶段 | 周期 | 范围 | 里程碑验收（DoD） | 前置依赖 |
|------|------|------|------------------|---------|
| **M0 嵌入式 MVP** | 第 1-3 月 | 单机存储引擎 + HNSW-SQ8 + 倒排 BM25 + DSL 子集 + 基础 REST API | 单容器跑通 RAG demo；1 亿文档基准报告（B1-B10 微缩版） | 无 |
| **M1 ES 兼容 GA** | 第 4-6 月 | DSL/Agg 兼容矩阵 80% full、PIT/highlight/suggester、template/alias/ILM 子集、多租户 shared-shard | elasticsearch-py / go-elasticsearch 测试套件通过率 ≥ 90% | M0 |
| **M2 云原生 GA** | 第 7-10 月 | Raft 元数据集群 + 对象存储 WAL + 存算分离 + 在线分裂 + Cross-Link 全量 + DiskANN 冷层 | 10 亿向量集群基准；混沌演练报告（kill -9 等 C1-C6） | M1 |
| **M3 生态与进阶** | 第 11-16 月 | GPU CAGRA、内置 rerank、geo_shape、percolate 完整化、插件 SDK、uniadm es-compat | 首个 ES 存量用户整体迁移案例 | M2 |

依赖关系：M0→M1 串行（兼容层建立在引擎之上）；M2 内部 meta/WAL/分裂三线并行；M3 各项独立可并行。

---

## 2. M0 嵌入式 MVP（第 1-3 月）WBS

### 第 1-2 周：工程骨架

| 任务 | 交付物 | 设计引用 | DoD |
|------|--------|---------|-----|
| W0.1 仓库初始化 | go module、目录骨架（[00 §4](go-design/00-overview.md)）、CI（lint+test+交叉编译） | 00 §4/§6 | CI 绿；三平台产物 |
| W0.2 公共基建 | errors 错误码、config 三层、slog、指标注册 | 00 §6 | `--dump-config` 可用 |
| W0.3 SIMD 基建 | avo 生成 dot/L2（AVX2+NEON）+ 探测降级 + 对齐单测 | 00 §3.2 | 纯 Go 参考实现比对通过 |

### 第 3-6 周：存储引擎

| 任务 | 交付物 | 设计引用 | DoD |
|------|--------|---------|-----|
| W1.1 数据模型与 meta 单机形态 | 类型系统、动态 mapping 推断 | [01 §1](go-design/01-storage-engine.md) | 推断表全过、冲突拒绝 |
| W1.2 段格式 v1 + 编码矩阵 | col/vec/src/footer + zone map | 01 §4 | 编码往返 fuzz + 跳块单测 |
| W1.3 MemTable + 本地 WAL | 冻结双缓冲、group commit、幂等窗口 | 01 §2/§3 | kill -9 恢复零丢失（模糊注入） |
| W1.4 Flush + DeleteBitmap | 原子可见、删除立即生效、UPDATE 原子 | 01 §5/§6 | flush 期查询零错误 |
| W1.5 size-tiered 合并 | 调度器 + 归并 | 01 §7 | 合并无查询停顿 |

### 第 5-8 周：索引引擎（与 1.3 后并行）

| 任务 | 交付物 | 设计引用 | DoD |
|------|--------|---------|-----|
| W2.1 HNSW 核心 | CSR 图 + 构建/查询 + SIMD 距离 | [02 §2.1-2.3](go-design/02-index-engine.md) | ann-benchmarks sift 召回 ≥ 基线（自建单段） |
| W2.2 SQ8 量化 + 精排回表 | 量化状态 + 混合内核 | 02 §2.5 | 召回 ≥98% @ef64；内存 ≤3.5GB/千万（B9） |
| W2.3 过滤规划器 | pre/post/迭代三档 | 02 §2.6 | B3 选择率分档用例 |
| W2.4 倒排 + BM25 | 排序数组词典、roaring posting、位置块、打分 | 02 §3 | BM25 分数与 Lucene 样本数值对齐（相对误差 <1e-6） |
| W2.5 分析器 8 个 | standard/simple/whitespace/keyword/chinese(gse)/ngram/edge_ngram/lowercase+stop 链 | 02 §4 | `_analyze` 输出 ES 同构 |
| W2.6 标量索引 | BKD(1D/2D)、位图、_id 哈希、布隆 | 02 §6 | 范围/geo 距离用例 |
| W2.7 Cross-Link 段间跳段 | crosslinks 构建 + 查询协议 | 02 §2.4.2 | 多段 vs 单大段召回差 <1% |

### 第 7-10 周：查询引擎

| 任务 | 交付物 | 设计引用 | DoD |
|------|--------|---------|-----|
| W3.1 DSL 子集解析 | bool/term/terms/range/exists/match/match_phrase/knn/sparse/hybrid/function_score + query_string | [03 §2](go-design/03-query-engine.md)、[04 §3](go-design/04-es-compat-layer.md) | 解析 fuzz 无 panic |
| W3.2 计划与优化器 | R1-R4/R6 + 物理算子管道 | 03 §3 | 计划打印与 trace 一致 |
| W3.3 向量化执行 | ColumnBatch + Scan/BitmapFilter/PredFilter/ANN/BM25Score/TopN | 03 §3.3 | B2 P99 达标 |
| W3.4 Fusion（rrf/weighted/relative_score） | 三路融合 | 03 §5 | B5 达标 |
| W3.5 from/size + _source 取回 | TopN + 解压 | 03 §8 | 翻页语义对齐 ES |

### 第 9-12 周：REST API 与收官

| 任务 | 交付物 | 设计引用 | DoD |
|------|--------|---------|-----|
| W4.1 REST 基础 | `GET /`、_doc、_bulk（流式）、_search、_count、_mget、mapping/alias/settings 基础、_analyze、_refresh、_cat(health/indices) | [04 §2/§6](go-design/04-es-compat-layer.md) | elasticsearch-py 连通冒烟 |
| W4.2 `--embedded` 集成 | 单二进制启动全链路 | 00 §5 | docker 单容器跑通 |
| W4.3 RAG demo | demo 应用（hybrid 检索 + 高亮占位） | 概要设计 §10 示例 2 | demo 可复现 |
| W4.4 基准报告 | 1 亿文档 B1/B2/B6/B9 全量 | [06 §5.2](go-design/06-quality-and-observability.md) | 报告归档 docs/benchmarks/ |
| W4.5 质量基建 | 模糊四 target + 金标准基线跑通 | 06 §5 | 夜间流水线建立 |

**M0 出口评审**：全部 DoD 勾选 + 基准报告 + RAG demo 演示。

---

## 3. M1 ES 兼容 GA（第 4-6 月）WBS

| 任务 | 交付物 | 设计引用 | DoD | 周次 |
|------|--------|---------|-----|------|
| W5.1 FST 词典 | 自研 FST + fuzzy 自动机 + 转换 02§3.1 接口 | 02 §3.1 | 词典内存较数组版降 ≥60%；fuzz 用例 | 1-3 |
| W5.2 WAND/BMW | 词项剪枝 + 段上界剪枝 | 02 §3.4 | B4 P99 <80ms | 2-4 |
| W5.3 聚合全量 | 向量化分组 + HLL/t-digest + partial state + pipeline | 03 §6 | §8.4 矩阵 full 项全过 | 2-6 |
| W5.4 search_after/PIT/scroll 转译 | 快照 pin + 游标 | 03 §8、04 §6.4 | 深分页 100 万行稳定 | 3-4 |
| W5.5 highlight/suggester | unified highlighter + term/phrase/completion | 02 §3.5/§7、03 §10 | ES 输出结构 diff 通过 | 4-6 |
| W5.6 脚本引擎 | painless 子集 + 向量函数 | 03 §9 | 步数预算注入 fuzz | 3-5 |
| W5.7 rescore/collapse/runtime_mappings/profile | 修饰族 + profile 结构 | 03 §10/§11 | profile 与 ES 字段对齐 | 5-6 |
| W5.8 API 全量 | _msearch/_field_caps/_terms_enum/_explain/by_query/_reindex/template/ILM/_cat 全量/settings 扩展 | 04 §2.2/§6 | 金标准用例逐 API 通过 | 3-8 |
| W5.9 BM25 全局统计 | 统计表 + global_stats | 03 §7 | 跨分片分数一致（模拟多分片单机） | 5-6 |
| W5.10 多租户 shared-shard | 资源组 + 配额四维 + 429 | [05 §5](go-design/05-distributed.md) | 配额超限注入测试 | 6-8 |
| W5.11 兼容收口 | 差异清单发布 + 通过率冲刺 | 04 §3/§9 | **elasticsearch-py/go-elasticsearch 套件 ≥90%** | 7-8 |

**M1 出口评审**：金标准报告 ≥90% + 兼容矩阵报告（✅ 占比 ≥80%，概要设计 §13）。

---

## 4. M2 云原生 GA（第 7-10 月）WBS

| 任务 | 交付物 | 设计引用 | DoD | 周次 |
|------|--------|---------|-----|------|
| W6.1 meta Raft 集群 | hashicorp/raft FSM + 快照 + Watch | [05 §1](go-design/05-distributed.md) | 3 节点选举/恢复注入 | 1-3 |
| W6.2 对象存储 WAL | LogStore 落 S3 + NVMe 双写 + 追赶协议 | 05 §2 | C3 慢 IO 注入 P99 退化 <2× | 2-5 |
| W6.3 Shard Raft 组 | 每分片一组 + ReadIndex + 故障接管 | 05 §4 | C1 kill -9 RTO <30s | 3-6 |
| W6.4 Coordinator 层 | ingest 攒批/背压 + query MPP | 05 §6、[03 §4](go-design/03-query-engine.md) | 分片预聚合归并正确性对拍单机 | 4-7 |
| W6.5 存算分离 + 冷热分层 | 段上对象存储 + TierManager + DiskANN | [01 §9](go-design/01-storage-engine.md)、02 §2.5 | 冷段查询 + 召回补偿验证 | 5-8 |
| W6.6 在线分裂 | 分裂协议 + 位图继承 + crosslinks 重定向 | 05 §3、01 §11 | C5 分裂期双杀；查询零感知 | 6-9 |
| W6.7 Cross-Link Merge 全量 | 增量合并替换全量重建 | 02 §2.4.3 | B10 召回 ≥ 基线-2% + 合并耗时对比报告 | 6-9 |
| W6.8 RaBitQ 温层 | 1-bit 量化 + 精排 | 02 §2.5 | 召回 ≥95% + 内存 1/32 验证 | 7-9 |
| W6.9 再均衡 + dedicated/cold 租户 | 三阶段迁移 + 租户模式 | 05 §3.3/§5 | 迁移期间查询零错误 | 8-10 |
| W6.10 混沌 + 10 亿基准 | C1-C6 每夜 + 全量 B 用例 | 05 §9、06 §5 | **演练报告 + 10 亿向量基准归档** | 9-12 |

**M2 出口评审**：混沌报告全绿 + 10 亿基准（B2/B5/B9 规模版）+ 存算分离成本报告。

---

## 5. M3 生态与进阶（第 11-16 月）WBS

| 任务 | 交付物 | 设计引用 | DoD |
|------|--------|---------|-----|
| W7.1 GPU CAGRA 索引 | sidecar 进程插件（GPU 资源隔离模型同步定稿：独占卡 vs MIG） | [02 §1.2](go-design/02-index-engine.md) | GPU 索引召回/延迟基准 |
| W7.2 内置 rerank 模型 | 本地推理（纯 Go 推理或 ONNX sidecar 评估） | [03 §5](go-design/03-query-engine.md) | rerank 延迟 <50ms（小模型） |
| W7.3 geo_shape | BKD 扩展（面/线编码） | 概要设计 §8.3 ❌→✅ | geo_shape 查询族过金标准 |
| W7.4 percolate 完整化 | 查询即文档存储与匹配 | 04 §3.3 | percolate 金标准 |
| W7.5 插件 SDK | 进程内注册文档化 + sidecar 协议 GA | 02 §1.2 | 第三方示例插件 |
| W7.6 uniadm es-compat | 存量 mapping/查询扫描评估工具 | 04 §9 | 对 3 个真实 ES 集群输出报告 |
| W7.7 K8s Operator | 分片调度/分裂/配额 CRD | 05 §8 | e2e 自动化用例 |
| W7.8 Go/Python SDK GA | `pkg/unisdk` + Python（gRPC 双通道） | 00 §4、概要设计 §10 | SDK 文档 + 版本化承诺 |

**M3 出口**：首个 ES 存量用户整体迁移案例（uniadm 报告 → 迁移 → 金标准回归通过）。

---

## 6. 团队与角色建议

| 分组 | 职责 | 人数（建议） | 主战场 |
|------|------|------------|--------|
| 引擎组 | 存储引擎 + 向量化执行 | 3-4 | 01/03 文档 |
| 检索组 | HNSW/倒排/分析器/BKD | 3-4 | 02 文档 |
| 分布式组 | Raft/WAL/分裂/租户 | 2-3 | 05 文档 |
| 兼容组 | ES API/DSL/金标准 | 2-3 | 04 文档 |
| 质量组（可虚线） | 基准/混沌/CI | 1-2 | 06 文档 |

**Rust→Go 人才风险**（概要设计 §14）：原风险「Rust 人才储备」随 Go 选型**自动消解**（Go 工程师池更大、上手快）；新增风险见 §7 R8/R9。核心引擎经验要求转为：C++/Rust 背景的**系统编程思维**（内存布局/缓存友好）+ Go 工程能力，面试与内部培训按此画像。

---

## 7. 风险登记册（继承概要设计 §14 + Go 特有新增）

| # | 风险 | 影响 | 概率 | 缓解 | 对策落点 |
|---|------|------|------|------|---------|
| R1 | ES DSL 兼容长尾深不见底 | GA 延期 | 高 | 矩阵分级 + 金标准门禁 + 拒绝静默兼容 | [04 §3/§9](go-design/04-es-compat-layer.md) |
| R2 | Cross-Link Merge 召回率未验证 | 核心卖点失效 | 中 | M0 起 ann-benchmarks 持续验证 + 回滚快照 | [02 §2.4.3](go-design/02-index-engine.md)、B10 |
| R3 | 稀疏向量生态变化 | 存储格式演进 | 中 | KV 张量与模型解耦 | [02 §5](go-design/02-index-engine.md) |
| R4 | BM25 全局统计实时性 | 分数漂移 | 低 | 刷新周期 + global_stats 可关 | [03 §7](go-design/03-query-engine.md) |
| R5 | 对象存储 WAL 抖动 | 写 P99 退化 | 中 | NVMe 双写热路径 | [05 §2.2](go-design/05-distributed.md)、C3 |
| R6 | 自研量超预期（FST/BKD/HNSW） | M0/M1 延期 | 中 | 接口先行 + M0 简化实现（数组词典/全量重建）预留演进位 | 00 §2.2、02 §2.4.3 |
| R7 | 在线分裂复杂度 | M2 延期 | 中 | Gen 双版本 + 位图继承已定案 + C5 演练 | [05 §3.2](go-design/05-distributed.md)、01 §11 |
| **R8（Go 特有）** | GC mark assist 尖刺破坏 P99 | 性能目标失守 | 中 | mmap 化 + 零指针布局 + 池化 + GOMEMLIMIT（go_* 指标持续观测） | [00 §3.1](go-design/00-overview.md)、06 §1 |
| **R9（Go 特有）** | SIMD 汇编维护成本（双架构） | 迭代减速 | 低 | avo 单点集中（8 内核）+ 纯 Go 参考实现永远在位（降级即正确） | 00 §3.2 |
| **R10（Go 特有）** | 无 tantivy 等价生态，倒排全自研 | 质量风险 | 中 | Lucene 行为金标样本对拍（BM25 数值/fuzz 自动机）+ 模糊测试 | 02 §3、06 §5.1 |

---

## 8. 开放问题跟踪（概要设计 §14 的详细设计结论）

| 概要设计开放问题 | 结论/计划 |
|---|---|
| 1. nested v1 扁平化深度 5+ 性能拐点 | **保持开放**：M1 中安排 PoC（W5.3 附带），数据集：5/7/10 层嵌套文档的索引体积与查询延迟曲线；若拐点显著，v2 评估块级 join 位图优化 |
| 2. 分片分裂的 Delete Bitmap 合并语义 | **已定案**：dual-read 窗口双写 + 路由域求交 + 引用计数回收（[01 §11](go-design/01-storage-engine.md)），M2 W6.6 验证 |
| 3. GPU 索引资源隔离模型 | **维持 M3 决策**：W7.1 启动时定独占卡 vs MIG（依据：目标客户 GPU 供给形态调研） |

---

## 9. 里程碑与文档联动规则

- 每个 WBS 任务开工前：责任人在对应详细设计文档 PR 中确认接口签名（设计是**契约**，实现 PR 偏离需先改设计文档并评审）；
- 每阶段出口评审：检查 [00 §7](go-design/00-overview.md) 痛点对策映射表逐条落地证据（基准/混沌/金标准报告编号）；
- 本文件与详细设计冲突时：**详细设计为准**，本文件仅修排期。

---

*排期基线：2026-10 概要设计冻结后起算。人员到位率不足时优先保 M0 存储/索引/查询主线（W1-W3），兼容层（W4.1）可让渡至 M1 初。*
