# M1 ES 兼容 GA Spec

## Why

M0 已完成嵌入式 MVP（R1–R18 全部通过），但 ES 兼容覆盖面仅限 M0 子集（bool/term/match/knn/hybrid + 基础 _doc/_bulk/_search/_count/_mget/mapping/alias/settings/_analyze/_refresh/_cat health+indices）。

真实 ES 存量迁移要求「金标准通过率 ≥90%」（development-plan §3 W5.11、概要设计 §11）。M1 把倒排词典升级到 FST、聚合全量上线、补齐 search_after/PIT/scroll/highlight/suggester/painless/profile 全套修饰能力、API 扩展到 _msearch/_field_caps/_terms_enum/_explain/_reindex/template/ILM 全量、引入 BM25 全局统计与多租户 shared-shard 配额，最终通过 elasticsearch-py 与 go-elasticsearch 套件的金标准门禁。

## What Changes

- **FST 词典**替换数组词典（02 §3.1），`TermDict` 接口签名不变；Fuzzy Levenshtein 自动机在 FST 上遍历（状态数 ≤32）
- **WAND / Block-Max WAND** 词项剪枝（02 §3.4）：纯词项查询 P99 <80ms
- **聚合全量**（03 §6）：向量化分组 + HLL++ / t-digest + `PartialState` protobuf 协议 + 18 bucket / 19 metric / 13 pipeline 类型
- **search_after / PIT / scroll**（03 §8、04 §6.4）：sortValues 严格全序游标 + PIT TTL 5 分钟 LRU 上限 10000 + scroll 转译为 PIT+search_after
- **highlight / suggester**（02 §3.5/§7、03 §10）：unified highlighter + term/phrase Levenshtein + completion FST
- **painless 脚本子集**（03 §9）：`Script{Lang,Source,Params}` + `Evaluator` AST 直译 + 步数预算 10 万步 + 向量函数一等（cosineSimilarity/dotProduct/l1norm/l2norm）
- **rescore / collapse / runtime_mappings / profile**（03 §10/§11）：修饰族算子 + ES profile JSON 结构
- **API 全量**（04 §2.2）：新增 _msearch、_field_caps、_terms_enum、_explain、_delete_by_query、_update_by_query、_reindex、_index_template、_component_template、_ilm/policy、_ilm/start、_cat/{shards,nodes,allocation}、_flush、_forcemerge、_search/scroll、_pit
- **BM25 全局统计**（03 §7）：`global_stats=true` settings 开关 + 段 footer term_stats 摘要增量合并
- **多租户 shared-shard**（05 §5）：资源组配额四维（存储/向量条数/查询 QPS/写入 TPS）+ cgroup v2 + 并发 semaphore + TPS 令牌桶 + 429
- **金标准门禁**（04 §9）：elasticsearch-py + go-elasticsearch 套件 ≥90%（GA 门槛）

## Impact

- **Affected specs**: M0 implement-m0-mvp（依赖其 R8 HNSW / R10 倒排 / R14 DSL / R15 执行 / R16 Fusion / R17 REST 基础）；下游 M2 在线分裂依赖 M1 全局统计 + 聚合 PartialState 协议
- **Affected code**:
  - `internal/index/inverted/` — 词典从数组升级 FST，posting 列追加块级 maxScore 上界、offsets 块
  - `internal/engine/agg/`（新）— 向量化分组 + HLL/t-digest + PartialState
  - `internal/engine/exec/` — 新增 AggOp / RescoreOp / CollapseOp / HighlightOp / SuggestOp / RuntimeMapOp / ScriptScoreOp
  - `internal/engine/dsl/` — 扩展 AST：aggs 子树、search_after、highlight、suggest、collapse、rescore、runtime_mappings、profile、painless 节点
  - `internal/engine/script/`（新）— painless 子集 AST 直译 + 向量函数
  - `internal/server/esapi/` — 新增 16 个 handler（msearch/fieldcaps/termsenum/explain/byquery/reindex/template/ilm/pit/scroll/cat 全量/flush/forcemerge）
  - `internal/meta/` — 租户模型 + 资源组 + 配额四维
  - `internal/embedded/` — shared-shard 配额接入 + global_stats settings + PIT LRU
  - `test/golden/`（新）— elasticsearch-py / go-elasticsearch 套件 + ES rest-api-spec yaml 子集

