# UniSearch 详细设计 00 —— 总体设计（Go 技术栈）

> 版本：v0.1
> 日期：2026-10-17
> 上游文档：[vector-database-design.md](../vector-database-design.md)（概要设计 v0.1）
> 状态：详细设计评审中
> 开发语言：**Go**（概要设计原定 Rust，本文档完成全量转译与重选型）

---

## 1. 文档定位与阅读路径

### 1.1 与概要设计的关系

概要设计回答「做什么、为什么这样架构」；本系列文档回答「用 Go 怎么实现」。概要设计的所有章节在本文档系列中的落点如下：

| 概要设计章节 | 内容 | 详细设计落点 |
|---|---|---|
| §2 竞品调研 | 痛点与对策 | §7（本文档）对策映射表 |
| §3 设计目标 | G1-G4 | 各文档验收标准 + [development-plan.md](../development-plan.md) |
| §4 总体架构 | 组件划分 | 本文档 §4-§5 |
| §5 存储引擎 | 段/列存/WAL | [01-storage-engine.md](01-storage-engine.md) |
| §6 索引引擎 | HNSW/倒排/标量 | [02-index-engine.md](02-index-engine.md) |
| §7 查询引擎 | DSL/执行/融合/聚合 | [03-query-engine.md](03-query-engine.md) |
| §8 ES 兼容层 | REST/DSL/Mapping | [04-es-compat-layer.md](04-es-compat-layer.md) |
| §9 分布式 | Raft/分片/租户 | [05-distributed.md](05-distributed.md) |
| §10 API 示例 | 用例 | 04 文档逐 API 规格 |
| §11 性能目标 | 指标 | [06-quality-and-observability.md](06-quality-and-observability.md) §7 |
| §12 技术选型 | Rust 选型 | 本文档 §2（Go 重选型，**取代概要设计 §12**） |
| §13 路线图 | M0-M3 | [development-plan.md](../development-plan.md) |
| §14 风险 | 风险登记 | development-plan.md §7 |

### 1.2 阅读路径

- **新成员入门**：概要设计 → 本文档 → 01/02/03（引擎三件套）→ development-plan.md；
- **ES 兼容贡献者**：概要设计 §8 → 04 文档；
- **分布式/SRE**：概要设计 §9 → 05 文档 → 06 文档；
- **项目管理**：概要设计 §13 → development-plan.md。

### 1.3 文档约定

- 每个设计小节标注落地里程碑：`[M0]` `[M1]` `[M2]` `[M3]`，与 [development-plan.md](../development-plan.md) 的 WBS 对齐；
- Go 代码块为**接口/结构体规格**，非可运行实现；签名以本文档为准，实现 PR 不得偏离；
- 跨文档引用使用相对路径链接；
- 术语表见本文档 §8（全系列统一命名）。

---

## 2. Go 技术选型总表（取代概要设计 §12）

### 2.1 选型总表

