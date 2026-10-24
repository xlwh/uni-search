# Spec: IResearch 特性引入 — 高级分析器 + I/O 优化 + 查询打分增强

## 1. 概述 (Overview)

本 Spec 将 iresearch 搜索引擎的 13 个特性分三个阶段引入 uni-search，覆盖高级分析器（4 个）、I/O 与存储优化（4 个）、查询与打分增强（5 个）。所有特性以纯 Go 实现，保持 CGO_ENABLED=0、单二进制、ES API 兼容三大约束。

### 变更清单

| 类型 | 文件路径 | 说明 |
|------|----------|------|
| 新增 | `internal/storage/mmap/mmap.go` | mmap 文件抽象（MmapFile 类型） |
| 新增 | `internal/storage/mmap/mmap_linux.go` | Linux madvise 常量 |
| 新增 | `internal/storage/mmap/mmap_darwin.go` | macOS madvise 常量 |
| 新增 | `internal/storage/mmap/mmap_test.go` | mmap 单元测试 |
| 新增 | `internal/engine/cache/filter_cache.go` | Proxy Filter 缓存（LRU+TTL） |
| 新增 | `internal/engine/cache/filter_cache_test.go` | Filter Cache 测试 |
| 新增 | `internal/index/analyzer/minhash_filter.go` | MinHash 签名 TokenFilter |
| 新增 | `internal/index/analyzer/collation_filter.go` | Collation 排序规则 TokenFilter |
| 新增 | `internal/index/analyzer/pipeline_analyzer.go` | Pipeline 串联分析器 |
| 新增 | `internal/index/analyzer/fasttext_model.go` | FastText 模型加载器 |
| 新增 | `internal/index/analyzer/fasttext_analyzer.go` | FastText 分析器 |
| 新增 | `internal/storage/cache/block_cache.go` | 块级 LRU 缓存 |
| 新增 | `internal/storage/cache/cached_input.go` | CachedIndexInput 包装器 |
| 新增 | `internal/storage/gc/segment_gc.go` | 段引用计数 GC |
| 新增 | `internal/index/inverted/ngram_scorer.go` | NGram 相似度打分器 |
| 新增 | `internal/index/inverted/tfidf.go` | TF-IDF 评分器 |
| 修改 | `internal/storage/segment/reader.go` | mmap 迁移 + Warmup + BlockCache 集成 |
| 修改 | `internal/storage/segment/column_writer.go` | 粒度桶统计写入 |
| 修改 | `internal/storage/segment/column_reader.go` | BlockMeta 新增 GranularZones |
| 修改 | `internal/storage/segment/codec.go` | FormatVersion v2→v3 |
| 修改 | `internal/storage/column_provider.go` | segIndexInput mmap 迁移 |
| 修改 | `internal/storage/shard.go` | recoverSegments advice + 预热触发 |
| 修改 | `internal/storage/segmentset.go` | 引用计数 + 段变更通知 |
| 修改 | `internal/storage/merge.go` | 删除逻辑改为标记可回收 |
| 修改 | `internal/storage/flush.go` | 预热 + tmp 清理接入 GC |
| 修改 | `internal/engine/dsl/ast.go` | SamePositionQuery / NGramSimilarityQuery |
| 修改 | `internal/engine/dsl/parser.go` | 新增解析分支 |
| 修改 | `internal/engine/exec/scan.go` | GranularRangePredicate |
| 修改 | `internal/engine/exec/operators.go` | FromCachedBitmap + NGramScoreOp |
| 修改 | `internal/engine/plan/optimizer.go` | R2 规则 GranularRangeSpec |
| 修改 | `internal/index/inverted/index.go` | SearchSamePosition + ScorerType |
| 修改 | `internal/index/inverted/wand.go` | TF-IDF 上界 |
| 修改 | `internal/index/analyzer/factory.go` | 新分析器分支 |
| 修改 | `internal/embedded/translator.go` | 新查询翻译 + 缓存集成 |
| 修改 | `internal/meta/mapping.go` | Similarity 新增 "tfidf" |
| 修改 | `internal/server/esapi/admin_handlers.go` | POST /_warmup |
| 修改 | `internal/config/config.go` | 新配置项 |
| 修改 | `go.mod` | 添加 golang.org/x/text |

## 2. 详细设计 (Detailed Design)

### 2.1 Phase 1 — 特性 1: IOAdvice + mmap 全量迁移

**变更内容**：

将段文件读取从 `os.ReadFile` 全量迁移到 `syscall.Mmap` + `syscall.Madvise`，通过启动探测 + 配置开关控制 mmap 启用。

**MmapFile 类型**：

```go
package mmap

// MmapAdvice hints the kernel about expected access patterns.
type MmapAdvice int

const (
    AdviseNormal     MmapAdvice = iota // MADV_NORMAL (default)
    AdviseRandom                        // MADV_RANDOM (disable readahead)
    AdviseSequential                    // MADV_SEQUENTIAL (increase readahead)
    AdviseWillNeed                      // MADV_WILLNEED (prefault pages)
    AdviseDontNeed                      // MADV_DONTNEED (release pages)
)

// MmapFile wraps a memory-mapped file region.
type MmapFile struct {
    data []byte    // mapped region
    fd   int       // underlying file descriptor
    size int64     // file size
}

// Open maps the file at path into memory. If mmap fails or is disabled,
// falls back to os.ReadFile and returns a MmapFile with data populated
// from the file content (fd = -1 indicates fallback mode).
func Open(path string, advice MmapAdvice) (*MmapFile, error)

// Bytes returns the mapped byte slice.
func (m *MmapFile) Bytes() []byte

// Advise updates the kernel advice for the mapped region.
func (m *MmapFile) Advise(advice MmapAdvice) error

// Close unmaps the region (or frees the fallback buffer) and closes the fd.
func (m *MmapFile) Close() error
```

**启动探测逻辑**：

```go
// probeMmap tests whether syscall.Mmap is functional by mapping a
// 1-byte temporary file. Returns nil if mmap is available.
func probeMmap() error

// Enabled is the global toggle, set once at startup based on
// config.StorageConfig.MmapAdvice.Enabled && probeMmap() == nil.
var Enabled bool
```

**段文件读取迁移**：

```go
// segment/reader.go — column() method migration
func (s *Segment) column(field types.FieldID) (*ColumnReader, error) {
    // ... lock + cache check unchanged ...
    data, err := mmapLoad(filepath.Join(s.dir, colFileName(field)), mmap.AdviseRandom)
    if err != nil {
        return nil, fmt.Errorf("segment: field %d not stored: %w", field, err)
    }
    r, err := ReadColumnFile(data)
    // ... cache + return unchanged ...
}

// mmapLoad is the unified file loader: uses MmapFile when Enabled,
// falls back to os.ReadFile when disabled.
func mmapLoad(path string, advice mmap.MmapAdvice) ([]byte, error)
```

**advice 策略映射**：

| 文件类型 | Advice | 理由 |
|----------|--------|------|
| 列文件 (col_*) | `MADV_RANDOM` | 块级跳读，非顺序 |
| 词典 (terms.sorted) | `MADV_RANDOM` | 二分查找 |
| 倒排链 (post.bin) | `MADV_SEQUENTIAL` | 顺序扫描 posting list |
| footer | `MADV_WILLNEED` | 打开时立即需要 |
| id.hash | `MADV_RANDOM` | 哈希查找 |
| src.zst | `MADV_SEQUENTIAL` | 顺序解压 |

