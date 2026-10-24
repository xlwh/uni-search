# 项目路线图 (Project Roadmap)

## 1. In Progress (正在进行)
*   [ ] 2026-10-20-gap-fill - 补齐设计文档 vs 代码审计的 9 个功能缺口
    *   已完成（2026-10-24 复审确认）: G1 Prometheus + G2 OTel + G3 CI/CD + G4 gRPC + G5 K8s Operator reconciler + G9 goccy/json
    *   剩余: G6 Online split（状态机真实，数据路径 hooks 未接线）/ G7 Cross-Link merge（核心合并仍是桩）/ G8 冷热分层迁移（量化与上传真实，写回与空间回收缺失）
    *   出口门禁: 审计覆盖率 ≥92%, go build/test/vet 全绿
    *   Spec: `.ai/active_specs/2026-10-20-gap-fill/`
    *   状态: in_progress
*   [ ] 2026-10-20-m2-cloud-native-ga - M2 云原生 GA 全量 W6.1-W6.10
    *   W6.1-W6.10 代码框架 + hashicorp/raft + minio-go + gRPC 集成
    *   25 包编译/测试/vet 全绿，17 个 M2 集成测试
    *   DoD 基础设施验证（混沌 C1-C6 / 10 亿基准 / 存算分离成本报告）待集群环境补齐
    *   Spec: `.ai/active_specs/2026-10-20-m2-cloud-native-ga/`
    *   状态: planned


## 2. Planned (已计划)
*   [ ] Project Initialization (Run `/spec-init`)


## 3. Completed (已完成)
*   [x] 2026-10-23-web-admin-console - 为 UniSearch 新增 Web 管控页面
    *   Vue 3 + TypeScript 前端，go:embed 内嵌，独立端口 :9201
    *   8 个管控模块：集群总览/索引管理/搜索控制台/文档管理/多租户管理/快照与备份/监控指标/同义词与管道
    *   出口门禁: make build 全绿, 8 个模块功能可用, go build/test/vet 全绿
    *   归档路径: `.ai/archive/2026-10-23-web-admin-console/`
    *   状态: archived
*   [x] 2026-10-23-qdrant-features-import - 从 Qdrant 引入高级搜索与存储功能到 UniSearch
    *   P0: 稀疏向量倒排索引 + DBSF 融合 + MMR 多样性搜索 + Float16 存储 + Query API 统一管道
    *   P1: TurboQuant 量化 + Multi-vector/ColBERT MaxSim + Recommend/Discover 搜索 + Formula 重排序 + WAL Delta 增量同步
    *   P2: P-square 分位数估计 + HNSW 图修复 + 多语言分词 + BM25 嵌入生成 + RBAC + 集合配额+审计
    *   出口门禁: make build 全绿, go build/test/vet 全绿, 现有 E2E 全绿, ES 兼容 Golden 100%, 段格式 v2 向后兼容 v1
    *   归档路径: `.ai/archive/2026-10-23-qdrant-features-import/`
    *   状态: archived
*   [x] 2026-10-23-iresearch-features-import - 从 iresearch 引入高级分析器 + I/O 优化 + 查询打分增强 13 个特性
    *   Phase 1 (P0): IOAdvice+mmap 全量迁移 + Proxy Filter 缓存过滤器
    *   Phase 2 (P1): MinHash/Collation/Pipeline 分析器 + BlockCache/列预热/段GC + Granular Range/SamePosition 查询
    *   Phase 3 (P2): FastText 分类/近邻分析器 + NGram 相似度打分 + TF-IDF 评分器
    *   出口门禁: go build/test/vet 全绿, 294 Python E2E 全绿, 78 新增测试全绿, 8 个预存测试修复
    *   归档路径: `.ai/archive/2026-10-23-iresearch-features-import/`
    *   状态: archived
*   [x] 2026-10-22-es-feature-gap-fill - 对比 ES 核心功能，补齐 9 个功能域 50+ 个功能点
    *   P0: prefix/regexp/constant_score/boosting/dis_max + rescore/collapse/post_filter + _open/_close/_stats/_segments + _cluster/state/stats/_nodes + 快照仓库+创建/恢复 + Ingest Pipeline 框架 + 异步搜索 + 同义词 CRUD + Term Vectors
    *   P1: function_score/script_score/geo_distance/geo_bounding_box + search_template/_validate/query + shrink/split/rollover/clone/resolve + _cluster/settings+_cat扩展 + 基础Processor+条件处理 + 同义词过滤器+分析器集成 + Term Vectors参数
    *   P2: more_like_this/nested(原生)/simple_query_string/combined_fields + runtime_mappings/script_fields + cache/clear/recovery + 高级Processor
    *   P3: distance_feature/rank_feature/pinned/terms_set/intervals + 数据流 + 插件机制
    *   出口门禁: ES 兼容 API 覆盖率 ~90%, go build/test/vet 全绿, golden test 通过
    *   归档路径: `.ai/archive/2026-10-22-es-feature-gap-fill/`
    *   状态: archived
