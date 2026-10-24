# Proposal: IResearch 特性引入 — 高级分析器 + I/O 优化 + 查询打分增强

## 1. 背景与动机 (Background & Motivation)

### 1.1 当前问题

IResearch 是一个 C++ 嵌入式全文检索引擎库（EMC → ArangoDB → 蚂蚁集团），在检索引擎底层优化方面有多年积累。UniSearch 是 Go 实现的融合检索数据库（ES 8.x 兼容），当前已完成 M0-M3 核心功能，但在以下方面存在差距：

- **分析器层**：仅有基于规则的分词+过滤链（standard/simple/whitespace/keyword/chinese-gse/ngram/edge_ngram + lowercase/stop/synonym），缺少 ML 驱动的分析器、LSH 签名、locale 排序规则、多分析器串联等高级文本处理能力。
- **存储 I/O 层**：段文件通过 `os.ReadFile` 全量读入内存（page-cache 语义），未使用 mmap/madvise；冷段从 S3 下载无本地缓存层；列加载惰性无预热机制；合并后旧段立即删除无引用计数保护。
- **查询与打分层**：范围查询仅块级 zone map 剪枝无块内细粒度索引；无过滤器缓存导致重复查询重复计算；无 SamePosition 位置查询；仅有 BM25 评分无 TF-IDF 替代；无 NGram 相似度打分。

### 1.2 用户期望

通过系统性地从 iresearch 引入 13 个特性，补齐 uni-search 在分析器、存储 I/O、查询打分三个方向的能力短板，提升检索性能、文本处理丰富度和企业级安全合规能力，同时保持纯 Go（CGO_ENABLED=0）、单二进制、ES API 兼容三大约束。

## 2. 目标 (Objective)

1. **基础设施层（Phase 1）**：建立 mmap + madvise 文件访问基础设施，引入过滤器缓存加速重复查询。
2. **核心功能层（Phase 2）**：补齐分析器（MinHash/Collation/Pipeline）、存储（BlockCache/列预热/段GC）、查询（Granular Range/SamePosition）三层的核心缺失能力。
3. **高级功能层（Phase 3）**：引入 ML 驱动的 FastText 分析器和兼容性评分器（NGram 相似度/TF-IDF）。

## 3. 方案设计 (Approach)

### 3.1 设计原则

- **纯 Go 实现**：所有特性通过 `syscall` 包或纯 Go 库实现，不引入 CGO 依赖。
- **向后兼容**：段格式变更通过 FormatVersion 升级，旧段在新代码下自适应读取；ES API 变更不破坏现有行为。
- **分阶段交付**：按依赖关系分三阶段，Phase 1 基础设施先行，Phase 2 核心功能并行开发，Phase 3 高级功能独立迭代。
- **复用现有架构**：新分析器通过 `factory.go` 注册机制接入，新查询通过 DSL AST + translator + exec operator 链路接入，新存储能力通过段 reader/writer 改造接入。

### 3.2 核心方案

#### 3.2.1 分阶段引入路线

```
Phase 1 (P0, 2-3 周): 基础设施层
┌─────────────────────────────────────────────┐
│ 特性1: IOAdvice + mmap 全量迁移              │
│ 特性2: Proxy Filter 缓存过滤器               │
└─────────────────────────────────────────────┘
         │ (特性1 为 Phase 2 存储特性提供基础)
         ▼
Phase 2 (P1, 4-5 周): 核心功能层
┌─────────────────────────────────────────────┐
│ 特性3: MinHash 签名分析器     (独立)         │
│ 特性4: Collation 排序规则     (独立)         │
│ 特性5: Pipeline 串联分析器    (依赖 3,4)     │
│ 特性6: BlockCache 缓存目录    (依赖 1)       │
│ 特性7: 列预热 Column Warmup   (依赖 1,6)     │
│ 特性8: 段GC 自动清理旧版本    (独立)         │
│ 特性9: Granular Range 粒度范围(独立,需版本) │
│ 特性10: SamePosition 查询    (独立)         │
└─────────────────────────────────────────────┘
         ▼
Phase 3 (P2, 3-4 周): 高级功能层
┌─────────────────────────────────────────────┐
│ 特性11: FastText 分类/近邻分析器(独立,高复杂) │
│ 特性12: NGram 相似度打分      (独立)         │
│ 特性13: TF-IDF 评分器         (独立)         │
└─────────────────────────────────────────────┘
```