| 层 | 选型 | 版本基线 | 理由 | 备选与取舍 | 里程碑 |
|----|------|---------|------|-----------|--------|
| 语言 | Go | 1.23+ | loopvar 语义修正、slog、PGO 成熟 | — | M0 |
| 编译约束 | `CGO_ENABLED=0` 静态编译 | — | 单二进制卖点（G3）；交叉编译矩阵免折腾；避免 C 依赖供应链 | 允许 CGO 会破坏 `linux/arm64` 等目标的一键发布 | M0 |
| HTTP 服务 | 标准库 `net/http` | Go 1.22+ ServeMux | 方法+路径模式路由（`POST /{index}/_search`）原生表达 ES REST；零中间件依赖 | gin/echo：路由能力反而不是刚需，多一层依赖与行为差异 | M0 |
| JSON | `encoding/json`（M0）→ `goccy/go-json`（M1 评估） | — | M0 求稳；M1 在 `_bulk`/`_source` 热点基准后决定是否替换 | bytedance/sonic：性能最强但汇编耦合高，作为 M1 备选 | M0/M1 |
| gRPC | `google.golang.org/grpc` + `google.golang.org/protobuf` | 1.6x | 内部数据面协议（分片子计划、partial state 流式传输） | — | M2 |
| Raft | `hashicorp/raft` | v1.3+ | Consul/Nomad 生产验证；`LogStore`/`StableStore`/`Transport` 三个接口全部可自定义——对象存储 WAL 的关键前提；单分片一组的 Multi-Raft 编排简单 | dragonboat：Multi-Raft 更强但 API 重、学习成本高；etcd/raft：无网络/存储配套，自研量大。详见 [05-distributed.md](05-distributed.md) §1 | M2 |
| 位图 | `github.com/RoaringBitmap/roaring` | v1.x | 事实标准；delete bitmap / posting list / 索引交并的基础件 | — | M0 |
| 压缩 | `github.com/klauspost/compress` | v1.17+ | zstd 纯 Go 实现，接近 C 性能（`zstd:level=3` 约 1GB/s+）；另有 s2（snappy 升级版）可选 | — | M0 |
| 中文分词 | `github.com/go-ego/gse` | v0.2x | 纯 Go（避免 gojieba 的 CGO），支持自定义词典、HMM/宝石分词 | sego：年久失修 | M0 |
| SIMD 代码生成 | `github.com/mmcloughlin/avo` | — | 以 Go 写汇编生成器，维护 dot/cosine/L2 内核（AVX2+FMA / NEON 双架构） | 手写 plan9 汇编：维护成本高； CGO+C 内核：违反禁 CGO 约束 | M0 |
| 对象存储 | `aws-sdk-go-v2`（S3 兼容端点） | — | S3 协议通吃 MinIO/OSS/COS/GCS-compat | minio-go：亦可，API 更轻但生态位重叠，选一即可 | M2 |
| 指标 | `prometheus/client_golang` | v1.19+ | `/_prometheus/metrics` 端点直接暴露 | — | M0 |
| 追踪 | OpenTelemetry `go.opentelemetry.io/otel` | v1.x | 对齐概要设计 §12 观测选型 | — | M1 |
| 日志 | `log/slog` 标准库 | Go 1.21+ | 结构化 JSON 日志，零依赖 | zap：性能略优但标准库已够用 | M0 |
| CLI 框架 | `spf13/cobra` + `pflag` | — | uniadm 管理工具、unisearch 启动参数 | — | M0 |
| 配置 | 自研轻量分层（默认值 → YAML 文件 → 环境变量） | — | 三层即可，不引入 viper 全家桶 | — | M0 |
| 测试 | 标准库 testing + `testify` + go fuzz | — | 表格驱动、模糊测试（DSL 解析/WAL 回放） | — | M0 |

### 2.2 自研清单（Go 生态无对应生产级组件）

| 组件 | 为什么自研 | 设计文档 |
|------|-----------|---------|
| HNSW 向量索引 | Cross-Link Merge（概要设计 §6.1 核心创新）要求图结构完全可控，任何第三方黑盒库（go-hnsw 等）都不支持增量合并与跨段邻接 | [02-index-engine.md](02-index-engine.md) §2 |
| FST 词典 | Go 生态无 tantivy/lucene 级 FST 库；M0 以有序数组+二分/ART 过渡，M1 自研 FST | [02-index-engine.md](02-index-engine.md) §3.1 |
| BKD 树 | 同上，标量空间索引需与段格式/zone map 深度集成 | [02-index-engine.md](02-index-engine.md) §6 |
| WAL | M0 本地 WAL（group commit/CRC32C）；M2 演进为 Raft log 的对象存储实现（Woodpecker 式） | [01-storage-engine.md](01-storage-engine.md) §3 / [05-distributed.md](05-distributed.md) §2 |
| 列存段格式 | 编码选择矩阵、zone map、footer 协议与查询引擎向量化批模型耦合设计 | [01-storage-engine.md](01-storage-engine.md) §4 |
| 表达式/脚本求值 | painless 子集 → 自研 AST 求值器（禁 CGO 排除了嵌入任何脚本语言 runtime 的可能） | [03-query-engine.md](03-query-engine.md) §9 |