*   [x] 2026-10-20-m3-ecosystem-advanced - M3 生态与进阶 W7.2-W7.8（W7.1 GPU 暂缓）
    *   W7.2 内置 rerank / W7.3 geo_shape / W7.4 percolate / W7.5 插件SDK
    *   W7.6 uniadm es-compat / W7.7 K8s Operator / W7.8 Go SDK
    *   出口门禁: 首个 ES 存量用户整体迁移案例（待达成，uniadm es-compat scan 已就绪）
    *   归档路径: `.ai/archive/2026-10-20-m3-ecosystem-advanced/`
    *   状态: archived
*   [x] 2026-10-22-clickhouse-features-import - 引入 ClickHouse 优秀功能到 UniSearch
    *   P1: F1 倒排索引可合并 + F2 可组合编解码器（基础设施）
    *   P2: F3 聚合哈希表特化（引擎优化）
    *   P3: F4 TTL 生命周期管理 + F5 异步 INSERT 批量合并
    *   P4: F6 投影/预计算（全量实现：minmax_count + 聚合投影 + 普通投影）
    *   出口门禁: 6 功能全量交付, 46 Go 测试 + 19 Python E2E 测试 + 182 全量回归通过, ES API 100% 兼容, go build/test/vet 全绿
    *   归档路径: `.ai/archive/2026-10-22-clickhouse-features-import/`
    *   状态: archived
*   [x] 2026-10-22-vsag-algo-integration - 从 vsag 引入优化算法提升向量搜索性能
    *   Phase 1: SIMD 距离内核（ISA 检测 + pure Go 基线 + 三级回退分发）
    *   Phase 2: RaBitQ 优化（投影矩阵扁平化 + 硬件 popcount + codes 扁平化）
    *   Phase 3: 标准 PQ 量化（K-means++ 训练 + LUT 查表）
    *   Phase 4: 自适应图构建（alpha 剪枝 + 距离缓存 + ODescent 批量构建）
    *   Phase 5: Elias-Fano 邻接压缩（层级编码，压缩 62.5%）
    *   出口门禁: 163 Python 集成测试全绿, go build/test 全绿, simd_asm 编译通过
    *   归档路径: `.ai/archive/2026-10-22-vsag-algo-integration/`
    *   状态: archived
*   [x] 2026-10-21-deep-gap-remediation - 深度审计差距修复：7 Bug + 67 项差距全量补齐
    *   第1批: 存储引擎+索引引擎（7 Bug修复 + 幂等写入 + 合并索引重建 + memtable搜索 + SIMD汇编 + 稀疏向量 + Cross-Link单堆 + Iterative过滤）
    *   第2批: 查询引擎+ES兼容（47种聚合全量 + PartialState + PIT/scroll/search_after + 路由补齐 + ES官方yaml金标准）
    *   第3批: 分布式架构（LogStore NVMe + raft接口 + Coordinator gRPC + ReadIndex + Rebalance + Cold mmap + 混沌RTO<30s）
    *   第4批: 质量保障/CI（日志规范 + 尾采样 + 健康探针 + uni-bench B1-B10 + GitHub Workflow + distroless + 性能回归）
    *   第5批: 二次审查修复（DedupWindow LRU刷新 + deprecation Warning + if_primary_term校验 + _forcemerge flush + _terms_enum实现）
    *   出口门禁: 审计覆盖率 ≥95%, go build/test/vet 全绿, 混沌 C1 RTO<30s
    *   归档路径: `.ai/archive/2026-10-21-deep-gap-remediation/`
    *   状态: archived
*   [x] 2026-10-20-m1-es-compat-ga - M0 引擎接线 + M1 ES 兼容 GA 全量
    *   Phase 0: M0 引擎接线（14/14 项）
    *   Phase 1-11: M1 ES 兼容 GA（W5.1-W5.11 + 9 个集成 Gap 全部关闭）
    *   Golden 27/27 = 100%, go build/test/vet 全绿
    *   归档路径: `.ai/archive/2026-10-20-m1-es-compat-ga/`
    *   状态: archived
*   [x] 2026-02-02-init-core-context-file - 创建核心文档 (`00_` - `05_`)
    *   归档路径: 无归档
*   [x] 2026-02-01-init-ai-structure - 初始化 `.ai` 根目录结构
    *   归档路径: 无归档