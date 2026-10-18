package scalar

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// Bloom filter (§6.4, optional mapping feature "bloom_filter: true").
// The design places a bloom per 1024-entry storage block as a scan
// pre-filter; the storage block layout is out of scalar-index reach, so
// the M0 form is one segment-level bloom per column, built by the
// bitmap index when the auto-enable rule (§6.3) rejects the column.
// It answers "definitely absent / probably present" for equality
// queries on high-cardinality keyword columns; it never enumerates
// rows — callers fall back to a column scan on "maybe".

const (
	bloomMagic   uint32 = 0x314D4C42 // "BLM1"
	bloomVersion uint8  = 1
	// DefaultBloomFPP is the target false-positive probability (§6.4:
	// fpp 1%).
	DefaultBloomFPP = 0.01
)

// Bloom is an immutable bloom filter over one column's values.
type Bloom struct {
	bits []uint64 // m bits, rounded to a multiple of 64
	k    uint32   // hash functions per value
	m    uint64   // bit count
	n    uint32   // values inserted (stats)
}

// newBloom sizes a filter for n entries at the given fpp using the
// standard optimal m/k: m = -n·ln(p)/(ln2)^2, k = (m/n)·ln2, both
// clamped to sane floors.
func newBloom(n int, fpp float64) *Bloom {
	if fpp <= 0 || fpp >= 1 || isNaN(fpp) {
		fpp = DefaultBloomFPP
	}
	if n < 0 {
		n = 0
	}
	m := math.Ceil(-float64(n) * math.Log(fpp) / (math.Ln2 * math.Ln2))
	if m < 64 {
		m = 64
	}
	if m > 1<<40 { // 128 MB cap of bit data; keeps sizing hostile-proof
		m = 1 << 40
	}
	w := uint64((m + 63) / 64)
	k := math.Round(float64(w*64) / float64(n) * math.Ln2)
	if k < 1 {
		k = 1
	}
	if k > 16 {
		k = 16
	}
	return &Bloom{bits: make([]uint64, w), k: uint32(k), m: w * 64, n: uint32(n)}
}

// fnvPair derives two independent 64-bit hashes of v (FNV-1a double
// hashing seed pair; combined per probe as h1 + i·h2, the Kirsch-
// Mitzenmacher construction).
func fnvPair(v []byte) (uint64, uint64) {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h1 := uint64(offset64)
	for _, c := range v {
		h1 ^= uint64(c)
		h1 *= prime64
	}
	// Second hash: same data, different seed (FNV-1a variant offset).
	const offset64b = 1469598103934665603
	h2 := uint64(offset64b)
	for _, c := range v {
		h2 = (h2 ^ uint64(c)) * prime64
		h2 ^= h2 >> 29
	}
	return h1, h2 | 1 // |1 keeps the step odd so it cycles the table
}

// Add inserts one value.
func (b *Bloom) Add(v []byte) {
	h1, h2 := fnvPair(v)
	for i := uint32(0); i < b.k; i++ {
		bit := (h1 + uint64(i)*h2) % b.m
		b.bits[bit/64] |= 1 << (bit % 64)
	}
}

// MightContain reports whether v may be in the set. false is
// definitive; true is probabilistic with fpp ≤ the configured bound.
func (b *Bloom) MightContain(v []byte) bool {
	h1, h2 := fnvPair(v)
	for i := uint32(0); i < b.k; i++ {
		bit := (h1 + uint64(i)*h2) % b.m
		if b.bits[bit/64]&(1<<(bit%64)) == 0 {
			return false
		}
	}
	return true
}

// Len reports the number of inserted values.
func (b *Bloom) Len() int { return int(b.n) }

// MemBytes reports the resident size for MemEstimate.
func (b *Bloom) MemBytes() int64 { return 8*int64(len(b.bits)) + 32 }

// WriteTo serializes the filter (little-endian):
// u32 magic | u8 version | u32 k | u64 m | u32 n | bit words.
func (b *Bloom) WriteTo(w io.Writer) (int64, error) {
	var hdr []byte
	hdr = binary.LittleEndian.AppendUint32(hdr, bloomMagic)
	hdr = append(hdr, bloomVersion)
	hdr = binary.LittleEndian.AppendUint32(hdr, b.k)
	hdr = binary.LittleEndian.AppendUint64(hdr, b.m)
	hdr = binary.LittleEndian.AppendUint32(hdr, b.n)
	wn, err := w.Write(hdr)
	if err != nil {
		return int64(wn), fmt.Errorf("scalar: write bloom header: %w", err)
	}
	total := int64(wn)
	p := make([]byte, 8)
	for _, word := range b.bits {
		binary.LittleEndian.PutUint64(p, word)
		wn, err := w.Write(p)
		total += int64(wn)
		if err != nil {
			return total, fmt.Errorf("scalar: write bloom bits: %w", err)
		}
	}
	return total, nil
}

// ReadBloom parses what WriteTo produced.
func ReadBloom(r io.Reader) (*Bloom, error) {
	hdr := make([]byte, 4+1+4+8+4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, fmt.Errorf("scalar: read bloom header: %w", err)
	}
	if binary.LittleEndian.Uint32(hdr[0:4]) != bloomMagic || hdr[4] != bloomVersion {
		return nil, fmt.Errorf("scalar: bad bloom header")
	}
	b := &Bloom{
		k: binary.LittleEndian.Uint32(hdr[5:9]),
		m: binary.LittleEndian.Uint64(hdr[9:17]),
		n: binary.LittleEndian.Uint32(hdr[17:21]),
	}
	if b.k == 0 || b.k > 16 || b.m < 64 || b.m%64 != 0 || b.m > 1<<40 {
		return nil, fmt.Errorf("scalar: corrupt bloom parameters (k=%d m=%d)", b.k, b.m)
	}
	b.bits = make([]uint64, b.m/64)
	buf := make([]byte, 8)
	for i := range b.bits {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("scalar: read bloom bits: %w", err)
		}
		b.bits[i] = binary.LittleEndian.Uint64(buf)
	}
	return b, nil
}