**配置项**：

```go
type MmapAdviceConfig struct {
    Enabled    bool   `yaml:"enabled"     default:"true"`
    Column     string `yaml:"column"      default:"random"`
    Terms      string `yaml:"terms"       default:"random"`
    Postings   string `yaml:"postings"    default:"sequential"`
    Footer     string `yaml:"footer"      default:"willneed"`
    IDHash     string `yaml:"id_hash"     default:"random"`
    Source     string `yaml:"source"      default:"sequential"`
}
```

### 2.2 Phase 1 — 特性 2: Proxy Filter 缓存过滤器

**变更内容**：

新增 `FilterCache`，缓存 filter DSL 匹配的文档 ID 集合（roaring.Bitmap，段级粒度），相同 filter 的后续查询直接复用。

**FilterCache 类型**：

```go
package cache

// FilterCache caches filter→bitmap results per segment.
type FilterCache struct {
    maxEntries int
    ttl        time.Duration
    mu         sync.RWMutex
    entries    map[uint64]*cacheEntry  // key = fnv1a(AST-normalized filter)
    lru        *list.List              // LRU eviction order
    notifyCh   chan SegmentChangeEvent // segment change notification
}

type cacheEntry struct {
    key        uint64
    segmentID  types.SegmentID
    bitmap     *roaring.Bitmap
    expireAt   time.Time
    lruElem    *list.Element
}

// NewFilterCache creates a cache with the given capacity and TTL.
func NewFilterCache(maxEntries int, ttl time.Duration) *FilterCache

// Get returns the cached bitmap for (key, segmentID), or nil if miss/expired.
func (fc *FilterCache) Get(key uint64, segID types.SegmentID) *roaring.Bitmap

// Put stores the bitmap for (key, segmentID).
func (fc *FilterCache) Put(key uint64, segID types.SegmentID, bm *roaring.Bitmap)

// InvalidateSegment removes all entries for the given segmentID.
// Called when segments are merged or flushed.
func (fc *FilterCache) InvalidateSegment(segID types.SegmentID)

// StartJanitor starts a background goroutine that evicts expired entries
// and processes segment change notifications.
func (fc *FilterCache) StartJanitor(ctx context.Context)
```

**AST 规范化 + hash**：

```go
// NormalizeFilter parses filter DSL JSON into an AST, performs structural
// normalization (sort field names, fold constants, canonicalize equivalent
// expressions), serializes the canonical form, and returns its FNV-1a hash.
func NormalizeFilter(filterJSON []byte) (uint64, error)
```

规范化步骤：
1. 解析 filter JSON 为 `dsl.Query` AST 节点
2. 递归规范化：对 `bool` 子句的 `must`/`should`/`filter` 数组按结构 hash 排序；合并嵌套的同类 bool；折叠常量表达式（如 `term: {a: 1}` AND `term: {a: 1}` → `term: {a: 1}`）
3. 序列化规范化 AST 为确定性 JSON（key 排序、无冗余空白）
4. 计算序列化字节的 FNV-1a 64-bit hash

**translator 集成**：

```go
// embedded/translator.go — translateBool filter path
func (t *Translator) translateBoolFilter(ctx context.Context, q *dsl.BoolQuery, seg *segment.Segment) (Operator, error) {
    filterKey, _ := NormalizeFilter(q.Filter.RawJSON)
    if cached := t.filterCache.Get(filterKey, seg.ID()); cached != nil {
        // Cache hit: skip ScanOp + PredFilterOp, use cached bitmap directly
        return NewBitmapFilterFromCached(cached), nil
    }
    // Cache miss: normal execution path
    op, bm := t.buildFilterScan(ctx, q.Filter, seg)
    if bm != nil {
        t.filterCache.Put(filterKey, seg.ID(), bm)
    }
    return op, nil
}
```

**BitmapFilterOp 新增构造器**：

```go
// FromCachedBitmap constructs a BitmapFilterOp that uses a pre-computed
// bitmap from the filter cache. No input operator is needed.
func NewBitmapFilterFromCached(bm *roaring.Bitmap) *BitmapFilterOp
```

### 2.3 Phase 2 — 特性 3: MinHash 签名分析器

**变更内容**：

新增 `MinHashFilter`，实现 `TokenFilter` 接口，计算 token 集合的 MinHash 签名。

```go
// MinHashFilter computes MinHash signatures over token Term sets.
type MinHashFilter struct {
    numPerm int          // signature dimension (default 128)
    hashFns []hashFunc   // parameterized: h_i(x) = (a_i * x + b_i) mod p
}

type hashFunc struct {
    a, b uint64  // random coefficients
    p    uint64  // large prime (2^61 - 1, Mersenne prime)
}

func NewMinHashFilter(numPerm int, seed int64) (*MinHashFilter, error)

// Filter implements TokenFilter. For each input token set, computes
// MinHash signatures and emits them as synthetic tokens:
// Term = "mh_<dim>_<hash>", Type = "<MINHASH>", Pos preserved.
func (f *MinHashFilter) Filter(tokens []Token) []Token
```

算法：
1. 初始化 `numPerm` 个参数化哈希函数 `h_i(x) = (a_i * x + b_i) mod p`，`p = 2^61 - 1`，`a_i`/`b_i` 由 `seed` 通过 `math/rand` 确定性生成
2. 对输入 token 集合，计算每个 token Term 的 FNV-1a hash 值 `x`
3. 对每个 `h_i`，取所有 `h_i(x)` 的最小值作为签名第 `i` 维
4. 输出 `numPerm` 个合成 token：`Term = fmt.Sprintf("mh_%d_%d", i, minHash_i)`, `Type = "<MINHASH>"`

**factory.go 注册**：

```go
NameMinHash = "minhash"
// params: num_perm (int, default 128), seed (int, default 42)
```

### 2.4 Phase 2 — 特性 4: Collation 排序规则

**变更内容**：

新增 `CollationFilter`，使用 `golang.org/x/text/collate` 将 token Term 转换为 locale 感知的排序键。

```go
import "golang.org/x/text/collate"
import "golang.org/x/text/language"

// CollationFilter converts tokens to locale-aware sort keys.
type CollationFilter struct {
    collator *collate.Collator
}

func NewCollationFilter(locale string, strength string) (*CollationFilter, error)

// Filter implements TokenFilter. Replaces each token's Term with the
// hex-encoded collation sort key. Start/End/Pos preserved.
func (f *CollationFilter) Filter(tokens []Token) []Token
```

实现：
1. `locale` 解析为 `language.Tag`（如 `"zh-Hans"` → `language.SimplifiedChinese`）
2. `strength` 映射到 `collate.CompareOption`：`primary` → `collate.Primary`，`secondary` → `collate.Secondary`，`tertiary` → `collate.Tertiary`
3. `collator.KeyString(token.Term)` 返回 `[]byte` sort key
4. hex 编码 sort key 替换 `token.Term`

**factory.go 注册**：

```go
NameCollation = "collation"
// params: locale (string, default "en"), strength (string, default "primary")
```

**sort 路径集成**（`translator.go`）：

