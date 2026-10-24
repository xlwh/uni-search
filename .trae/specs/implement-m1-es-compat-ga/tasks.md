# Tasks — M1 ES 兼容 GA

> 任务编号 W5.x 对齐 development-plan.md §3；每个任务内嵌 Go 单元测试与 DoD 验证，测试通过且符合设计契约方可勾选。

## 任务序列

- [ ] **T19 FST 词典与 Fuzzy 自动机（W5.1，周 1-3）**
  - [ ] T19.1 在 `internal/index/inverted/fst.go` 实现 `TermDict` 接口的 FST 版本（`Get/Fingerprint/Iterate` 签名不变）
  - [ ] T19.2 实现 Levenshtein 自动机（状态数 ≤32 防爆炸），在 FST 上遍历产出 fuzzy 命中词项
  - [ ] T19.3 SegmentBuilder 在冻结表 flush 时按 sorted 顺序流式喂入构建 FST；FST 只读 mmap 加载
  - [ ] T19.4 单测：FST vs 数组词典 `Get` 全量词项结果一致；1 亿词项内存较数组版降 ≥60%；fuzziness AUTO 返回结果与无状态上限一致
  - [ ] T19.5 回归：M0 R10 BM25 分数与 R14 DSL fuzz 解析全绿

- [ ] **T20 WAND / Block-Max WAND 剪枝（W5.2，周 2-4）**
  - [ ] T20.1 posting 块头顺手记录块内 `maxTF`；SegmentBuilder footer 记录段级 `maxScore` 上界
  - [ ] T20.2 在 `internal/engine/exec/operators.go` 新增 `WANDOp`：词项 posting 游标 + 上界分数堆剪枝；Block-Max 增强用块内 maxTF 收紧
  - [ ] T20.3 触发条件：纯词项查询（无 filter）+ `track_total_hits=false` + TopK 排序
  - [ ] T20.4 单测：B4 场景（1 亿文档纯 BM25 长尾查询）P99 <80ms；与暴力扫描 top-k 重合率 = 1.0；复用 R9 框架对拍

- [ ] **T21 聚合全量（W5.3，周 2-6）**
  - [ ] T21.1 新增 `internal/engine/agg/` 包：向量化分组（flat 开放寻址 + 池化）+ `sum/min/max/count` SIMD 归约（复用 `internal/simd`）+ `avg` 拆 sum+count
  - [ ] T21.2 HLL++（sparse/dense 自适应 + `precision_threshold` 语义）+ t-digest centroid 序列化
  - [ ] T21.3 `api/proto/plan.proto` 追加 `PartialState` message（HLLSketch/TDigestSketch/TopState/SumState oneof）；Go 生成桩代码
  - [ ] T21.4 final 阶段合并：terms 桶同 key 加和、HLL 寄存器按 max、t-digest centroid 归并重压缩；桶裁剪 size/shard_size 双级
  - [ ] T21.5 在 `internal/engine/exec/operators.go` 新增 `AggOp`；DSL parser 扩展 `aggs` 子树 AST
  - [ ] T21.6 实现 bucket 18 类 / metric 19 类 / pipeline 13 类（03 §6.3 清单逐条对齐 §8.4 矩阵）
  - [ ] T21.7 单测：partial+final 合并结果与单段全量一致（cardinality 误差 <1%）；pipeline 链 `date_histogram + derivative + moving_avg` 数值与 ES 对拍 <1e-9

- [ ] **T22 search_after / PIT / scroll（W5.4，周 3-4）**
  - [ ] T22.1 `Snapshot{ReadUpTo}` 注册表（TTL 5 分钟、LRU 上限 10000、过期回收）
  - [ ] T22.2 sortValues 序列化：`(sortValues..., _shard_doc)` 严格全序 + tie-breaker 强制追加；base64 透明传输；逐 key 二进制比较器
  - [ ] T22.3 新增 `POST /{index}/_pit` + `POST /_pit/delete` 路由与 handler（`internal/server/esapi/pit.go`）
  - [ ] T22.4 `scroll` 转译：`?scroll=5m` → 内部创建 PIT → 首响 `scroll_id`；`POST /_search/scroll` 推进 search_after；`DELETE /_search/scroll` 销毁 PIT
  - [ ] T22.5 scroll `slice` 自动切分返回 unsupported（差异清单标注 workaround: `pit_slice` 扩展参数）
  - [ ] T22.6 单测：深分页 100 万行稳定（无 OOM、每页 <100ms、无重无漏）；PIT LRU 回收后访问返回 404；scroll 路径结果与 search_after 路径一致

