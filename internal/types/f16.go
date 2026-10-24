// Package types f16.go implements IEEE 754 half-precision (binary16)
// conversion between float16 and float32. Float16 storage halves the
// memory footprint of dense_vector fields with minimal precision loss
// (<1% recall@k in typical ANN workloads).
//
// @spec: ./archive/2026-10-23-qdrant-features-import/02_spec.md#2.4
package types

import "math"

// F16ToF32 converts an IEEE 754 half-precision (binary16) value stored
// as uint16 to a float32. Handles subnormals, infinities, and NaN.
func F16ToF32(h uint16) float32 {
	sign := uint32(h>>15) & 1
	exp := uint32(h>>10) & 0x1F
	mant := uint32(h) & 0x3FF

	switch {
	case exp == 0 && mant == 0:
		// ±0
		return math.Float32frombits(sign << 31)
	case exp == 0:
		// Subnormal: denormalize to normal range
		// Value = (-1)^s * 2^(-14) * (0.mantissa)
		e := int32(-14)
		for mant&0x400 == 0 {
			mant <<= 1
			e--
		}
		mant &= 0x3FF // clear the implicit leading bit
		return math.Float32frombits(sign<<31 | uint32(e+112+15)<<23 | mant<<13)
	case exp == 0x1F:
		// Inf or NaN
		return math.Float32frombits(sign<<31 | 0x7F800000 | mant<<13)
	default:
		// Normal: exponent bias adjustment (15 for f16, 127 for f32)
		return math.Float32frombits(sign<<31 | (exp+112)<<23 | mant<<13)
	}
}

// F32ToF16 converts a float32 to an IEEE 754 half-precision (binary16)
// value stored as uint16. Uses round-to-nearest-even (RNE) rounding.
// Values exceeding the f16 range saturate to ±Inf.
func F32ToF16(f float32) uint16 {
	bits := math.Float32bits(f)
	sign := uint16(bits>>31) & 1
	exp := int32(bits>>23) & 0xFF
	mant := bits & 0x7FFFFF

	switch {
	case exp == 0xFF:
		// Inf or NaN
		if mant != 0 {
			// NaN: preserve the sign and a non-zero mantissa
			return sign<<15 | 0x7C00 | uint16(mant>>13)
		}
		// Inf
		return sign<<15 | 0x7C00
	case exp == 0 && mant == 0:
		// ±0
		return sign << 15
	}

	// Real exponent: exp - 127 (f32 bias)
	realExp := exp - 127

	if realExp > 15 {
		// Overflow: saturate to Inf
		return sign<<15 | 0x7C00
	}

	if realExp < -24 {
		// Underflow: too small for subnormal f16, round to 0
		return sign << 15
	}

	if realExp >= -14 {
		// Normal f16: exp field = realExp + 15.
		// f32 mantissa is 23 fractional bits; f16 needs 10 fractional bits.
		// Shift right by 13 (without implicit 1 bit).
		mantF16 := mant >> 13
		roundBit := (mant >> 12) & 1
		stickyBits := mant & 0xFFF

		// Round to nearest even.
		if roundBit != 0 && (stickyBits != 0 || (mantF16&1) != 0) {
			mantF16++
		}

		// Check if mantissa overflowed (all 1s + 1 = 0x400).
		if mantF16 >= 0x400 {
			realExp++
			mantF16 = 0
		}

		if realExp > 15 {
			return sign<<15 | 0x7C00 // Inf
		}

		return sign<<15 | uint16(realExp+15)<<10 | uint16(mantF16)
	}

	// Subnormal f16: realExp in [-24, -15].
	// mant32 = 1.xxx * 2^23 (24 significant bits including implicit 1).
	mant32 := mant | 0x800000
	shiftAmount := uint(-(realExp + 1))

	shifted := mant32 >> shiftAmount
	roundBit := (mant32 >> (shiftAmount - 1)) & 1
	var stickyBit uint32
	if shiftAmount > 1 {
		stickyBit = mant32 & ((1 << (shiftAmount - 1)) - 1)
	}

	result := shifted
	if roundBit != 0 && (stickyBit != 0 || (result&1) != 0) {
		result++
	}

	// Check if rounding promoted to normal range.
	if result >= 0x400 {
		return sign<<15 | 1<<10 | uint16(result-0x400)
	}

	return sign<<15 | uint16(result)
}