> 自研量评估与人力规划见 [development-plan.md](../development-plan.md) §6。

### 2.3 明确不引入

| 组件 | 原因 |
|------|------|
| bleve / bluge（Go 全文检索库） | 行存+面向单机设计，段格式/合并策略/打分统计均不可控，与列存+分布式目标冲突；仅允许**阅读参考**其 analyzer 实现思路 |
| gojieba（CGO） | 违反禁 CGO 约束，以 gse 替代 |
| etcd（外置） | 违反零外部依赖卖点（G3），元数据 Raft 内置 |
| Kafka/Pulsar | 同上，WAL 即 Raft 日志 |
| Go plugin（`plugin` 包） | 仅 Linux 可用且 ABI 不稳定，M3 索引插件走进程内注册 +（可选）独立进程 sidecar 模式，见 [02-index-engine.md](02-index-engine.md) §1.2 |

---

## 3. Go vs Rust 差异与对策（概要设计 §12 的修订说明）

概要设计选择 Rust 的三大理由：无 GC 确定性延迟、SIMD/内存布局控制、单二进制。转 Go 后逐条对策：

### 3.1 GC 延迟对策 `[M0]`

**问题**：Go GC STW 通常 <1ms，但 mark 阶段的辅助标记（mark assist）会让分配密集的请求延迟抖动。

**对策体系**：

1. **不可变段 → mmap 按需读取，不驻堆**
   - 段文件（`col.*.bin`/`vec.*.bin`/`inv.*`）通过 `mmap` 映射为 `[]byte`，用 `unsafe.Slice` 转为 `[]float32/[]int32` 视图直接访问；
   - 关键收益：亿级向量的 page cache 由 OS 管理，Go 堆只见一个 `[]byte` header（48 字节），**堆大小与数据量解耦**；
   - 这是「ES RAM 随数据线性增长」痛点（概要设计 §2.3.2）在 Go 侧的强化解法。

2. **指针极简化的内存布局**
   - HNSW 图 CSR 邻接表：`neighbors []int32` + `offsets []int32`（扁平数组 + 索引寻址，节点间零指针）；
   - posting list、BKD、列存块同理：所有大数组用值类型切片，**GC 扫描成本 O(数组头)** 而非 O(元素)；
   - 查询执行中间态（ColumnBatch、TopK 堆）从 `sync.Pool` 复用，避免高频分配。

3. **后台任务 GC 预算**
   - merge / 量化降级 / 索引构建任务按批处理（每批 ≤ 64MB 分配量），批间 `runtime.Gosched()` + 检查取消；
   - 进程启动设置 `GOMEMLIMIT`（默认物理内存 75%），`GOGC=100` 起步，实测后随基准报告调优；
   - 后台任务与前台查询的 goroutine 通过**per-shard 写锁 + 信号量**隔离 CPU 争抢（概要设计 §2.3.2 ES merge 争抢痛点的对称防御）。

4. **延迟预算**

| 项 | 目标 | 手段 |
|----|------|------|
| GC STW | < 1ms | Go 1.23 默认可达，无需干预 |
| mark assist 尖刺 | P99 查询额外延迟 < 5ms | 堆稳态（mmap 化）+ 对象池 + GOMEMLIMIT |
| 大对象分配 | 查询路径零 >32KB 堆分配 | ColumnBatch 池化预分配 |

### 3.2 SIMD 对策 `[M0]`

| 项 | 设计 |
|----|------|
| 内核 | dot / cosine / L2 距离、批量比较、过滤位图统计（`popcount`） |
| 生成 | avo 以 Go 语法写生成器 → 产出 `.s` 文件入库（asm 不在运行时生成） |
| 架构 | amd64（AVX2+FMA，16 float/batch）与 arm64（NEON，8 float/batch）双实现 |
| 探测 | 运行时 `internal/cpu` 探测特性，init 期绑定函数指针；无 AVX2 时降级纯 Go 循环（unrolled 4×） |
| 位置 | `internal/simd` 包，单测比对纯 Go 参考实现（随机数据 + 边界长度 0/1/15/16/17） |

