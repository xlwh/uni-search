# UniSearch 详细设计 04 —— Elasticsearch 兼容层

> 版本：v0.1
> 日期：2026-10-17
> 上游：[概要设计 §8](../vector-database-design.md) / [00-overview.md](00-overview.md)
> 前置：[03-query-engine.md](03-query-engine.md)（AST/执行）
> 范围：REST 路由、请求/响应规范、DSL 兼容矩阵、mapping/settings 语义、并发控制、_cat、批处理任务、ILM、兼容测试金标准
> 代码位置：`internal/server/esapi/`

---

## 1. 兼容策略（继承概要设计 §8.1）

1. **协议级兼容**：`http://host:9200` 直接替换 ES 主机名；
2. **语义优先，实现自研**：DSL → AST（03 §2），不内嵌 Lucene；
3. **兼容分级**：✅ full / 🟡 partial（附差异清单）/ ❌ unsupported（ES 风格错误 + `unsupported_capability`，00 §6.1）。

**单一事实源**：本文件 §3 的兼容矩阵是 AST 节点与 DSL 特性的权威映射表，[03-query-engine.md](03-query-engine.md) §2.1 引用此处不重复。

---

## 2. REST API 路由表 `[M0 基础 / M1 全量]`

### 2.1 路由实现

Go 1.22+ `ServeMux` 原生表达 ES REST（方法 + 路径参数）：

```go
// internal/server/esapi/router.go（节选）
mux.HandleFunc("GET /{$}", handleRoot)                        // 版本探测
mux.HandleFunc("POST /{index}/_search", withIndex(handleSearch))
mux.HandleFunc("POST /_bulk", handleBulk)
mux.HandleFunc("POST /{index}/_doc/{id}", handleIndexDoc)     // 自动生成 _id
mux.HandleFunc("PUT /{index}/_doc/{id}", handleIndexDoc)
mux.HandleFunc("GET /{index}/_doc/{id}", handleGetDoc)
mux.HandleFunc("DELETE /{index}/_doc/{id}", handleDeleteDoc)
mux.HandleFunc("POST /{index}/_update/{id}", handleUpdateDoc)
// ...全量路由注册见 §2.2 清单
```

通配（`GET /_cat/{cat}`）与多索引表达式（`logs-*`、`_all`）在中间件层展开：索引名解析（alias → 索引列表）→ 逗号/通配展开 → 逐索引分发。

### 2.2 路由清单（对齐概要设计 §8.2，标注 handler 与兼容级别）

