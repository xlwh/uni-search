//go:build !simd_asm

// Package simd dot_purego.go binds the pure Go baseline kernels to the
// public function pointers when the simd_asm build tag is NOT active
// (the default build). The actual kernel implementations live in
// kernel_purego.go (no build tag) so they are shared between both paths.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.3
package simd

func init() {
	DotF32 = dotF32PureGo
	L2F32 = l2F32PureGo
	DotF32Batch16 = dotF32Batch16PureGo
	DotI8F32 = dotI8F32PureGo
	HammingDistanceBatch = hammingDistanceBatchPureGo
	DotF16F32 = dotF16F32PureGo
}
