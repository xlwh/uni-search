# ADR-002: ClickHouse Feature Import

**Status**: Accepted
**Date**: 2026-10-22
**Task**: 2026-10-22-clickhouse-features-import

## Context

UniSearch 需要提升存储效率、查询性能和数据管理能力。通过对 ClickHouse 源码的代码级深度探索，识别出 6 个可以引入的核心创新功能：倒排索引可合并、可组合编解码器、聚合哈希表特化、TTL 生命周期管理、异步 INSERT 批量合并、投影/预计算。

## Decision

### F1: 倒排索引可合并 — 复用 provider 偏移逻辑

**选择**: 复用 `mergeTextColumnProvider` 的 `newRowID` 递增逻辑，通过 `ComputeRowOffsets` 函数计算 oldDocID→newDocID 映射，`FilterAndRemap` 方法过滤删除行并重映射。

**理由**: 避免修改 `copyMergeRows` 的返回值和调用链，偏移计算独立完成，与现有合并流程零耦合。合并失败时回退到 `ix.Build()` 全量重建。

**替代方案**: (A) 在 `copyMergeRows` 中收集映射表 — 增加内存开销和接口变更；(B) 用存活行数算偏移 — 要求 posting list 已是连续 ID，无法保证。

### F2: 可组合编解码器 — 块 payload 前缀存储链描述

**选择**: `EncodingComposite(7)` 时在 payload 前缀存储 `u8 chain_len + codec IDs`，不改 `BlockIndex` 结构和 `FormatVersion`。

**理由**: 单编码段（1-6）新旧版本均可读，完全向后兼容。复合编码仅 2-3 字节前缀开销。

**替代方案**: (A) BlockIndex 新增 protowire tag — 改变 wire 格式；(B) 列级配置 — 无法按块自适应。

### F3: 聚合哈希表特化 — 同时接入执行管线+优化

**选择**: 在 F3 中同时完成 `GroupAggregator` 接入 `buildOperator`（`case *plan.Aggregate`）和特化哈希表替换。

**理由**: `GroupAggregator` 此前未接入执行管线（生产聚合走 `engine.go` 逐文档解析），分开做会引入中间状态。一步到位减少迭代次数。

### F4: TTL 生命周期管理 — 同一队列+TTL 优先

**选择**: 在 `pickCandidates` 中增加 Rule 0（TTL 优先），TTL 合并与正常合并共享并发槽。

**理由**: 避免独立线程和队列管理的复杂度。TTL 过期段优先于 size-tiered 候选，但不阻塞正常合并。

### F5: 异步 INSERT — 全部记录+按需传播

**选择**: 所有 flush 错误记录到日志和监控；`wait=true` 通过 `close(done)` 广播 + `err` 字段传播；提供 `GET /_async_insert/errors`。

**理由**: 兼顾安全性和可用性。wait=true 符合 ES refresh 语义，wait=false 不阻塞但错误可追溯。

### F6: 投影/预计算 — 混合方案

**选择**: 聚合投影用新 `ProjectionScan` 逻辑节点 + `ProjectionScanOp` 算子；普通投影复用 `Scan` 节点添加排序列引用。

**理由**: 两种投影有不同的执行路径（读预计算状态 vs 读排序列文件），职责清晰。聚合投影是全新路径需要独立节点，普通投影是现有 Scan 的扩展。

## Consequences

- **向后兼容**: 所有 6 个功能均保持 ES API 100% 兼容，F2 复合编码选择暂禁用以维护现有测试
- **新增依赖**: 无外部依赖，全部 Go 原生实现
- **测试覆盖**: 46 个 Go 单元测试 + 19 个 Python E2E 测试 + 182 个全量回归通过
- **后续激活**: F2 `chooseCompositeCodec` 需基准测试验证后激活；F6 `ProjectionScan` 需投影存储层实现后激活
