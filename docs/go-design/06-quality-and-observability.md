# UniSearch 详细设计 06 —— 质量保障与可观测性

> 版本：v0.1
> 日期：2026-10-17
> 上游：[概要设计 §11](../vector-database-design.md) / [00-overview.md](00-overview.md)
> 范围：指标、追踪、日志、健康探针、测试策略（单测/模糊/金标准/基准/混沌）、CI/CD、性能预算看护
> 代码位置：`internal/server/middleware/`、`test/`、`build/`

---

## 1. 指标体系 `[M0]`

Prometheus（`prometheus/client_golang`），端点 `GET /_prometheus/metrics`（对齐 ES 监控习惯的暴露路径，概要设计 §12）。指标名 `unisearch_` 前缀，五组核心：

| 组 | 指标（节选） | 类型 | 标签 |
|----|------|------|------|
| 写入 | `unisearch_write_docs_total`、`unisearch_write_batch_duration_seconds`（P99 直方图）、`unisearch_write_rejected_total`（背压 429） | counter/histogram | tenant, index, shard |
| 查询 | `unisearch_query_requests_total`、`unisearch_query_duration_seconds`、`unisearch_query_rows_scanned_total`（扫描行数/查询——跳块有效性观测）、`unisearch_query_errors_total` | counter/histogram | tenant, index, phase(plan/scan/agg/fusion) |
| 段 | `unisearch_segments_open`、`unisearch_flush_duration_seconds`、`unisearch_merge_running`、`unisearch_merge_bytes_total`、`unisearch_deletes_pending`（位图基数） | gauge/histogram | index, shard, tier(hot/warm/cold) |
| 索引 | `unisearch_hnsw_search_duration_seconds`、`unisearch_hnsw_nodes_visited`（ef 效率观测）、`unisearch_bm25_wand_pruned_ratio` | histogram | field |
| 租户配额 | `unisearch_tenant_quota_usage_ratio`、`unisearch_tenant_throttled_total` | gauge/counter | tenant, dim(storage/vectors/qps/tps) |

GC 与运行时：`go_*` 原生指标全量保留（GC pause 直方图直接消费，00 §3.1 延迟预算的验证数据源）。

**ES 指标名映射表**（Grafana 面板迁移）：`unisearch_query_duration_seconds` ↔ ES `http_total_duration`；`unisearch_segments_open` ↔ ES `segments_count`；映射表随文档发布，面板 JSON 模板入 `build/observability/`。

---

## 2. 追踪 `[M1]`

OpenTelemetry，span 树四级（概要设计 §7.1 两阶段执行的可解释性基础设施）：

```
http.request（REST/gRPC 入口，含 DSL 摘要属性）
└─ query.plan（优化器规则命中：R1-R7）
   └─ shard.search（per 分片，M2 跨进程传播 traceparent）
      └─ segment.scan / index.hnsw / index.bm25 / agg.partial（段与算子级）
```

- 采样：默认 parent-based + 尾采样（错误全采、慢查询 >1s 全采、其余 1%）；
- `profile` API（03 §11）与 trace 共享计时采集点，避免双倍开销。

---

## 3. 日志 `[M0]`

`slog` JSON（00 §6.4 字段规范）。慢查询日志（>1s）：`took_ms/shards_scanned/rows_read/plan_summary`（R6 跳块效果审计数据源）。错误日志必带 `trace_id`（与 §2 关联）。

---

## 4. 健康探针 `[M0]`

`GET /_cluster/health`：

| 状态 | 计算规则（对齐 ES 语义） |
|------|------------------------|
| green | 所有分片副本组 quorum 健康 |
| yellow | 副本缺失但主副本在（如副本数配置 2 实际 1） |
| red | 任一分片无可用副本组 |

`?wait_for_status=&timeout=`（ES 参数兼容）；`/_cat/health` 消费同数据源。

---

## 5. 测试策略 `[M0 起持续]`

### 5.1 分层

| 层 | 工具 | 覆盖重点 |
|----|------|---------|
| 单元测试 | testing + testify（表格驱动） | 编码矩阵往返（01 §4.2）、SIMD vs 纯 Go 对齐（00 §3.2）、BM25 数值金标（Lucene 结果样本比对）、BKD/FST 边界 |
| 模糊测试 | go fuzz（`go test -fuzz`） | **四个常驻 target**：DSL 解析器（随机 JSON → 不 panic + 错误路径合法）、WAL 回放（随机字节流截断/CRC 破坏 → 不 panic 恢复终止）、段格式（随机 footer 破坏 → 明确报错）、query_string 语法 |
| 集成测试 | docker-compose（3 shard + 3 meta + MinIO） | 崩溃恢复、分裂、再均衡端到端 |
| **兼容金标准** | [04 §9](04-es-compat-layer.md)：elasticsearch-py / go-elasticsearch / rest-api-spec yaml | **发布门禁 ≥95%**（M1 GA ≥90%） |
| 基准 | `uni-bench`（自研）+ ann-benchmarks | 见 §5.2 |
| 混沌 | [05 §9](05-distributed.md) C1-C6 | 每夜演练 |

