//go:build amd64

// Package simd cpu_amd64.go probes x86_64 SIMD features via golang.org/x/sys/cpu.
// AVX2+FMA enables the 256-bit AVX2 kernels; SSE2 provides a 128-bit
// fallback; POPCNT accelerates Hamming distance for RaBitQ.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.1
package simd

import "golang.org/x/sys/cpu"

func detectCPUFeatures() {
	hasAVX2 = cpu.X86.HasAVX2
	hasFMA = cpu.X86.HasFMA
	hasPOPCNT = cpu.X86.HasPOPCNT
	hasSSE2 = cpu.X86.HasSSE2
}
