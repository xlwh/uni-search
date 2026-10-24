package types

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.4

func TestF16ToF32_Basic(t *testing.T) {
	// 1.0 in f16: sign=0, exp=15, mant=0
	assert.Equal(t, float32(1.0), F16ToF32(0x3C00))
	// -1.0 in f16: sign=1, exp=15, mant=0
	assert.Equal(t, float32(-1.0), F16ToF32(0xBC00))
	// 0.5 in f16: exp=14, mant=0
	assert.Equal(t, float32(0.5), F16ToF32(0x3800))
	// 2.0 in f16: exp=16, mant=0
	assert.Equal(t, float32(2.0), F16ToF32(0x4000))
}

func TestF16ToF32_Zero(t *testing.T) {
	assert.Equal(t, float32(0.0), F16ToF32(0x0000))
	assert.Equal(t, math.Float32frombits(1<<31), F16ToF32(0x8000)) // -0
}

func TestF16ToF32_Inf(t *testing.T) {
	assert.True(t, math.IsInf(float64(F16ToF32(0x7C00)), 1), "positive inf")
	assert.True(t, math.IsInf(float64(F16ToF32(0xFC00)), -1), "negative inf")
}

func TestF16ToF32_NaN(t *testing.T) {
	nan := F16ToF32(0x7C01)
	assert.True(t, math.IsNaN(float64(nan)), "should be NaN")
}

func TestF16ToF32_SmallValues(t *testing.T) {
	// Smallest normal f16: 2^-14 ≈ 6.1e-5
	small := F16ToF32(0x0400)
	assert.True(t, math.Abs(float64(small)-6.1035156e-5) < 1e-8)
	// Largest normal f16: (2-2^-10) * 2^15 ≈ 65504
	large := F16ToF32(0x7BFF)
	assert.True(t, math.Abs(float64(large)-65504.0) < 0.1)
}

func TestF32ToF16_Basic(t *testing.T) {
	assert.Equal(t, uint16(0x3C00), F32ToF16(1.0))
	assert.Equal(t, uint16(0xBC00), F32ToF16(-1.0))
	assert.Equal(t, uint16(0x3800), F32ToF16(0.5))
	assert.Equal(t, uint16(0x4000), F32ToF16(2.0))
}

func TestF32ToF16_Zero(t *testing.T) {
	assert.Equal(t, uint16(0x0000), F32ToF16(0.0))
	assert.Equal(t, uint16(0x8000), F32ToF16(math.Float32frombits(1<<31)))
}

func TestF32ToF16_Inf(t *testing.T) {
	assert.Equal(t, uint16(0x7C00), F32ToF16(float32(math.Inf(1))))
	assert.Equal(t, uint16(0xFC00), F32ToF16(float32(math.Inf(-1))))
}

func TestF32ToF16_NaN(t *testing.T) {
	nan := F32ToF16(float32(math.NaN()))
	// NaN should have exp=0x1F and non-zero mantissa
	assert.Equal(t, uint16(0x7C00), nan&0xFC00, "NaN exp bits")
	assert.True(t, nan&0x3FF != 0, "NaN should have non-zero mantissa")
}

func TestF32ToF16_Rounding(t *testing.T) {
	// Values that need rounding: 1.0001 should round to 1.0 in f16
	// f16 has 10-bit mantissa, so resolution near 1.0 is 2^-10 ≈ 0.000977
	// 1.0 + 2^-11 = 1.000488... should round to 1.0 (round to even)
	assert.Equal(t, uint16(0x3C00), F32ToF16(1.0+float32(math.Ldexp(1, -11))))

	// 1.0 + 2^-10 = 1.000977 should be exactly representable
	result := F16ToF32(F32ToF16(1.0 + float32(math.Ldexp(1, -10))))
	assert.True(t, math.Abs(float64(result)-(1.0+math.Ldexp(1, -10))) < 1e-6)
}

func TestF32ToF16_Overflow(t *testing.T) {
	// Values > 65504 should saturate to +Inf
	assert.Equal(t, uint16(0x7C00), F32ToF16(70000.0))
	assert.Equal(t, uint16(0xFC00), F32ToF16(-70000.0))
}

func TestF32ToF16_Underflow(t *testing.T) {
	// Very small values should round to 0
	assert.Equal(t, uint16(0x0000), F32ToF16(1e-40))
	assert.Equal(t, uint16(0x8000), F32ToF16(-1e-40))
}

func TestF16RoundTrip(t *testing.T) {
	// Test round-trip conversion for various values.
	testVals := []float32{
		0.0, 1.0, -1.0, 0.5, 2.0, 3.14159, 100.0, -50.0,
		0.001, 1000.0, 65504.0, -65504.0,
	}

	for _, v := range testVals {
		f16 := F32ToF16(v)
		back := F16ToF32(f16)
		// Allow small error due to f16 precision loss
		relErr := math.Abs(float64(back-v)) / math.Max(math.Abs(float64(v)), 1e-10)
		assert.True(t, relErr < 0.002, // f16 has ~0.1% relative error
			"round-trip F32ToF16ToF32(%v) = %v, rel err %v", v, back, relErr)
	}
}

func TestF16VectorConversion(t *testing.T) {
	// Test batch conversion of vectors.
	vec := []float32{1.0, 2.0, 3.0, 4.0, 5.0}
	f16Vec := make([]uint16, len(vec))
	for i, v := range vec {
		f16Vec[i] = F32ToF16(v)
	}
	// Convert back
	back := make([]float32, len(f16Vec))
	for i, h := range f16Vec {
		back[i] = F16ToF32(h)
	}
	for i := range vec {
		assert.True(t, math.Abs(float64(back[i]-vec[i])) < 0.01,
			"vec[%d]: %v vs %v", i, back[i], vec[i])
	}
}
