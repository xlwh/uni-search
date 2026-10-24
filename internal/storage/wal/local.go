package wal

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/unisearch-db/unisearch/pkg/types"
)

const (
	walMagic       = "USWAL01\x00" // 8-byte segment header magic (design 01 §3.1)
	headerSize     = 16            // magic(8) + shard_id u64
	recordOverhead = 13            // crc(4) + seq_no(8) + entry_type(1)
	logExt         = ".log"
)

// crcTable is the hardware-accelerated Castagnoli table used for record
// checksums (design 01 §3.1).
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// segmentFile tracks one on-disk segment for rolling and TruncateBefore.
// minSeq/maxSeq span the records the file (still) contains; an empty
// segment has maxSeq 0.
type segmentFile struct {
	seq    uint64
	path   string
	minSeq types.SeqNo
	maxSeq types.SeqNo
}

// LocalWAL is the M0 single-node WAL: an ordered sequence of segment files
// in <data-dir>/wal/{shard_id}/{seq}.log, appended through a group-commit
// loop. One background goroutine (the commit leader) drains queued
// entries: it encodes the whole batch, appends to the active segment
// (rolling at maxSegmentSize), fsyncs once and broadcasts durability to
// every waiting writer.
type LocalWAL struct {
	dir     string
	shardID types.ShardID
	opts    options

	zstdEnc *zstd.Encoder
	zstdDec *zstd.Decoder

	mu        sync.Mutex
	cond      *sync.Cond // broadcast on commitSeq advance or commitErr change
	pending   []Entry    // assigned seqs, not yet durable
	pendBytes int
	nextSeq   types.SeqNo
	commitSeq types.SeqNo // last durable seq
	failSeq   types.SeqNo // last seq of the most recently failed batch
	commitErr error       // error of that failed batch (commitSeq < seq <= failSeq)
	closed    bool

	flushedUpTo types.SeqNo // highest SegmentFlushed marker payload

	notify   chan struct{} // wakes the commit loop (cap 1, coalesced)
	quit     chan struct{} // closed by Close: drain pending and exit
	loopDone chan struct{}

	file     *os.File
	fileSeq  uint64
	fileSize int64
	segments []segmentFile // ascending file seq; last is active

	syncCount atomic.Int64 // fsyncs performed (test/observability hook)
}

// compile-time interface check.
var _ WAL = (*LocalWAL)(nil)

// Open creates or recovers the WAL for one shard under dataDir. Recovery
// scans every segment file, CRC-verifies each record, truncates a torn or
// corrupt tail (§3.1: a bad record means everything after it was never
// committed) and resumes seq assignment after the last intact record.
// A newest segment whose header is missing or invalid is dropped entirely
// and reused; a corrupt header in a non-tail file, or a header carrying a
// different shard id, is an error rather than silent data loss.
func Open(dataDir string, shardID types.ShardID, opts ...Option) (*LocalWAL, error) {
	o := defaultOptions()
	for _, apply := range opts {
		apply(&o)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	// Cap decompression so a garbage frame can never request absurd memory
	// on the recovery path (record payloads are bounded by segment size).
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(1<<30))
	if err != nil {
		return nil, err
	}
	w := &LocalWAL{
		dir:      filepath.Join(dataDir, "wal", strconv.FormatUint(uint64(shardID), 10)),
		shardID:  shardID,
		opts:     o,
		nextSeq:  1,
		notify:   make(chan struct{}, 1),
		quit:     make(chan struct{}),
		loopDone: make(chan struct{}),
		zstdEnc:  enc,
		zstdDec:  dec,
	}
	w.cond = sync.NewCond(&w.mu)
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		dec.Close()
		return nil, err
	}
	if err := w.recoverSegments(); err != nil {
		dec.Close()
		return nil, err
	}
	go w.commitLoop()
	return w, nil
}

// recoverSegments scans existing segment files (ascending), truncates bad
// tails and opens the newest one as the append target.
func (w *LocalWAL) recoverSegments() error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	var seqs []uint64
	bySeq := make(map[uint64]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), logExt) {
			continue // foreign files (e.g. .DS_Store) are none of the WAL's business
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(e.Name(), logExt), 10, 64)
		if err != nil {
			continue // not a {seq}.log segment
		}
		seqs = append(seqs, n)
		bySeq[n] = filepath.Join(w.dir, e.Name())
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	var lastSize int64
	for i, seq := range seqs {
		seg := segmentFile{seq: seq, path: bySeq[seq]}
		size, err := w.scanSegment(&seg, i == len(seqs)-1)
		if err != nil {
			return err
		}
		w.segments = append(w.segments, seg)
		if seg.maxSeq >= w.nextSeq {
			w.nextSeq = seg.maxSeq + 1
		}
		lastSize = size
	}

	if len(w.segments) == 0 {
		return w.createFile(1)
	}
	last := w.segments[len(w.segments)-1]
	return w.openActive(last, lastSize)
}

