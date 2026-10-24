# Spec: 从 vsag 引入优化算法提升向量搜索性能

## 1. 概述 (Overview)

本 Spec 基于 Proposal `2026-10-22-vsag-algo-integration`，将 vsag 中的 5 个优化算法引入 uni-search，全部以纯 Go 实现（Go 汇编除外）。5 个 Phase 按依赖顺序实施：Phase 1（SIMD 内核）→ Phase 2（RaBitQ 优化）→ Phase 4（图构建）→ Phase 3（PQ 量化）→ Phase 5（内存压缩）。

### 变更清单

| 类型 | 文件路径 | 说明 |
|------|----------|------|
| 新增 | `internal/simd/cpu.go` | ISA 特性检测模块 |
| 新增 | `internal/simd/cpu_amd64.go` | amd64 AVX2/FMA/POPCNT 探测 |
| 新增 | `internal/simd/cpu_arm64.go` | arm64 NEON 探测 |
| 新增 | `internal/simd/sse2_amd64.go` | SSE2 fallback 内核 |
| 新增 | `internal/simd/asm/dot_amd64.go` | avo 生成器：DotF32/L2F32 AVX2 内核 |
| 新增 | `internal/simd/asm/batch_amd64.go` | avo 生成器：DotF32Batch16 AVX2 内核 |
| 新增 | `internal/simd/asm/doti8f32_amd64.go` | avo 生成器：DotI8F32 AVX2 内核 |
| 新增 | `internal/simd/asm/hamming_amd64.go` | avo 生成器：Hamming popcount 内核 |
| 新增 | `internal/simd/asm/dot_arm64.go` | avo 生成器：DotF32/L2F32 NEON 内核 |
| 新增 | `internal/simd/asm/batch_arm64.go` | avo 生成器：DotF32Batch16 NEON 内核 |
| 新增 | `internal/simd/dist_amd64.s` | avo 生成的 amd64 汇编 |
| 新增 | `internal/simd/dist_arm64.s` | avo 生成的 arm64 汇编 |
| 新增 | `internal/simd/hamming.go` | Hamming 距离函数指针 |
| 新增 | `internal/index/hnsw/pq.go` | PQ 量化器 |
| 新增 | `internal/index/hnsw/adaptive_pruning.go` | 自适应剪枝 |
| 新增 | `internal/index/hnsw/odescent.go` | ODescent 批量构建 |
| 新增 | `internal/index/hnsw/elias_fano.go` | Elias-Fano 编码 |
| 新增 | `internal/index/hnsw/pq_test.go` | PQ 测试 |
| 新增 | `internal/index/hnsw/adaptive_pruning_test.go` | 自适应剪枝测试 |
| 新增 | `internal/index/hnsw/odescent_test.go` | ODescent 测试 |
| 新增 | `internal/index/hnsw/elias_fano_test.go` | Elias-Fano 测试 |
| 修改 | `internal/simd/dist.go` | 新增 DotI8F32/HammingDistanceBatch 指针 |
| 修改 | `internal/simd/dot_purego.go` | 增加 `!simd_asm` tag；新增 pure Go 基线 |
| 修改 | `internal/simd/dist_asm.go` | 实现 init() 运行时分发 |
| 修改 | `internal/simd/asm/gen.go` | 重写为完整 avo 生成器 |
| 修改 | `internal/index/hnsw/rabitq.go` | 投影矩阵扁平化 + 硬件 popcount |
| 修改 | `internal/index/hnsw/quant.go` | PQ tier 管理 + 搜索路径 |
| 修改 | `internal/index/hnsw/hnsw.go` | Config 新增字段 + heuristic alpha + Graph 新增字段 |
| 修改 | `internal/index/hnsw/core.go` | distFunc 缓存支持 |
| 修改 | `internal/index/hnsw/search.go` | PQ 路由分支 |
| 修改 | `internal/index/hnsw/index.go` | 序列化扩展 + ODescent 构建路径 |
| 修改 | `internal/index/registry.go` | QuantLevel 新增 QuantPQ |
| 修改 | `go.mod` | 新增 avo + x/sys 依赖 |

## 2. 详细设计 (Detailed Design)

### 2.1 Phase 1: SIMD 距离计算内核

#### 2.1.1 ISA 检测模块

**文件**: `internal/simd/cpu.go`, `cpu_amd64.go`, `cpu_arm64.go`

```go
// cpu.go — 平台无关声明
package simd

var (
    hasAVX2   bool
    hasFMA    bool
    hasPOPCNT bool
    hasSSE2   bool
    hasNEON   bool
)

func init() {
    detectCPUFeatures()
}
```

```go
// cpu_amd64.go
//go:build amd64

package simd

import "golang.org/x/sys/cpu"

func detectCPUFeatures() {
    hasAVX2 = cpu.X86.HasAVX2
    hasFMA = cpu.X86.HasFMA
    hasPOPCNT = cpu.X86.HasPOPCNT
    hasSSE2 = cpu.X86.HasSSE2
}
```

```go
// cpu_arm64.go
//go:build arm64

package simd

import "golang.org/x/sys/cpu"

func detectCPUFeatures() {
    hasNEON = cpu.ARM64.HasASIMD
}
```

#### 2.1.2 函数指针声明

**文件**: `internal/simd/dist.go`（修改）

新增两个函数指针：

```go
// DotI8F32 computes the dot product of a float32 query against an int8
// code row decoded as unsigned uint8 levels (SQ8 distance kernel).
// Contract: panics when len(q) != len(code); empty vectors yield 0.
var DotI8F32 func(q []float32, code []int8) float32

// HammingDistanceBatch computes Hamming distances between a query
// bitmap and multiple candidate bitmaps. codesFlat is a contiguous
// byte array; codeOffsets[i] gives the start offset of candidate i's
// bitmap in codesFlat; nBits is the bitmap length in bits (= byte
// length * 8). out[i] receives the Hamming distance for ids[i].
var HammingDistanceBatch func(qBits []byte, codesFlat []byte, codeOffsets []int32, ids []int32, out []int)
```

#### 2.1.3 Pure Go 基线

**文件**: `internal/simd/dot_purego.go`（修改）

增加 `//go:build !simd_asm` 约束，新增 pure Go 实现：

