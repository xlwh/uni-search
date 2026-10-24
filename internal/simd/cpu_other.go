//go:build !amd64 && !arm64

// Package simd cpu_other.go is the no-op fallback for architectures
// without hardware SIMD (e.g. riscv64, wasm). Every flag stays false
// so the pure Go baseline is used unconditionally.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.1
package simd

func detectCPUFeatures() {
	// No hardware SIMD on this architecture.
}
