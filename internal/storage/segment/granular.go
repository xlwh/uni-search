// Granular Range types for intra-block bucket-level range pruning.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.9
package segment

// GranularZone records min/max/rowCount for one bucket within a block.
// When a range query evaluates a block, it checks each granular zone
// to skip rows in buckets that don't overlap the query range.
type GranularZone struct {
	Min      int64
	Max      int64
	RowCount uint16
}

// GranularZones is the set of buckets for one block, written after
// the block payload in FormatVersion >= V3. nil for V2 segments
// (no intra-block pruning, degrades to full block scan).
type GranularZones struct {
	Zones []GranularZone
}

// NumBuckets returns the number of granular zones.
func (gz *GranularZones) NumBuckets() int {
	if gz == nil {
		return 0
	}
	return len(gz.Zones)
}

// OverlapsRange checks if any bucket overlaps the given [lower, upper]
// range. Returns false if no overlap (entire block can be skipped).
func (gz *GranularZones) OverlapsRange(lower, upper int64, hasLower, hasUpper bool) bool {
	if gz == nil || len(gz.Zones) == 0 {
		return true // no granular zones → can't prune
	}
	for _, z := range gz.Zones {
		if z.RowCount == 0 {
			continue // empty bucket
		}
		if hasLower && z.Max < lower {
			continue // bucket entirely below range
		}
		if hasUpper && z.Min > upper {
			continue // bucket entirely above range
		}
		return true // overlap found
	}
	return false // no bucket overlaps
}

// MatchingRows returns the row indices within the block that fall
// in buckets overlapping the given range. For V2 segments (nil zones),
// returns nil (caller should scan all rows).
func (gz *GranularZones) MatchingRows(lower, upper int64, hasLower, hasUpper bool) []int {
	if gz == nil || len(gz.Zones) == 0 {
		return nil // no pruning possible
	}
	var rows []int
	rowOffset := 0
	for i, z := range gz.Zones {
		if z.RowCount == 0 {
			continue
		}
		// Check if this bucket overlaps the query range.
		overlaps := true
		if hasLower && z.Max < lower {
			overlaps = false
		}
		if hasUpper && z.Min > upper {
			overlaps = false
		}
		if overlaps {
			for r := 0; r < int(z.RowCount); r++ {
				rows = append(rows, rowOffset+r)
			}
		}
		_ = i
		rowOffset += int(z.RowCount)
	}
	return rows
}

// computeAdaptiveBuckets chooses bucket count based on block row count.
// - nrows <= 16:    4 buckets
// - nrows <= 64:    16 buckets
// - nrows <= 256:   32 buckets
// - nrows <= 8192:  64 buckets
// - nrows > 8192:   128 buckets
func computeAdaptiveBuckets(nrows int) int {
	switch {
	case nrows <= 0:
		return 0
	case nrows <= 16:
		return 4
	case nrows <= 64:
		return 16
	case nrows <= 256:
		return 32
	case nrows <= 8192:
		return 64
	default:
		return 128
	}
}