```go
// When field has collation analyzer configured, sort uses the
// analyzed sort key instead of raw field value.
func (t *Translator) translateSort(sortSpec []SortItem, mapping *meta.Mapping) {
    for _, item := range sortSpec {
        field := mapping.Fields[item.Field]
        if field.Analyzer == "collation" {
            // Use sort key from analyzer output for comparison
            item.SortKeyFn = func(val string) string {
                tokens := t.analyzers[field.Analyzer].Analyze([]byte(val))
                if len(tokens) > 0 { return tokens[0].Term }
                return val
            }
        }
    }
}
```

### 2.5 Phase 2 — 特性 5: Pipeline 串联分析器

**变更内容**：

新增 `PipelineAnalyzer`，将多个完整 Analyzer 串联运行，使用链式映射表维护 offset 精确性。

```go
// PipelineAnalyzer chains multiple Analyzers in sequence.
type PipelineAnalyzer struct {
    stages []Analyzer
}

func NewPipelineAnalyzer(stages ...Analyzer) *PipelineAnalyzer

// Analyze implements Analyzer. Stage 1 analyzes the raw text.
// Stage N analyzes the concatenation of stage N-1's token Terms.
// Offsets are mapped back to the original text via chained
// offset mapping tables maintained per stage.
func (p *PipelineAnalyzer) Analyze(text []byte) []Token
```

**链式映射表**：

```go
// offsetMap records how stage N's output offsets map to stage N-1's
// output offsets, which transitively map back to the original text.
type offsetMap struct {
    // forward[i] = (stageN_Start, stageN_end) → (stageN-1_start, stageN-1_end)
    forward []offsetPair
}

type offsetPair struct {
    inStart, inEnd int   // offsets in stage N's input text
    outStart, outEnd int // offsets in stage N-1's output text
}
```

算法：
1. Stage 1: `tokens1 = stages[0].Analyze(text)`，offsets 直接对应原始文本
2. Stage 2: 将 `tokens1` 的 Term 拼接为 `text2`（用空格分隔），构建 `offsetMap1`（记录 `text2` 的每个字符到原始文本的映射）
3. `tokens2 = stages[1].Analyze(text2)`，通过 `offsetMap1` 将 `tokens2` 的 Start/End 映射回原始文本
4. 以此类推，每阶段构建一个 `offsetMap`，最终通过链式映射回原始文本
5. 最终 token 的 Pos 从 0 重新编号

**factory.go 注册**：

```go
NamePipeline = "pipeline"
// params: analyzers ([]string, required) — ordered analyzer names
// Factory recursively calls Get() for each sub-analyzer name.
```

### 2.6 Phase 2 — 特性 6: BlockCache 缓存目录

**变更内容**：

分层缓存策略：冷段（S3 来源）用块级 LRU 缓存按需加载，热段（本地 NVMe）不缓存依赖 mmap page cache。

```go
package cache

// BlockCache is a block-level LRU cache for cold segment data.
// Hot segments rely on mmap page cache and are NOT cached here.
type BlockCache struct {
    maxSize  int64                    // max bytes
    curSize  int64                    // current bytes
    mu       sync.Mutex
    entries  map[blockCacheKey]*blockCacheEntry
    lru      *list.List
}

type blockCacheKey struct {
    segmentID  types.SegmentID
    fieldID    types.FieldID
    blockIndex int
}

type blockCacheEntry struct {
    key     blockCacheKey
    data    *segment.BlockData
    size    int64
    lruElem *list.Element
}

func NewBlockCache(maxSizeMB int) *BlockCache

// Get returns the cached BlockData for (segID, field, blockIdx), or nil.
func (bc *BlockCache) Get(segID types.SegmentID, field types.FieldID, blockIdx int) *segment.BlockData

// Put stores a decoded BlockData in the cache. Evicts LRU entries
// when maxSize is exceeded.
func (bc *BlockCache) Put(segID types.SegmentID, field types.FieldID, blockIdx int, data *segment.BlockData)

// InvalidateSegment removes all entries for the given segment.
func (bc *BlockCache) InvalidateSegment(segID types.SegmentID)
```

**分层策略实现**：

```go
// segment/reader.go — ColumnReader integration
type ColumnReader struct {
    // ... existing fields ...
    blockCache *cache.BlockCache  // nil for hot segments, non-nil for cold
    isCold     bool               // true if segment is from S3
}

func (r *ColumnReader) NewIterator() ColumnIterator {
    return &columnIterator{
        reader:    r,
        blockIdx:   0,
        blockCache: r.blockCache,  // may be nil
        isCold:     r.isCold,
    }
}

// Next decodes the next block. For cold segments, checks BlockCache first.
func (it *columnIterator) Next() (*BlockData, error) {
    if it.isCold && it.blockCache != nil {
        if cached := it.blockCache.Get(it.reader.segID, it.reader.fieldID, it.blockIdx); cached != nil {
            it.blockIdx++
            return cached, nil
        }
    }
    data, err := it.reader.decodeBlockAt(it.blockIdx)
    if err != nil { return nil, err }
    if it.isCold && it.blockCache != nil {
        it.blockCache.Put(it.reader.segID, it.reader.fieldID, it.blockIdx, data)
    }
    it.blockIdx++
    return data, nil
}
```

冷段判定：`tiering/tier_manager.go` 标记段为 `TierCold` 时，在 `SegmentReader` 上设置 `isCold = true` 并传入 `BlockCache` 实例。热段 `isCold = false`，`blockCache = nil`。

### 2.7 Phase 2 — 特性 7: 列预热 (Column Warmup)

**变更内容**：

在 `Segment` 上新增 `Warmup` 方法，并发预解码指定列的所有块。

```go
// Warmup pre-decodes and caches the specified columns' blocks.
// For hot segments: triggers MADV_WILLNEED on mmap regions.
// For cold segments: decodes all blocks and fills BlockCache.
func (s *Segment) Warmup(ctx context.Context, fields []types.FieldID) error {
    g, ctx := errgroup.WithContext(ctx)
    g.SetLimit(runtime.GOMAXPROCS(0))
    for _, field := range fields {
        field := field
        g.Go(func() error {
            if s.isCold && s.blockCache != nil {
                // Cold: decode all blocks into BlockCache
                r, err := s.column(field)
                if err != nil { return err }
                it := r.NewIterator()
                for {
                    _, err := it.Next()
                    if err == io.EOF { break }
                    if err != nil { return err }
                }
            } else {
                // Hot: trigger kernel prefetch via MADV_WILLNEED
                if mmapFile := s.getMmapFile(field); mmapFile != nil {
                    mmapFile.Advise(mmap.AdviseWillNeed)
                }
            }
            return nil
        })
    }
    return g.Wait()
}
```

**Admin API**：

```
POST /{index}/_warmup
Content-Type: application/json
{"fields": ["title", "content", "_id"]}

→ 200 {"acknowledged": true, "warmed_fields": ["title","content","_id"], "segment_count": 5}
```

### 2.8 Phase 2 — 特性 8: 段 GC 自动清理旧版本

**变更内容**：

基于持久化引用计数的段文件 GC，崩溃后可精确恢复引用计数。