```go
//go:build !simd_asm

package simd

func init() {
    DotF32 = dotF32PureGo
    L2F32 = l2F32PureGo
    DotF32Batch16 = dotF32Batch16PureGo
    DotI8F32 = dotI8F32PureGo
    HammingDistanceBatch = hammingDistanceBatchPureGo
}

func dotI8F32PureGo(q []float32, code []int8) float32 {
    if len(q) != len(code) {
        panic(fmt.Sprintf("simd: DotI8F32: length mismatch: len(q)=%d, len(code)=%d", len(q), len(code)))
    }
    var s0, s1, s2, s3, s4, s5, s6, s7 float32
    n := len(q)
    i := 0
    for ; i+8 <= n; i += 8 {
        s0 += q[i+0] * float32(uint8(code[i+0]))
        s1 += q[i+1] * float32(uint8(code[i+1]))
        s2 += q[i+2] * float32(uint8(code[i+2]))
        s3 += q[i+3] * float32(uint8(code[i+3]))
        s4 += q[i+4] * float32(uint8(code[i+4]))
        s5 += q[i+5] * float32(uint8(code[i+5]))
        s6 += q[i+6] * float32(uint8(code[i+6]))
        s7 += q[i+7] * float32(uint8(code[i+7]))
    }
    for ; i < n; i++ {
        s0 += q[i] * float32(uint8(code[i]))
    }
    return ((s0+s1)+(s2+s3)+(s4+s5))+(s6+s7)
}

func hammingDistanceBatchPureGo(qBits []byte, codesFlat []byte, codeOffsets []int32, ids []int32, out []int) {
    nBytes := len(qBits)
    for i, id := range ids {
        off := int(codeOffsets[id])
        b := codesFlat[off : off+nBytes]
        dist := 0
        // Process 8 bytes at a time using math/bits.OnesCount64
        j := 0
        for ; j+8 <= nBytes; j += 8 {
            x := binary.LittleEndian.Uint64(qBits[j:]) ^ binary.LittleEndian.Uint64(b[j:])
            dist += bits.OnesCount64(x)
        }
        for ; j < nBytes; j++ {
            dist += bits.OnesCount8(qBits[j] ^ b[j])
        }
        out[i] = dist
    }
}
```

#### 2.1.4 SSE2 Fallback 内核

**文件**: `internal/simd/sse2_amd64.go`（新增）

```go
//go:build simd_asm && amd64 && !no_sse2

package simd

// SSE2 fallback: 4 float32/cycle, 128-bit XMM registers.
// Used when AVX2+FMA unavailable but SSE2 available.
// Implemented in pure Go assembly (plan9) inline or via avo.

func dotF32SSE2(a, b []float32) float32
func l2F32SSE2(a, b []float32) float32
func dotI8F32SSE2(q []float32, code []int8) float32
```

#### 2.1.5 汇编路径运行时分发

**文件**: `internal/simd/dist_asm.go`（修改）

```go
//go:build simd_asm

package simd

func init() {
    // Three-level fallback: AVX2+FMA → SSE2 → (implicit pure Go via no tag)
    if hasAVX2 && hasFMA {
        DotF32 = dotF32Asm
        L2F32 = l2F32Asm
        DotF32Batch16 = dotF32Batch16Asm
        DotI8F32 = dotI8F32Asm
        if hasPOPCNT {
            HammingDistanceBatch = hammingDistanceBatchAsm
        } else {
            HammingDistanceBatch = hammingDistanceBatchPureGo
        }
    } else if hasSSE2 {
        DotF32 = dotF32SSE2
        L2F32 = l2F32SSE2
        DotF32Batch16 = dotF32Batch16SSE2
        DotI8F32 = dotI8F32SSE2
        HammingDistanceBatch = hammingDistanceBatchPureGo
    }
    // If neither AVX2 nor SSE2, function pointers stay nil;
    // a safety init() in cpu.go binds pure Go as final fallback.
}
```

#### 2.1.6 avo 汇编生成器

**文件**: `internal/simd/asm/gen.go`（重写）

生成器入口，调用各子模块生成 amd64 和 arm64 汇编：

```go
//go:build ignore

package main

func main() {
    // Generate amd64 AVX2+FMA assembly
    generateDotAmd64()      // → dist_amd64.s
    generateBatchAmd64()    // → dist_amd64.s (appended)
    generateDotI8F32Amd64() // → dist_amd64.s (appended)
    generateHammingAmd64()   // → dist_amd64.s (appended)
    // Generate arm64 NEON assembly
    generateDotArm64()      // → dist_arm64.s
    generateBatchArm64()    // → dist_arm64.s (appended)
}
```

**核心 AVX2 内核算法**（`dotF32Asm`）:

```
// Pseudocode for avo-generated assembly
TEXT ·dotF32Asm(SB), NOSPLIT, $0-32
    // Args: a ptr (AX), b ptr (BX), len (CX), ret ptr (DX)
    XOR result YMM3  // accumulator
    loop:
        CMPQ CX, $8
        JL tail
        VMOVUPS (AX), YMM0    // load 8 floats from a
        VMOVUPS (BX), YMM1    // load 8 floats from b
        VFMADD231PS YMM1, YMM0, YMM3  // YMM3 += YMM0 * YMM1
        ADDQ $32, AX
        ADDQ $32, BX
        SUBQ $8, CX
        JMP loop
    tail:
        // Horizontal sum YMM3 → scalar
        VHADDPS YMM3, YMM3, YMM3
        // Extract upper 128 bits, add to lower
        VEXTRACTF128 $1, YMM3, XMM4
        ADDPS XMM3, XMM4
        // Handle remaining 0-7 elements with scalar
        ...
        MOVSS XMM4, ret+24(FP)
        RET
```

### 2.2 Phase 2: RaBitQ 优化

#### 2.2.1 投影矩阵扁平化

**文件**: `internal/index/hnsw/rabitq.go`（修改）

```go
// BEFORE:
type RaBitQuantizer struct {
    dim   int
    proj  [][]float32  // dim × nBits, cache-unfriendly
    nBits int
}

// AFTER:
type RaBitQuantizer struct {
    dim   int
    proj  []float32  // dim*nBits, contiguous
    nBits int
}
```

`NewRaBitQuantizer` 修改：
```go
func NewRaBitQuantizer(dim int) *RaBitQuantizer {
    nBits := dim
    q := &RaBitQuantizer{
        dim:   dim,
        proj:  make([]float32, dim*nBits),
        nBits: nBits,
    }
    rng := rand.New(rand.NewSource(42))
    for i := 0; i < dim; i++ {
        for j := 0; j < nBits; j++ {
            u1, u2 := rng.Float64(), rng.Float64()
            z := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
            q.proj[i*nBits+j] = float32(z) / float32(math.Sqrt(float64(nBits)))
        }
    }
    return q
}
```

`Quantize` 修改（转置循环顺序，cache 友好）：
```go
func (q *RaBitQuantizer) Quantize(vec []float32) []byte {
    nBytes := (q.nBits + 7) / 8
    out := make([]byte, nBytes)
    // Outer: dim (continuous proj access), inner: nBits
    dots := make([]float32, q.nBits)
    for i := 0; i < q.dim; i++ {
        vi := vec[i]
        base := i * q.nBits
        for j := 0; j < q.nBits; j++ {
            dots[j] += vi * q.proj[base+j]
        }
    }
    for j := 0; j < q.nBits; j++ {
        if dots[j] > 0 {
            out[j/8] |= 1 << (j % 8)
        }
    }
    return out
}
```

