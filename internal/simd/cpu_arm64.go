//go:build arm64

// Package simd cpu_arm64.go probes ARM NEON (Advanced SIMD) availability
// via golang.org/x/sys/cpu. Go's arm64 compiler already emits NEON for
// float32 loops, but the dedicated assembly kernels still benefit from
// explicit NEON register usage.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.1
package simd

import "golang.org/x/sys/cpu"

func detectCPUFeatures() {
	hasNEON = cpu.ARM64.HasASIMD
}