| API | Handler（`internal/server/esapi`） | 级别 | 里程碑 |
|-----|------|------|--------|
| `GET /`（版本探测） | `root.go` | ✅ | M0 |
| `GET /_cluster/health` | `cluster.go` | ✅ | M0 |
| `GET /_cat/{indices,health,shards,nodes,allocation}` | `cat.go` | ✅ | M0(health/indices)/M1 |
| `PUT/GET/DELETE /{index}` | `index.go` | ✅ | M0 |
| `PUT/GET /{index}/_mapping` | `mapping.go` | ✅ | M0 |
| `PUT/GET /_index_template[/name]`、`/_component_template` | `template.go` | ✅ | M1 |
| `POST/PUT /{index}/_doc/{id}`、`PUT /{index}/_create/{id}`、`GET/DELETE /{index}/_doc/{id}` | `doc.go` | ✅ | M0 |
| `POST /_bulk`、`POST /{index}/_bulk` | `bulk.go` | ✅ | M0 |
| `POST/GET /{index}/_search`、`POST /_search` | `search.go` | ✅ | M0 |
| `POST /{index}/_count`、`/{index}/_explain/{id}` | `count.go` / `explain.go` | ✅ | M0/M1 |
| `POST /{index}/_msearch`、`POST /_msearch` | `msearch.go` | ✅ | M1 |
| `POST /{index}/_pit` + `POST /_pit/delete` + `search_after` | `pit.go` | ✅ | M1 |
| `POST /{index}/_search?scroll=` + `POST /_search/scroll` + `DELETE /_search/scroll` | `scroll.go` | 🟡 内部转译 PIT（§6.4） | M1 |
| `_delete_by_query` / `_update_by_query` / `_reindex` | `byquery.go` / `reindex.go` | ✅/🟡 | M1 |
| `POST /{index}/_update/{id}`（scripted upsert） | `doc.go` | 🟡 painless 子集（03 §9） | M1 |
| `POST /{index}/_refresh` / `_flush` / `_forcemerge` | `admin.go` | ✅ 语义映射（§6.3） | M0(refresh)/M1 |
| `PUT/GET /{index}/_settings` | `settings.go` | 🟡 子集（§4.3） | M0 |
| `PUT /{index}/_alias`、`POST /_aliases` | `alias.go` | ✅ 原子切换 | M0 |
| `PUT /_ilm/policy`、`PUT /{index}/_ilm/start` | `ilm.go` | 🟡 rollover/delete/forcemerge/shrink | M1 |
| `POST /{index}/_analyze` | `analyze.go` | ✅ | M0 |
| `GET /{index}/_field_caps` | `fieldcaps.go` | ✅ | M1 |
| `POST /_mget`、`POST /{index}/_mget` | `mget.go` | ✅ | M0 |
| `GET /{index}/_terms_enum` | `termsenum.go` | ✅ | M1 |
| `POST /{index}/_knn_search` | `search.go` 转译 knn query | ✅ | M0 |
| `_security` / `_xpack` / ML / watcher / transform | 统一 `unsupported.go` | ❌ 显式报错 | — |

---

## 3. Query DSL / Aggregations 兼容矩阵（单一事实源）

### 3.1 全文检索类

