// Package wal implements the M0 local write-ahead log (design
// 01-storage-engine §3). Segment files live under
// <data-dir>/wal/{shard_id}/{seq}.log; each starts with a 16-byte header
// (magic "USWAL01\0" + shard id u64 LE) followed by records
//
//	[len u32 | crc32c u32 | seq_no u64 | entry_type u8 | payload zstd]
//
// where all integers are little-endian and the crc32c checksum (Castagnoli,
// hash/crc32) covers the seq_no, entry_type and payload bytes that follow
// it. Appends are batched by a group-commit loop (§3.2): a batch flushes to
// disk with a single fsync when either the commit window (default 2ms) or
// the commit byte threshold (default 4MB) is reached, whichever comes
// first. Recovery (§3.1) scans every segment file, CRC-verifies each
// record, truncates the first bad tail as uncommitted and lets the caller
// replay only the records after the last SegmentFlushed marker.
//
// M2 replaces the persistence channel with a Raft log but keeps these
// record semantics (design 01 §3.3); the WAL interface below is that seam.
package wal

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// EntryType is the record type byte (design 01 §3.1).
type EntryType uint8

const (
	// EntryInsert carries one document; payload = the document JSON in its
	// canonical write-path form, which embeds the document _id so replay
	// can rebuild the memtable primary-key index.
	EntryInsert EntryType = 0
	// EntryDelete removes one document; payload = the raw _id bytes.
	EntryDelete EntryType = 1
	// EntryUpdate replaces one document; payload = [id_len u16 | _id |
	// document JSON].
	EntryUpdate EntryType = 2
	// EntrySegmentFlushed marks that every record with seq_no <= payload
	// (u64 LE) is durable in a published segment; recovery replays only
	// records after the last marker.
	EntrySegmentFlushed EntryType = 3
	// EntryMetadataTick carries an opaque metadata blob (e.g. a settings
	// snapshot); replay consumers decide its meaning.
	EntryMetadataTick EntryType = 4
)

// ErrClosed is returned by Append after Close.
var ErrClosed = errors.New("wal: closed")

// Entry is one WAL record. SeqNo is assigned by Append on the write path
// (also written back into the caller's entries) and populated by Replay on
// the read path.
type Entry struct {
	SeqNo   types.SeqNo
	Type    EntryType
	Payload []byte
}

// NewInsert builds an Insert entry for one document (payload = document
// JSON; see EntryInsert for the _id contract).
func NewInsert(documentJSON []byte) Entry {
	return Entry{Type: EntryInsert, Payload: documentJSON}
}

// NewDelete builds a Delete entry for one _id.
func NewDelete(id string) Entry {
	return Entry{Type: EntryDelete, Payload: []byte(id)}
}

// NewUpdate builds an Update entry (§6.2: UPDATE = DELETE + INSERT).
func NewUpdate(id string, documentJSON []byte) Entry {
	p := make([]byte, 2+len(id)+len(documentJSON))
	binary.LittleEndian.PutUint16(p, uint16(len(id)))
	copy(p[2:], id)
	copy(p[2+len(id):], documentJSON)
	return Entry{Type: EntryUpdate, Payload: p}
}

// NewSegmentFlushed builds the flush marker covering all records with
// seq_no <= upTo.
func NewSegmentFlushed(upTo types.SeqNo) Entry {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint64(p, uint64(upTo))
	return Entry{Type: EntrySegmentFlushed, Payload: p}
}

// NewMetadataTick builds a metadata tick entry.
func NewMetadataTick(blob []byte) Entry {
	return Entry{Type: EntryMetadataTick, Payload: blob}
}

// InsertDoc returns the Insert payload (the document JSON).
func (e Entry) InsertDoc() []byte { return e.Payload }

// DeleteID returns the Delete payload as the deleted _id.
func (e Entry) DeleteID() string { return string(e.Payload) }

// UpdateID returns the _id targeted by an Update entry.
func (e Entry) UpdateID() string {
	if len(e.Payload) < 2 {
		return ""
	}
	n := int(binary.LittleEndian.Uint16(e.Payload))
	if 2+n > len(e.Payload) {
		return ""
	}
	return string(e.Payload[2 : 2+n])
}

// UpdateDoc returns the replacement document of an Update entry.
func (e Entry) UpdateDoc() []byte {
	if len(e.Payload) < 2 {
		return nil
	}
	n := int(binary.LittleEndian.Uint16(e.Payload))
	if 2+n > len(e.Payload) {
		return nil
	}
	return e.Payload[2+n:]
}

// FlushUpTo decodes the SegmentFlushed marker payload: the highest seq_no
// whose data is durable in segments.
func (e Entry) FlushUpTo() types.SeqNo {
	if len(e.Payload) != 8 {
		return 0
	}
	return types.SeqNo(binary.LittleEndian.Uint64(e.Payload))
}

// WAL is the storage-engine write-ahead-log contract (design 01 §10). M0
// ships LocalWAL; M2 swaps in a Raft-backed implementation behind the same
// record semantics.
type WAL interface {
	// Append guarantees group-commit semantics: once it returns, every
	// entry is fsync-durable and the returned seq_no is the highest seq
	// assigned within this call's entries.
	Append(ctx context.Context, entries ...Entry) (types.SeqNo, error)
	// Replay feeds every intact record with SeqNo >= from to fn in seq
	// order; a corrupt or torn record ends its segment file.
	Replay(ctx context.Context, from types.SeqNo, fn func(Entry) error) error
	// TruncateBefore garbage-collects segment files whose records all
	// precede seq. Callers must first append a SegmentFlushed marker
	// covering seq (design 01 §5) so recovery keeps skipping the flushed
	// prefix; the active segment file is always retained.
	TruncateBefore(seq types.SeqNo) error
}

// Group-commit and segment-rolling defaults (design 01 §3.1/§3.2).
const (
	DefaultMaxSegmentSize int64 = 256 << 20 // roll to a new {seq}.log above this
	DefaultCommitWindow         = 2 * time.Millisecond
	DefaultCommitBytes          = 4 << 20
)

type options struct {
	maxSegmentSize int64
	commitWindow   time.Duration
	commitBytes    int
}

func defaultOptions() options {
	return options{
		maxSegmentSize: DefaultMaxSegmentSize,
		commitWindow:   DefaultCommitWindow,
		commitBytes:    DefaultCommitBytes,
	}
}

// Option configures a LocalWAL.
type Option func(*options)

// WithMaxSegmentSize overrides the segment rolling threshold.
func WithMaxSegmentSize(bytes int64) Option {
	return func(o *options) {
		if bytes > 0 {
			o.maxSegmentSize = bytes
		}
	}
}

// WithCommitWindow overrides the group-commit time window.
func WithCommitWindow(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.commitWindow = d
		}
	}
}

// WithCommitBytes overrides the group-commit byte threshold.
func WithCommitBytes(b int) Option {
	return func(o *options) {
		if b > 0 {
			o.commitBytes = b
		}
	}
}
