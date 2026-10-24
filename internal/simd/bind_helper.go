// Package simd bind_helper.go provides the bindPureGo helper used by
// all dispatch_*.go files to bind the pure Go kernel functions as
// fallback when no assembly kernel is available. This file has no
// build tag so it is always available.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.5
package simd

// bindPureGo binds every public function pointer to its pure Go
// baseline implementation. Called by dispatch_*.go under the simd_asm
// tag when no hardware SIMD kernel is available.
func bindPureGo() {
	DotF32 = dotF32PureGo
	L2F32 = l2F32PureGo
	DotF32Batch16 = dotF32Batch16PureGo
	DotI8F32 = dotI8F32PureGo
	HammingDistanceBatch = hammingDistanceBatchPureGo
	DotF16F32 = dotF16F32PureGo
}
