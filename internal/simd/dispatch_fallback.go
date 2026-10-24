//go:build simd_asm && !amd64 && !arm64

// Package simd dispatch_fallback.go binds the pure Go baseline on
// architectures without hardware SIMD even when the simd_asm tag is
// active. This ensures the package always compiles and runs.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.5
package simd

func dispatchSIMD() {
	bindPureGo()
}