#### 3.2.2 依赖关系图

```
特性1 IOAdvice/mmap ──┬──> 特性6 BlockCache ──> 特性7 列预热
                      └──> 特性8 段GC (交互)
                      └──> 特性9 Granular Range (协同)

特性3 MinHash ──┐
特性4 Collation ┼──> 特性5 Pipeline 串联分析器

特性2 Proxy Filter (独立)
特性10 SamePosition (独立)
特性11 FastText (独立, Phase 3)
特性12 NGram 相似度 (独立)
特性13 TF-IDF (独立)
```

### 3.3 技术选型

| 决策点 | 选择 | 理由 |
|--------|------|------|
| mmap 实现 | `syscall.Mmap` + `syscall.Madvise` | Go 标准库 syscall 包，CGO_ENABLED=0 兼容 |
| 平台兼容 | build tag 分文件 (`_linux.go` / `_darwin.go`) | Linux/macOS madvise 常量名不同 |
| Collation 库 | `golang.org/x/text/collate` | 纯 Go ICU 级别 collation 支持 |
| FastText 模型 | 纯 Go 解析 `.bin`/`.ftz` 格式 | 不依赖 C++ FastText 库，复用 `internal/simd` 加速矩阵乘法 |
| Filter 缓存 | LRU + FNV-1a hash | 简单高效，roaring.Bitmap 作为值 |
| Granular Range | 新增 FormatVersion (v2→v3) | 向后兼容，旧段不写粒度桶，新段写入，读取层自适应 |

## 4. 变更范围 (Impact)

### 4.1 新增文件

| 文件路径 | 说明 |
|----------|------|
| `internal/storage/mmap/mmap.go` | mmap 文件抽象（MmapFile 类型、Advise 常量、Open/Bytes/Close 方法） |
| `internal/storage/mmap/mmap_linux.go` | Linux 平台 madvise 常量映射 |
| `internal/storage/mmap/mmap_darwin.go` | macOS 平台 madvise 常量映射 |
| `internal/engine/cache/filter_cache.go` | Proxy Filter 缓存（LRU + TTL + 段变更失效） |
| `internal/index/analyzer/minhash_filter.go` | MinHash 签名 TokenFilter |
| `internal/index/analyzer/collation_filter.go` | Collation 排序规则 TokenFilter |
| `internal/index/analyzer/pipeline_analyzer.go` | Pipeline 串联分析器（多 Analyzer 链） |
| `internal/storage/cache/block_cache.go` | 块级 LRU 缓存 |
| `internal/storage/cache/cached_input.go` | CachedIndexInput 包装器 |
| `internal/storage/gc/segment_gc.go` | 段文件引用计数 GC |
| `internal/index/inverted/ngram_scorer.go` | NGram 相似度打分器 |
| `internal/index/inverted/tfidf.go` | TF-IDF 评分器 |
| `internal/index/analyzer/fasttext_model.go` | 纯 Go FastText 模型加载器 |
| `internal/index/analyzer/fasttext_analyzer.go` | FastText 分类/近邻分析器 |

### 4.2 修改文件