## ADDED Requirements

### Requirement: R19 FST 词典与 Fuzzy 自动机

系统 SHALL 提供 `TermDict` 接口的 FST 实现（替换 M0 排序数组），保持 `Get(term []byte) (TermID, bool)`、`Fingerprint(term []byte) uint64`、`Iterate(prefix []byte) TermIterator` 三个方法签名不变。FST 字节序构建、输出共享前缀最大化压缩、只读 mmap；`fuzziness` DSL 在 FST 上走 Levenshtein 自动机遍历，自动机状态数上限 32（对齐 ES 限制）。

#### Scenario: FST 词典内存压缩
- **WHEN** 1 亿词项的索引从数组词典迁移到 FST 词典
- **THEN** FST 词典内存占用较数组版降低 ≥60%
- **AND** `Get` 查询延迟与数组二分同档（≤1.5×）

#### Scenario: Fuzzy 自动机状态爆炸防护
- **WHEN** 用户提交 `fuzziness: AUTO`（默认 2）+ 极长查询词项
- **THEN** Levenshtein 自动机状态数被裁剪到 ≤32
- **AND** 返回结果与无状态上限版本完全一致

#### Scenario: FST 前缀遍历
- **WHEN** `prefix` / `wildcard` / `regexp` 查询触发 `Iterate(prefix)`
- **THEN** 返回的 `TermIterator` 按 TermID 升序输出所有匹配词项
- **AND** 不全量物化（流式接口）

### Requirement: R20 WAND / Block-Max WAND 剪枝

系统 SHALL 对纯词项查询（无 filter、`track_total_hits=false`、TopK 排序）启用 WAND 与 Block-Max WAND 剪枝：词项 posting 游标 + 上界分数（`maxTF × IDF`）堆剪枝；块内最大 tf 上界（posting 块头顺手记录）进一步收紧；跨段以「段级 maxScore 上界」（构建期 footer 记录）再做一层剪枝。

#### Scenario: 纯 BM25 长尾查询 P99
- **WHEN** 1 亿文档库执行纯词项 TopK 查询（无 filter、track_total_hits=false）
- **THEN** P99 延迟 <80ms（基准 B4）
- **AND** 返回结果与暴力扫描完全一致（top-k 重合率 = 1.0）

#### Scenario: Block-Max 上界不丢结果
- **WHEN** posting 块内 tf 上界被记录在块头
- **THEN** WAND 剪枝跳过的块其上界 < 当前第 K 名分数
- **AND** 与「先过滤后暴力」对拍完全一致（复用 R9 用例框架）

### Requirement: R21 聚合全量

系统 SHALL 实现向量化分组聚合（flat 开放寻址 `[]uint64 keys + []float64/[]int64 slots`，零指针池化）、HLL++（sparse/dense 自适应 + `precision_threshold`）、t-digest centroid、`PartialState` protobuf 协议（HLLSketch/TDigestSketch/TopState/SumState oneof）。聚合类型清单：bucket 18 类、metric 19 类、pipeline 13 类（03 §6.3 逐条对齐概要设计 §8.4 矩阵）。

#### Scenario: PartialState 跨段归并正确性
- **WHEN** 单机多段执行 terms + cardinality + percentiles 聚合
- **THEN** partial 阶段产出 `PartialState` protobuf
- **AND** final 阶段合并后结果与单段全量聚合一致（cardinality 误差 <1%）

#### Scenario: bucket / shard_size 双级裁剪
- **WHEN** terms 聚合指定 `size` 与 `shard_size`
- **THEN** partial 与 final 两级桶裁剪均生效（ES 同构）
- **AND** 返回桶按 doc_count 降序，size 限制生效