Rust `core::arch` 可在普通代码内联 SIMD，Go 必须汇编——这是转译后**真实增加的实现成本**，集中在 `internal/simd`（预计 8 个内核函数 × 2 架构），范围可控。

### 3.3 并发模型 `[M0]`

Rust tokio → Go goroutine，反而简化：

- **写路径**：per-shard 单写者（写入 goroutine 串行消费 batch channel），读路径无锁（mmap 段不可变 + MemTable RWMutex）；
- **flush/merge**：后台 goroutine + `errgroup` 并行编码各列；
- **查询**：per-request goroutine，分片并行用 `errgroup.WithContext`；
- **取消**：全链路 `context.Context` 传递（见 §6.2）。

### 3.4 转译结论

概要设计 §12 的其余选型理由（DSL 手写解析、gRPC、OTel、Go SDK 一等公民）在 Go 下**自然成立**；「单二进制静态发布」由 `CGO_ENABLED=0 go build` 直接达成。整体结论：**架构不变，工程代价向 SIMD 汇编与内存布局纪律转移**。

---

## 4. 模块划分与仓库布局（monorepo）`[M0]`

```
unisearch/                          # Go module: github.com/unisearch-db/unisearch
├── cmd/
│   ├── unisearch/                  # 主二进制：server 模式（--embedded 单机 / --cloud 云原生）
│   └── uniadm/                     # 管理工具：es-compat 扫描、基准、备份恢复
├── internal/                       # 私有实现（Go 强约束，不对 SDK 泄漏）
│   ├── server/                     # HTTP/gRPC 接入层：路由、中间件、ES 兼容 handler
│   │   ├── esapi/                  #   ES REST 逐 API handler（04 文档）
│   │   └── middleware/             #   限流、鉴权、trace、panic recovery
│   ├── engine/                     # 查询引擎（03 文档）
│   │   ├── dsl/                    #   ES DSL → AST 解析
│   │   ├── plan/                   #   逻辑计划 → 物理计划 → 优化器
│   │   ├── exec/                   #   向量化执行算子 + ColumnBatch
│   │   ├── agg/                    #   聚合（HLL/t-digest partial state）
│   │   ├── fusion/                 #   RRF/weighted/relative_score/rerank
│   │   └── script/                 #   表达式求值器（painless 子集）
│   ├── index/                      # 索引引擎（02 文档）
│   │   ├── hnsw/                   #   HNSW + Cross-Link Merge + 量化
│   │   ├── inverted/               #   倒排：词典(ART/FST) + posting + WAND
│   │   ├── scalar/                 #   BKD / 位图 / 布隆 / 主键哈希
│   │   ├── analyzer/               #   20+ 分析器（standard/gse/ngram/...）
│   │   └── registry.go             #   Index 接口注册表
│   ├── storage/                    # 存储引擎（01 文档）
│   │   ├── memtable/               #   列式 MemTable + 冻结语义
│   │   ├── wal/                    #   M0 本地 WAL → M2 Raft log 存储
│   │   ├── segment/                #   段读写、编码矩阵、footer
│   │   ├── merge/                  #   size-tiered 合并调度
│   │   └── tiering/                #   冷热分层（M2）
│   ├── meta/                       # 元数据：M0 单机 JSON + fsync → M2 Raft 状态机
│   ├── raft/                       # M2：hashicorp/raft 封装 + 对象存储 WAL
│   ├── tenant/                     # 多租户：配额、资源组、cold 生命周期
│   ├── coordinator/                # M2：Query/Ingest 协调器（无状态）
│   ├── objstore/                   # S3 客户端封装（M2）
│   ├── config/                     # 分层配置
│   ├── errors/                     # 错误码体系（§6.1）
│   └── simd/                       # avo 生成内核 + 特性探测
├── pkg/                            # 公开 API
│   ├── unisdk/                     # Go 官方 SDK（概要设计 §10 示例 4）
│   └── types/                      # 公共类型：FieldID/SeqNo/SegmentID/DocID 等
├── api/proto/                      # gRPC：shard.proto / meta.proto / admin.proto（05 文档 §7）
├── test/                           # 集成测试、ES 客户端金标准、混沌（06 文档）
│   ├── golden/                     #   ES official client test suite 适配
│   └── chaos/                      #   混沌场景
├── build/                          # 交叉编译脚本、Dockerfile、golangci 配置
└── docs/
```