| 文件路径 | 修改内容 |
|----------|----------|
| `internal/storage/segment/reader.go` | column()/idIndex() 迁移到 mmap；新增 Warmup 方法；ColumnReader 集成 BlockCache |
| `internal/storage/column_provider.go` | segIndexInput.Open() 迁移到 mmap + advice |
| `internal/storage/shard.go` | recoverSegments() 设置 advice；Snapshot() 后触发预热 |
| `internal/storage/segmentset.go` | segmentSetHolder 集成引用计数 + 段变更通知 |
| `internal/storage/merge.go` | MergeSegments 删除逻辑改为标记可回收 |
| `internal/storage/flush.go` | flushTable 末尾可选预热；tmp 目录清理接入 GC |
| `internal/storage/segment/column_writer.go` | 块编码新增粒度桶统计写入 |
| `internal/storage/segment/column_reader.go` | BlockMeta 新增 GranularZones 字段 |
| `internal/storage/segment/wire.go` | BlockIndex proto 新增 granular_zones 字段 |
| `internal/storage/segment/codec.go` | FormatVersion 升级 v2→v3 |
| `internal/storage/tiering/tier_manager.go` | 冷段读取路径集成 BlockCache |
| `internal/engine/dsl/ast.go` | 新增 SamePositionQuery / NGramSimilarityQuery AST 节点 |
| `internal/engine/dsl/parser.go` | 新增 same_position / ngram_similarity 解析分支 |
| `internal/engine/exec/scan.go` | 新增 GranularRangePredicate 块内桶级过滤 |
| `internal/engine/exec/operators.go` | BitmapFilterOp 新增 FromCachedBitmap；新增 NGramScoreOp |
| `internal/engine/plan/optimizer.go` | R2 规则扩展支持 GranularRangeSpec |
| `internal/index/inverted/index.go` | 新增 SearchSamePosition 方法；Config 新增 ScorerType；ScoreDocs 支持 TF-IDF |
| `internal/index/inverted/wand.go` | computeUpperBound 支持 TF-IDF 上界 |
| `internal/index/analyzer/factory.go` | 新增 minhash/collation/pipeline/fasttext 分支 |
| `internal/embedded/translator.go` | 新增 translateSamePosition/translateNGramSimilarity；translateBool 集成 FilterCache；translateMatch 支持 ScorerType 选择；sort 路径支持 collation |
| `internal/meta/mapping.go` | FieldDef.Similarity 新增 "tfidf" 值 |
| `internal/server/esapi/admin_handlers.go` | 新增 POST /_warmup handler |
| `internal/config/config.go` | 新增 MmapAdvice / FilterCache / CacheDir / SegmentRetention 配置 |
| `go.mod` | 添加 golang.org/x/text 依赖 |

### 4.3 不涉及

- **不修改 HNSW 向量索引核心**：iresearch 无向量搜索能力，本任务不涉及 HNSW 算法变更。
- **不修改 Raft 共识层**：段 GC 的引用计数在存储层内解决，不涉及 Raft 协议变更。
- **不修改 gRPC 协议**：新特性（warmup API 等）通过 REST API 暴露，不修改 proto 定义。
- **不修改 Web 管控前端**：新分析器和查询类型通过现有 `_analyze` 和 `_search` API 验证，不新增前端页面。
- **不修改 K8s Operator**：新配置项通过 `unisearch.yaml` 传递，不涉及 CRD 变更。

## 5. 接口定义 (Interface Definition)

### 5.1 新增 REST API

#### POST /_warmup — 列预热

```
POST /{index}/_warmup
Content-Type: application/json

{
  "fields": ["title", "content", "_id"]
}
```

响应：
```json
{
  "acknowledged": true,
  "warmed_fields": ["title", "content", "_id"],
  "segment_count": 5
}
```

### 5.2 新增 Analyzer 配置

#### MinHash 分析器
```json
{
  "settings": {
    "analysis": {
      "analyzer": {
        "my_minhash": {
          "type": "minhash",
          "num_perm": 128,
          "seed": 42
        }
      }
    }
  }
}
```

#### Collation 分析器
```json
{
  "settings": {
    "analysis": {
      "analyzer": {
        "my_collation": {
          "type": "collation",
          "locale": "zh-Hans",
          "strength": "primary"
        }
      }
    }
  }
}
```

#### Pipeline 分析器
```json
{
  "settings": {
    "analysis": {
      "analyzer": {
        "my_pipeline": {
          "type": "pipeline",
          "analyzers": ["standard", "my_collation", "ngram"]
        }
      }
    }
  }
}
```

#### FastText 分析器
```json
{
  "settings": {
    "analysis": {
      "analyzer": {
        "my_fasttext": {
          "type": "fasttext",
          "model_path": "/data/models/lid.176.ftz",
          "mode": "classification",
          "dim": 16
        }
      }
    }
  }
}
```

