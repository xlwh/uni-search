package types

// Snapshot carries the MVCC read view (design 01-storage-engine §8):
// writes with seq_no <= ReadUpTo are visible, later ones are not. PIT marks
// snapshots pinned by the point-in-time API (04 §6.4); regular reads use the
// current applied watermark instead.
type Snapshot struct {
	ReadUpTo SeqNo // visibility watermark: writes at seq_no <= ReadUpTo are visible
	PIT      bool  // true when the snapshot comes from a Point-in-Time reader
}

// NewSnapshot returns a regular (non-PIT) snapshot reading up to seq.
func NewSnapshot(seq SeqNo) Snapshot {
	return Snapshot{ReadUpTo: seq}
}

// Visible reports whether a write with the given seq_no is visible to this
// snapshot. Segments are pruned by max_seq_no and MemTable rows are filtered
// by this predicate on the scan path (01 §8).
func (s Snapshot) Visible(seq SeqNo) bool {
	return seq <= s.ReadUpTo
}