**分层依赖规则**（由上至下单向）：

```
server → engine → index → storage → (meta, objstore)
                  ↑___________|        index 与 storage 互不 import，
                                         通过 segment 文件格式与接口解耦（01 §4 / 02 §1）
```

- `pkg/` 只依赖 `pkg/types`，绝不 import `internal/`（SDK 二进制兼容承诺）；
- `internal/index` 与 `internal/storage` **互相不依赖**：索引读写段文件（01 §4 格式）而非调用存储代码，这是索引插件化（02 §1）的前提。

---

## 5. 进程模型 `[M0 嵌入式 / M2 云原生]`

同一二进制 `unisearch`，两种启动模式：

### 5.1 嵌入式（M0）

```
unisearch --embedded --data-dir /var/lib/unisearch --listen :9200
```

单进程 = Gateway + 单点 Raft（元数据）+ 全部分片 + 后台任务。数据全部落本地盘（NVMe 直写），无对象存储依赖。对标 Milvus Lite / ClickHouse 单机。

### 5.2 云原生（M2）

```
unisearch --cloud --role meta|coordinator|shard --config unisearch.yaml
```

- `meta`：元数据 Raft 组成员（3/5 节点）；
- `coordinator`：无状态查询/写入协调器，可 LB 水平扩展；
- `shard`：Shard Node，承载分片 Raft 组 + 存储引擎 + 本地执行器，段与 WAL 归档上对象存储。

角色拓扑、路由与故障切换详见 [05-distributed.md](05-distributed.md)。**嵌入式 → 云原生升级**：`--embedded` 数据目录可被 3 节点 meta 集群直接接管（快照导入），无重导数据。

---

## 6. 公共规范

### 6.1 错误码体系 `[M0]`

对齐 ES 错误体（概要设计 §8.6），所有 handler 错误走统一结构：

```go
// internal/errors/errors.go

// APIError 对齐 ES 错误体；unsupported_capability 为本系统扩展字段。
type APIError struct {
    Status  int    `json:"-"`               // HTTP 状态码：400/404/409/429/500/501
    Type    string `json:"type"`            // 对齐 ES：illegal_argument / index_not_found / ...
    Reason  string `json:"reason"`
    Index   string `json:"index,omitempty"`
    // unsupported_capability：当 Type == "unsupported_capability" 时给出
    // 迁移指引（哪个 API/DSL、从哪个版本计划支持、workaround）。
    Unsupported *UnsupportedInfo `json:"unsupported_capability,omitempty"`
}

type UnsupportedInfo struct {
    Feature   string `json:"feature"`    // 如 "query.intervals.unordered"
    Milestone string `json:"milestone"`  // 如 "v1.2 (M1)"
    Workaround string `json:"workaround,omitempty"`
}
```

规则（继承概要设计 §8.6）：
1. **不静默吞参**：不支持的参数 → `400` + `unsupported_capability`；
2. 内部错误（`500`）不泄漏堆栈，完整信息进日志（关联 trace_id）；
3. 错误码注册表集中在 `internal/errors/codes.go`，禁止 handler 手拼 JSON。

### 6.2 Context 与超时 `[M0]`

- 入口 handler 创建 `context`（默认超时：`_search` 30s、`_bulk` 60s、可由 `?timeout` 参数覆盖）；
- 全链路（coordinator → shard → 算子 → SIMD 内核外层）传播取消；
- 后台任务用独立 `context`（不随请求取消），但受 shutdown 信号控制；
- graceful shutdown 顺序：停止接入 → 排空进行中请求（≤10s）→ flush MemTable → 关 WAL → 退出。

### 6.3 配置规范 `[M0]`