### 5.3 新增 Query DSL

#### SamePosition 查询
```json
{
  "query": {
    "same_position": {
      "field": "synonyms",
      "terms": ["happy", "glad"],
      "boost": 1.2
    }
  }
}
```

#### NGram 相似度查询
```json
{
  "query": {
    "ngram_similarity": {
      "field": "title",
      "text": "hello world",
      "ngram_size": 2,
      "threshold": 0.3
    }
  }
}
```

### 5.4 新增 Similarity 配置

```json
{
  "mappings": {
    "properties": {
      "content": {
        "type": "text",
        "similarity": "tfidf"
      }
    }
  }
}
```

### 5.5 新增配置项

```yaml
storage:
  mmap_advice:
    column: random
    terms: random
    postings: sequential
    footer: willneed
  cache_dir: /data/cache
  cache_size_mb: 1024
  segment_retention:
    max_old_segments: 3
    temp_dir_ttl: 300s

query:
  filter_cache:
    max_size: 10000
    ttl: 60s
```

## 6. 稳定性架构 (Stability Architecture)

### 6.1 限流与熔断

- **Filter Cache 限流**: LRU 容量上限 + TTL 过期，防止缓存无限增长。`MaxSize` 默认 10000 条目，`TTL` 默认 60s。
- **BlockCache 内存限制**: 按内存字节数配置（`CacheSizeMB` 默认 1024MB），超容量淘汰最久未访问的块。
- **FastText 模型加载**: 单次加载，`atomic.Pointer` 原子切换，不阻塞查询。模型文件大小限制（建议 < 500MB）。

### 6.2 资源隔离与限制

- **mmap 内存**: mmap 映射区域不计入 Go 堆，通过 madvise 控制页面驻留。`MADV_DONTNEED` 主动释放不活跃段页面。
- **段 GC 回收**: 后台 goroutine 周期扫描（默认 60s 间隔），引用计数为 0 的段才可回收。`MaxOldSegments` 限制保留旧段数量。
- **列预热并发**: `Warmup` 方法使用 `errgroup` 并发预解码，并发度受 GOMAXPROCS 约束。

### 6.3 灾难恢复

- **mmap 失败回退**: `MmapFile.Open` 失败时回退到 `os.ReadFile`，记录 warning 日志，不中断服务。
- **Filter Cache 失败**: 缓存读写失败时降级为无缓存模式，正常执行 filter，不影响查询正确性。
- **段 GC 崩溃恢复**: 引用计数持久化到段元数据文件，进程重启后从元数据恢复引用计数。`.tmp` 目录在 `recoverSegments` 时清理（已有逻辑，GC 增强）。
- **FastText 模型缺失**: 模型文件不存在时分析器返回原始 token（passthrough），不报错。

## 7. 可测性设计 (Testability Design)

### 7.1 本地手动验证

- **Step 1**: `make build` 确认编译通过
- **Step 2**: `./unisearch --embedded --data-dir ./data --listen :9200` 启动服务
- **Step 3**: 创建索引配置 MinHash/Collation/Pipeline 分析器，通过 `POST /_analyze` 验证分析器输出
- **Step 4**: 写入文档，执行 `same_position` / `ngram_similarity` 查询验证结果
- **Step 5**: 配置 `similarity: "tfidf"` 字段，执行 match 查询对比 BM25 和 TF-IDF 分数差异
- **Step 6**: `POST /{index}/_warmup` 触发列预热，观察日志确认预热完成
- **Step 7**: 重复执行相同 filter 查询，观察 `/_prometheus/metrics` 中 filter_cache_hit_ratio 指标
- **Step 8**: `cat /proc/$(pidof unisearch)/smaps | grep -A1 mmap` 验证 madvise 生效

### 7.2 自动化测试策略

