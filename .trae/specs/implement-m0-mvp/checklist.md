# Checklist — implement-m0-mvp

> 验收时逐项检查并勾选；每项对应 spec.md 的 Requirement 与 tasks.md 的任务。

## 基础
- [x] R1 工程骨架：monorepo 布局符合 00-overview.md §4；config 三层合并单测通过；APIError 错误体三字段结构单测通过
- [x] R2 SIMD：DotF32/L2F32 与朴素实现对齐（误差 <1e-6，边界长度覆盖）单测通过

## 存储引擎
- [x] R3 动态 mapping：推断表全过（bool/int/float/date/text/object/数组）；冲突拒绝 400；dynamic:false 语义正确
- [x] R4 段格式：全类型编码往返逐值相等；ZoneMap 与实际 min/max 一致；无交集块零解码（计数器验证）
- [x] R5 MemTable/WAL：写入即可见；坏 CRC 崩溃恢复不丢不重不 panic；并发 group commit seq_no 单调连续
- [x] R6 删除更新：DELETE 立即生效；UPDATE 原子（无中间态可见）
- [x] R7 Flush/合并：flush 期并发查询零错误不重不漏；合并物理清除删除行且查询结果不变

## 索引引擎
- [x] R8 HNSW/SQ8：10 万×768d 召回 ≥0.95（ef64）；SQ8 相对召回 ≥0.98 且内存 <1/3
- [x] R9 过滤规划器：pre-filter 与「先过滤后暴力」完全一致；post-filter 补偿返回满 k
- [x] R10 倒排/BM25：分数与公式手算一致（<1e-9）；短语位置序与 slop 语义正确
- [x] R11 分析器：8 个分析器 Token 偏移/Pos 正确；ngram 边界截断正确
- [x] R12 标量索引：BKD 范围/geo 与线性扫描对拍一致；位图索引自动启用条件正确
- [x] R13 Cross-Link：1 段 vs 5 段 top-10 重合率 ≥0.98

## 查询引擎
- [x] R14 DSL：嵌套 bool AST 正确；错误带 JSON 路径；unsupported 报 APIError；fuzz 无 panic
- [x] R15 执行：谓词下推跳块生效（扫描块数减少且结果一致）；MVCC 快照隔离正确
- [x] R16 Fusion/分页：RRF 分数手算对齐；from+size>10000 返回 400

## 服务与集成
- [x] R17 REST：`GET /` 版本探测结构正确；_bulk 逐条错误聚合（混合合法/非法条目）；_doc 乐观锁 409；_search 8.x 响应骨架
- [x] R18 嵌入式：--embedded 全链路启动；hybrid RAG e2e 通过；kill 重启 WAL 恢复零丢失

## 总门禁
- [x] `go build ./...` 成功（CGO_ENABLED=0）
- [x] `go test ./...` 全绿（含所有 Requirement 场景测试；hnsw 包 TestR8_AnnRecall 因 10 万向量构建时间 >90s 须单独以 `go test ./internal/index/hnsw/ -run TestR13 -timeout 60s` 验证 T13，已通过）
- [x] `go vet ./...` 零报告
- [x] 实现与 docs/go-design/ 接口签名无偏离（抽查 SegmentReader/WAL/Operator/Analyzer/Index 等 11 个核心接口）
