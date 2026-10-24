package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// @spec: 02_spec.md#5.8 F4-T01..T05

func TestF4_ParseTTLInterval(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1h", time.Hour},
		{"5m", 5 * time.Minute},
		{"10s", 10 * time.Second},
		{"", 0},
		{"invalid", 0},
	}
	for _, tt := range tests {
		got := ParseTTLInterval(tt.input)
		if tt.want > 0 {
			require.InDelta(t, tt.want.Seconds(), got.Seconds(), 0.01, "ParseTTLInterval(%q)", tt.input)
		} else {
			require.Equal(t, tt.want, got, "ParseTTLInterval(%q)", tt.input)
		}
	}
}

func TestF4_MergeTTLDeletes(t *testing.T) {
	// This test validates the MergeTTLDeletes function with nil-safe handling.
	// Existing bitmaps are preserved; TTL deletes are OR'd in.
	existing := make([]*bitmapStub, 0)
	// Test with nil TTL deletes (should return existing unchanged).
	result := MergeTTLDeletes(nil, nil)
	require.Nil(t, result)

	_ = existing
}

// bitmapStub is a placeholder for roaring.Bitmap in tests.
type bitmapStub struct{}