#### Scenario: pipeline 聚合链
- **WHEN** `date_histogram` + `derivative` + `moving_avg` 链式组合
- **THEN** pipeline 算子按父桶序计算导数与滑动均值
- **AND** 数值与 ES 输出对拍一致（相对误差 <1e-9）

### Requirement: R22 search_after / PIT / scroll 兼容

系统 SHALL 实现 `search_after` 排序键序列化（`(sortValues..., _shard_doc)` 严格全序 + tie-breaker 强制 `_shard_doc`，base64 透明传输，逐 key 二进制比较）；`POST /{index}/_pit` 注册 `Snapshot{ReadUpTo}`（TTL 5 分钟、LRU 上限 10000 个）；`scroll` 接收并内部转译为 search_after+PIT（scroll_id 兼容返回），不维护 ES 原生 scroll 上下文。

#### Scenario: 深分页 100 万行稳定
- **WHEN** 客户端通过 search_after+PIT 翻页 100 万行
- **THEN** 全程不出现 OOM
- **AND** 每页延迟 <100ms
- **AND** 无重复、无遗漏（行数总和 = 100 万）

#### Scenario: scroll 转译兼容
- **WHEN** 客户端使用 `?scroll=5m` + `POST /_search/scroll` + `DELETE /_search/scroll`
- **THEN** 首响返回合法 `scroll_id`（编码 PIT id + sort cursor）
- **AND** 逐批推进结果等价于 search_after+PIT 路径
- **AND** scroll 期间 `slice` 自动切分返回 `unsupported`（差异清单标注 workaround: PIT + search_after + `pit_slice`）

#### Scenario: PIT TTL 与 LRU 回收
- **WHEN** PIT 注册数超过 LRU 上限 10000
- **THEN** 最旧 PIT 被 LRU 回收
- **AND** 后续该 PIT 的 search_after 请求返回 404（pit_not_found_exception）

### Requirement: R23 highlight 与 suggester

系统 SHALL 实现 unified highlighter（posting `pos.*` 的 offsets 块为数据源；TopK 文档按分析器重切分原文 → 匹配词项 offsets → density × coverage 选 top fragments）；term/phrase suggester（词典 Levenshtein 自动机，复用 R19 FST 基建）；completion suggester（独立小 FST `suggest.{field}.fst`，前缀遍历 + top-weight 剪枝）。

#### Scenario: unified highlighter 输出 diff
- **WHEN** _search body 携带 `highlight: {fields: {content: {}}}`
- **THEN** 返回 `hits.hits[].highlight.content[]` 与 ES 同结构（fragment 字符串数组）
- **AND** fragment 边界与 `<em>` 标签位置与 ES 输出 diff 通过

#### Scenario: completion suggester 前缀查询
- **WHEN** 用户提交 `suggest: {song-suggest: {prefix: "nor", completion: {field: "suggest"}}}`
- **THEN** 返回 top-weight 排序的 completion 选项
- **AND** 每项含 `_source` / `text` / `score`（weight）

### Requirement: R24 painless 脚本子集

系统 SHALL 实现 `Script{Lang, Source, Params}` 与 `Evaluator` 接口（AST 直译，v2 预留字节码编译）。支持子集：算术/逻辑/比较、字符串 length/substring/startsWith 等、`doc['f'].value`、`params.x`、`_score`、`Math.*` 子集、三目、`emit`（script_fields）。向量函数一等公民：`cosineSimilarity(query, doc['emb'])`、`dotProduct`、`l1norm`、`l2norm`（复用 `internal/simd`）。安全：无 IO/反射/无限循环；静态语法白名单 + 步数预算 10 万步超限中断报错。

#### Scenario: script_score 向量打分
- **WHEN** 查询携带 `script_score: {script: {source: "cosineSimilarity(params.query, 'emb') + 1.0"}}`
- **THEN** 每个候选文档的 _score = `cosineSimilarity(query, doc['emb']) + 1.0`
- **AND** 不触发衰减（script_score 向量打分不衰减）