```go
package gc

// SegmentGC manages segment file lifecycle via persisted reference counts.
type SegmentGC struct {
    segDir       string
    interval     time.Duration
    maxOldSegs   int
    tmpTTL       time.Duration
    mu           sync.Mutex
    refCounts    map[types.SegmentID]*refCountedSeg
    stopCh       chan struct{}
}

type refCountedSeg struct {
    segID     types.SegmentID
    dir       string
    refCount  int64      // atomic, persisted to refcount.json
    markTime  time.Time  // when marked for collection
}

// IncRef increments the reference count for a segment (called on Snapshot).
func (gc *SegmentGC) IncRef(segID types.SegmentID)

// DecRef decrements the reference count (called on query completion).
// When refCount reaches 0, the segment is marked for collection.
func (gc *SegmentGC) DecRef(segID types.SegmentID)

// MarkForCollection marks a segment as reclaimable (called after merge
// replaces old segments with a new one).
func (gc *SegmentGC) MarkForCollection(segID types.SegmentID)

// Start runs the background GC goroutine.
func (gc *SegmentGC) Start(ctx context.Context)
```

**持久化引用计数**：

```go
// refcount.json format per segment directory:
// {"segment_id": "seg_abc123", "ref_count": 2, "updated_at": "2026-10-23T..."}

func (rs *refCountedSeg) persist() error {
    path := filepath.Join(rs.dir, "refcount.json")
    data, _ := json.Marshal(map[string]any{
        "segment_id":  rs.segID,
        "ref_count":   atomic.LoadInt64(&rs.refCount),
        "updated_at":  time.Now().UTC().Format(time.RFC3339),
    })
    return os.WriteFile(path, data, 0644)  // no fsync: best-effort persistence
}

// On startup, Recover loads refcounts from disk. Segments without
// refcount.json are assumed to have refCount = 0 (safe default).
func (gc *SegmentGC) Recover() error
```

**GC goroutine 逻辑**：

```go
func (gc *SegmentGC) run(ctx context.Context) {
    ticker := time.NewTicker(gc.interval)  // default 60s
    for {
        select {
        case <-ctx.Done(): return
        case <-ticker.C:
            gc.collect()
        }
    }
}

func (gc *SegmentGC) collect() {
    gc.mu.Lock()
    defer gc.mu.Unlock()
    for segID, rs := range gc.refCounts {
        if atomic.LoadInt64(&rs.refCount) == 0 && rs.markTime.IsSet() {
            // Safe to delete: refCount = 0 AND marked for collection
            os.RemoveAll(rs.dir)
            delete(gc.refCounts, segID)
        }
    }
    // Clean stale .tmp directories
    gc.cleanTmpDirs()
}
```

### 2.9 Phase 2 — 特性 9: Granular Range 粒度范围查询

**变更内容**：

升级段格式 FormatVersion v2→v3，块写入时自适应选择桶数，范围查询时桶级剪枝。

**段格式变更**：

```go
// segment/codec.go
const (
    FormatVersionV2 = 2  // pre-existing
    FormatVersionV3 = 3  // adds granular_zones
)
var FormatVersion = FormatVersionV3  // current supported version
```

**自适应桶统计写入**（`column_writer.go`）：

```go
// GranularZone records min/max/rowCount for one bucket within a block.
type GranularZone struct {
    Min       int64
    Max       int64
    RowCount  uint16
}

// GranularZones is written after the block payload, before the
// block trailer. Format: u8 numBuckets | GranularZone[numBuckets]
// Only written for FormatVersion >= V3.
type GranularZones struct {
    Zones []GranularZone
}

// computeAdaptiveZones chooses bucket count based on data distribution.
// - nrows <= 16:    4 buckets
// - nrows <= 64:    16 buckets
// - nrows <= 256:   32 buckets
// - nrows <= 8192:  64 buckets
// - nrows > 8192:   128 buckets
func computeAdaptiveZones(data *BlockData) GranularZones
```

**块解码扩展**（`column_reader.go`）：

```go
type BlockMeta struct {
    // ... existing fields ...
    GranularZones *GranularZones  // nil for V2 segments
}

// decodeBlockMeta reads granular zones when FormatVersion >= V3.
func decodeBlockMeta(data []byte, formatVersion int) (BlockMeta, error)
```

**执行层桶级剪枝**（`exec/scan.go`）：

```go
// GranularRangePredicate extends zone map filtering with intra-block
// bucket-level pruning. For each block, after zone map accepts,
// checks granular zones to skip non-overlapping row ranges.
type GranularRangePredicate struct {
    field   types.FieldID
    lower   int64
    upper   int64
    hasMin  bool
    hasMax  bool
}

// FilterBlock returns the set of row indices within the block that
// could match the range, based on granular zones. For V2 segments
// (no granular zones), returns all rows (no intra-block pruning).
func (p *GranularRangePredicate) FilterBlock(meta BlockMeta) []int
```

**优化器集成**（`plan/optimizer.go` R2 规则）：

```go
// R2 rule: when RangeQuery targets a field with granular zones
// (FormatVersion >= V3), upgrade ZoneMapSpec to GranularRangeSpec.
func (o *Optimizer) applyR2(plan *LogicalPlan) *LogicalPlan {
    for _, node := range plan.ScanNodes {
        if node.HasRangePredicate && node.SegmentFormatVersion >= FormatVersionV3 {
            node.RangeSpec = &GranularRangeSpec{
                Predicate: &GranularRangePredicate{...},
            }
        }
    }
}
```

### 2.10 Phase 2 — 特性 10: SamePosition 查询

**变更内容**：

新增 `SamePositionQuery` DSL 类型和倒排索引 `SearchSamePosition` 方法。

**DSL AST 节点**：

```go
// dsl/ast.go
type SamePositionQuery struct {
    Field string
    Terms []string
    Boost float64
}

func (q *SamePositionQuery) Type() string { return "same_position" }
```

**倒排索引方法**：

```go
// index/inverted/index.go
// SearchSamePosition finds documents where all terms appear at the
// same token position. Returns doc IDs and their match positions.
func (idx *LoadedIndex) SearchSamePosition(field types.FieldID, terms []string) ([]SamePositionHit, error)

type SamePositionHit struct {
    DocID    types.RowID
    Position uint16  // the shared position
    Score    float64 // BM25 score of the combined match
}
```

算法：
1. 对每个 term 查倒排链，获取 `[]PostingEntry`（含 positions）
2. 取所有 term 的文档交集（roaring bitmap AND）
3. 对每个交集中的文档，取各 term 的 `[]uint16` positions 列表
4. 对 position 列表做排序归并交集：非空则匹配，记录共同 position
5. 用 BM25 公式计算组合分数

### 2.11 Phase 3 — 特性 11: FastText 分类/近邻分析器

**变更内容**：

纯 Go 实现 FastText 模型加载和推理，支持分类和近邻两种模式。

**模型加载器**：

```go
// fasttext_model.go
type Model struct {
    args        *ModelArgs     // hyperparameters
    dict        *Dictionary    // word + ngram dictionary
    inputMatrix  []float32      // [dictSize × dim] embedding matrix
    outputMatrix []float32      // [nLabels × dim] classification matrix
    quantized   bool            // .ftz quantized format
}

type ModelArgs struct {
    Dim       int
    Bucket    int    // n-gram bucket size
    MinN      int    // min n-gram length (default 3)
    MaxN      int    // max n-gram length (default 6)
    NLabels   int
    LossType  string // "softmax" | "ns" | "hs" | "ova"
}

// LoadModel parses a FastText .bin or .ftz file.
func LoadModel(path string) (*Model, error)
```

