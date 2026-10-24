# UniSearch 可开展工作梳理报告

> 基准日期：2026-10-24 ｜ 梳理范围：`docs/` 全部文档、`.ai/` 规格体系、Go 源码全量、测试与 CI
> 方法：三路并行探索（规格体系 / 文档体系 / 源码实现度）+ 关键疑点逐一核实（所有引用均经过实际代码验证）
> 代码基线：`go build ./...` 与 `go vet ./...` 全绿（commit `39dfb49`）

---

## 1. 项目现状总览

### 1.1 里程碑状态

| 里程碑 | 代码状态 | 出口 DoD | 遗留 |
|--------|---------|---------|------|
| M0 嵌入式 MVP | ✅ 完成 | 基本达成 | RAG demo、1 亿文档基准报告、fuzz 收官件标注"可后补"，至今未补 |
| M1 ES 兼容 GA | ✅ 完成 | Golden 27/27=100%，9 个集成 Gap 全关闭 | 无 |
| M2 云原生 GA | ✅ 代码完成 | **未闭环** | 混沌 C1-C6 报告、10 亿向量集群基准、存算分离成本报告"待集群环境补齐" |
| M3 生态与进阶 | ✅ W7.2-W7.8 完成（W7.1 GPU 暂缓） | **未达成** | 出口门禁"首个 ES 存量用户整体迁移案例"无交付证据 |
| 竞品特性引入 | ✅ vsag / ClickHouse / iresearch / Qdrant 四轮 + Web 管控台 | — | — |

### 1.2 规格体系状态（.ai/active_specs/，共 7 个）

| 规格 | meta.json 状态 | 实际状态 |
|------|--------------|---------|
| 2026-10-20-gap-fill | **planned** | **过时**：G1/G2/G4/G9 已验证被后续规格实现（见 §3.4） |
| 2026-10-20-m2-cloud-native-ga | planned | 代码完成，DoD 验证未闭环 |
| 2026-10-20-m1-es-compat-ga | implemented | 已完成，未归档 |
| 2026-10-20-m3-ecosystem-advanced | implemented | 已完成，未归档 |
| 2026-10-22-es-feature-gap-fill | implemented | 已完成，未归档 |
| 2026-10-23-qdrant-features-import | implemented | 已完成，未归档（roadmap 仍标 proposed） |
| 2026-10-23-web-admin-console | implemented | 已完成，未归档；**缺 02_spec.md** |

### 1.3 测试与 CI 基线

- Go 测试：78 个测试文件、约 541 个 `Test` 函数 + 4 个 fuzz target；CI 以 `go test -race ./...` + golden 套件为门禁。
- Python e2e：47 个文件（43 功能 + 4 集群），**不在 CI 内**。
- HTTP 路由：110+ 个，未支持能力统一返回 400 + `unsupported_capability` 信封（设计决定，非 501）。

---

## 2. 工程实施项

### P0 孤儿模块接线（实现已写完，接线即生效，性价比最高）