#### 2.2.2 Hamming 距离优化

**文件**: `internal/index/hnsw/rabitq.go`（修改）

```go
import "math/bits"
import "encoding/binary"

// HammingDistance now uses math/bits.OnesCount64 for hardware POPCNT.
func HammingDistance(a, b []byte) int {
    if len(a) != len(b) {
        return -1
    }
    dist := 0
    i := 0
    for ; i+8 <= len(a); i += 8 {
        x := binary.LittleEndian.Uint64(a[i:]) ^ binary.LittleEndian.Uint64(b[i:])
        dist += bits.OnesCount64(x)
    }
    for ; i < len(a); i++ {
        dist += bits.OnesCount8(a[i] ^ b[i])
    }
    return dist
}
```

#### 2.2.3 RaBitQState.codes 扁平化

**文件**: `internal/index/hnsw/quant.go`（修改）

```go
// BEFORE:
type RaBitQState struct {
    quantizer   *RaBitQuantizer
    codes       [][]byte  // n slice headers, GC overhead
    nBits       int
}

// AFTER:
type RaBitQState struct {
    quantizer   *RaBitQuantizer
    codesFlat   []byte    // contiguous n*nBytes
    codeOffsets []int32   // per-node offset; codeOffsets[i] = i*nBytes
    nBits       int
    nBytes      int
}
```

`searchRaBitQ` 修改（使用批量 Hamming）：
```go
func (g *Graph) searchRaBitQ(rb *RaBitQState, st *searchState, q []float32, k, ef int, filter func(int32) bool, budget searchBudget) ([]Hit, error) {
    efQ := int(math.Ceil(float64(ef) * 1.5))
    if efQ < ef { efQ = ef }
    qBits := rb.quantizer.Quantize(q)
    dists := func(ids []int32, out []float32) {
        hamOut := make([]int, len(ids))
        simd.HammingDistanceBatch(qBits, rb.codesFlat, rb.codeOffsets, ids, hamOut)
        for i := range ids {
            out[i] = float32(hamOut[i])
        }
    }
    // ... rest unchanged (rerank, beam search) ...
}
```

### 2.3 Phase 3: 标准 PQ 量化

#### 2.3.1 PQ 量化器

**文件**: `internal/index/hnsw/pq.go`（新增）

```go
package hnsw

// PQState holds Product Quantization state: M subspaces, each with
// 256 K-means centroids. Every vector is encoded as M bytes (one
// 8-bit code per subspace). Search uses LUT (look-up table) for
// coarse ranking + float32 rerank.
type PQState struct {
    m         int           // number of subspaces
    dsub      int           // dims per subspace = dims / m
    centroids [][256]float32 // m × 256 × dsub, flattened as m*256*dsub
    codes     []byte         // n*m bytes, FixedLayout: code[i] at codes[i*m:(i+1)*m]
    n         int
    dims      int
}

// trainPQ trains PQ with K-means++ initialization.
// M is auto-selected: max(dims/32, 8), capped at dims.
func trainPQ(g *Graph, m int) (*PQState, error) {
    dims := g.dims
    if m <= 0 {
        m = dims / 32
        if m < 8 { m = 8 }
        if m > dims { m = dims }
    }
    dsub := dims / m
    if dsub*m != dims {
        return nil, fmt.Errorf("pq: dims %d not divisible by m %d", dims, m)
    }
    pq := &PQState{
        m:    m,
        dsub: dsub,
        n:    int(g.n),
        dims: dims,
        codes: make([]byte, int(g.n)*m),
        centroids: make([][256]float32, m),
    }
    // Train each subspace independently
    for j := 0; j < m; j++ {
        subvecs := extractSubspace(g.vecs, int(g.n), dims, j, dsub)
        centroids := kmeansPP(subvecs, int(g.n), dsub, 256, 25)
        copy(pq.centroids[j][:], centroids)
    }
    // Encode all vectors
    for i := 0; i < int(g.n); i++ {
        v := g.Vector(int32(i))
        for j := 0; j < m; j++ {
            sub := v[j*dsub : (j+1)*dsub]
            pq.codes[i*m+j] = uint8(nearestCentroid(sub, pq.centroids[j][:], dsub))
        }
    }
    return pq, nil
}
```

#### 2.3.2 K-means++ 初始化

```go
// kmeansPP runs K-means with K-means++ initialization.
// maxIters=25, convergence threshold=1e-4.
func kmeansPP(data []float32, n, d, k, maxIters int) [256]float32 {
    // Step 1: K-means++ initialization
    centroids := make([]float32, k*d)
    // First center: random point
    rng := rand.New(rand.NewSource(42))
    first := rng.Intn(n)
    copy(centroids[0:d], data[first*d:(first+1)*d])
    // Subsequent centers: weighted by squared distance to nearest center
    minDistSq := make([]float64, n)
    for i := range minDistSq {
        minDistSq[i] = math.MaxFloat64
    }
    for c := 1; c < k; c++ {
        // Update min distances
        for i := 0; i < n; i++ {
            distSq := l2Squared(data[i*d:(i+1)*d], centroids[(c-1)*d:c*d], d)
            if distSq < minDistSq[i] {
                minDistSq[i] = distSq
            }
        }
        // Weighted random selection
        total := 0.0
        for _, d := range minDistSq { total += d }
        r := rng.Float64() * total
        cum := 0.0
        selected := 0
        for i := 0; i < n; i++ {
            cum += minDistSq[i]
            if cum >= r { selected = i; break }
        }
        copy(centroids[c*d:(c+1)*d], data[selected*d:(selected+1)*d])
    }
    // Step 2: Lloyd iterations
    assignments := make([]int, n)
    for iter := 0; iter < maxIters; iter++ {
        // Assignment step
        changed := 0
        for i := 0; i < n; i++ {
            nn := nearestCentroidIdx(data[i*d:(i+1)*d], centroids, k, d)
            if assignments[i] != nn {
                assignments[i] = nn
                changed++
            }
        }
        if iter > 0 && changed == 0 { break }
        // Update step
        counts := make([]int, k)
        sums := make([]float32, k*d)
        for i := 0; i < n; i++ {
            c := assignments[i]
            counts[c]++
            for j := 0; j < d; j++ {
                sums[c*d+j] += data[i*d+j]
            }
        }
        for c := 0; c < k; c++ {
            if counts[c] > 0 {
                for j := 0; j < d; j++ {
                    centroids[c*d+j] = sums[c*d+j] / float32(counts[c])
                }
            }
        }
    }
    // Return as [256]float32
    var result [256]float32
    copy(result[:], centroids[:k*d])
    return result
}
```