// scanSegment verifies one segment file end to end. It records the intact
// record range into seg and truncates a bad tail in place. It returns the
// valid file size (0 when the header itself was invalid and dropped).
func (w *LocalWAL) scanSegment(seg *segmentFile, isLast bool) (int64, error) {
	// Read-write: recovery truncates corrupt tails, which needs write
	// access on every supported platform.
	f, err := os.OpenFile(seg.path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()

	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return w.dropTornHeader(seg, f, isLast, err)
	}
	if string(hdr[:8]) != walMagic {
		return w.dropTornHeader(seg, f, isLast, nil)
	}
	if binary.LittleEndian.Uint64(hdr[8:]) != uint64(w.shardID) {
		// A well-formed header carrying another shard's id is foreign
		// data, not a torn tail; refuse instead of destroying it.
		return 0, fmt.Errorf("wal: segment %s belongs to shard %d, expected %d",
			seg.path, binary.LittleEndian.Uint64(hdr[8:]), w.shardID)
	}

	r := bufio.NewReader(f)
	off := int64(headerSize)
	var lenBuf [4]byte
	for off+4 <= size {
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			break // torn len prefix
		}
		rlen := int64(binary.LittleEndian.Uint32(lenBuf[:]))
		if rlen < recordOverhead || off+4+rlen > size {
			break // impossible/corrupt length: uncommitted tail
		}
		body := make([]byte, rlen)
		if _, err := io.ReadFull(r, body); err != nil {
			break // torn record
		}
		if crc32.Checksum(body[4:], crcTable) != binary.LittleEndian.Uint32(body[0:4]) {
			break // CRC mismatch: uncommitted tail
		}
		typ := EntryType(body[12])
		if typ > EntryMetadataTick {
			break // unknown record type from a newer/foreign format
		}
		payload, err := w.zstdDec.DecodeAll(body[13:], nil)
		if err != nil {
			break // undecodable payload
		}
		seq := types.SeqNo(binary.LittleEndian.Uint64(body[4:12]))
		if seg.maxSeq == 0 {
			seg.minSeq = seq
		}
		if seq > seg.maxSeq {
			seg.maxSeq = seq
		}
		if typ == EntrySegmentFlushed {
			if v, ok := decodeFlushUpTo(payload); ok && v > w.flushedUpTo {
				w.flushedUpTo = v
			}
		}
		off += 4 + rlen
	}
	if off < size {
		if err := f.Truncate(off); err != nil {
			return 0, err
		}
	}
	return off, nil
}

// dropTornHeader handles a segment whose header is absent, torn or invalid
// magic. Only the newest segment gets this leniency: it means the file was
// created but its header never committed. Non-tail corruption is an error.
func (w *LocalWAL) dropTornHeader(seg *segmentFile, f *os.File, isLast bool, cause error) (int64, error) {
	if !isLast {
		if cause != nil {
			return 0, fmt.Errorf("wal: corrupt segment header in non-tail file %s: %w", seg.path, cause)
		}
		return 0, fmt.Errorf("wal: invalid segment header in non-tail file %s", seg.path)
	}
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	seg.minSeq, seg.maxSeq = 0, 0
	return 0, nil
}