**① Script 引擎接线**
- 现状：[script.go](file:///Users/zhaoyan/workspace/uni-search/internal/engine/script/script.go)（763 行完整 Painless 子集解释器：递归下降 parser + 树遍历解释器、算术/逻辑/三元/`doc['field'].value`/`_score`/params/Math.*/向量函数、10 万步预算）**零调用方**。
- 线上三处假实现：
  - `script` query 匹配所有文档（[planner.go:1394](file:///Users/zhaoyan/workspace/uni-search/internal/embedded/planner.go#L1394)，`MatchAllQuery` 兜底）
  - script_fields/runtime_mappings 走正则简化版 `evalSimpleScript`（[engine.go:605](file:///Users/zhaoyan/workspace/uni-search/internal/embedded/engine.go#L605)，仅支持 `字段 * N` / `字段 / N`，其余返回 nil）
  - script_score 直接委托内层查询，脚本不参与评分（[translator.go:644](file:///Users/zhaoyan/workspace/uni-search/internal/embedded/translator.go#L644)）
- 工作：三处路径替换为 `script.Parse` + 解释器求值；补脚本引擎单测。
- 验收：`script`/`script_score`/`script_fields` 三类请求返回真实求值结果；复杂脚本按步数预算报错。

**② Percolate 引擎接线**
- 现状：[percolator.go](file:///Users/zhaoyan/workspace/uni-search/internal/engine/percolate/percolator.go)（347 行，Register/Unregister/Match/Count + DSL 递归匹配）零调用方；线上 percolate 查询"匹配所有文档"（[planner.go:1409](file:///Users/zhaoyan/workspace/uni-search/internal/embedded/planner.go#L1409)）；路由层无任何 `_percolate` 路由（已 Grep 验证零匹配）。
- 工作：新增 `/_percolate` 路由与 handler；注册/存储 percolator 查询（可复用 synonyms store 模式）；替换假实现。
- 验收：注册查询后 `_percolate` 只返回匹配文档；注意包内注释声称支持 prefix/wildcard/ids 但 `evalQuery` 无此 case，接线时需对齐（补 case 或修注释）。

**③ Suggest 引擎接线**
- 现状：[suggest.go](file:///Users/zhaoyan/workspace/uni-search/internal/engine/suggest/suggest.go)（200 行，term/phrase/completion 三种 suggester）零调用方；DSL parser 把 `suggest` 仅当 search body 标志键（[parser.go:192-201](file:///Users/zhaoyan/workspace/uni-search/internal/engine/dsl/parser.go#L192)）不解析内容；`SearchResult.Suggest` 恒为 nil，`handlers.go:577` 的分支永不触发。
- 工作：parser 解析 suggest 段结构；引擎侧调用 suggester 填充结果；completion 索引数据源（需从段词典构建，注释已预留"F2 可换 FST"路径）。
- 验收：`_search` 带 suggest 段返回建议词；三种 suggester 各有 e2e 用例。

**④ uniadm CLI 实现**
- 现状：[main.go](file:///Users/zhaoyan/workspace/uni-search/cmd/uniadm/main.go) 占位直接 `exit(1)`；escompat 扫描库（ScanMapping/ScanQueries）已实现却未被引用。
- 工作：实现 `uniadm es-compat scan <url>` 子命令（输出 mapping/query 兼容性报告）；bench/backup/restore 子命令按需排期。
- 验收：对一个运行中的 UniSearch 实例输出扫描报告；这是 M3 出口"ES 存量迁移案例"的前置工具。

### P1 假实现修复（用户可感知的功能缺陷）

**⑤ `_rollover` 假 ack**：[rollover_handler.go:45-52](file:///Users/zhaoyan/workspace/uni-search/internal/server/esapi/rollover_handler.go#L45) 注释自认 "P2 simplified: just acknowledge"，不创建新索引、不切别名、不检查条件。修复需：创建新索引（应用模板）→ 原子切换别名 → 可选删除旧别名。

**⑥ geo 查询走 BKD 索引**：geo_distance/geo_bounding_box 翻译为 `_id Scan + postFilterHits` 暴力过滤（[translator.go:650-674](file:///Users/zhaoyan/workspace/uni-search/internal/embedded/translator.go#L650)，注释自认 "P1 uses a simple scan"）；scalar 包已有 `bkd2d.go` 索引未接入查询路径。修复后是大数据量 geo 查询的数量级提升。

**⑦ 缺失查询类型补齐**：`wildcard`、`ids`（规划 v1.2/M1）、`semantic`（v1.3/M2）、terms lookup（parser.go:323 显式 400）。wildcard/ids 属基础查询，ES 用户高频使用。

**⑧ 统计值修复**：termvectors `DocFreq` 恒 1（[termvectors.go:133](file:///Users/zhaoyan/workspace/uni-search/internal/server/esapi/termvectors.go#L133)）；mini 倒排 `tf` 恒 1（[miniinv.go:151](file:///Users/zhaoyan/workspace/uni-search/internal/embedded/miniinv.go#L151)，影响 memtable 阶段 BM25 评分精度）。

**⑨ memtable 阶段简化路径**：geo_shape 仅检查字段存在（planner.go:1415）、intervals 匹配所有（planner.go:1428）、nested 简化（planner.go:1433）。

### P2 架构遗留（工程量大，需单独立项）

**⑩ shard 数据层 Raft 接线**：[shard_raft.go:41](file:///Users/zhaoy/workspace/uni-search/internal/storage/shard_raft.go#L41) `Write` 直接委托 `Shard.Write`，注释自认 "for now, it delegates"；`NewShardRaftGroup` 零调用方，`cloud_shard.go` 无 raft 引用。**现状：只有 meta 层是真 Raft（raft_cluster.go），shard 数据多副本共识未进生产路径**——这是 M2 "kill -9 RTO<30s" 承诺的数据层基础。同时可清理孤儿 mock `internal/raft/cluster.go`（MetaCluster，零调用）。

**⑪ SIMD 汇编内核**：AVX2+FMA/SSE2/NEON 内核全部 TODO 占位（[dispatch_amd64.go:23,35](file:///Users/zhaoyan/workspace/uni-search/internal/simd/dispatch_amd64.go#L23)、[dispatch_arm64.go:13](file:///Users/zhaoyan/workspace/uni-search/internal/simd/dispatch_arm64.go#L13)、[asm/gen.go](file:///Users/zhaoyan/workspace/uni-search/internal/simd/asm/gen.go)），运行时全部落在 pure Go 标量路径。README 宣称"SIMD 加速"名不副实（短期可先改 README，长期做内核）。

**⑫ 优化器/编码遗留 TODO**：投影裁剪规则 R8 no-op（[optimizer.go:218](file:///Users/zhaoyan/workspace/uni-search/internal/engine/plan/optimizer.go#L218) TODO F6，投影元数据未接线）；组合编解码器选择禁用（[encoding.go:123](file:///Users/zhaoyan/workspace/uni-search/internal/storage/segment/encoding.go#L123) TODO F2）。

**⑬ 插件系统**：[registry.go](file:///Users/zhaoyan/workspace/uni-search/internal/plugin/registry.go) 零调用方，sidecar 适配器自认 placeholder（Build/Load 返回 "requires IndexService proto (not yet defined)"）。需先定义 IndexService proto 才能接线。

### P3 承诺类（外部期待，非内部缺陷）

- **⑭ Python SDK**：W7.8 与概要设计 §12 承诺 "Go/Python SDK GA"，仅 Go 落地（`test/python/` 是测试套件不是 SDK）。
- **⑮ RAG demo**：M0 收官件（W4.3"高亮占位"）至今未补，是项目对外展示的短板。
- **⑯ 其余简化路径**：ingest ScriptProcessor 仅支持 `doc.field = expr`（processors_advanced.go:136）、cloud Analyze 是 whitespace split（coordinator_engine.go:778）。

---

## 3. 文档/规格一致性

| # | 问题 | 位置 | 修复方式 |
|---|------|------|---------|
| 3.1 | **SDK 指南严重失实**：文档示例无法编译——`NewClient(Config{...})` 实际是 `NewClient(addr string)`；`DeleteIndex`/`Create` 方法、`ErrNotFound`/`ErrTimeout` 哨兵不存在；除 CreateIndex 外多数方法不检查 HTTP 状态码 | `docs/user-guide/sdk-guide.md` vs [client.go](file:///Users/zhaoyan/workspace/uni-search/pkg/unisdk/client.go) | 二选一：**A** 按实际实现重写文档（快）；**B** 增强 SDK 到文档承诺水平（Config/认证/错误哨兵/状态码检查/补方法，配合 §2-⑭ 生态目标）。建议 B，SDK 是对外门面 |
| 3.2 | 设计文档滞后：04-es-compat-layer 仍标 geo_shape ❌（M3 已实现 `geo_shape.go`）、稀疏向量标 [M1]（Qdrant 导入已实现 sparse_index.go） | `docs/go-design/04` | 按实现更新矩阵（development-plan §9 要求"实现偏离先改设计文档"） |
| 3.3 | 兼容口径夸大：es-compatibility.md 将 _settings/_reindex/scroll 等标"完全兼容"（设计文档为 🟡 partial）；`_search/template` 标"不支持"但代码已实现（template_handlers.go） | `docs/user-guide/es-compatibility.md` | 统一矩阵口径，partial 项如实标注 |
| 3.4 | **gap-fill 规格过时**：meta.json 标 planned，但 G1 Prometheus（metrics.go + `/_prometheus/metrics` 已接线）、G2 OTel 四级 span（tracing.go）、G4 gRPC（cloud_shard.go:88、server.go:98 已 Serve）、G9 goccy/json（jsonx 已入热路径）均已验证实现 | `.ai/active_specs/2026-10-20-gap-fill/00_meta.json` | 对照剩余 G 项（G5 Operator reconciler、G6 split、G7 crosslink_merge、G8 tiering 代码均存在）逐项审计 → 更新 meta.json → 归档 |
| 3.5 | roadmap 与 meta.json 三处不同步：qdrant/web-admin-console 标 proposed（实际 implemented）；iresearch 已归档仍在 In Progress 列表 | `.ai/02_ROADMAP.md` | 状态同步 + 将 5 个 implemented 规格走归档流程 |
| 3.6 | CI Go 版本冲突：ci.yml `go-version: '1.23'` vs go.mod `go 1.26.0`（当前靠 GOTOOLCHAIN 自动下载兜底） | `.github/workflows/ci.yml` | 统一为 1.26 |
| 3.7 | README 宣称失实："SIMD 加速（AVX2+FMA/NEON）"（内核未写）、"REST API 100% 兼容 ES 8.x"（wildcard/ids 等显式 400）、"Raft 内置 WAL 即 Raft log"（shard 层未接线） | `README.md` | 或修文案，或作为 §2 对应项完成后的验收 |
| 3.8 | 承诺的基准报告归档路径 `docs/benchmarks/` 不存在；所有性能指标仍标"[设计目标] 未经实测" | `docs/development-plan.md`、`docs/go-design/06` §5/§8 | 建立 docs/benchmarks/ 并随基准工作（§4-2）填充 |

---

## 4. 战略/路线图

1. **M2 DoD 闭环**（代码就绪，需集群环境）：C1-C6 混沌演练报告、10 亿向量基准（B2/B5/B9 规模版）、存算分离成本报告。`test/chaos/` 已有 10 个混沌测试可复用，缺的是"集群环境 + 报告归档"。
2. **M3 出口达成**：用 ③ 的 uniadm es-compat scan + ① 的 script 接线（存量 ES 用户脚本普遍）做一次真实/模拟迁移演练并归档报告——这是 M3 唯一未关闭的门禁。
3. **M4 候选方向**（当前无下一里程碑规划，建议三选一或组合）：
   - **A 生产就绪强化**（建议优先）：§2 P0+P1 全部清零（孤儿接线 + 假实现修复），使"宣称的能力 = 实际的能力"——当前最大风险是假实现静默返回错误结果（percolate 匹配所有文档、script 匹配所有文档），对用户是正确性事故而非功能缺失。
   - **B 生态补全**：Python SDK、SDK 增强（3.1-B 方案）、RAG demo、基准报告体系。
   - **C 性能收尾**：SIMD 内核、shard Raft 接线、geo 走 BKD、优化器 R3/F2。
4. **W7.1 GPU CAGRA**：维持暂缓（与 `CGO_ENABLED=0` 单二进制约束冲突），若重启需先做架构决策（ADR）：改约束或改形态（sidecar GPU 进程）。
5. **20 个"部分实现"功能点**：gap-fill 提案明确排除在范围外，从未清单化。建议在 M4-A 中一并审计补齐。

---

## 5. 测试质量

1. **孤儿模块零测试**（与 §2 P0 清单完全重合，接线时必须同步补测）：script（763 行无一个测试）、percolate、suggest、plugin、unisdk、escompat、MetaCluster、ShardRaftGroup。
2. **CI 覆盖缺口**：前端构建（web/admin，`make build-frontend`）不在 CI——embed 的 dist 资产可能过期；Python e2e（47 文件）只在本地跑，建议 nightly job。
3. **兼容通过率口径**：M1 出口承诺"elasticsearch-py/go-elasticsearch 官方套件 ≥90%"，实际口径是内部 27 个 golden test 100% + 自建 pytest。`test/golden/es_spec/loader.go` 有 rest-api-spec 加载器但无完整通过率报告。建议：跑一次官方 rest-api-spec 对比并归档，给出真实兼容率。
4. **fuzz 已有 4 个 target**（make fuzz），质量良好，无需追加。

---

## 6. 建议执行顺序

| 批次 | 内容 | 依赖 | 量级 |
|------|------|------|------|
| 快赢 | 3.2-3.7 文档一致性修缮 + gap-fill 审计归档 + roadmap 状态同步 | 无 | 快赢 |
| 第一批 | P0 孤儿接线：① script ② percolate ③ suggest ④ uniadm（每个含单测） | 无 | 中 |
| 第二批 | P1 假实现修复：⑤ rollover ⑥ geo BKD ⑦ wildcard/ids ⑧ 统计值 | 第一批（script 可复用） | 中 |
| 第三批 | 架构级：⑩ shard Raft 接线 ⑪ SIMD 内核 ⑫ R3/F2 | 需单独立项 + ADR | 大 |
| 持续 | M2 DoD 验证（需集群环境）、M3 迁移演练（依赖 uniadm+script）、Python SDK、基准报告体系 | 集群环境/前置批次 | 大 |

**核心判断**：项目最大风险不是缺功能，而是**假实现静默返回错误结果**（percolate/script 查询匹配所有文档、rollover 假 ack）——用户拿到的答案是对的语法、错的语义。建议 M4-A（生产就绪强化）作为下一阶段主线，先清快赢与第一批。

---

*本报告为梳理交付物，未修改任何源码/文档/规格状态。引用均基于 2026-10-24 代码验证。*
