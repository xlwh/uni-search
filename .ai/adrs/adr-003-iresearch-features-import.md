# ADR-003: IResearch 特性引入 — mmap/FilterCache/高级分析器/查询打分

**日期**: 2026-10-23
**状态**: Accepted

## 背景

UniSearch 段文件通过 `os.ReadFile` 全量读入内存（page-cache 语义），未使用 mmap/madvise；无过滤器缓存；分析器仅有基于规则的分词+过滤链；仅有 BM25 评分。IResearch（C++ 嵌入式全文检索引擎）在这些方面有多年优化积累。

## 决策

### 1. mmap + madvise 全量迁移

**选择**: 通过 `syscall.Mmap` + `syscall.Madvise` 将所有段文件（列数据、倒排索引、footer、id.hash、src.zst）从 `os.ReadFile` 迁移到 mmap，按文件类型设置不同 advice（RANDOM/SEQUENTIAL/WILLNEED）。

**理由**: mmap 区域不计入 Go 堆，通过 madvise 控制页面驻留；相比 page-cache 方式减少一次内核→用户空间拷贝；`MADV_DONTNEED` 可主动释放不活跃段页面。

**回退策略**: 启动探测 + 配置开关。启动时 `probeMmap()` 测试 mmap 可用性，不可用则全局禁用 mmap 路径，回退 `os.ReadFile`。

### 2. Filter Cache with AST 规范化

**选择**: LRU + TTL 缓存 filter→roaring.Bitmap 结果，key 为 AST 规范化后的 FNV-1a hash。AST 规范化包括字段排序、常量折叠、等价表达式归一化。

**理由**: 语义相同的 filter DSL（如字段顺序不同）产生相同 hash，最大化缓存命中率。段合并/刷新后按 segmentID 批量失效。

### 3. 段格式 V3 — Granular Range

**选择**: 升段格式 FormatVersion v2→v3，块尾新增自适应粒度桶统计（4-128 桶，按块行数自适应）。V3 代码读 V2 段时降级为块级 zone map（向后兼容）。

**理由**: 块内桶级剪枝跳过不匹配的行范围，相比块级 zone map 剪枝粒度更细。

### 4. FastText 纯 Go 模型加载器

**选择**: 纯 Go 解析 FastText `.bin`/`.ftz` 格式，矩阵乘法复用 `internal/simd` 的 AVX2+FMA/NEON 内核。

**理由**: 不引入 C++ FastText 库，保持 CGO_ENABLED=0 约束。模型加载为一次性操作，推理路径不涉及 IO。

## 影响

- 新增 16 个 Go 文件，修改 10 个文件
- 段格式向后兼容（V3 代码读 V2 段）
- ES API 兼容性不受影响
- 新增 78 个 Go 单元测试