#### 2.3.3 LUT 查表距离

```go
// pqDistance computes PQ approximate distance via LUT lookup.
// LUT[j][c] = squared L2 distance from query subvector j to centroid c.
// Result = sum of LUT[j][code[j]] across all subspaces.
func pqDistance(lut [][]float32, code []byte, m int) float32 {
    var dist float32
    for j := 0; j < m; j++ {
        dist += lut[j][code[j]]
    }
    return dist
}

// buildLUT precomputes the LUT for a query vector.
func buildLUT(q []float32, centroids [][256]float32, m, dsub int) [][]float32 {
    lut := make([][]float32, m)
    for j := 0; j < m; j++ {
        lut[j] = make([]float32, 256)
        sub := q[j*dsub : (j+1)*dsub]
        for c := 0; c < 256; c++ {
            lut[j][c] = l2Squared(sub, centroids[j][c*dsub:(c+1)*dsub], dsub)
        }
    }
    return lut
}
```

#### 2.3.4 搜索路径

**文件**: `internal/index/hnsw/quant.go`（修改）

```go
type pqState = atomic.Pointer[PQState]

// In Graph struct: add `pq pqState` field

func (g *Graph) EnablePQ(m int) error {
    if g.cfg.Metric == MetricL2 {
        return fmt.Errorf("hnsw: PQ quantization requires a dot-based metric, got l2")
    }
    pq, err := trainPQ(g, m)
    if err != nil { return err }
    g.pq.Store(pq)
    return nil
}

func (g *Graph) DisablePQ() { g.pq.Store(nil) }

func (g *Graph) searchPQ(pq *PQState, st *searchState, q []float32, k, ef int, filter func(int32) bool, budget searchBudget) ([]Hit, error) {
    efQ := int(math.Ceil(float64(ef) * 1.5))
    if efQ < ef { efQ = ef }
    lut := buildLUT(q, pq.centroids, pq.m, pq.dsub)
    m := pq.m
    dists := func(ids []int32, out []float32) {
        for i, id := range ids {
            code := pq.codes[int(id)*m : int(id+1)*m]
            out[i] = -pqDistance(lut, code, m) // negate for ascending sort
        }
    }
    dists = countDists(dists, budget)
    rerank := func(id int32, coarseD float32) float32 {
        return -simd.DotF32(q, g.Vector(id))
    }
    // ... beam search same pattern as searchQuantized ...
}
```

#### 2.3.5 搜索路由

**文件**: `internal/index/hnsw/search.go`（修改）

```go
// In Search(), after sq check and before rb check:
if pq := g.pq.Load(); pq != nil {
    return g.searchPQ(pq, st, q, k, ef, filter, budget)
}
```

Routing order: sq → pq → rb → exact

#### 2.3.6 序列化扩展

**文件**: `internal/index/hnsw/index.go`（修改）

`quant.state` tier 字段扩展：

| tier 值 | 含义 |
|---------|------|
| 0 | QuantNone（无量化） |
| 1 | SQ8 |
| 2 | RaBitQ（新增） |
| 3 | PQ（新增） |

PQ 序列化格式（tier=3 之后追加）：
```
[int32 m] [int32 dsub] [m*256*dsub float32 centroids] [n*m byte codes]
```

### 2.4 Phase 4: 自适应图构建优化

#### 2.4.1 Config 扩展

**文件**: `internal/index/hnsw/hnsw.go`（修改）

```go
type BuildMode int8

const (
    BuildModeInsert   BuildMode = 0  // 逐条插入（默认）
    BuildModeODescent BuildMode = 1  // ODescent 批量构建
)

type Config struct {
    M              int
    EFConstruction int
    EFSearch       int
    Metric         Metric
    Seed           uint64
    Alpha          float64   // 自适应剪枝参数；0 或 1.0 = 标准 Alg.4
    BuildMode      BuildMode // 构建模式选择
}
```

#### 2.4.2 自适应剪枝

**文件**: `internal/index/hnsw/adaptive_pruning.go`（新增）

```go
package hnsw

// adaptiveHeuristic implements heuristic neighbor selection with
// adaptive alpha parameter. When alpha == 1.0, behavior is identical
// to the standard Malkov Alg. 4. When alpha > 1.0, more remote
// neighbors are kept (relaxed); when alpha < 1.0, stricter pruning.
func (b *builder) adaptiveHeuristic(cands []cand, m int, alpha float64) []cand {
    if alpha <= 0 || alpha == 1.0 {
        return b.heuristic(cands, m)  // delegate to standard
    }
    if len(cands) <= m {
        out := make([]cand, len(cands))
        copy(out, cands)
        return out
    }
    sel := make([]cand, 0, m)
    for _, c := range cands {
        if len(sel) >= m { break }
        ok := true
        for _, s := range sel {
            // Adaptive: reject if dist(e,s) < alpha * dist(e,q)
            if b.cachedDist(c.id, s.id) < alpha*c.d {
                ok = false
                break
            }
        }
        if ok {
            sel = append(sel, c)
        }
    }
    return sel
}

// AdaptiveAlpha runs RELAX/TIGHTEN two-phase alpha tuning.
// Phase 1 (RELAX): build with alpha=1.2, track acceptance rate.
// Phase 2 (TIGHTEN): if acceptance > 0.7, reduce alpha to 1.0;
//   if acceptance < 0.3, increase alpha to 1.5.
func (b *builder) adaptiveAlpha(cands []cand, m int) []cand {
    alpha := 1.2
    result := b.adaptiveHeuristic(cands, m, alpha)
    acceptRate := float64(len(result)) / float64(len(cands))
    if acceptRate > 0.7 {
        alpha = 1.0
    } else if acceptRate < 0.3 {
        alpha = 1.5
    }
    return b.adaptiveHeuristic(cands, m, alpha)
}
```

#### 2.4.3 距离缓存

**文件**: `internal/index/hnsw/core.go`（修改）

```go
// builder gains a distCache field. Adaptive selection: matrix for
// small candidate sets (ef_construction <= 256), map for larger.

type distCache interface {
    get(a, b int32) (float32, bool)
    put(a, b int32, v float32)
}

// matrixCache: pre-allocated [n][n]float32 for n <= 256
type matrixCache struct {
    data []float32
    n    int
}

func (c *matrixCache) get(a, b int32) (float32, bool) {
    if a > b { a, b = b, a }  // symmetry
    v := c.data[int(a)*c.n+int(b)]
    return v, v != 0  // 0 = not cached (distance 0 is self-distance, skip)
}

func (c *matrixCache) put(a, b int32, v float32) {
    if a > b { a, b = b, a }
    c.data[int(a)*c.n+int(b)] = v
}

// mapCache: map[[2]int32]float32 for n > 256
type mapCache struct {
    m map[[2]int32]float32
}

func (c *mapCache) get(a, b int32) (float32, bool) {
    if a > b { a, b = b, a }
    v, ok := c.m[[2]int32{a, b}]
    return v, ok
}

func (c *mapCache) put(a, b int32, v float32) {
    if a > b { a, b = b, a }
    c.m[[2]int32{a, b}] = v
}

// cachedDist wraps distOne with cache lookup.
func (b *builder) cachedDist(a, b int32) float32 {
    if v, ok := b.cache.get(a, b); ok {
        return v
    }
    v := b.distOne(b.vecRow(int(a)), b)
    b.cache.put(a, b, v)
    return v
}
```

