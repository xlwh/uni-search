// Package simd provides the float32 vector-distance kernels consumed by
// the HNSW index engine (design 02-index-engine.md §2.3).
//
// Kernels are exposed as package-level function pointers that are bound
// once at init time. M0 binds the pure Go unrolled baseline (dot_purego.go);
// avo-generated assembly kernels for amd64 (AVX2+FMA) and arm64 (NEON) can
// later take over behind the reserved "simd_asm" build tag without touching
// any call site (design 00-overview.md §3.2).
package simd

// DotF32 returns the dot product a·b of two equal-length float32 vectors.
//
// Contract, enforced by every implementation bound to this pointer:
//   - DotF32 panics when len(a) != len(b): a length mismatch is a caller
//     bug that must fail fast instead of silently corrupting search
//     results (returning NaN would just poison TopK rankings downstream).
//   - Two empty vectors yield 0.
var DotF32 func(a, b []float32) float32

// L2F32 returns the SQUARED Euclidean distance ||a-b||² between two
// equal-length float32 vectors.
//
// The square root is deliberately not taken: the squared distance is
// strictly monotonic w.r.t. the true distance, which is all HNSW neighbor
// comparisons need, and skipping it removes one sqrt per candidate
// evaluation. Callers that require the true distance apply math.Sqrt to
// the result themselves.
//
// Contract: identical to DotF32 (panics when len(a) != len(b), empty
// vectors yield 0).
var L2F32 func(a, b []float32) float32

// DotF32Batch16 computes the dot products of 16 candidate vectors against
// a single query vector q and stores them in out[0:16].
//
// cands holds the candidates contiguously: candidate i occupies
// cands[i*dims : (i+1)*dims]; only the first 16*dims elements are read.
// This is the HNSW graph-traversal hot path — one query vector scored
// against a whole expanded neighbor list in a single cache-friendly sweep
// (design 02-index-engine.md §2.3).
//
// Contract, enforced by every implementation bound to this pointer:
//   - Panics when len(q) != dims, len(cands) < 16*dims, or len(out) < 16.
//   - out[16:] is left untouched.
//   - Each out[i] agrees with DotF32(q, cands[i*dims:(i+1)*dims]) within
//     the same relative-error budget as against the naive reference (the
//     pure Go binding reuses the very same inner loop, so its results are
//     bit-identical to the individual calls).
var DotF32Batch16 func(q []float32, cands []float32, dims int, out []float32)