#### Scenario: 步数预算 fuzz
- **WHEN** painless 脚本源包含死循环（语法白名单允许的 while 但条件恒真）
- **THEN** 求值步数到达 10 万步上限后中断
- **AND** 返回 `script_exception` 错误体（步数超限）

### Requirement: R25 修饰能力族（rescore/collapse/runtime_mappings/profile）

系统 SHALL 在执行管道后挂接：`rescore`（TopN window 内 `Rescore` 算子二次打分，`query_weight`/`rescore_query_weight` 语义对齐 ES，window 默认 10）；`collapse`（按字段分组取首 + `inner_hits`）；`runtime_mappings`（Scan 后 `RuntimeMap` 算子逐批计算虚拟列，仅参与过滤/排序/聚合不可索引）；`profile`（`?_profile=true` 输出 ES profile JSON 结构：`profile.shards[].searches[].query[]` 含 `type/description/time_in_nanos/breakdown`）。

#### Scenario: rescore window 二次打分
- **WHEN** _search body 携带 `rescore: [{query: {...}, window_size: 50, query_weight: 0.3, rescore_query_weight: 0.7}]`
- **THEN** Top-50 内文档按 `0.3·原分 + 0.7·新分` 重排
- **AND** Top-50 之外的文档顺序不变

#### Scenario: collapse + inner_hits
- **WHEN** `collapse: {field: "user_id"}` + `inner_hits: {name: "group", size: 3}`
- **THEN** 每个 user_id 组仅返回首文档作为主 hit
- **AND** 主 hit 的 `inner_hits.group.hits.hits[]` 携带组内前 3 文档

#### Scenario: runtime_mappings 虚拟列
- **WHEN** `runtime_mappings: {price_usd: {type: "double", script: {source: "doc['price_eur'].value * 1.1"}}}`
- **THEN** 后续 `aggs` 与 `_source` 可引用 `price_usd` 字段
- **AND** 该字段不可被索引（不支持 term/range 查询下推）

#### Scenario: profile 输出对齐 ES
- **WHEN** `_search?_profile=true` 执行 bool + term + agg 查询
- **THEN** 返回 `profile.shards[].searches[].query[]` 含 `type/description/time_in_nanos/breakdown`
- **AND** `type` 映射 ES 查询名（BooleanQuery/TermQuery…）
- **AND** `breakdown` 字段名与 ES 对齐（plan/scan/filter/score/agg 阶段填充）

### Requirement: R26 ES API 全量扩展

系统 SHALL 在 M0 路由表基础上扩展以下路由：`POST /{index}/_msearch` + `POST /_msearch`；`GET /{index}/_field_caps`；`GET /{index}/_terms_enum`；`POST /{index}/_explain/{id}`；`POST /{index}/_delete_by_query`；`POST /{index}/_update_by_query`；`POST /_reindex`；`PUT /_index_template/{name}` + `GET /_index_template` + `PUT /_component_template/{name}`；`PUT /_ilm/policy/{name}` + `PUT /{index}/_ilm/start`；`GET /_cat/{shards,nodes,allocation}`；`POST /{index}/_flush` + `POST /{index}/_forcemerge`；`POST /_pit` + `POST /_pit/delete`；`POST /_search/scroll` + `DELETE /_search/scroll`。

#### Scenario: _msearch 批量查询
- **WHEN** 客户端 POST NDJSON（每行一个 _search body）到 `/{index}/_msearch`
- **THEN** 返回 `responses[]` 数组，每项是一个完整 _search 响应骨架
- **AND** 顺序与请求一一对应

#### Scenario: _field_caps 元数据
- **WHEN** `GET /{index}/_field_caps?fields=*`
- **THEN** 返回 `fields.{field_name}.{type}.{searchable, aggregatable}` 结构
- **AND** 与 ES 输出 diff 通过