| 测试类型 | 覆盖范围 |
|----------|----------|
| **单元测试** | 每个新特性至少 3 个测试用例：正常路径、边界条件、错误处理 |
| | MinHash: 签名维度正确性、空输入、seed 确定性 |
| | Collation: 多 locale 排序正确性、strength 级别、空输入 |
| | Pipeline: 多阶段 offset 映射、单分析器退化、空链 |
| | mmap: Open/Close/Advise、mmap 失败回退、大文件映射 |
| | FilterCache: 命中/未命中、TTL 过期、段变更失效、LRU 淘汰 |
| | BlockCache: 块命中/未命中、容量淘汰、并发安全 |
| | SegmentGC: 引用计数增减、0 引用回收、并发查询中不回收 |
| | GranularRange: 桶级剪枝正确性、边界值、空桶 |
| | SamePosition: 位置交集匹配、无位置信息、多 term |
| | NGram 相似度: Dice 系数计算、阈值过滤、空文本 |
| | TF-IDF: 分数计算正确性、与 BM25 对比、WAND 上界 |
| | FastText: 模型加载、分类输出、近邻搜索、模型缺失回退 |
| **集成测试** | Python E2E: 新查询类型通过 `_search` API 验证；新分析器通过 `_analyze` API 验证；warmup API 端到端 |
| **回归测试** | 现有 163+ Python E2E 测试全绿；现有单元测试全绿；B1-B10 基准性能不退化 |
| **兼容测试** | 旧段（FormatVersion v2）在新代码（v3）下正常读取；新段在旧代码下跳过粒度桶（向前兼容） |
| **混沌测试** | mmap 失败回退场景；Filter Cache 段合并后失效正确性；段 GC 并发查询中回收保护 |

## 8. 风险与缓解

| 风险 | 缓解措施 |
|------|----------|
| **mmap 全量迁移涉及所有段文件读取路径** | 分文件类型逐步迁移（先列文件，后倒排索引），每步编译+测试验证；mmap 失败回退 `os.ReadFile`；Linux/macOS 交叉编译验证 |
| **段格式升级（v2→v3）向后兼容** | `codec.go` 版本判断自适应：v3 代码读 v2 段跳过粒度桶解析；v2 代码读 v3 段跳过未知扩展字段（proto 向前兼容）；新增专门兼容测试用例 |
| **Filter Cache 缓存失效正确性** | 监听 `segmentSetHolder.store` 事件，段合并/刷新后失效相关条目；缓存条目携带 segmentID，段变更时按 segmentID 批量失效 |
| **FastText 纯 Go 实现性能** | 矩阵乘法复用 `internal/simd` 的 AVX2+FMA/NEON 内核；模型加载为一次性操作，推理路径不涉及 IO；大模型(>1GB)限制+文档警告 |
| **Pipeline 分析器 offset 映射错位** | 每阶段维护 offset 映射表，最终映射回原始文本；单测覆盖多阶段 offset 正确性；退化场景（单分析器）offset 直接透传 |
| **段 GC 引用计数并发安全** | 引用计数用 `atomic.Int64`；查询持有段引用期间不可回收；GC goroutine 与查询路径通过引用计数同步，不依赖锁 |

## 9. 验收标准

- [ ] **编译**: `CGO_ENABLED=0 go build ./...` 和 `go vet ./...` 全绿
- [ ] **测试**: `go test ./...` 全绿，每个特性有对应的单元测试
- [ ] **ES 兼容**: 新增查询类型和 API 不破坏现有 ES 8.x REST API 行为（Python E2E 测试 163+ 用例全绿）
- [ ] **段格式兼容**: Granular Range 的 FormatVersion v3 向后兼容 v2，旧段可正常读取
- [ ] **mmap 验证**: 段文件 mmap 后内存占用不超过 page cache 方式，madvise 提示生效（`/proc/<pid>/smaps` 验证）
- [ ] **缓存验证**: Filter Cache 命中率 > 80%（重复查询场景），BlockCache 冷段读取延迟降低 > 50%
- [ ] **分析器验证**: MinHash/Collation/Pipeline/FastText 分析器通过 `_analyze` API 可验证输出
- [ ] **性能不退化**: 引入后现有基准测试（B1-B10）性能不退化