| DSL | AST（03 §2.1） | 级别 | 差异备注（🟡 项必填） |
|-----|------|------|------|
| `match`（operator/fuzziness/minimum_should_match） | `MatchQuery` | ✅ | fuzziness：FST Levenshtein 自动机（02 §3.1） |
| `match_phrase` / `match_phrase_prefix` | `MatchPhraseQuery` | ✅ | 位置索引（02 §3.2 pos 块） |
| `multi_match`（best/most/cross_fields/phrase） | `MultiMatchQuery` | ✅ | cross_fields = blended per-term IDF |
| `match_bool_prefix` | `MatchBoolPrefix` | ✅ | |
| `intervals` | `IntervalsQuery` | 🟡 | 支持 match/all_of/any_of/Prefix；`filter` 内 wildcard ❶ 不支持 |
| `combined_fields` | — | 🟡 | 转译 cross_fields multi_match，`rank_constant` 差异标注 |
| `query_string` / `simple_query_string` | `QueryStringQuery` | ✅ | Lucene 语法：AND/OR/NOT/()/?/*/""/~；字段限定 `field:term` |
| `semantic`（8.15+） | `SemanticQuery` | 🟡 | 映射内置 embedding + knn；模型 `inference` 配置（M2 后） |

### 3.2 词项级与组合

| DSL | AST | 级别 | 备注 |
|-----|-----|------|------|
| `term`/`terms`/`terms_set` | `Term/Terms/TermsSet` | ✅ | |
| `range`（date math `now-7d`） | `RangeQuery` | ✅ | |
| `exists`/`prefix`/`wildcard`/`regexp`/`fuzzy`/`ids` | 对应节点 | ✅ | wildcard/regexp 长度 ≤1000、状态数 ≤10000 上限保护 |
| `constant_score`/`boosting`/`dis_max`/`function_score` | 对应节点 | ✅ | function_score 四函数（03 §10） |
| `script_score` | `ScriptScoreQuery` | 🟡 | painless 子集 + 向量函数一等（03 §9） |
| `bool`（must/should/filter/must_not 嵌套） | `BoolQuery` | ✅ | 基石 |

### 3.3 向量/混合（核心增强）

| DSL | AST | 级别 | 备注 |
|-----|-----|------|------|
| `knn` 顶层子句（8.12+）与 `knn` as query（8.14+） | `KnnQuery` | ✅ | filter 共享下推（02 §2.6） |
| `dense_vector` mapping（dims/element_type/similarity） | mapping（01 §1.3） | ✅ | bfloat16 v2（差异清单） |
| `sparse_vector` mapping + query | `SparseVectorQuery` | ✅ | 原生一等公民（02 §5） |
| `hybrid`（本系统独有） | `HybridQuery` | ✅ | 03 §5 融合 |
| `percolate` | `PercolateQuery` | 🟡 | 查询即文档（M3 完整化） |
| `rank_feature(s)` | `RankFeatureQuery` | 🟡 | saturation/log/sigmoid |
| `distance_feature` | `DistanceFeatureQuery` | ✅ | date/geo 衰减 |
| `more_like_this` / `pinned` / `wrapper` | 对应节点 | ✅ | |
| `geo_shape` | — | ❌ | v1/早v2 规划；geo_point ✅（BKD 2D） |

### 3.4 Suggester / 其他

| DSL | 级别 | 备注 |
|-----|------|------|
| `term`/`phrase` suggester | ✅ M1 | 词典编辑距离（02 §3.1） |
| `completion` suggester | ✅ M1 | FST（02 §7） |
| `highlight`（unified 为主） | ✅ M1 | 02 §3.5 |
| `collapse` + `inner_hits` | ✅ M1 | 03 §10 |
| `rescore` | ✅ M1 | 03 §10 |
| `search_after`/`sort`（_doc/_score/field/geo_distance/script） | ✅ M0/M1 | 03 §8 |
| `runtime_mappings` / `script_fields` | 🟡 M1 | 表达式子集（03 §9） |
| `profile` | ✅ M1 | 03 §11 |
| `aggs` | ✅ M1 全量 | 03 §6.3 清单；🟡：diversified_sampler、geohash_grid、scripted_metric、extended_stats_bucket；❌：parent/child（join） |

**差异清单维护规则**：每个 🟡 项在本表「差异备注」列必须给出差异内容与 workaround；发布时自动生成兼容报告（§9）。

---

## 4. Mapping / Settings 语义映射（对齐概要设计 §8.5）

### 4.1 字段级

| ES 概念 | UniSearch 实现 | 差异处理 |
|---------|----------------|---------|
| `doc_values` | 列存恒开（01 §4） | `false` 接受 + deprecation 告警（响应 header `Warning`） |
| `_source: false` | 不生成 `src.zst` | top_hits/highlight 返回错误并指明因 `_source` 关闭 |
| `dynamic` / `dynamic_templates` | 01 §1.3 同语义 | |
| `nested` | v1 扁平化（01 §1.2） | 深度 >5 性能差异写入文档 |
| `copy_to` / multi-fields | ✅ 写入期复制/多列 | |
| `coerce` / `similarity` / `null_value` / `ignore_above` | ✅ | coerce 关闭时类型不匹配报 ES 同款错误 |
| `index: false` | 仅列存不建索引 | |
| `norms` | 忽略 | 告警（fldLen bucket 等价内建，02 §3.2） |

### 4.2 元字段

`_id`（自动生成/路由）、`_routing`（`hash(routing)%logical_shards`，兼容 ES 路由算法；在线分裂差异见 [05-distributed.md](05-distributed.md) §3.1）、`_seq_no`/`_primary_term`（§5）、`_field_names`（exists 路由）、`_ignored`（ignore_above 落选）。

### 4.3 Settings 子集

支持：`number_of_shards/replicas`（逻辑分片，05 §3）、`refresh_interval`（映射 MemTable flush 时限，01 §2.2）、`analysis`（analyzer/tokenizer/filter 组合注册，02 §4）、`index.hidden`、`sort`（索引预排序，v2）。忽略 + 告警：ES 段级调优类（`merge.*` 内部项映射到合并调度参数但不透出语义）。不支持：显式 `unsupported` 报错。

---

## 5. 并发控制 `[M0]`

`if_seq_no` / `if_primary_term` 乐观锁：

- `SeqNo` 即 MVCC 版本（01 §8），`primary_term` = 分片 leader 任期（M0 恒 1；M2 Raft term）；
- 写路径校验：`_id` 当前 `seq_no != if_seq_no` → `409 version_conflict_engine_exception` 同款错误体；
- `?if_seq_no` 缺省 = 任意覆盖（ES upsert 语义）；`_create` = 存在即 `409`；
- GET 响应含 `_seq_no/_primary_term` 字段（8.x 格式）。

---

## 6. 关键 API 实现规格

### 6.1 `GET /` 与 `_bulk` `[M0]`

```json
// GET / —— 版本探测（elasticsearch-py 探测 compatible mode 的依据）
{ "name" : "node-1", "cluster_name" : "unisearch", "cluster_uuid" : "...",
  "version" : { "number" : "8.12.0", "build_flavor" : "default",
                "distribution" : "unisearch", "lucene_version" : "none" },
  "tagline" : "You Know, for Search" }
```

- `version.number` 报 **8.12 基线**（本系统兼容矩阵锚定的 ES 版本），`distribution` 标识真实身份（ES Java client 会校验此字段族，实测对齐）；
- `_bulk` NDJSON **流式解析**：`bufio.Scanner` 逐行读 action+source，**不整包驻内存**；单条上限 100MB（ES 同款）；错误逐条聚合（`errors: true` + items[].error），HTTP 200（ES 语义：bulk 级网络成功 + 条目级错误）；
- `refresh` 参数三态：`true`（flush 触发）、`wait_for`（等 group commit，01 §3.2）、`false`（默认）。

### 6.2 `_search` 响应骨架 `[M0]`

对齐 8.x 结构：`took / timed_out / _shards{total,successful,skipped,failed} / hits{total{value,relation}, max_score, hits[]{_index,_id,_score,_source,_seq_no,_primary_term,highlight,sort,inner_hits,matched_queries}} / aggregations / suggest / profile`。`hits.total.relation`：`eq`（精确）/`gte`（`track_total_hits` 截断，语义同 ES）。

### 6.3 `_refresh` / `_flush` / `_forcemerge` `[M0/M1]`

| ES API | 内部映射 |
|--------|---------|
| `_refresh` | MemTable 无需 refresh 即可见（01 §2.1）→ **no-op 返回成功**（语义超集，响应兼容） |
| `_flush` | 触发冻结表立即 flush（01 §5） |
| `_forcemerge?max_num_segments=N` | 提交合并任务到目标段数（01 §7.1 规则 3），异步返回（ES 语义同） |

### 6.4 scroll 兼容（转译协议）`[M1]`

接收 `?scroll=5m` → 内部创建 PIT（TTL=scroll 保持时长）→ 首响返回 `scroll_id`（编码 PIT id + sort cursor，03 §8）；`POST /_search/scroll` 逐批推进 search_after；`DELETE /_search/scroll` 销毁 PIT。**不维护 ES 原生 scroll 上下文**——差异：scroll 期间 `slice` 自动切分不支持（报 unsupported，workaround：PIT + search_after + `pit_slice` 扩展参数）。

### 6.5 `_cat` 输出 `[M0/M1]`

文本列格式（`?v&format=json` 均支持）：
- `indices`：`health status index uuid pri rep docs.count docs.deleted store.size pri.store.size`（值从 meta + 段 footer 聚合）；
- `health`：green/yellow/red 计算（06 §4）；
- `shards` / `nodes` / `allocation`：路由表 + 心跳（05 §1）。

### 6.6 `_mget` / `_field_caps` / `_terms_enum` / `_explain` / `_analyze` `[M0/M1]`

`_mget`：批量 id.hash 点查（01 §6.5）；`_field_caps`：meta mapping 聚合（searchable/aggregatable 元数据）；`_terms_enum`：位图索引/词典前缀遍历；`_explain`：单文档计划执行 + 打分分解（03 §3.2 trace）；`_analyze`：直接暴露 `Analyzer.Analyze`（02 §4）输出 ES 同构 token 结构。

### 6.7 `_reindex` / `_delete_by_query` / `_update_by_query` `[M1]`

批处理任务框架：`TaskManager`（in-flight 任务表：`_tasks` API 可查进度/取消）；执行 = 内部滚动扫描（search_after+PIT）+ 批量写入（默认 1000 docs/批）+ `requests_per_second` 限速（令牌桶）；冲突策略 `conflicts=proceed|abort`。

### 6.8 ILM 子集 `[M1]`

策略状态机：`hot→warm→cold→delete` + `rollover`（`max_size/max_age` 触发别名切换新索引）/ `forcemerge` / `shrink`（目标分片数，利用在线分裂逆操作）/ `delete`。状态迁移事件由后台 ticker 驱动，执行动作复用 §6.7 任务框架。

---

## 7. 多索引与别名 `[M0]`

- 索引模式解析：`logs-*`（通配展开）、`_all`/`*`、`logs-{a,b}-*`（花括号展开）——与 ES 语法一致；
- 别名：`POST /_aliases`（`add/remove` 原子批）→ 路由层双版本切换（蓝绿发布惯用法，概要设计 §8.2）；写别名 + `index_routing` 支持；
- `is_write_index` 语义对齐。

---

## 8. 错误与告警规范 `[M0]`

- 错误体：00 §6.1 `APIError`，`type` 枚举对齐 ES（`index_not_found_exception`/`document_parsing_exception`/`illegal_argument_exception`/`search_phase_execution_exception`…）；
- HTTP 状态映射：400 参数/404 资源/409 冲突/429 拒绝（写入背压，01 §2.2）/501 unsupported；
- deprecation：`Warning` response header（`299 unisearch-8.12 "..."`）——`doc_values:false`、`norms`、scroll 建议迁移等场景。

---

## 9. 兼容性测试金标准 `[M1 门禁]`

- 套件：`elasticsearch-py`（rest-api tests yaml 子集）+ `go-elasticsearch` 集成测试用例 + ES 官方 `rest-api-spec` yaml（按本文件矩阵筛可用例）；
- 运行：`test/golden/` 每夜全量 + PR 增量（按路由变更筛选）；断言 HTTP 状态 + JSON 结构（字段级 diff，忽略时间戳类抖动字段白名单）；
- **门禁：主流 client test suite 通过率 ≥ 95%**（概要设计 §11，GA 门槛 ≥90% 于 M1 末）；
- 报告：自动生成兼容矩阵报告（本文件 §3 表 + 实测状态列），随版本发布（概要设计 §8.6.3）；
- `uniadm es-compat --index-pattern logs-*`（M3）：扫描存量 mapping/查询日志 → 兼容性评估报告（复用解析器 + 矩阵规则，静态分析无集群连接模式）。

---

## 10. 里程碑汇总

| 能力 | M0 | M1 | M2+ |
|------|----|----|-----|
| `GET /`、_doc、_bulk、_search（DSL §3 的 M0 子集）、_count、_mget、mapping/alias/settings 基础、_analyze、_refresh | ✅ | | |
| _cat 全量、_msearch、PIT/search_after、scroll 转译、highlight/suggester、by_query/reindex、template、ILM、field_caps、terms_enum、explain、profile | | ✅ | |
| `semantic`（inference 配置）、percolate 完整化 | | | M2/M3 |
| 兼容金标准 CI 门禁 | 基线建立 | ≥90% → GA | 持续 ≥95% |

---

*本文件矩阵变更（任何 ✅↔🟡↔❌ 移动）必须：同步 [03-query-engine.md](03-query-engine.md) AST 节点、更新差异清单、在 CHANGELOG 标注兼容性变化（概要设计 §8.6.3 RFC 流程）。*
