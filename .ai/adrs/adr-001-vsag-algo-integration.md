# ADR-001: 从 vsag 引入优化算法

**日期**: 2026-10-22
**状态**: Accepted

## 背景

uni-search 的向量搜索引擎存在多个性能瓶颈：SIMD 汇编内核未实现（仅有 pure Go 基线）、RaBitQ 量化路径存在内存布局和计算效率问题、图构建算法缺少自适应优化、量化方式仅有 SQ8/RaBitQ 两种、邻接表内存占用大。

通过对 vsag（C++ 向量搜索库）的深入代码分析，识别出 5 个可引入的优化方向。

## 决策

### 1. SIMD 汇编：avo 框架 + 可选 build tag

**选择**: 使用 `github.com/mmcloughlin/avo` 生成 Go plan9 汇编（非 CGO），通过 `simd_asm` build tag 可选启用，运行时用 `golang.org/x/sys/cpu` 检测 ISA 自动分发。

**理由**: avo 生成的 Go 汇编不受 `CGO_ENABLED=0` 限制；可选启用策略确保默认构建零风险。

**替代方案**: CGO + C intrinsics（被否决：违反 CGO_ENABLED=0 约束）、纯 Go 优化（性能不足）。

### 2. RaBitQ 优化：扁平化 + 硬件 popcount

**选择**: 投影矩阵从 `[][]float32` 扁平化为 `[]float32`，Hamming 距离用 `math/bits.OnesCount64`，codes 从 `[][]byte` 扁平化为 `[]byte` + `codeOffsets`。

**理由**: 消除间接寻址和 GC 开销，利用硬件 POPCNT 指令。

### 3. PQ 量化：标准 8bit + K-means++

**选择**: 先实现标准 8bit PQ（K-means++ 初始化 + LUT 查表），FastScan 4bit 交织打包作为后续优化。

**理由**: 降低初始实现复杂度，标准 PQ 已提供足够的内存/速度平衡。

### 4. 图构建：自适应剪枝 + ODescent

**选择**: heuristic 加入 alpha 参数（RELAX/TIGHTEN 两阶段），距离缓存自适应（matrix ≤256 / map >256），ODescent 批量构建用混合策略（<100k 暴力 / ≥100k LSH）。

### 5. Elias-Fano：层级编码

**选择**: 上层用原始 `[]int32`，层 0 用 Elias-Fano 编码。平衡压缩比和访问速度。

## 新增依赖

| 依赖 | 用途 | 类型 |
|------|------|------|
| `github.com/mmcloughlin/avo` | Go 汇编生成器 | dev 依赖（生成 .s 文件 checked in） |
| `golang.org/x/sys/cpu` | 运行时 ISA 特性检测 | 直接依赖 |

## 影响

- 新增 12 个源文件，修改 8 个现有文件
- `quant.state` 序列化格式扩展（tier=2 RaBitQ, tier=3 PQ）
- `Config` 新增 `Alpha`/`BuildMode`/`EnableEF` 字段
- `QuantLevel` 新增 `QuantPQ` 常量
- 向后兼容：旧格式 graph 文件可正确加载