**分析器**：

```go
// fasttext_analyzer.go
type FastTextAnalyzer struct {
    model *Model
    mode  string  // "classification" | "nearest"
    dim   int     // output vector dimensions
}

func NewFastTextAnalyzer(modelPath string, mode string, dim int) (*FastTextAnalyzer, error)

// Analyze implements Analyzer. Tokenizes input, computes word vectors
// via model, aggregates to document vector (mean pooling), then:
// - classification: output matrix × doc vector → label scores → top-k labels as tokens
// - nearest: compute cosine similarity to dictionary words → top-k nearest words as tokens
func (a *FastTextAnalyzer) Analyze(text []byte) []Token
```

**矩阵乘法加速**：复用 `internal/simd` 包的 dot product 内核（AVX2+FMA / NEON），批量计算 doc vector × output matrix。

**模型热重载**：复用 `hotreload.go` 模式，`atomic.Pointer[Model]` 原子切换，SIGHUP 或 mtime 轮询触发重载。

### 2.12 Phase 3 — 特性 12: NGram 相似度打分

**变更内容**：

新增 `NGramSimilarityQuery` 和 `NGramScoreOp` 执行算子。

```go
// dsl/ast.go
type NGramSimilarityQuery struct {
    Field     string
    Text      string
    NGramSize int     // default 2
    Threshold float64 // min similarity, default 0.0
    Boost     float64
}

// index/inverted/ngram_scorer.go
type NGramScorer struct {
    queryNGrams  map[string]struct{}  // query text n-grams set
    querySize    int                  // |Q|
    ngramSize    int
    threshold    float64
}

// Score computes Dice similarity: 2|Q∩D| / (|Q| + |D|)
// where |D| is estimated from field length (stored in norm column).
func (s *NGramScorer) Score(matchedNGrams int, docFieldLen int) float64
```

**执行算子**：

```go
// exec/operators.go
type NGramScoreOp struct {
    input    Operator
    scorer   *inverted.NGramScorer
    fieldID  types.FieldID
    fieldLen func(types.RowID) int  // norm column lookup
    out      *ColumnBatch
}

func NewNGramScore(input Operator, scorer *inverted.NGramScorer, field types.FieldID, fieldLenFn func(types.RowID) int) *NGramScoreOp
```

### 2.13 Phase 3 — 特性 13: TF-IDF 评分器

**变更内容**：

新增 `TFIDFScorer`，作为 BM25 的可选替代。

```go
// index/inverted/tfidf.go
type TFIDFParams struct {
    K1      float64  // unused (TF-IDF has no saturation)
    B       float64  // unused
    Boost   float64
}

type TFIDFScorer struct {
    params TFIDFParams
}

// ScoreTerm computes: tf * log(N / df)
// where N = total docs, df = docs containing term, tf = term frequency.
func (s *TFIDFScorer) ScoreTerm(tf, df, N uint64) float64 {
    if df == 0 { return 0 }
    return float64(tf) * math.Log(float64(N)/float64(df)) * s.params.Boost
}

// IDF computes log(N / df) — used for WAND upper bound.
func (s *TFIDFScorer) IDF(df, N uint64) float64
```

**Config 集成**：

```go
// index/inverted/index.go
type Config struct {
    // ... existing fields ...
    ScorerType string  // "bm25" (default) | "tfidf"
    BM25       BM25Params
    TFIDF      TFIDFParams
}
```

**WAND 上界计算**：

```go
// wand.go — computeUpperBound dispatches by ScorerType
func (w *WANDScorer) computeUpperBound(blockMaxFreq, df, N uint64) float64 {
    switch w.config.ScorerType {
    case "tfidf":
        return w.tfidf.IDF(df, N) * float64(blockMaxFreq)
    default: // "bm25"
        return w.bm25.IDF(df, N) * ... // existing BM25 upper bound
    }
}
```

## 3. 接口设计 (Interface Design)

### 3.1 REST API

#### POST /{index}/_warmup — 列预热

```
POST /{index}/_warmup
Content-Type: application/json

{"fields": ["title", "content", "_id"]}
```

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| fields | []string | 是 | 需要预热的字段名列表 |

**返回值**:

| 类型 | 说明 |
|------|------|
| JSON | `{"acknowledged": bool, "warmed_fields": []string, "segment_count": int}` |

### 3.2 Analyzer 配置接口

#### MinHash 分析器

| 参数名 | 类型 | 必填 | 默认值 | 说明 |
|--------|------|------|--------|------|
| type | string | 是 | - | 必须为 `"minhash"` |
| num_perm | int | 否 | 128 | 签名维度 |
| seed | int | 否 | 42 | 随机种子 |

#### Collation 分析器

| 参数名 | 类型 | 必填 | 默认值 | 说明 |
|--------|------|------|--------|------|
| type | string | 是 | - | 必须为 `"collation"` |
| locale | string | 否 | `"en"` | BCP 47 locale tag |
| strength | string | 否 | `"primary"` | 比较强度 |

#### Pipeline 分析器

| 参数名 | 类型 | 必填 | 默认值 | 说明 |
|--------|------|------|--------|------|
| type | string | 是 | - | 必须为 `"pipeline"` |
| analyzers | []string | 是 | - | 有序分析器名称列表 |

#### FastText 分析器

| 参数名 | 类型 | 必填 | 默认值 | 说明 |
|--------|------|------|--------|------|
| type | string | 是 | - | 必须为 `"fasttext"` |
| model_path | string | 是 | - | 模型文件路径 |
| mode | string | 否 | `"classification"` | `"classification"` 或 `"nearest"` |
| dim | int | 否 | 16 | 输出维度 |

### 3.3 Query DSL 接口

#### SamePosition 查询

```json
{"same_position": {"field": "synonyms", "terms": ["happy","glad"], "boost": 1.2}}
```

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| field | string | 是 | 字段名 |
| terms | []string | 是 | 需在同位置出现的 term 列表（≥2） |
| boost | float | 否 | 权重（默认 1.0） |

#### NGram 相似度查询

```json
{"ngram_similarity": {"field": "title", "text": "hello world", "ngram_size": 2, "threshold": 0.3}}
```

| 参数名 | 类型 | 必填 | 默认值 | 说明 |
|--------|------|------|--------|------|
| field | string | 是 | - | 字段名 |
| text | string | 是 | - | 查询文本 |
| ngram_size | int | 否 | 2 | n-gram 大小 |
| threshold | float | 否 | 0.0 | 最低相似度阈值 |
| boost | float | 否 | 1.0 | 权重 |

### 3.4 Similarity 配置

```json
{"content": {"type": "text", "similarity": "tfidf"}}
```

| 值 | 说明 |
|------|------|
| `"bm25"` | 默认 BM25 评分（默认值） |
| `"tfidf"` | TF-IDF 评分 |

### 3.5 配置接口

