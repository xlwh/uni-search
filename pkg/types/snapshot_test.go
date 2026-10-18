package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestR3_SnapshotVisibility covers the MVCC snapshot contract of R3
// (design 01 §8): a write is visible iff its seq_no <= ReadUpTo, and the
// PIT flag round-trips on the struct.
func TestR3_SnapshotVisibility(t *testing.T) {
	snap := NewSnapshot(42)
	assert.Equal(t, SeqNo(42), snap.ReadUpTo, "watermark must be preserved")
	assert.False(t, snap.PIT, "regular snapshots are not PIT")

	assert.True(t, snap.Visible(0), "seq 0 is below the watermark")
	assert.True(t, snap.Visible(42), "seq at the watermark is visible")
	assert.False(t, snap.Visible(43), "seq above the watermark is invisible")

	pit := Snapshot{ReadUpTo: 7, PIT: true}
	assert.True(t, pit.PIT, "PIT flag must round-trip")
	assert.True(t, pit.Visible(7))
	assert.False(t, pit.Visible(8))
}
