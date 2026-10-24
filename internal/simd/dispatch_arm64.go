//go:build simd_asm && arm64

// Package simd dispatch_arm64.go selects the best available kernel set
// on ARM64 at init time. Two-level fallback:
//  1. NEON  → assembly kernels (when available)
//  2. pure Go → kernel_purego.go functions (always available)
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.5
package simd

func dispatchSIMD() {
	if hasNEON {
		// TODO: bind NEON assembly kernels when available:
		// DotF32 = dotF32Neon
		// L2F32 = l2F32Neon
		// DotF32Batch16 = dotF32Batch16Neon
		// DotI8F32 = dotI8F32Neon
		// HammingDistanceBatch = hammingDistanceBatchPureGo
		bindPureGo()
	} else {
		bindPureGo()
	}
}