```yaml
# unisearch.yaml —— 三层：内置默认 < YAML < 环境变量（UNISEARCH_ 前缀）
mode: embedded            # embedded | cloud
listen: ":9200"
data:
  dir: /var/lib/unisearch
  memtable:
    flush_size_mb: 512
    flush_interval: 30s
storage:
  segment_block_rows: 8192
index:
  hnsw: { m: 16, ef_construction: 200, quantization: sq8 }
tenant:
  default_mode: shared-shard
```

所有配置项必须在 `internal/config` 声明默认值并生成文档（`unisearch --dump-config`），禁止零值隐式行为。

### 6.4 日志与 trace 字段 `[M0/M1]`

`slog` JSON handler，必带字段：`ts level msg tenant index shard_id segment_id trace_id span_id`。查询慢日志（`>1s`）额外输出 `took_ms shards_scanned rows_read plan_summary`。完整规范见 [06-quality-and-observability.md](06-quality-and-observability.md)。

---

## 7. 概要设计痛点对策 → 详细设计索引

概要设计 §2.4 逐条对策在本系列中的落点（评审检查表）：

| 概要设计痛点 | 对策 | 详细设计落点 |
|---|---|---|
| ES：段多图多、merge 重建图 | 全局统一图 + Cross-Link Merge | [02 §2.4](02-index-engine.md) |
| ES：RAM 线性增长 | 分级索引（热 HNSW/温 RaBitQ/冷 DiskANN） | [02 §2.5](02-index-engine.md) + 本文档 §3.1（mmap 化）+ [01 §9](01-storage-engine.md) |
| CH：mutation 重写 part | 主键索引 + delete bitmap | [01 §6](01-storage-engine.md) |
| Milvus：外部依赖多 | 全内置 Raft WAL + 元数据 | [05 §1-§2](05-distributed.md) |
| Milvus：broadcast-collect 汇聚瓶颈 | 两阶段 MPP（分片预聚合） | [03 §4](03-query-engine.md) |
| Weaviate：集群规模受限 | 元数据只存路由/schema，Raft 组可分片 | [05 §1.3](05-distributed.md) |
| 各家：实时性/一致性折衷 | Raft WAL 强一致 + 可见性水位 + MVCC 快照读 | [01 §8](01-storage-engine.md) |
| ES：IDF 分片局部不可比 | 全局词频统计表 + global_stats 开关 | [03 §7](03-query-engine.md) |
| ES：refresh 近实时 | 写入即入 MemTable 可见（强一致读） | [01 §2](01-storage-engine.md) |

---

## 8. 全系列术语表（命名规范）

| 术语 | 含义 | 出现文档 |
|------|------|---------|
| `SeqNo` (`seq_no`) | MVCC 版本号，Raft 提交序号复用；同时承载 ES `if_seq_no` 语义 | 01/03/04/05 |
| `Watermark` | 可见性水位：小于等于该 seq_no 的写入对查询可见 | 01/05 |
| `SegmentID` / `RowID` | 段内行定位二元组 `(segment_id, row_id)`，delete bitmap 寻址基础 | 01/02/03 |
| `MemTable` | 列式内存写缓冲，冻结后 flush 为 Segment | 01 |
| `ColumnBatch` | 向量化执行批（默认 4096 行），全查询引擎中间态单位 | 03 |
| `ZoneMap` | 段内块级 min/max 跳数索引 | 01/02/03 |
| `DeleteBitmap` | `map[SegmentID]*roaring.Bitmap`，逻辑删除位图 | 01/03 |
| `Cross-Link` | 段间 HNSW 顶层邻接与增量合并算法族 | 02 |
| `Fusion` | 混合检索多路结果融合器（rrf/weighted/relative_score/rerank） | 03 |
| `PartialState` | 分片级可序列化聚合中间态（HLL/t-digest 等） | 03/05 |
| `PIT` | Point-in-Time，MVCC 快照游标 | 03/04 |
| `LogicalShard` | 逻辑分片（路由单位，可在线分裂），区别于物理副本 Raft 组 | 05 |

---

*本文档为系列总纲。修订需同步检查 §1.1 映射表与 §7 检查表。*