#### 2.4.4 ODescent 批量构建

**文件**: `internal/index/hnsw/odescent.go`（新增）

```go
package hnsw

// BuildGraphODesc builds an HNSW graph using the ODescent algorithm:
// 1. Build initial kNN graph (hybrid: brute-force for <100k, LSH for >=100k)
// 2. Iteratively refine: for each node, expand candidates from
//    neighbors-of-neighbors, re-select with heuristic+alpha
// 3. Assign HNSW levels based on degree/distance
// Entry point has the same signature as BuildGraph.
func BuildGraphODesc(cfg Config, n, dims int, vecs []float32) (*Graph, error) {
    cfg = cfg.resolve()
    if n == 0 {
        return &Graph{cfg: cfg, dims: dims, offsets: []int32{0}}, nil
    }
    b := &builder{
        cfg:    cfg,
        n:      n,
        dims:   dims,
        vecs:   vecs,
        adj:    make([][][]int32, n),
        levels: make([]uint8, n),
    }
    // Normalize for cosine
    if cfg.Metric == MetricCosine {
        for i := 0; i < n; i++ {
            normalizeInto(vecs[i*dims:(i+1)*dims], vecs[i*dims:(i+1)*dims])
        }
    }
    // Step 1: Initial kNN graph
    k := 2 * cfg.M
    initialGraph := b.buildInitialKNNG(n, dims, vecs, k)
    // Step 2: Iterative refinement (3 rounds)
    for round := 0; round < 3; round++ {
        b.refineGraph(initialGraph, k, cfg.Alpha)
    }
    // Step 3: Assign levels
    b.assignLevels(n)
    return b.finish(), nil
}

// buildInitialKNNG uses hybrid strategy:
// <100k vectors: chunked brute force
// >=100k vectors: random projection LSH
func (b *builder) buildInitialKNNG(n, dims int, vecs []float32, k int) [][]int32 {
    if n < 100000 {
        return b.bruteForceKNNG(n, dims, vecs, k)
    }
    return b.lshKNNG(n, dims, vecs, k)
}

// bruteForceKNNG: chunk into 10k blocks, compute pairwise distances
// within each block, merge top-K across blocks.
func (b *builder) bruteForceKNNG(n, dims int, vecs []float32, k int) [][]int32 {
    result := make([][]int32, n)
    blockSize := 10000
    for start := 0; start < n; start += blockSize {
        end := start + blockSize
        if end > n { end = n }
        for i := start; i < end; i++ {
            vi := vecs[i*dims : (i+1)*dims]
            cands := make([]cand, 0, k*2)
            for j := 0; j < n; j++ {
                if i == j { continue }
                d := b.distOne(vi, int32(j))
                cands = append(cands, cand{int32(j), d})
            }
            sortCands(cands)
            if len(cands) > k { cands = cands[:k] }
            result[i] = idsOf(cands)
        }
    }
    return result
}

// lshKNNG: random projection LSH for approximate kNN.
// Uses L=8 hash tables, each with D/4 projection dimensions.
func (b *builder) lshKNNG(n, dims int, vecs []float32, k int) [][]int32 {
    // ... LSH implementation with random projections ...
    // Bucket vectors, compute exact kNN within buckets + neighbors
}

// refineGraph: one round of neighbor refinement.
// For each node, gather candidates from current neighbors + their
// neighbors (2-hop expansion), re-select with heuristic+alpha.
func (b *builder) refineGraph(graph [][]int32, k int, alpha float64) {
    for i := 0; i < b.n; i++ {
        // Collect 2-hop candidates
        seen := make(map[int32]bool)
        cands := make([]cand, 0, k*4)
        for _, nb := range graph[i] {
            if !seen[nb] {
                seen[nb] = true
                cands = append(cands, cand{nb, b.distOne(b.vecRow(i), nb)})
            }
            for _, nnb := range graph[nb] {
                if nnb != int32(i) && !seen[nnb] {
                    seen[nnb] = true
                    cands = append(cands, cand{nnb, b.distOne(b.vecRow(i), nnb)})
                }
            }
        }
        sortCands(cands)
        selected := b.adaptiveHeuristic(cands, k, alpha)
        graph[i] = idsOf(selected)
    }
}

// assignLevels: geometric distribution same as randomLevel
func (b *builder) assignLevels(n int) {
    for i := 0; i < n; i++ {
        b.levels[i] = uint8(b.randomLevel())
        b.adj[i] = make([][]int32, int(b.levels[i])+1)
    }
}
```

#### 2.4.5 构建入口选择

**文件**: `internal/index/hnsw/index.go`（修改）

```go
// In Build(), replace BuildGraph call:
func (ix *Index) Build(ctx context.Context, col index.ColumnProvider, w index.IndexOutput) error {
    // ... collect vectors ...
    var g *Graph
    var err error
    if ix.cfg.BuildMode == BuildModeODescent {
        g, err = BuildGraphODesc(ix.cfg, n, dims, vecs)
    } else {
        g, err = BuildGraph(ix.cfg, n, dims, vecs)
    }
    if err != nil { return err }
    // ... writeGraphFiles ...
}
```

### 2.5 Phase 5: Elias-Fano 邻接压缩

#### 2.5.1 Elias-Fano 编码器

**文件**: `internal/index/hnsw/elias_fano.go`（新增）

