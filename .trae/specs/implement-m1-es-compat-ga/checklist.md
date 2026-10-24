# Checklist — M1 ES 兼容 GA

> 验收时逐项检查并勾选；每项对应 spec.md 的 Requirement 与 tasks.md 的任务。

## 索引引擎
- [ ] R19 FST 词典：1 亿词项内存较数组版降 ≥60%；Fuzzy Levenshtein 自动机状态数 ≤32；`Get/Fingerprint/Iterate` 签名不变
- [ ] R20 WAND/BMW：纯词项查询 P99 <80ms；top-k 与暴力扫描重合率 = 1.0；触发条件（无 filter + track_total_hits=false + TopK）

## 查询引擎
- [ ] R21 聚合全量：partial+final 合并结果与单段全量一致（cardinality <1%）；bucket 18/metric 19/pipeline 13 类清单全过；§8.4 矩阵 full 项全过
- [ ] R22 search_after/PIT/scroll：深分页 100 万行稳定（无 OOM、每页 <100ms、无重无漏）；PIT TTL 5 分钟 LRU 上限 10000；scroll 转译等价；slice 返回 unsupported + workaround 标注
- [ ] R23 highlight/suggester：unified highlighter fragment 与 ES diff 通过；completion top-weight 排序；term/phrase Levenshtein 正确
- [ ] R24 painless 脚本：script_score 向量打分不衰减；步数预算 10 万步死循环中断；不支持的语法返回 `script_exception`
- [ ] R25 rescore/collapse/runtime_mappings/profile：window 二次打分数值正确；collapse+inner_hits 结构对齐；runtime_mappings 虚拟列可 agg 引用不可索引；profile type/breakdown 对齐 ES

## 服务与集成
- [ ] R26 ES API 全量：新增 16 个 handler 路由注册全；_msearch/_field_caps/_terms_enum/_explain/_reindex/by_query/template/ILM/_cat 全量/_flush/_forcemerge/_pit/_search/scroll 逐 API 金标准用例通过
- [ ] R27 BM25 全局统计：global_stats=true 跨段分数一致（<1e-9）；新词项回退局部；default=false 与 M0 R10 行为一致
- [ ] R28 多租户 shared-shard：配额四维超限 429 + `circuit_breaking_exception`；cgroup v2 配置可观测；per-tenant semaphore 第 N+1 等待+超时
- [ ] R29 兼容金标准门禁：elasticsearch-py + go-elasticsearch 套件通过率 ≥90%；PR 增量筛选；兼容矩阵报告自动生成归档

## 总门禁
- [ ] `go build ./...` 成功（CGO_ENABLED=0）
- [ ] `go test ./...` 全绿（含所有 R19-R29 场景测试；慢测试可按 M0 模式以 `-run` 子集单独验证并标注）
- [ ] `go vet ./...` 零报告
- [ ] 实现与 docs/go-design/02 §3.1/3.4/3.5/7、03 §6/7/8/9/10/11、04 §2.2/3/6/9、05 §5 接口签名无偏离（抽查 TermDict/Evaluator/AggOp/PIT/Suggester/Highlighter/TaskManager/TenantMeta 等核心接口）
- [ ] 金标准套件通过率 ≥90%（GA 门槛，development-plan §3 W5.11 DoD）
- [ ] 兼容矩阵报告归档（✅ 占比 ≥80%，概要设计 §13）
