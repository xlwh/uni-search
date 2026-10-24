package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// SegmentStats is the per-segment summary the merge scheduler (§7.1)
// evaluates candidates against. The fields mirror the design 01 §7.1
// rule inputs: delete rate (DeletedRows/NumRows), segment count and
// tier (Bytes), and seq range for order-preserving merge.
type SegmentStats struct {
	ID           types.SegmentID
	NumRows      int
	DeletedRows  int
	Bytes        int64
	MinSeq       types.SeqNo
	MaxSeq       types.SeqNo
	MaxTimestamp int64 // F4: max value of the TTL timestamp column. @spec: 02_spec.md#5.3
}

// ListMergeCandidates returns stats for every published segment the
// scheduler may consider. The DeletedRows count comes from the
// DeleteTracker's per-segment bitmap (immediate-effect deletes).
// Bytes is approximate (footer only carries checksum, not file
// sizes); M0 walks the segment directory for an on-disk size.
func (s *Shard) ListMergeCandidates() []SegmentStats {
	set := s.setHolder.load()
	out := make([]SegmentStats, 0, len(set.items))
	for _, e := range set.items {
		if e.Reader == nil {
			continue
		}
		sr := e.Reader
		f := sr.Footer()
		bm := s.dt.SegmentBitmap(sr.ID())
		deleted := 0
		if bm != nil {
			deleted = int(bm.GetCardinality())
		}
		out = append(out, SegmentStats{
			ID:          sr.ID(),
			NumRows:     sr.NumRows(),
			DeletedRows: deleted,
			Bytes:       segmentDirSize(s.segDir, sr),
			MinSeq:      f.MinSeqNo,
			MaxSeq:      f.MaxSeqNo,
		})
	}
	return out
}

// segmentDirSize returns the on-disk size of one segment's directory
// (col_*.bin + footer + vec.idx). It walks the directory once and
// sums file sizes; subdirectories (vec.idx) are recursed. Returns 0
// if the directory is unreadable — the scheduler treats the segment
// as zero-size for ordering purposes.
func segmentDirSize(segParentDir string, sr *segment.Segment) int64 {
	dir := filepath.Join(segParentDir, fmt.Sprintf("seg_%d", sr.ID()))
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