- [ ] **T23 highlight / suggester（W5.5，周 4-6）**
  - [ ] T23.1 posting `pos.*` offsets 块（02 §3.2）在 SegmentBuilder 落地（高亮字段才建）
  - [ ] T23.2 `internal/engine/exec/highlight.go`：unified highlighter（TopK 后处理 → 按分析器重切分原文 → 匹配词项 offsets → density × coverage 选 top fragments）
  - [ ] T23.3 term/phrase suggester：词典 Levenshtein 自动机（复用 R19 FST 基建）
  - [ ] T23.4 completion suggester：独立小 FST `suggest.{field}.fst`，前缀遍历 + top-weight 剪枝
  - [ ] T23.5 单测：highlight fragment 与 ES 输出 diff 通过；completion top-weight 排序正确；term suggester 编辑距离返回正确

- [ ] **T24 painless 脚本子集（W5.6，周 3-5）**
  - [ ] T24.1 新增 `internal/engine/script/` 包：`Script{Lang,Source,Params}` + `Evaluator` 接口（AST 直译，v2 预留字节码编译接口）
  - [ ] T24.2 支持：算术/逻辑/比较、字符串 length/substring/startsWith、`doc['f'].value`、`params.x`、`_score`、`Math.*` 子集、三目、`emit`
  - [ ] T24.3 向量函数一等：`cosineSimilarity`/`dotProduct`/`l1norm`/`l2norm`（复用 `internal/simd`）
  - [ ] T24.4 安全：静态语法白名单 + 求值步数预算 10 万步超限中断（无 IO/反射/无限循环）
  - [ ] T24.5 单测：`script_score` 向量打分不衰减（数值正确）；死循环脚本 10 万步后中断返回 `script_exception`；不支持的语法返回 `script_exception`

- [ ] **T25 rescore / collapse / runtime_mappings / profile（W5.7，周 5-6）**
  - [ ] T25.1 `RescoreOp`（TopN window 内二次打分，`query_weight`/`rescore_query_weight` 语义对齐 ES，window 默认 10）
  - [ ] T25.2 `CollapseOp`（按字段分组取首 + `inner_hits` 组内 top-N 保留）
  - [ ] T25.3 `RuntimeMapOp`（Scan 后逐批计算虚拟列，仅参与过滤/排序/聚合不可索引）
  - [ ] T25.4 `?_profile=true` 输出 ES profile JSON：`profile.shards[].searches[].query[]` 含 `type/description/time_in_nanos/breakdown`，breakdown 字段名对齐 ES（plan/scan/filter/score/agg 阶段填充）
  - [ ] T25.5 单测：rescore 数值正确；collapse+inner_hits 结构对齐；runtime_mappings 虚拟列可被 agg 引用但不可被索引；profile type 映射 BooleanQuery/TermQuery 等

- [ ] **T26 ES API 全量扩展（W5.8，周 3-8）**
  - [ ] T26.1 新增 16 个 handler（`internal/server/esapi/` 下）：msearch / fieldcaps / termsenum / explain / delete_by_query / update_by_query / reindex / template / ilm / cat 全量 / flush / forcemerge / pit / scroll
  - [ ] T26.2 路由按 04 §2.2 矩阵注册：`POST /{index}/_msearch` + `POST /_msearch`；`GET /{index}/_field_caps`；`GET /{index}/_terms_enum`；`POST /{index}/_explain/{id}`；by_query 三端点；`POST /_reindex`；`PUT /_index_template/{name}` + `PUT /_component_template/{name}` + `GET /_index_template`；`PUT /_ilm/policy/{name}` + `PUT /{index}/_ilm/start`；`GET /_cat/{shards,nodes,allocation}`；`POST /{index}/_flush` + `POST /{index}/_forcemerge`；`POST /_pit` + `POST /_pit/delete`；`POST /_search/scroll` + `DELETE /_search/scroll`
  - [ ] T26.3 `_reindex` / by_query 走 `TaskManager`（in-flight 任务表，`_tasks` API 可查进度/取消，requests_per_second 令牌桶，conflicts=proceed|abort）
  - [ ] T26.4 ILM 子集：`hot→warm→cold→delete` + rollover/forcemerge/shrink/delete，状态机 ticker 驱动
  - [ ] T26.5 单测：每端点逐 API 金标准用例通过（响应骨架与 ES diff）；_reindex 任务可取消；ILM rollover 别名切换正确

