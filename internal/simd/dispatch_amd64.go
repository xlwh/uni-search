//go:build simd_asm && amd64

// Package simd dispatch_amd64.go selects the best available kernel set
// on x86_64 at init time. Three-level fallback:
//  1. AVX2+FMA  → 256-bit assembly kernels (when available)
//  2. SSE2       → 128-bit fallback kernels (when available)
//  3. pure Go    → kernel_purego.go functions (always available)
//
// Until the avo-generated assembly (.s files) are checked in, every
// path falls back to pure Go. The dispatch infrastructure is in place
// so adding assembly later only requires implementing the assembly
// function stubs and updating the binding here.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.1.5
package simd

func dispatchSIMD() {
	// Three-level fallback: AVX2+FMA → SSE2 → pure Go.
	// Assembly kernels (dotF32Asm etc.) will be bound here when the
	// avo-generated .s files are checked in. For now, pure Go is
	// the only available implementation on every path.
	if hasAVX2 && hasFMA {
		// TODO: bind AVX2+FMA assembly kernels when available:
		// DotF32 = dotF32Asm
		// L2F32 = l2F32Asm
		// DotF32Batch16 = dotF32Batch16Asm
		// DotI8F32 = dotI8F32Asm
		// if hasPOPCNT {
		//     HammingDistanceBatch = hammingDistanceBatchAsm
		// } else {
		//     HammingDistanceBatch = hammingDistanceBatchPureGo
		// }
		bindPureGo()
	} else if hasSSE2 {
		// TODO: bind SSE2 fallback kernels when available:
		// DotF32 = dotF32SSE2
		// L2F32 = l2F32SSE2
		// DotF32Batch16 = dotF32Batch16SSE2
		// DotI8F32 = dotI8F32SSE2
		// HammingDistanceBatch = hammingDistanceBatchPureGo
		bindPureGo()
	} else {
		bindPureGo()
	}
}