#### Scenario: _reindex 批处理
- **WHEN** `POST /_reindex` body 含 `source.dest` 与 `conflicts=proceed`
- **THEN** 后台滚动扫描（search_after+PIT）+ 批量写入（默认 1000 docs/批）+ `requests_per_second` 令牌桶限速
- **AND** 返回 `task: {_id}` 可在 `_tasks` API 查进度/取消

#### Scenario: _cat 全量
- **WHEN** `GET /_cat/shards?v` / `GET /_cat/nodes?v` / `GET /_cat/allocation?v`
- **THEN** 返回文本列格式 + `?format=json` JSON 格式
- **AND** 字段集合与 ES 对齐（`shards`: index/shard/prirep/state/docs/store/IP/node）

#### Scenario: _forcemerge 异步
- **WHEN** `POST /{index}/_forcemerge?max_num_segments=1`
- **THEN** 提交合并任务，异步返回（ES 语义同）
- **AND** 完成后段数 ≤ `max_num_segments`

### Requirement: R27 BM25 全局统计

系统 SHALL 维护全局词频统计表 `docFreq_global(term) = Σ 段 docFreq`。段 footer 的 `term_stats` 摘要（高频 top 10 万词项）随段注册上报 → meta 集群聚合（增量合并，5 分钟周期刷新）→ coordinator 下推到各分片快照缓存。`global_stats=true` settings 开关（默认 false 对齐 ES）开启时 IDF 用全局表，否则段局部。

#### Scenario: 全局 IDF 跨段一致
- **WHEN** 多段索引执行 `global_stats=true` 的 match 查询
- **THEN** 各段打分使用同一 IDF（来自全局表）
- **AND** 同一文档在跨段场景下分数一致（与单大段对拍，相对误差 <1e-9）

#### Scenario: 新词项回退局部
- **WHEN** 全局统计刷新周期内出现新词项（5 分钟窗口内）
- **THEN** IDF 回退到段局部计算
- **AND** 差异清单标注该文档可能跨段不可比

### Requirement: R28 多租户 shared-shard 配额

系统 SHALL 实现 `shared-shard` 隔离模式：分片混部 + 资源组配额四维（存储字节 / 向量条数 / 查询 QPS / 写入 TPS）+ cgroup v2（CPU/内存/IO 由节点代理下发）+ per-tenant 查询并发 semaphore + 写入 TPS 令牌桶。配额由心跳聚合到 meta，超额写 `429` + 租户标识错误体（`_cat/tenant` 扩展 cat 端点）。

#### Scenario: 配额超限 429
- **WHEN** 租户 A 的查询 QPS 突破配额上限
- **THEN** 后续请求返回 `429` + `circuit_breaking_exception` 错误体
- **AND** 响应携带租户标识 header

#### Scenario: per-tenant 并发 semaphore
- **WHEN** 租户 A 在执行长查询期间有 N+1 个并发请求（N = 并发预算）
- **THEN** 第 N+1 个请求在 semaphore 等待
- **AND** 等待超时返回 429（与配额超限同错误体）

### Requirement: R29 兼容金标准门禁

系统 SHALL 在 `test/golden/` 维护金标准套件：elasticsearch-py（rest-api tests yaml 子集）+ go-elasticsearch 集成测试 + ES 官方 `rest-api-spec` yaml（按 04 §3 矩阵筛可用例）。每夜全量运行 + PR 增量（按路由变更筛选）。断言 HTTP 状态 + JSON 结构（字段级 diff，忽略时间戳类抖动字段白名单）。

#### Scenario: GA 门槛通过率
- **WHEN** M1 末出口评审执行金标准套件
- **THEN** elasticsearch-py + go-elasticsearch 套件通过率 ≥90%（GA 门槛）
- **AND** 兼容矩阵报告（04 §3 表 + 实测状态列）自动生成并归档

#### Scenario: PR 增量筛选
- **WHEN** PR 仅修改 `/{index}/_search` 路由相关代码
- **THEN** 金标准仅运行 `_search` 相关 yaml 用例
- **AND** 不触发未变更路由的全量回归
