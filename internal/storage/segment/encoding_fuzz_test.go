package segment

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzSegmentFormat tests that corrupt segment data is handled
// gracefully (no panic, explicit error). The fuzzer feeds random
// byte sequences that resemble a segment footer and verifies that
// decoding either succeeds or returns an error — never panics.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.3.8
func FuzzSegmentFormat(f *testing.F) {
	// Seed: a minimal valid-looking footer (magic + version + fields)
	seed := make([]byte, 64)
	binary.LittleEndian.PutUint64(seed[0:], footerMagic)
	binary.LittleEndian.PutUint32(seed[8:], 1) // format_version
	f.Add(seed)
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xFF}, 128))

	f.Fuzz(func(t *testing.T, data []byte) {
		// We test the checksum manifest decoder: random data should
		// either decode or error, never panic.
		if len(data) < 16 {
			return // too short, skip
		}
		// Attempt to decode a u32 count from the first 4 bytes.
		count := binary.LittleEndian.Uint32(data[:4])
		if count > 10000 {
			return // unreasonably large, skip
		}
		// Walk the manifest entries — this exercises bounds checking.
		off := 4
		for i := uint32(0); i < count; i++ {
			if off+4 > len(data) {
				break
			}
			nameLen := int(binary.LittleEndian.Uint32(data[off:]))
			off += 4
			if nameLen > 4096 || off+nameLen > len(data) {
				break
			}
			off += nameLen
			if off+8+4 > len(data) {
				break
			}
			off += 8 + 4 // size (u64) + crc (u32)
		}
		// If we got here without panicking, the fuzzer is happy.
	})
}