```go
package hnsw

import "math/bits"

// eliasFano encodes a monotonically increasing int32 sequence using
// Elias-Fano coding: low l bits packed via bitpack, high bits via
// unary (bitmap). Supports Select and sequential decode.
type eliasFano struct {
    lowBits   int      // l = floor(log2(upperBound/n))
    lowData   []byte   // bitpacked low parts
    highData  []byte   // unary-encoded high parts (bitmap)
    n         int      // number of elements
    upperBound int32   // max value
}

// encodeEF builds an Elias-Fano encoding for a sorted int32 sequence.
func encodeEF(vals []int32) *eliasFano {
    if len(vals) == 0 {
        return &eliasFano{n: 0}
    }
    n := len(vals)
    upper := vals[n-1]
    u := uint64(upper)
    l := 0
    if u > 0 && n > 0 {
        l = bits.Len64(u / uint64(n))
        if l > 31 { l = 31 }
    }
    // Low parts: n values of l bits each
    lowVals := make([]uint64, n)
    for i, v := range vals {
        lowVals[i] = uint64(v) & ((1 << uint(l)) - 1)
    }
    lowData := bitpackEncode(lowVals, l) // reuse storage/segment bitpack
    // High parts: unary encoding as bitmap
    // high[i] = vals[i] >> l; position = high[i] + i in the bitmap
    highBitLen := int(u>>uint(l)) + n + 1
    highWords := make([]uint64, (highBitLen+63)/64)
    for i, v := range vals {
        hi := uint64(v) >> uint(l)
        pos := hi + uint64(i)
        highWords[pos>>6] |= 1 << (pos & 63)
    }
    highData := make([]byte, len(highWords)*8)
    for i, w := range highWords {
        // little-endian put
        // ... (use binary.LittleEndian.PutUint64)
    }
    return &eliasFano{
        lowBits:    l,
        lowData:    lowData,
        highData:   highData,
        n:          n,
        upperBound: upper,
    }
}

// decodeAll decodes the entire sequence (used for neighbor list access).
func (ef *eliasFano) decodeAll() []int32 {
    if ef.n == 0 { return nil }
    result := make([]int32, ef.n)
    lowVals := bitpackDecode(ef.lowData, ef.n, ef.lowBits)
    // Walk high bitmap: each set bit at position p gives
    // high = p - index, value = (high << l) | low[index]
    idx := 0
    for pos := 0; pos < len(ef.highData)*8 && idx < ef.n; pos++ {
        byteIdx := pos >> 3
        bitIdx := pos & 7
        if ef.highData[byteIdx]&(1<<uint(bitIdx)) != 0 {
            hi := int32(pos - idx)
            result[idx] = (hi << uint(ef.lowBits)) | int32(lowVals[idx])
            idx++
        }
    }
    return result
}

// memBytes reports the encoded size.
func (ef *eliasFano) memBytes() int {
    return len(ef.lowData) + len(ef.highData)
}
```

#### 2.5.2 层级编码集成

**文件**: `internal/index/hnsw/hnsw.go`（修改）

```go
// Graph gains neighborsEF field for level-0 EF encoding.
type Graph struct {
    // ... existing fields ...
    neighborsEF *efLevelData // level-0 EF-encoded neighbors; nil = use raw []int32
}

// efLevelData stores per-node EF-encoded neighbor lists for level 0.
type efLevelData struct {
    data    []*eliasFano  // one per node (level 0)
    // Upper levels use raw []int32 via existing neighbors/offsets
}

// layerNeighbors: if neighborsEF active and level == 0, decode from EF.
func (g *Graph) layerNeighbors(node int32, level int) []int32 {
    if g.neighborsEF != nil && level == 0 {
        return g.neighborsEF.data[node].decodeAll()
    }
    base := int(node)*MaxLevel + level
    return g.neighbors[g.offsets[base]:g.offsets[base+1]]
}
```

**`finish()` 修改**：构建完成后，对 level-0 邻居列表做 EF 编码：
```go
func (b *builder) finish() *Graph {
    g := &Graph{ /* ... existing fields ... */ }
    // ... existing CSR packing ...
    // Phase 5: EF-encode level-0 neighbors
    if b.cfg.EnableEF {
        efData := make([]*eliasFano, b.n)
        for node := 0; node < b.n; node++ {
            neighbors0 := g.neighbors[g.offsets[node*MaxLevel]:g.offsets[node*MaxLevel+1]]
            sorted := append([]int32(nil), neighbors0...)
            slices.Sort(sorted)
            efData[node] = encodeEF(sorted)
        }
        g.neighborsEF = &efLevelData{data: efData}
    }
    return g
}
```

## 3. 接口设计 (Interface Design)

### 3.1 SIMD 函数指针

```go
// internal/simd/dist.go

var DotF32 func(a, b []float32) float32           // existing
var L2F32 func(a, b []float32) float32             // existing
var DotF32Batch16 func(q, cands []float32, dims int, out []float32)  // existing
var DotI8F32 func(q []float32, code []int8) float32                  // new
var HammingDistanceBatch func(qBits, codesFlat []byte, codeOffsets []int32, ids []int32, out []int)  // new
```

### 3.2 Graph 量化接口

```go
// Existing
func (g *Graph) EnableSQ8() error
func (g *Graph) DisableSQ8()
func (g *Graph) EnableRaBitQ() error
func (g *Graph) DisableRaBitQ()

// New
func (g *Graph) EnablePQ(m int) error  // m = subspace count; 0 = auto
func (g *Graph) DisablePQ()
func (g *Graph) PQActive() bool
```

### 3.3 构建接口

```go
// Existing
func BuildGraph(cfg Config, n, dims int, vecs []float32) (*Graph, error)

// New
func BuildGraphODesc(cfg Config, n, dims int, vecs []float32) (*Graph, error)
```

### 3.4 Registry 扩展

```go
// internal/index/registry.go

const (
    QuantNone   QuantLevel = 0  // existing
    QuantSQ8    QuantLevel = 1  // existing
    QuantPQ     QuantLevel = 2  // new
)
```

### 3.5 Config 扩展

```go
// internal/index/hnsw/hnsw.go

type Config struct {
    M              int
    EFConstruction int
    EFSearch       int
    Metric         Metric
    Seed           uint64
    Alpha          float64    // new; 0 or 1.0 = standard
    BuildMode      BuildMode // new; 0 = insert (default), 1 = ODescent
    EnableEF       bool      // new; true = Elias-Fano encode level 0
}
```

## 4. 内部设计 (Internal Design)

### 4.1 SIMD 模块职责

| 组件 | 职责 |
|------|------|
| `cpu.go` | 运行时 ISA 特性检测，暴露 hasAVX2 等 |
| `dist.go` | 函数指针声明（所有调用方通过指针访问） |
| `dot_purego.go` | Pure Go 基线（`!simd_asm` tag） |
| `dist_asm.go` | 汇编路径 init() 分发（`simd_asm` tag） |
| `sse2_amd64.go` | SSE2 fallback 内核（`simd_asm && amd64`） |
| `asm/*.go` | avo 生成器（`ignore` tag，dev only） |
| `*.s` | avo 生成的汇编文件（checked in） |

### 4.2 HNSW 量化 Tier 管理

```
Search() 路由顺序:
  sq.Load() != nil  → searchQuantized (SQ8)
  pq.Load() != nil  → searchPQ (PQ)
  rb.Load() != nil  → searchRaBitQ (RaBitQ)
  (none)            → searchExact (float32)

Quantize(ctx, to) 映射:
  QuantNone  → DisableSQ8() + DisablePQ() + DisableRaBitQ()
  QuantSQ8   → EnableSQ8()
  QuantPQ    → EnablePQ(0)  (auto-select M)
```