// openActive opens the newest scanned segment for appending. A size of 0
// means the header was invalid and dropped, so it is rewritten fresh.
func (w *LocalWAL) openActive(seg segmentFile, size int64) error {
	f, err := os.OpenFile(seg.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if size == 0 {
		if err := writeHeader(f, w.shardID); err != nil {
			f.Close()
			return err
		}
		size = headerSize
	}
	w.file = f
	w.fileSeq = seg.seq
	w.fileSize = size
	return nil
}

// createFile starts a brand-new segment file with a fresh header.
func (w *LocalWAL) createFile(seq uint64) error {
	path := filepath.Join(w.dir, strconv.FormatUint(seq, 10)+logExt)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := writeHeader(f, w.shardID); err != nil {
		f.Close()
		return err
	}
	w.file = f
	w.fileSeq = seq
	w.fileSize = headerSize
	w.segments = append(w.segments, segmentFile{seq: seq, path: path})
	return nil
}

func writeHeader(f *os.File, shard types.ShardID) error {
	hdr := make([]byte, headerSize)
	copy(hdr[:8], walMagic)
	binary.LittleEndian.PutUint64(hdr[8:], uint64(shard))
	_, err := f.Write(hdr)
	return err
}

func decodeFlushUpTo(p []byte) (types.SeqNo, bool) {
	if len(p) != 8 {
		return 0, false
	}
	return types.SeqNo(binary.LittleEndian.Uint64(p)), true
}

// Append queues entries for the next group commit and blocks until its
// batch is fsync-durable (§3.2 wait_for semantics). Seq numbers are
// assigned in queue order, written back into the caller's entries, and
// the returned value is the highest seq of this call's entries. After
// Close it returns ErrClosed. A canceled ctx aborts the wait; the entries
// may still be committed by the leader, so callers must treat ctx
// failure as "unknown durability", not "not written".
func (w *LocalWAL) Append(ctx context.Context, entries ...Entry) (types.SeqNo, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, ErrClosed
	}
	if len(entries) == 0 {
		cur := w.commitSeq
		w.mu.Unlock()
		return cur, nil
	}
	seq := w.nextSeq
	for i := range entries {
		entries[i].SeqNo = seq
		w.pendBytes += len(entries[i].Payload) + 4 + recordOverhead
		seq++
	}
	w.pending = append(w.pending, entries...)
	w.nextSeq = seq
	mySeq := seq - 1
	w.mu.Unlock()

	select {
	case w.notify <- struct{}{}:
	default: // a wake-up is already pending
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if w.commitSeq >= mySeq {
			return mySeq, nil
		}
		if w.commitErr != nil && w.failSeq >= mySeq {
			return 0, w.commitErr
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		// Every event that can settle this wait broadcasts on w.cond; the
		// commit window bounds how long a Wait can last.
		w.cond.Wait()
	}
}

// AppendAsync appends entries without waiting for fsync (design 01 §3.2:
// refresh=false). The commit loop will fsync in the background (within
// the commit window). Returns the assigned SeqNo immediately.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.8
func (w *LocalWAL) AppendAsync(ctx context.Context, entries ...Entry) (types.SeqNo, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, ErrClosed
	}
	if len(entries) == 0 {
		cur := w.commitSeq
		w.mu.Unlock()
		return cur, nil
	}
	seq := w.nextSeq
	for i := range entries {
		entries[i].SeqNo = seq
		w.pendBytes += len(entries[i].Payload) + 4 + recordOverhead
		seq++
	}
	w.pending = append(w.pending, entries...)
	w.nextSeq = seq
	mySeq := seq - 1
	w.mu.Unlock()

	select {
	case w.notify <- struct{}{}:
	default:
	}

	// Return immediately without waiting for fsync.
	_ = mySeq
	return mySeq, nil
}

// commitLoop is the commit leader (§3.2): it waits for queued entries,
// gathers the batch until the time window or byte threshold fires
// (whichever first), then flushes once and broadcasts completion.
func (w *LocalWAL) commitLoop() {
	defer close(w.loopDone)
	for {
		select {
		case <-w.notify:
		case <-w.quit:
			w.flushPending() // final drain: durability for in-flight appends
			return
		}
		if w.opts.commitWindow <= 0 {
			w.flushPending()
			continue
		}
		timer := time.NewTimer(w.opts.commitWindow)
	window:
		for {
			w.mu.Lock()
			full := w.pendBytes >= w.opts.commitBytes
			empty := len(w.pending) == 0
			closed := w.closed
			w.mu.Unlock()
			if full || empty || closed {
				break window
			}
			select {
			case <-timer.C:
				break window
			case <-w.notify: // more entries arrived; recheck thresholds
			case <-w.quit:
				break window
			}
		}
		timer.Stop()
		w.flushPending()
	}
}

// flushPending drains the queue into one batch: encode, roll if the active
// segment would overflow, append, fsync once, publish the new commit
// watermark to all waiting writers.
func (w *LocalWAL) flushPending() error {
	w.mu.Lock()
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return nil
	}
	batch := w.pending
	w.pending = nil
	w.pendBytes = 0
	w.mu.Unlock()

	lastSeq := batch[len(batch)-1].SeqNo
	err := w.writeBatch(batch)

	w.mu.Lock()
	if err != nil {
		w.commitErr = err
		w.failSeq = lastSeq
	} else {
		w.commitErr = nil
		w.commitSeq = lastSeq
		for i := range batch {
			if batch[i].Type == EntrySegmentFlushed {
				if v, ok := decodeFlushUpTo(batch[i].Payload); ok && v > w.flushedUpTo {
					w.flushedUpTo = v
				}
			}
		}
	}
	w.cond.Broadcast()
	w.mu.Unlock()
	return err
}