```yaml
storage:
  mmap_advice:
    enabled: true
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

## 4. 内部设计 (Internal Design)

### 4.1 MmapFile (internal/storage/mmap)

**职责**: 封装 mmap 文件映射和 madvise 提示，提供统一的文件加载接口。

| 方法名 | 签名 | 说明 |
|--------|------|------|
| Open | `(path string, advice MmapAdvice) → (*MmapFile, error)` | 打开并映射文件 |
| Bytes | `() → []byte` | 返回映射区域 |
| Advise | `(advice MmapAdvice) → error` | 更新 madvise 提示 |
| Close | `() → error` | 解除映射 |
| probeMmap | `() → error` | 启动时探测 mmap 可用性 |

**依赖关系**: 无外部依赖，使用 Go `syscall` 包。

### 4.2 FilterCache (internal/engine/cache)

**职责**: 缓存 filter→bitmap 映射，加速重复 filter 查询。

| 方法名 | 签名 | 说明 |
|--------|------|------|
| NewFilterCache | `(maxEntries int, ttl time.Duration) → *FilterCache` | 构造 |
| Get | `(key uint64, segID SegmentID) → *roaring.Bitmap` | 查询 |
| Put | `(key uint64, segID SegmentID, bm *roaring.Bitmap)` | 写入 |
| InvalidateSegment | `(segID SegmentID)` | 段变更失效 |
| StartJanitor | `(ctx context.Context)` | 后台清理 |
| NormalizeFilter | `(filterJSON []byte) → (uint64, error)` | AST 规范化+hash |

**依赖关系**: 依赖 `dsl` 包解析 AST，依赖 `roaring.Bitmap`，依赖 `segmentset` 段变更事件。

### 4.3 BlockCache (internal/storage/cache)

**职责**: 冷段块级 LRU 缓存。

| 方法名 | 签名 | 说明 |
|--------|------|------|
| NewBlockCache | `(maxSizeMB int) → *BlockCache` | 构造 |
| Get | `(segID, fieldID, blockIdx) → *BlockData` | 查询 |
| Put | `(segID, fieldID, blockIdx, data)` | 写入+淘汰 |
| InvalidateSegment | `(segID)` | 段失效 |

**依赖关系**: 依赖 `segment.BlockData` 类型。

### 4.4 SegmentGC (internal/storage/gc)

**职责**: 段文件引用计数管理和自动回收。

| 方法名 | 签名 | 说明 |
|--------|------|------|
| NewSegmentGC | `(segDir string, interval, maxOld, tmpTTL) → *SegmentGC` | 构造 |
| IncRef | `(segID)` | 增引用 |
| DecRef | `(segID)` | 减引用 |
| MarkForCollection | `(segID)` | 标记可回收 |
| Start | `(ctx context.Context)` | 启动后台 GC |
| Recover | `() → error` | 崩溃恢复 |

**依赖关系**: 依赖 `segmentset` 引用计数事件。

### 4.5 PipelineAnalyzer (internal/index/analyzer)

**职责**: 多 Analyzer 串联，链式 offset 映射。

| 方法名 | 签名 | 说明 |
|--------|------|------|
| NewPipelineAnalyzer | `(stages ...Analyzer) → *PipelineAnalyzer` | 构造 |
| Analyze | `(text []byte) → []Token` | 分析+链式映射 |

**依赖关系**: 依赖 `Analyzer` 接口。

### 4.6 FastTextAnalyzer (internal/index/analyzer)

**职责**: FastText 模型推理，分类/近邻模式。

| 方法名 | 签名 | 说明 |
|--------|------|------|
| NewFastTextAnalyzer | `(modelPath, mode string, dim int) → (*FastTextAnalyzer, error)` | 构造 |
| Analyze | `(text []byte) → []Token` | 推理+输出 |
| Reload | `() → error` | 模型热重载 |

**依赖关系**: 依赖 `internal/simd` 加速矩阵乘法，依赖 `hotreload` 模式。

## 5. 数据模型 (Data Model)

### 5.1 GranularZone

```go
type GranularZone struct {
    Min      int64
    Max      int64
    RowCount uint16
}
```

| 字段名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| Min | int64 | 是 | 桶内最小值 |
| Max | int64 | 是 | 桶内最大值 |
| RowCount | uint16 | 是 | 桶内行数 |

### 5.2 FilterCacheEntry

```go
type cacheEntry struct {
    Key       uint64
    SegmentID types.SegmentID
    Bitmap    *roaring.Bitmap
    ExpireAt  time.Time
}
```

### 5.3 SegmentRefCount

```go
type refCountedSeg struct {
    SegID    types.SegmentID
    Dir      string
    RefCount int64     // atomic
    MarkTime  time.Time
}
```

### 5.4 段格式 V3 块布局

```
┌─────────────────────────────────────────┐
│ Block Payload (codec-encoded)          │
├─────────────────────────────────────────┤
│ Null Prefix (u32 count + bitmap)        │
├─────────────────────────────────────────┤
│ Granular Zones (V3 only):               │
│   u8 numBuckets                         │
│   GranularZone[numBuckets]              │
├─────────────────────────────────────────┤
│ Block Trailer (u16 nrows, u16 codecID) │
└─────────────────────────────────────────┘
```

**约束条件**:
- Granular Zones 仅在 `FormatVersion >= V3` 时写入
- V2 段在 V3 代码下读取时，`GranularZones` 为 nil，桶级剪枝降级为全块扫描
- `numBuckets` 范围 4-128，由 `computeAdaptiveZones` 根据块行数决定

## 6. 错误处理 (Error Handling)

### 6.1 错误类型定义

| 错误码/类型 | 触发条件 | 处理方式 | 用户提示 |
|-------------|----------|----------|----------|
| `ErrMmapDisabled` | mmap 启动探测失败 | 回退到 os.ReadFile，设置 `Enabled=false` | 日志 warning: "mmap disabled, falling back to file I/O" |
| `ErrMmapFailed` | 单个文件 mmap 失败（Enabled=true） | 该文件回退 os.ReadFile | 日志 warning: "mmap failed for {file}, using ReadFile" |
| `ErrFilterCacheMiss` | 缓存未命中 | 正常执行 filter 并写入缓存 | 无（内部行为） |
| `ErrModelNotFound` | FastText 模型文件不存在 | 分析器返回原始 token（passthrough） | 日志 warning: "fasttext model not found at {path}" |
| `ErrInvalidNGramSize` | ngram_size < 1 或 > 10 | 返回 illegal_argument 错误 | "ngram_size must be 1-10, got {n}" |
| `ErrSamePositionTerms` | terms 数量 < 2 | 返回 illegal_argument 错误 | "same_position requires at least 2 terms" |
| `ErrPipelineCyclic` | Pipeline 分析器链存在循环引用 | 返回 illegal_argument 错误 | "pipeline analyzer [{name}] has cyclic dependency" |
| `ErrSegmentGCInUse` | 尝试回收引用计数 > 0 的段 | 跳过，等待引用计数降为 0 | 无（内部行为） |
| `ErrFormatVersionTooNew` | 段 FormatVersion > 当前支持版本 | 拒绝打开段 | "segment format version {v} newer than supported {v}" |

### 6.2 异常传播策略

- **mmap 错误**: 不传播到上层，自动降级为 os.ReadFile，记录 warning 日志。
- **Filter Cache 错误**: 缓存读写失败降级为无缓存模式，不影响查询正确性。
- **BlockCache 错误**: 缓存操作失败降级为直接读取，不影响正确性。
- **段 GC 错误**: GC 回收失败（如权限问题）记录 error 日志，下次周期重试。
- **FastText 推理错误**: 返回 passthrough token，不中断索引/查询。
- **Granular Range 解析错误**: V2 段无 granular zones 时降级为块级 zone map（不报错）。

### 6.3 边界情况处理

| 场景 | 输入示例 | 期望行为 |
|------|----------|----------|
| MinHash 空输入 | `Analyze([]byte(""))` | 返回空 token 列表 |
| Collation 未知 locale | `locale: "xx-YY"` | 回退到 `language.Und`，记录 warning |
| Pipeline 空分析器链 | `analyzers: []` | 返回 illegal_argument 错误 |
| Pipeline 单分析器 | `analyzers: ["standard"]` | 正常工作，offset 直接透传 |
| SamePosition term 无倒排链 | term 不存在于索引中 | 返回空结果 |
| NGram 空文本 | `text: ""` | 返回空结果 |
| TF-IDF df=0 | term 在所有文档中出现 | ScoreTerm 返回 0（避免 log(0)） |
| FastText 大模型 | 模型 > 500MB | 加载成功但记录 warning |
| Granular Range 空块 | 块内所有值为 null | numBuckets=0，跳过桶级剪枝 |
| BlockCache 容量为 0 | `cache_size_mb: 0` | 不缓存，直接读取 |

## 7. 测试规划 (Test Plan)

### 7.1 测试策略表

| 测试类型 | 覆盖范围 | 目标覆盖率 | 说明 |
|----------|----------|------------|------|
| 单元测试 | 核心业务逻辑（mmap/cache/gc/scorer/analyzer） | ≥ 90% | 每个新类型至少 3 个用例 |
| 单元测试 | 工具/辅助代码（hashFns/offsetMap/normalizeFilter） | ≥ 70% | 辅助函数覆盖 |
| 集成测试 | 段格式 V3 兼容、Filter Cache 端到端、warmup API | ≥ 80% | Go test + Python E2E |
| E2E 测试 | 新分析器 `_analyze`、新查询 `_search`、warmup API | - | Python test/python/tests/ |

### 7.2 关键测试场景清单

#### 正向用例（主成功场景）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| mmap 打开列文件 | 段目录下 col_N 文件 | MmapFile.Bytes() 返回正确数据 | P0 |
| madvise 设置 | MmapFile.Open 后调用 Advise(AdviseRandom) | 无错误，/proc/smaps 确认 | P0 |
| Filter Cache 命中 | 相同 filter 查询两次 | 第二次 Get 返回 bitmap | P0 |
| MinHash 签名 | 10 个 token，num_perm=128 | 输出 128 个 `<MINHASH>` token | P1 |
| Collation 中文排序 | locale="zh-Hans"，3 个中文 token | sort key 按拼音排序 | P1 |
| Pipeline 两阶段 | standard → ngram | 最终 token 是 ngram 结果，offset 映射回原文 | P1 |
| BlockCache 冷段命中 | 冷段块查询两次 | 第二次从缓存返回 | P1 |
| 列预热 | Warmup(["title","_id"]) | 日志确认预热完成，后续查询无延迟 | P1 |
| 段 GC 回收 | markForCollection + refCount→0 | 后台 GC 删除段目录 | P1 |
| Granular Range 剪枝 | 范围查询，块内部分桶不重叠 | 跳过不匹配的行 | P1 |
| SamePosition 匹配 | 2 个 term 在同位置 | 返回匹配文档 | P1 |
| FastText 分类 | 模型+文本输入 | 输出分类标签 token | P2 |
| NGram 相似度 | 查询文本+文档 | 返回 Dice 系数 > 阈值的文档 | P2 |
| TF-IDF 打分 | match 查询，similarity="tfidf" | 分数为 tf*log(N/df) | P2 |

#### 边界用例（输入边界、状态边界、并发边界）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| mmap 大文件 | 1GB 段文件 | 正常映射，无 OOM | P0 |
| mmap 关闭后访问 | Close() 后 Bytes() | panic 或 error | P0 |
| Filter Cache TTL 过期 | TTL=1s，等待 2s 后 Get | 返回 nil | P0 |
| Filter Cache LRU 淘汰 | maxEntries=2，Put 3 个 | 淘汰最久未访问 | P0 |
| BlockCache 容量淘汰 | maxSize=1MB，写入超量 | 淘汰最久未访问块 | P1 |
| 段 GC 并发查询中 | 查询持有引用时 GC | 不回收（refCount>0） | P1 |
| Granular Range 空块 | 块内全 null | numBuckets=0，全行返回 | P1 |
| Granular Range V2 段 | FormatVersion=V2 的段 | GranularZones=nil，降级 zone map | P1 |
| Pipeline 空输入 | Analyze([]byte("")) | 返回空 token 列表 | P1 |
| Collation 未知 locale | locale="xx-YY" | 回退 language.Und + warning | P1 |
| SamePosition term 不存在 | term 不在索引中 | 返回空结果 | P1 |
| NGram 空文本 | text="" | 返回空结果 | P2 |
| TF-IDF df=0 | 全文档含该 term | ScoreTerm 返回 0 | P2 |
| FastText 模型缺失 | model_path 不存在 | 返回 passthrough token | P2 |

#### 异常用例（错误输入、依赖失败、权限拒绝）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| mmap 不可用 | 启动探测失败 | Enabled=false，回退 os.ReadFile | P0 |
| mmap 单文件失败 | 权限不足 | 该文件回退 os.ReadFile + warning | P0 |
| Filter Cache 段变更 | 合并产生新段 | InvalidateSegment 清除旧段条目 | P0 |
| Pipeline 循环引用 | A→B→A | 返回 ErrPipelineCyclic | P1 |
| SamePosition terms<2 | terms=["a"] | 返回 illegal_argument | P1 |
| NGram size 越界 | ngram_size=0 | 返回 illegal_argument | P2 |
| FastText 破损模型 | 二进制截断 | 返回 passthrough token + error 日志 | P2 |
| 段 GC 磁盘满 | os.RemoveAll 失败 | 记录 error，下次重试 | P1 |

#### 兼容用例（与现有模块的交互）

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| V2 段在 V3 代码读取 | FormatVersion=2 的段 | 正常读取，GranularZones=nil | P0 |
| V3 段在 V2 代码读取 | FormatVersion=3 的段 | 拒绝打开（version too new） | P0 |
| 新分析器 + 现有索引 | minhash analyzer + text field | 正常索引+查询 | P1 |
| TF-IDF + 现有 WAND | similarity="tfidf" + WAND 查询 | 正常剪枝 | P2 |
| 现有 163+ Python E2E | 全量回归 | 全绿 | P0 |
| B1-B10 基准 | 性能回归测试 | 性能不退化 | P0 |

### 7.3 Mock 清单

| 依赖项 | Mock 方式 | 模拟行为 | 使用场景 |
|--------|-----------|----------|----------|
| 文件系统 | tmpdir + 临时文件 | 创建/删除临时段目录 | mmap/GC/BlockCache 测试 |
| roaring.Bitmap | 真实类型 | 直接构造 | FilterCache 测试 |
| Analyzer | FakeAnalyzer | 返回固定 token 列表 | Pipeline 测试 |
| FastText 模型 | 临时小模型文件 | 预构造的 .bin 文件 | FastText 分析器测试 |
| 段变更事件 | Channel injection | 手动发送 SegmentChangeEvent | FilterCache 失效测试 |
| golang.org/x/text/collate | 真实类型 | 直接构造 Collator | Collation 测试 |

### 7.4 测试数据设计

- **mmap 测试数据**: 临时目录中创建不同大小的二进制文件（1KB/1MB/100MB/1GB）
- **FilterCache 测试数据**: 预构造的 filter DSL JSON 字符串（等价但不同写法、嵌套 bool、常量折叠场景）
- **Granular Range 测试数据**: 预构造的 V3 格式段文件（含已知 granular zones 分布）+ V2 格式段文件（向后兼容）
- **Pipeline 测试数据**: 多语言文本（中文+英文+德语）用于测试多阶段 offset 映射
- **FastText 测试数据**: 预训练的 lid.176.ftz 小模型（< 1MB）用于语言识别测试
- **SamePosition 测试数据**: 预构造的倒排索引（含同义词在同一位置的场景）

### 7.5 验证方式

1. **CI 集成**: `make ci`（lint + test + build）全绿
2. **本地手动验证**: 按 Proposal §7.1 的 8 步验证流程
3. **段格式兼容验证**: 使用旧版本写入的 V2 段目录，在新版本代码中 `OpenSegment` + `ScanColumn` 正常工作
4. **mmap 验证**: `cat /proc/$(pidof unisearch)/smaps | grep -A5 "mmap"` 确认 advice 标记
5. **缓存指标**: `curl /_prometheus/metrics | grep filter_cache` 查看 hit ratio
6. **Python E2E**: `cd test/python && python -m pytest tests/` 全绿

## 8. 验收标准 (Acceptance Criteria)

- [ ] **类与函数签名**: 13 个特性的所有新增类型和方法签名完整，通过 `go vet`
- [ ] **核心算法**: mmap 迁移、Filter Cache AST 规范化、MinHash 计算、Pipeline 链式映射、自适应桶统计、SamePosition 位置交集、FastText 推理算法步骤清晰可执行
- [ ] **数据结构**: GranularZone、FilterCacheEntry、refCountedSeg、段格式 V3 块布局定义完毕
- [ ] **测试计划**: 覆盖正向/边界/异常/兼容四类用例，Mock 清单完整，测试数据设计完整
- [ ] **可测性**: 包含本地手动验证 8 步流程 + CI 集成方案
- [ ] **编译**: `CGO_ENABLED=0 go build ./...` 和 `go vet ./...` 全绿
- [ ] **测试**: `go test ./...` 全绿，163+ Python E2E 全绿
- [ ] **段格式兼容**: V2 段在 V3 代码下正常读取
- [ ] **性能**: B1-B10 基准性能不退化
- [ ] **ES 兼容**: 新查询/API 不破坏现有 ES 8.x REST API 行为

## 9. 实现清单 (Implementation Checklist)

### Phase 1 (P0)
- [ ] 1.1 新增 `internal/storage/mmap/` 包（MmapFile + probeMmap + 平台分文件）
- [ ] 1.2 修改 `segment/reader.go` column()/idIndex() 迁移到 mmap
- [ ] 1.3 修改 `column_provider.go` segIndexInput 迁移到 mmap
- [ ] 1.4 修改 `shard.go` recoverSegments() 设置 advice
- [ ] 1.5 修改 `config.go` 新增 MmapAdvice 配置
- [ ] 1.6 新增 `internal/engine/cache/filter_cache.go`（LRU+TTL+AST 规范化）
- [ ] 1.7 修改 `translator.go` translateBool 集成 FilterCache
- [ ] 1.8 修改 `operators.go` 新增 NewBitmapFilterFromCached
- [ ] 1.9 修改 `segmentset.go` 段变更通知机制
- [ ] 1.10 修改 `config.go` 新增 FilterCache 配置
- [ ] 1.11 Phase 1 全量测试（单元+集成+回归）

### Phase 2 (P1)
- [ ] 2.1 新增 `minhash_filter.go` + factory.go 注册
- [ ] 2.2 新增 `collation_filter.go` + factory.go 注册 + translator sort 集成
- [ ] 2.3 新增 `pipeline_analyzer.go` + factory.go 注册（递归构建 + 循环检测）
- [ ] 2.4 新增 `internal/storage/cache/block_cache.go` + `cached_input.go`
- [ ] 2.5 修改 `reader.go` ColumnReader 集成 BlockCache（冷段分层策略）
- [ ] 2.6 修改 `tiering/tier_manager.go` 冷段读取路径集成 BlockCache
- [ ] 2.7 修改 `reader.go` 新增 Warmup 方法 + `shard.go` 预热触发
- [ ] 2.8 修改 `flush.go` 段发布后预热高频列
- [ ] 2.9 新增 `admin_handlers.go` POST /_warmup handler
- [ ] 2.10 新增 `internal/storage/gc/segment_gc.go`（持久化引用计数 + 后台 GC）
- [ ] 2.11 修改 `segmentset.go` 引用计数集成
- [ ] 2.12 修改 `merge.go` 删除逻辑改为标记可回收
- [ ] 2.13 修改 `flush.go` tmp 清理接入 GC
- [ ] 2.14 修改 `codec.go` FormatVersion v2→v3
- [ ] 2.15 修改 `column_writer.go` 自适应桶统计写入
- [ ] 2.16 修改 `column_reader.go` BlockMeta 新增 GranularZones
- [ ] 2.17 修改 `exec/scan.go` GranularRangePredicate
- [ ] 2.18 修改 `plan/optimizer.go` R2 规则 GranularRangeSpec
- [ ] 2.19 新增 `dsl/ast.go` SamePositionQuery + `dsl/parser.go` 解析
- [ ] 2.20 新增 `inverted/index.go` SearchSamePosition 方法
- [ ] 2.21 新增 `translator.go` translateSamePosition
- [ ] 2.22 Phase 2 全量测试

### Phase 3 (P2)
- [ ] 3.1 新增 `fasttext_model.go` 模型加载器
- [ ] 3.2 新增 `fasttext_analyzer.go` 分析器 + factory.go 注册
- [ ] 3.3 新增 `inverted/ngram_scorer.go` NGram 相似度打分器
- [ ] 3.4 新增 `dsl/ast.go` NGramSimilarityQuery + `dsl/parser.go` 解析
- [ ] 3.5 新增 `exec/operators.go` NGramScoreOp
- [ ] 3.6 新增 `translator.go` translateNGramSimilarity
- [ ] 3.7 新增 `inverted/tfidf.go` TF-IDF 评分器
- [ ] 3.8 修改 `inverted/index.go` Config.ScorerType + ScoreDocs 分发
- [ ] 3.9 修改 `inverted/wand.go` TF-IDF 上界
- [ ] 3.10 修改 `meta/mapping.go` Similarity 新增 "tfidf"
- [ ] 3.11 修改 `translator.go` translateMatch 评分器选择
- [ ] 3.12 修改 `flush.go` buildInvertedIndexes 传递 ScorerType
- [ ] 3.13 修改 `go.mod` 添加 golang.org/x/text
- [ ] 3.14 Phase 3 全量测试
- [ ] 3.15 最终全量回归（go build/test/vet + 163+ Python E2E + B1-B10 基准）