### 4.3 ODescent 构建流程

```
BuildGraphODesc:
  1. normalize (cosine)
  2. buildInitialKNNG:
     ├─ n < 100k → bruteForceKNNG (chunked, 10k blocks)
     └─ n >= 100k → lshKNNG (L=8 tables, D/4 projections)
  3. refineGraph × 3 rounds:
     ├─ 2-hop candidate expansion
     ├─ adaptiveHeuristic(cands, k, alpha)
     └─ distance cache (matrix if ≤256, map if >256)
  4. assignLevels (geometric distribution)
  5. finish() → CSR packing + optional EF encoding
```

## 5. 数据模型 (Data Model)

### 5.1 quant.state 序列化格式

```
[int8 tier]  // 0=none, 1=SQ8, 2=RaBitQ, 3=PQ

tier=1 (SQ8):
  [int32 n] [int32 dims]
  [dims × float32 mins] [dims × float32 scales]
  [n × dims × int8 codes]

tier=2 (RaBitQ):  // 新增
  [int32 n] [int32 dims] [int32 nBits] [int32 nBytes]
  [dims × nBits × float32 projFlat]  // 扁平化投影矩阵
  [n × nBytes × byte codesFlat]       // 扁平化编码
  [n × int32 codeOffsets]

tier=3 (PQ):  // 新增
  [int32 n] [int32 dims] [int32 m] [int32 dsub]
  [m × 256 × dsub × float32 centroids]
  [n × m × byte codes]
```

### 5.2 graph 文件格式（不变）

`hnsw.graph` 格式保持 HGR1 magic + CSR arrays 不变。Elias-Fano 编码的 `neighborsEF` 不写入 graph 文件，而是在 `finish()` 时由内存中的 CSR 数据编码生成。加载时从 CSR 重建 EF（通过 Config.EnableEF 标志控制）。

## 6. 错误处理 (Error Handling)

### 6.1 错误类型定义

| 错误 | 触发条件 | 处理方式 |
|------|----------|----------|
| `pq: dims %d not divisible by m %d` | dims 不能被子空间数整除 | 返回 error，自动 fallback 到 SQ8 |
| `pq: empty graph` | 对空图训练 PQ | 返回 nil PQState，不报错 |
| `hnsw: PQ quantization requires a dot-based metric` | L2 度量下启用 PQ | 返回 error |
| `hnsw: ODescent build failed: %w` | ODescent 构建内部错误 | 回退到 BuildGraph 逐条插入 |
| `simd: DotI8F32: length mismatch` | int8 code 长度不匹配 | panic（调用方 bug） |

### 6.2 边界情况处理

| 场景 | 输入 | 期望行为 |
|------|------|----------|
| 空图 PQ 训练 | n=0 | 返回空 PQState，不报错 |
| dims < 8 的 PQ | dims=4 | m=4, dsub=1, 正常工作 |
| ODescent 对 <100 节点 | n=50 | 暴力 kNN 仍可工作 |
| EF 编码空邻居列表 | node 无 level-0 邻居 | encodeEF([]) 返回空 EF |
| SIMD 不可用 | 非 amd64/arm64 | pure Go 基线自动生效 |
| AVX2 无 FMA | hasAVX2=true, hasFMA=false | 回退到 SSE2 |

## 7. 测试规划 (Test Plan)

### 7.1 测试策略表

| 测试类型 | 覆盖范围 | 目标覆盖率 | 说明 |
|----------|----------|------------|------|
| 单元测试 | SIMD 内核 | ≥ 95% | 合约测试 + 精度验证 |
| 单元测试 | PQ 量化器 | ≥ 90% | 训练/编码/LUT/搜索 |
| 单元测试 | 自适应剪枝 | ≥ 90% | alpha 参数 + 距离缓存 |
| 单元测试 | ODescent | ≥ 85% | 构建/精化/层级分配 |
| 单元测试 | Elias-Fano | ≥ 90% | 编码/解码/压缩比 |
| 集成测试 | 端到端搜索 | ≥ 80% | Build→Quantize→Search |
| Benchmark | 性能回归 | - | SIMD vs pureGo, PQ vs SQ8 |

### 7.2 关键测试场景清单

#### 正向用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| DotF32 AVX2 精度 | 768d 随机向量 | 与 pure Go 1e-6 相对误差内 | P0 |
| L2F32 AVX2 精度 | 768d 随机向量 | 与 pure Go 1e-6 相对误差内 | P0 |
| DotI8F32 精度 | 128d int8 codes | 与 float64 参考 1e-5 内 | P0 |
| Hamming 硬件 popcount | 96 字节 bitmap | 与软件 popcount 完全一致 | P0 |
| PQ recall@10 | 10k×768d 随机数据 | ≥95%（with rerank） | P0 |
| ODescent recall@10 | 10k×768d 随机数据 | 与逐条插入 ±2% | P0 |
| EF 搜索结果一致 | 1k 节点图 | 与 CSR 搜索结果完全一致 | P0 |
| 自适应剪枝 alpha=1.0 | 200 候选 | 与标准 heuristic 输出一致 | P0 |

#### 边界用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| DotF32 空向量 | len=0 | 返回 0，不 panic | P0 |
| DotF32 长度 1 | len=1 | 正确返回单元素积 | P1 |
| DotF32 长度不匹配 | len(a)≠len(b) | panic | P0 |
| DotF32Batch16 尾部 | 17 候选（仅前 16 处理） | out[0:16] 正确 | P1 |
| PQ dims 不能整除 m | dims=100, m=8 | 返回 error | P1 |
| PQ 空图 | n=0 | 返回空 PQState | P1 |
| ODescent n=1 | 单节点 | 不 panic，图含 1 节点 | P1 |
| EF 空序列 | vals=[] | decodeAll 返回 nil | P1 |
| EF 单元素 | vals=[42] | decodeAll 返回 [42] | P1 |
| RaBitQ 扁平化矩阵 | dim=768 | Quantize 结果与旧格式一致 | P0 |
| SSE2 fallback | hasAVX2=false | 正确计算距离 | P1 |

#### 异常用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| PQ 对 L2 度量 | MetricL2 | 返回 error | P0 |
| ODescent 构建失败 | 损坏输入 | 回退到 BuildGraph | P1 |
| 序列化旧格式加载 | tier=0 quant.state | 正常加载，无量化 | P0 |
| 序列化旧格式加载 | 无 quant.state 文件 | 正常加载（兼容） | P0 |
| SIMD tag 但无 AVX2 | -tags simd_asm, 无 AVX2 | SSE2 或 pure Go 回退 | P0 |

#### 兼容用例

