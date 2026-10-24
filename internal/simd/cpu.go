// Package simd cpu.go declares the ISA feature flags consumed by the
// dispatch init() to select the best available kernel at startup. The
// per-arch files (cpu_amd64.go, cpu_arm64.go) populate these variables
// via detectCPUFeatures(); on unsupported architectures the function
// is a no-op so every flag stays false and the pure Go baseline is
// used unconditionally.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.1
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