### 5.2 uni-bench 基准套件 `[M0]`

对标概要设计 §11 指标（全部 [设计目标] 项的复测责任方）：

| 用例 | 数据集 | 断言 |
|------|--------|------|
| B1 写入吞吐 | 768d 向量 + 2KB 文档合成流 | ≥20k docs/s（16C64G，强一致） |
| B2 向量 P99 | 5000 万 768d | P99 <50ms（HNSW-SQ8, ef=64, k=10, 无过滤） |
| B3 过滤向量 P99 | 同上 + 5% 选择率过滤 | P99 <80ms（pre-filter 路径） |
| B4 BM25 P99 | 1 亿文档（wikipedia 切片） | P99 <80ms（WAND，M1） |
| B5 混合三路 P99 | B2+B4 数据 | P99 <120ms（rrf, window=500） |
| B6 可见性 | 持续写+读 | P99 <200ms |
| B7 1TB 冷导入 | 合成 | <2h（顺序段构建 + 并行量化） |
| B8 聚合吞吐 | 10 亿行 | ≥ ClickHouse 同硬件 50%（CH 官方 star schema 作对照） |
| B9 内存效率 | 千万 768d | 向量索引内存 ≤3.5GB |
| B10 召回率 | ann-benchmarks（glove/sift） | Cross-Link Merge 前后召回曲线对比 ≥ 基线-2%（02 §2.4.3 回滚判据） |

运行：每夜全量 + 大版本报告归档 `docs/benchmarks/`；B10 集成 ann-benchmarks 官方 harness（容器跑，结果 PR 评论回帖）。

---

## 6. CI/CD `[M0]`

| 环节 | 工具 | 规则 |
|------|------|------|
| 静态检查 | golangci-lint（errcheck/govet/staticcheck/revive + `gocritic`） | 阻塞 |
| 单测+模糊种子 | go test -race | 阻塞（race 必须零报告） |
| 交叉编译 | goreleaser 或 build/ 脚本：`CGO_ENABLED=0` → linux/amd64, linux/arm64, darwin/arm64 | 阻塞 |
| 镜像 | distroless 静态多架构 | 每 tag |
| 金标准增量 | 按路由 diff 筛选用例 | PR |
| 性能回归 | B1/B2/B6 微缩版（10 分钟内） | PR 报告（±10% 阈值告警） |

发布节奏：M0 起双周 tag（`v0.x`），兼容矩阵报告随 tag 生成（04 §9）。

---

## 7. 性能预算看护（概要设计 §11 → 用例映射）`[M0]`

| 概要设计指标 | 基准用例 | CI 看护级别 |
|---|---|---|
| 写入吞吐 ≥20k docs/s | B1 | 夜间 |
| 向量 P99 <50ms | B2 | 夜间 |
| 过滤向量 P99 <80ms | B3 | 夜间 |
| BM25 P99 <80ms | B4 | 夜间（M1 起） |
| 混合 P99 <120ms | B5 | 夜间 |
| 可见性 P99 <200ms | B6 | PR 微缩版 |
| 1TB 导入 <2h | B7 | 大版本 |
| 聚合 ≥CH 50% | B8 | 大版本 |
| 内存 ≤3.5GB/千万 | B9 | 夜间 |
| 兼容 ≥95% | 金标准 | 发布门禁 |

预算超支处理：回归引入者 48h 内响应（优化或回滚）；[设计目标] 标注项未实测前在报告中保持标注状态（概要设计 §11 脚注要求的延续）。

---

## 8. 里程碑汇总

| 能力 | M0 | M1 | M2 |
|------|----|----|----|
| 指标五组 + GC 观测 + slog + 健康探针 | ✅ | | |
| OTel 四级 span + 尾采样 | | ✅ | 跨进程传播 |
| 模糊四 target + 单测全量 + B1-B10 微缩版 | ✅ 建立持续 | | |
| 金标准 CI 门禁 | 基线 | ≥90% GA | ≥95% |
| 混沌每夜 + K8s 发布链 | | | ✅ |

---

*本文件是概要设计 §11 性能承诺的唯一验证出口；基准数据与兼容报告归档路径：`docs/benchmarks/`（随 tag）。*