| 场景 | 输入 | 期望输出 | 优先级 |
|------|------|----------|--------|
| 旧格式 graph 加载 + 新 PQ | HGR1 graph + QuantPQ | 正常加载并搜索 | P0 |
| 旧 SQ8 quant.state 加载 | tier=1 | 正确恢复 SQ8 tier | P0 |
| Cross-Link + ODescent 图 | ODescent 构建 + Crosslink | 兼容 | P1 |
| Merge + ODescent | ODescent 图合并 | 正常 Merge | P1 |

### 7.3 Mock 清单

| 依赖项 | Mock 方式 | 模拟行为 | 使用场景 |
|--------|-----------|----------|----------|
| ISA 检测 | 测试辅助变量 | 强制 hasAVX2=false | SSE2 fallback 测试 |
| K-means 随机种子 | 固定 seed=42 | 确定性训练结果 | PQ 回归测试 |
| ODescent 初始图 | 替换为已知 kNN | 跳过 LSH/暴力 | 精化算法测试 |

### 7.4 测试数据设计

- **SIMD 精度测试**: 768d 随机 float32（seed=42），对照 float64 参考实现
- **PQ recall 测试**: 10k×768d 随机数据 + 100 查询向量，计算 recall@10
- **ODescent 测试**: 2k×64d 小规模（验证正确性）+ 100k×768d 大规模（验证性能）
- **EF 测试**: 1k 节点 M=16 图，测量压缩比和搜索延迟
- **序列化兼容**: 旧格式 graph 文件 fixture（HGR1 magic，无 quant.state）

### 7.5 验证方式

```bash
# 默认构建（pure Go）
go test ./internal/simd/ -v -count=1
go test ./internal/index/hnsw/... -v -count=1

# SIMD 汇编路径
go test -tags simd_asm ./internal/simd/ -v -count=1
go test -tags simd_asm ./internal/index/hnsw/... -v -count=1

# 性能基准
go test -tags simd_asm ./internal/simd/ -bench Benchmark -benchmem
go test ./internal/index/hnsw/ -bench BenchmarkPQ -benchmem

# 交叉编译验证
GOARCH=arm64 go build -tags simd_asm ./...
```

## 8. 验收标准 (Acceptance Criteria)

- [ ] `go build ./...` 和 `go build -tags simd_asm ./...` 均编译通过
- [ ] `go test ./internal/simd/` 全绿（pure Go 路径）
- [ ] `go test -tags simd_asm ./internal/simd/` 全绿（汇编路径）
- [ ] `go test ./internal/index/hnsw/...` 全绿（含 PQ/ODescent/EF 新测试）
- [ ] SIMD 内核合约：DotF32/L2F32 1e-6 相对误差、DotI8F32 1e-5 相对误差
- [ ] PQ recall@10 ≥ 95%（with rerank，10k×768d 随机数据）
- [ ] ODescent recall@10 与逐条插入 ±2%（10k×768d）
- [ ] Elias-Fano 编码图搜索结果与 CSR 完全一致
- [ ] 序列化向后兼容：旧格式 HGR1 graph 可正确加载
- [ ] `GOARCH=arm64 go build -tags simd_asm` 交叉编译通过
- [ ] 基准测试：DotF32 AVX2 vs pure Go 加速 ≥ 4x（768d）
- [ ] 基准测试：Hamming OnesCount64 vs 软件 popcount 加速 ≥ 5x

## 9. 实现清单 (Implementation Checklist)

### Phase 1: SIMD 距离内核
- [ ] 新增 `golang.org/x/sys` 和 `mmcloughlin/avo` 依赖
- [ ] 实现 `cpu.go` / `cpu_amd64.go` / `cpu_arm64.go` ISA 检测
- [ ] 在 `dist.go` 新增 `DotI8F32` / `HammingDistanceBatch` 函数指针
- [ ] 在 `dot_purego.go` 新增 pure Go 基线（`dotI8F32PureGo` / `hammingDistanceBatchPureGo`）
- [ ] 给 `dot_purego.go` 加 `//go:build !simd_asm` tag
- [ ] 实现 avo 生成器（`asm/dot_amd64.go` 等 5 个文件）
- [ ] 运行 gen.go 生成 `dist_amd64.s` / `dist_arm64.s`
- [ ] 实现 `sse2_amd64.go` SSE2 fallback 内核
- [ ] 重写 `dist_asm.go` 的 `init()` 三级分发逻辑
- [ ] 编写 SIMD 合约测试（精度 + 边界 + 性能基准）

### Phase 2: RaBitQ 优化
- [ ] `rabitq.go`: `proj [][]float32` → `proj []float32` 扁平化
- [ ] `rabitq.go`: `Quantize` 转置循环 + 8x 展开
- [ ] `rabitq.go`: `HammingDistance` 用 `bits.OnesCount64`
- [ ] `quant.go`: `RaBitQState.codes` → `codesFlat` + `codeOffsets`
- [ ] `quant.go`: `searchRaBitQ` 改用 `HammingDistanceBatch`
- [ ] `index.go`: 序列化格式适配扁平矩阵（tier=2）
- [ ] 测试：扁平化正确性 + Hamming 性能基准 + 序列化兼容

### Phase 3: 标准 PQ 量化
- [ ] `pq.go`: K-means++ 训练 + 编码 + LUT 查表
- [ ] `registry.go`: 新增 `QuantPQ` 常量
- [ ] `quant.go`: `PQState` + `EnablePQ` / `DisablePQ` / `searchPQ`
- [ ] `hnsw.go`: Graph 新增 `pq` 字段
- [ ] `search.go`: 搜索路由增加 PQ 分支
- [ ] `index.go`: `Quantize` 支持 `QuantPQ`；序列化 tier=3
- [ ] 测试：PQ 编码/解码 + recall + 集成

### Phase 4: 自适应图构建
- [ ] `hnsw.go`: Config 新增 `Alpha` / `BuildMode` / `EnableEF`
- [ ] `adaptive_pruning.go`: `adaptiveHeuristic` + `adaptiveAlpha`
- [ ] `core.go`: `distCache` 接口 + `matrixCache` / `mapCache`
- [ ] `hnsw.go`: `builder` 增加 `cache` 字段 + `cachedDist`
- [ ] `odescent.go`: `BuildGraphODesc` + `bruteForceKNNG` + `lshKNNG` + `refineGraph`
- [ ] `index.go`: `Build` 根据 `BuildMode` 选择构建路径
- [ ] 测试：自适应剪枝 + 距离缓存 + ODescent

### Phase 5: Elias-Fano 压缩
- [ ] `elias_fano.go`: `encodeEF` / `decodeAll` / `memBytes`
- [ ] `hnsw.go`: Graph 新增 `neighborsEF` + `efLevelData` + `layerNeighbors` EF 分支
- [ ] `hnsw.go`: `finish()` 支持 EF 编码选项
- [ ] 测试：EF 编码/解码 + 压缩比 + 搜索结果一致性