// writeBatch encodes and persists one batch with a single write+fsync.
// Called only from the commit loop goroutine.
func (w *LocalWAL) writeBatch(batch []Entry) error {
	if w.file == nil {
		return errors.New("wal: no active segment file")
	}
	buf := make([]byte, 0, 64*len(batch))
	for i := range batch {
		buf = w.encodeRecord(buf, batch[i])
	}
	// Roll to {seq+1}.log when the batch would push the active segment
	// past the size cap (a fresh segment always accepts the batch).
	if w.fileSize > headerSize && w.fileSize+int64(len(buf)) > w.opts.maxSegmentSize {
		if err := w.file.Close(); err != nil {
			return err
		}
		if err := w.createFile(w.fileSeq + 1); err != nil {
			return err
		}
	}
	if _, err := w.file.Write(buf); err != nil {
		return err
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	w.syncCount.Add(1)
	w.fileSize += int64(len(buf))
	last := &w.segments[len(w.segments)-1]
	if last.maxSeq == 0 {
		last.minSeq = batch[0].SeqNo
	}
	last.maxSeq = batch[len(batch)-1].SeqNo
	return nil
}

// encodeRecord appends one record to dst in the §3.1 wire format:
// [len u32 | crc32c u32 | seq_no u64 | entry_type u8 | payload zstd], the
// CRC covering seq_no+entry_type+payload (the on-disk record body).
func (w *LocalWAL) encodeRecord(dst []byte, e Entry) []byte {
	comp := w.zstdEnc.EncodeAll(e.Payload, nil)
	total := recordOverhead + len(comp)
	start := len(dst)
	dst = append(dst, make([]byte, 4+total)...)
	binary.LittleEndian.PutUint32(dst[start:], uint32(total))
	binary.LittleEndian.PutUint64(dst[start+8:], uint64(e.SeqNo))
	dst[start+16] = byte(e.Type)
	copy(dst[start+17:], comp)
	binary.LittleEndian.PutUint32(dst[start+4:], crc32.Checksum(dst[start+8:], crcTable))
	return dst
}

// FlushedUpTo returns the highest seq_no covered by the last
// SegmentFlushed marker (0 when none exists). Recovery replays only
// records after this point, skipping the already-flushed prefix.
func (w *LocalWAL) FlushedUpTo() types.SeqNo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushedUpTo
}

// Replay feeds every intact record with SeqNo >= from to fn, across all
// segment files in seq order (a corrupt/torn record ends its file). It
// reflects only durable batches: pending in-memory entries are not
// visible until their fsync completes.
func (w *LocalWAL) Replay(ctx context.Context, from types.SeqNo, fn func(Entry) error) error {
	w.mu.Lock()
	paths := make([]string, len(w.segments))
	for i := range w.segments {
		paths[i] = w.segments[i].path
	}
	w.mu.Unlock()
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.replaySegment(p, from, fn); err != nil {
			return err
		}
	}
	return nil
}

// replaySegment streams one segment file through fn. Corruption stops the
// file quietly (recovery already truncated such tails); genuine I/O
// errors propagate; fn errors stop replay immediately.
func (w *LocalWAL) replaySegment(path string, from types.SeqNo, fn func(Entry) error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // truncated concurrently
		}
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size < headerSize {
		return nil
	}
	r := bufio.NewReader(f)
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(r, hdr); err != nil || string(hdr[:8]) != walMagic {
		return nil
	}
	off := int64(headerSize)
	var lenBuf [4]byte
	for {
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil // clean or torn end
			}
			return err
		}
		rlen := int64(binary.LittleEndian.Uint32(lenBuf[:]))
		if rlen < recordOverhead || off+4+rlen > size {
			return nil // corrupt/torn tail
		}
		body := make([]byte, rlen)
		if _, err := io.ReadFull(r, body); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		if crc32.Checksum(body[4:], crcTable) != binary.LittleEndian.Uint32(body[0:4]) {
			return nil
		}
		typ := EntryType(body[12])
		if typ > EntryMetadataTick {
			return nil
		}
		payload, err := w.zstdDec.DecodeAll(body[13:], nil)
		if err != nil {
			return nil
		}
		off += 4 + rlen
		seq := types.SeqNo(binary.LittleEndian.Uint64(body[4:12]))
		if seq < from {
			continue
		}
		if err := fn(Entry{SeqNo: seq, Type: typ, Payload: payload}); err != nil {
			return err
		}
	}
}

// TruncateBefore removes whole segment files whose records all precede
// seq (§10: flush-marker GC). The active segment is always retained; a
// SegmentFlushed marker covering seq must live in a surviving file, which
// the flush flow guarantees by appending the marker before truncating.
func (w *LocalWAL) TruncateBefore(seq types.SeqNo) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	removed := 0
	for removed < len(w.segments)-1 && w.segments[removed].maxSeq < seq {
		if err := os.Remove(w.segments[removed].path); err != nil && !os.IsNotExist(err) {
			return err
		}
		removed++
	}
	if removed > 0 {
		w.segments = append(w.segments[:0], w.segments[removed:]...)
	}
	return nil
}

// Close flushes any queued entries (waiters get their durability), stops
// the commit loop and closes the active segment file. Idempotent.
func (w *LocalWAL) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	close(w.quit)
	<-w.loopDone

	w.mu.Lock()
	defer w.mu.Unlock()
	w.zstdDec.Close()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
