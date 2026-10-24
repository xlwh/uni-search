package storage

import (
	"context"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// TTL merge support: computes delete bitmaps for rows whose timestamp
// column value is older than the TTL interval, and merges them with
// existing delete bitmaps so copyMergeRows naturally skips expired rows.
// @spec: 02_spec.md#5.5

// ComputeTTLDeletes scans the timestamp column of a segment and returns
// a bitmap of rows whose value is older than now - interval. Returns nil
// if the column doesn't exist or no rows are expired.
// @spec: 02_spec.md#5.5
func ComputeTTLDeletes(ctx context.Context, sr *segment.Segment, fid types.FieldID, interval time.Duration, now time.Time) *roaring.Bitmap {
	if interval <= 0 {
		return nil
	}
	threshold := now.UnixMilli() - interval.Milliseconds()
	bm := roaring.New()

	cr, err := sr.Column(fid)
	if err != nil {
		return nil // column not found, no TTL deletes
	}
	it := cr.NewIterator()
	rowOffset := 0
	for !it.Done() {
		blk, err := it.Next(ctx)
		if err != nil {
			return bm
		}
		longs := blk.Longs()
		for i := 0; i < blk.Rows; i++ {
			if !blk.IsNull(i) && longs[i] < threshold {
				bm.Add(uint32(rowOffset + i))
			}
		}
		rowOffset += blk.Rows
	}
	if bm.IsEmpty() {
		return nil
	}
	return bm
}

// MergeTTLDeletes merges TTL-expired row bitmaps with existing delete
// bitmaps. Returns new bitmaps where each bitmap[i] = oldBitmap[i] OR
// ttlBitmap[i]. If ttlBitmap is nil, returns oldBitmap unchanged.
// @spec: 02_spec.md#5.5
func MergeTTLDeletes(existing []*roaring.Bitmap, ttlDeletes []*roaring.Bitmap) []*roaring.Bitmap {
	if len(ttlDeletes) == 0 {
		return existing
	}
	merged := make([]*roaring.Bitmap, len(existing))
	for i := range existing {
		merged[i] = existing[i]
		if i < len(ttlDeletes) && ttlDeletes[i] != nil {
			if merged[i] == nil {
				merged[i] = ttlDeletes[i].Clone()
			} else {
				clone := merged[i].Clone()
				clone.Or(ttlDeletes[i])
				merged[i] = clone
			}
		}
	}
	return merged
}

// ParseTTLInterval parses a TTL interval string like "30d", "1h", "5m"
// into a time.Duration. @spec: 02_spec.md#5.3
func ParseTTLInterval(s string) time.Duration {
	if s == "" {
		return 0
	}
	// Simple parser: number + unit (d/h/m/s)
	d := s
	if len(d) < 2 {
		return 0
	}
	unit := d[len(d)-1]
	num := d[:len(d)-1]
	var hours float64
	switch unit {
	case 'd':
		hours = parseFloat(num) * 24
	case 'h':
		hours = parseFloat(num)
	case 'm':
		hours = parseFloat(num) / 60
	case 's':
		hours = parseFloat(num) / 3600
	default:
		return 0
	}
	return time.Duration(hours * float64(time.Hour))
}

func parseFloat(s string) float64 {
	var f float64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			f = f*10 + float64(c-'0')
		} else {
			break
		}
	}
	return f
}

// ResolveTTLConfig extracts TTL settings from IndexMeta and returns the
// TTL column field ID and interval for the first DELETE-mode TTL rule.
// @spec: 02_spec.md#5.3
func ResolveTTLConfig(m *meta.IndexMeta, fidMap map[string]types.FieldID) (types.FieldID, time.Duration) {
	if m == nil || m.Settings.TTL == nil {
		return 0, 0
	}
	for _, expr := range m.Settings.TTL.Exprs {
		if expr.Mode == "delete" || expr.Mode == "" {
			fid, ok := fidMap[expr.Column]
			if !ok {
				continue
			}
			interval := ParseTTLInterval(expr.Interval)
			if interval > 0 {
				return fid, interval
			}
		}
	}
	return 0, 0
}