- [ ] **T27 BM25 全局统计（W5.9，周 5-6）**
  - [ ] T27.1 SegmentBuilder footer 记录 `term_stats` 摘要（高频 top 10 万词项的 docFreq）
  - [ ] T27.2 meta 集群聚合：段注册上报 → 增量合并（5 分钟周期刷新）→ coordinator 下推到各分片快照缓存
  - [ ] T27.3 `global_stats` settings 开关（默认 false 对齐 ES）；启用时 IDF 用全局表
  - [ ] T27.4 新词项回退局部 IDF；差异清单标注（04 §3.3）
  - [ ] T27.5 单测：跨段场景同文档分数一致（相对误差 <1e-9）；新词项回退路径覆盖；global_stats=false 时与 M0 R10 行为一致

- [ ] **T28 多租户 shared-shard 配额（W5.10，周 6-8）**
  - [ ] T28.1 `internal/meta/` 增 `TenantMeta{Mode, Quota, ResourceGroup}` 与租户上下文透传（默认 `default` 租户）
  - [ ] T28.2 配额四维：存储字节 / 向量条数 / 查询 QPS / 写入 TPS；心跳聚合到 meta
  - [ ] T28.3 cgroup v2（CPU/内存/IO 由节点代理下发）+ per-tenant 查询并发 semaphore + 写入 TPS 令牌桶
  - [ ] T28.4 超额返回 `429` + `circuit_breaking_exception` + 租户标识 header；`GET /_cat/tenant` 扩展端点
  - [ ] T28.5 单测：配额超限注入测试返回 429；并发 semaphore 第 N+1 请求等待并超时返回 429；cgroup 配置下发可观测

- [ ] **T29 兼容金标准门禁（W5.11，周 7-8）**
  - [ ] T29.1 `test/golden/` 维护 elasticsearch-py rest-api tests yaml 子集 + go-elasticsearch 集成测试 + ES 官方 rest-api-spec yaml（按 04 §3 矩阵筛可用例）
  - [ ] T29.2 每夜全量 + PR 增量（按路由变更筛选）；断言 HTTP 状态 + JSON 结构（字段级 diff + 时间戳白名单）
  - [ ] T29.3 自动生成兼容矩阵报告（04 §3 表 + 实测状态列），随版本发布
  - [ ] T29.4 出口评审：elasticsearch-py + go-elasticsearch 套件通过率 ≥90%（GA 门槛）

## 任务依赖

- **T19 FST** 是 T20 (WAND)、T23 (highlight/suggester) 的前置（共享 FST 词典与 Levenshtein 自动机基建）
- **T21 聚合** 是 T25 (runtime_mappings → agg 引用虚拟列) 与 T26 (_msearch 走 _search+aggs) 的前置
- **T22 PIT** 是 T23 (scroll 转译)、T26 (_reindex/by_query 滚动扫描) 的前置
- **T24 painless** 是 T25 (runtime_mappings/script_fields/bucket_script) 的前置
- **T27 BM25 全局统计** 与 T20 WAND 共享段 footer term_stats 摘要；可与 T20 并行但合并到主分支前需对齐 footer schema
- **T28 多租户** 与 T29 门禁独立，可与 T26 并行；但 T29 必须在 T26 全量 API 完成后才能跑全量金标准
- **T29 金标准门禁** 是出口评审的最终门禁，依赖 T19-T28 全部完成

## 可并行批次建议

- **批次 A（周 1-3）**：T19 FST 基建独立推进
- **批次 B（周 2-6）**：T20/T21/T22/T24 共四线并行（T20 等 T19；T22 独立；T24 独立；T21 独立）
- **批次 C（周 4-6）**：T23 等 T19；T25 等 T24；T26 部分路由可早开（pit/scroll/cat 依赖 T22）
- **批次 D（周 5-8）**：T27 与 T20 共享 footer schema；T28 独立；T26 剩余路由收口；T29 是最后门禁
