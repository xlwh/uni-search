package agg

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
)

// HLL is a HyperLogLog++ cardinality estimator (design 03 §6.1).
// Uses sparse representation when few registers are set, switching
// to dense when the register count exceeds a threshold.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.3
type HLL struct {
	p           int        // precision: bits for register index (4-18)
	m           uint64     // number of registers: 2^p
	registers   []uint8    // dense registers (nil when sparse)
	sparse      map[uint32]uint8 // sparse representation
	sparseOnly  bool
}

// NewHLL creates an HLL with the given precision (default 14).
func NewHLL(precision int) *HLL {
	if precision < 4 {
		precision = 4
	}
	if precision > 18 {
		precision = 18
	}
	m := uint64(1) << precision
	return &HLL{
		p:          precision,
		m:          m,
		sparse:     make(map[uint32]uint8, 128),
		sparseOnly: true,
	}
}

// Add hashes a value and updates the HLL.
func (h *HLL) Add(hash uint64) {
	idx, rho := h.getRegister(hash)
	val := uint8(rho)
	if h.sparseOnly {
		if cur, ok := h.sparse[uint32(idx)]; !ok || val > cur {
			h.sparse[uint32(idx)] = val
		}
		if len(h.sparse) > int(h.m)/4 {
			h.toDense()
		}
		return
	}
	if h.registers[idx] < val {
		h.registers[idx] = val
	}
}

// getRegister extracts the register index and the number of leading
// zeros + 1 (rho) from a 64-bit hash.
func (h *HLL) getRegister(hash uint64) (uint64, uint8) {
	idx := hash >> (64 - h.p)
	w := hash<<h.p | (1 << (h.p - 1)) // remaining bits with leading 1
	rho := uint8(1)
	mask := uint64(1) << (63 - h.p)
	for mask > 0 && w&mask == 0 {
		rho++
		mask >>= 1
	}
	return idx, rho
}

// toDense converts from sparse map to dense register array.
func (h *HLL) toDense() {
	h.registers = make([]uint8, h.m)
	for idx, val := range h.sparse {
		h.registers[idx] = val
	}
	h.sparse = nil
	h.sparseOnly = false
}

// Estimate returns the approximate distinct count.
func (h *HLL) Estimate() uint64 {
	if h.sparseOnly {
		if len(h.sparse) == 0 {
			return 0
		}
		// Use linear counting for small counts.
		v := uint64(len(h.sparse))
		return uint64(float64(h.m) * math.Log(float64(h.m)/float64(h.m-v)))
	}
	return h.estimateDense()
}

func (h *HLL) estimateDense() uint64 {
	var sum float64
	zeros := 0
	for i := uint64(0); i < h.m; i++ {
		sum += math.Pow(2.0, -float64(h.registers[i]))
		if h.registers[i] == 0 {
			zeros++
		}
	}
	alpha := h.alpha()
	est := alpha * float64(h.m) * float64(h.m) / sum

	// Small range correction.
	if est < 2.5*float64(h.m) && zeros > 0 {
		est = float64(h.m) * math.Log(float64(h.m)/float64(zeros))
	}

	// Large range correction (HLL++ improvement).
	if est > (1.0/30.0)*math.Pow(2, 32) {
		est = -math.Pow(2, 32) * math.Log(1-est/math.Pow(2, 32))
	}

	return uint64(est + 0.5)
}

// alpha is the bias correction constant.
func (h *HLL) alpha() float64 {
	switch h.p {
	case 4:
		return 0.673
	case 5:
		return 0.697
	case 6:
		return 0.709
	default:
		return 0.7213 / (1.0 + 1.079/float64(h.m))
	}
}

// Merge combines another HLL into this one.
func (h *HLL) Merge(other *HLL) {
	if other == nil {
		return
	}
	if h.p != other.p {
		// Different precision: re-add all registers.
		other.toDense()
		h.toDense()
		for i := uint64(0); i < other.m; i++ {
			if other.registers[i] > h.registers[i*uint64(h.m)/other.m] {
				h.registers[i*uint64(h.m)/other.m] = other.registers[i]
			}
		}
		return
	}
	if other.sparseOnly {
		for idx, val := range other.sparse {
			if h.sparseOnly {
				if cur, ok := h.sparse[idx]; !ok || val > cur {
					h.sparse[idx] = val
				}
			} else {
				if val > h.registers[idx] {
					h.registers[idx] = val
				}
			}
		}
		return
	}
	other.toDense()
	h.toDense()
	for i := uint64(0); i < h.m; i++ {
		if other.registers[i] > h.registers[i] {
			h.registers[i] = other.registers[i]
		}
	}
}

// Reset clears all registers.
func (h *HLL) Reset() {
	h.sparse = make(map[uint32]uint8, 128)
	h.sparseOnly = true
	h.registers = nil
}

// Bytes serializes the HLL.
func (h *HLL) Bytes() []byte {
	h.toDense()
	buf := make([]byte, 4+len(h.registers))
	binary.LittleEndian.PutUint32(buf[:4], uint32(h.p))
	copy(buf[4:], h.registers)
	return buf
}

// FromBytes deserializes an HLL.
func FromBytes(data []byte) (*HLL, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("hll: data too short")
	}
	p := int(binary.LittleEndian.Uint32(data[:4]))
	if p < 4 || p > 18 {
		return nil, fmt.Errorf("hll: invalid precision %d", p)
	}
	m := uint64(1) << p
	if len(data) < 4+int(m) {
		return nil, fmt.Errorf("hll: data too short for %d registers", m)
	}
	h := NewHLL(p)
	h.toDense()
	copy(h.registers, data[4:4+m])
	return h, nil
}

// HashString computes an FNV-1a hash for string input.
func HashString(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
