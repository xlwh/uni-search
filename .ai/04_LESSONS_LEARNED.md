# 经验教训 (Lessons Learned)

> **原则**: 长期记忆与踩坑记录。每次解决疑难杂症后，Append 此文件。

## [2026-10-22] miniInv.add() 未在写入路径调用
*   **Context**: 深度审计差距修复，实现 memtable 搜索功能
*   **Problem**: `miniInvertedIndex.add()` 在 `Engine.Write` 中从未被调用，导致 memtable 中的未刷新数据无法被 match 查询搜索到，违反"写入即可见"设计承诺
*   **Solution**: 在 `Engine.Write` 成功写入 shard 后，遍历文档的 text 字段调用 `miniInv.add(fieldID, text, rowID, analyzer)` 更新迷你倒排索引

## [2026-10-22] terms 聚合未注册到 agg registry
*   **Context**: 实现 47 种聚合类型时，e2e 测试发现 terms 聚合返回空结果
*   **Problem**: `terms` bucket 类型由 `GroupAggregator` 处理但未通过 `Register("terms", ...)` 注册到 agg registry，导致 `runAggregations` 调用 `NewAggregator("terms", ...)` 时找不到工厂
*   **Solution**: 在 `bucket_aggs.go` 中添加 `termsAgg` 类型并注册到 registry

## [2026-10-22] _cat/health 不支持 JSON 格式输出
*   **Context**: 完善 e2e 测试，验证 _cat 端点
*   **Problem**: `_cat/health` handler 始终返回文本格式，即使 Accept: application/json 也不返回 JSON，导致客户端 JSON 解析失败
*   **Solution**: 在 handler 中检查 `Accept` header 和 `?format=json` 参数，条件返回 JSON 数组格式

## [2026-10-22] Go build tag 分离内核实现与 init 绑定
*   **Context**: SIMD 汇编内核实现，需要 pure Go 函数在 `simd_asm` 和 `!simd_asm` 两个路径下都可用
*   **Problem**: 初始将 pure Go 内核实现和 `init()` 绑定放在同一个文件（`dot_purego.go`），加 `//go:build !simd_asm` 后，`simd_asm` 路径下无法访问 pure Go 函数作为 fallback
*   **Solution**: 将内核函数实现移到无 build tag 的文件（`kernel_purego.go`），`init()` 绑定留在有 build tag 的文件中。这是一个通用模式：实现和绑定分离

## [2026-10-22] Go 1.20+ math/rand 全局源自动种子化导致测试不稳定
*   **Context**: RaBitQ recall 测试使用 `rand.Float32()` 生成测试数据
*   **Problem**: Go 1.20+ 将 `math/rand` 全局源改为自动随机种子，每次运行生成不同数据，导致 recall 测试时通过时失败（0%-20% 波动）
*   **Solution**: 测试中始终使用 `rand.New(rand.NewSource(seed))` 创建确定性 RNG，确保跨运行的测试可复现性

## [2026-10-22] 函数内局部 type 定义遮蔽包级 type 导致编译错误
*   **Context**: ODescent 的 `refineGraph` 方法中定义了局部 `type cand struct` 用于候选排序
*   **Problem**: 局部 `cand` 类型遮蔽了 `core.go` 中的包级 `cand` 类型，传递给 `adaptiveHeuristic(cands []cand)` 时类型不匹配
*   **Solution**: 移除局部 type 定义，直接使用包级 `cand` 类型。Go 中函数内定义的 type 会遮蔽包级同名 type，跨函数调用时产生隐蔽的类型不兼容

## [2026-10-22] ALP 浮点编码：IEEE 754 乘法与除法路径不同导致无损性检查失败
*   **Context**: F2 可组合编解码器，实现 ALP（Adaptive Lossless floating-Point）编码
*   **Problem**: `float64(i) * 0.1` 与 `float64(i) / 10.0` 在 IEEE 754 双精度中可能产生不同结果（因 0.1 不是精确表示），导致 ALP 无损性检查 `float64(iv)/factor != vals[i]` 正确拒绝了看似可编码的值
*   **Solution**: 在 `findALPExponent` 中添加严格往返检查 `float64(iv)/factor == v`，确保只有真正无损的值才被 ALP 编码；当无无损指数时返回 error 让调用方回退到 Gorilla 编码

## [2026-10-22] 异步队列 wait=true 共享 batch 需要广播机制
*   **Context**: F5 异步 INSERT 批量合并，多个 wait=true 调用者共享同一 batch
*   **Problem**: 初版使用 `chan error`（buffer 1）通知 wait=true 调用者，但多个调用者等待同一 channel 时只有第一个能收到结果，其余阻塞直至 30s 超时
*   **Solution**: 改用 `chan struct{}` (done) + `error` 字段的广播模式：flush 完成时设置 err 字段并 close(done)，所有等待者通过 `<-done` 同时解除阻塞并读取 err

## [2026-10-22] 编码选择矩阵变更需保守以维护向后兼容
*   **Context**: F2 chooseCompositeCodec 改变了 double 列的编码选择（Gorilla → ALP+ZSTD Composite）
*   **Problem**: 现有测试 `TestR4_EncodingRoundtrip` 验证每列的固定 Encoding 值，新编码选择改变了 double 块从 EncodingGorilla(4) 到 EncodingComposite(7)，导致测试失败
*   **Solution**: `chooseCompositeCodec` 暂时返回 nil（禁用自动选择），F2 基础设施就位但选择逻辑需基准测试验证后才能激活。复合编码通过专用 F2 测试验证正确性

## [2026-10-23] golang.org/x/text/collate API: KeyString vs KeyFromString
*   **Context**: 实现 Collation 排序规则分析器，使用 `golang.org/x/text/collate` 生成 locale 感知排序键
*   **Problem**: 代码使用 `collator.KeyString(s)` 但实际方法签名为 `KeyFromString(buf *Buffer, str string) []byte`，需要传入 `*collate.Buffer` 参数
*   **Solution**: 在 CollationFilter 中持有一个 `collate.Buffer` 字段，调用 `collator.KeyFromString(&f.buf, tok.Term)`。使用 `grep` 查看模块缓存中的实际方法签名确认 API

## [2026-10-23] Go 测试中 blank import 注册索引工厂
*   **Context**: 存储合并测试 `TestR7_Merge*` 失败，错误 `unknown index kind: "bitmap"`
*   **Problem**: `internal/index/scalar/bitmap.go` 通过 `init()` 注册 bitmap 索引工厂，但 `merge_test.go` 未 import `scalar` 包，导致 init() 未执行
*   **Solution**: 在测试文件中添加 `_ "github.com/unisearch-db/unisearch/internal/index/scalar"` blank import。Go 中 init() 只在被 import 时执行，测试需要显式 import 依赖的注册包

## [2026-10-23] 测试断言过于宽泛导致误报
*   **Context**: `TestR1_DumpConfigFlag` 的 `bad: []string{":9300"}` 断言失败
*   **Problem**: 测试检查 config dump 输出不包含 `:9300`，但 cloud 配置默认值 `raft_addr: :9300` 和 `grpc_addr: :9300` 也被匹配，导致测试误报
*   **Solution**: 将断言改为精确匹配 `bad: []string{"listen: :9300"}`。测试的 `bad` 检查应精确到具体字段，避免匹配到其他配置节中碰巧包含相同子串的值
