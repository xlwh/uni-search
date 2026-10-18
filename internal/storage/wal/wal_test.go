package wal

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/pkg/types"
)

func segPath(dataDir string, shard types.ShardID, seq uint64) string {
	return filepath.Join(dataDir, "wal", fmt.Sprint(uint64(shard)), fmt.Sprintf("%d%s", seq, logExt))
}

func insertDoc(i int) []byte {
	return []byte(fmt.Sprintf(`{"_id":"doc-%d","n":%d}`, i, i))
}

// recordOffsets parses a segment file per the §3.1 wire format and returns
// the start offset of every intact record — doubling as a format check.
func recordOffsets(t *testing.T, path string) []int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(b), headerSize)
	require.Equal(t, walMagic, string(b[:8]))
	var offs []int64
	off := int64(headerSize)
	for off+4 <= int64(len(b)) {
		rlen := int64(binary.LittleEndian.Uint32(b[off : off+4]))
		if rlen < recordOverhead || off+4+rlen > int64(len(b)) {
			break
		}
		offs = append(offs, off)
		off += 4 + rlen
	}
	return offs
}

func replayAll(t *testing.T, w *LocalWAL) []Entry {
	t.Helper()
	var got []Entry
	require.NoError(t, w.Replay(context.Background(), 0, func(e Entry) error {
		got = append(got, e)
		return nil
	}))
	return got
}

// TestR5_CrashRecovery covers spec R5 scenario 2: after injecting a bad
// CRC into the WAL tail (simulating a crash mid-commit), reopening replays
// every record before the corruption, drops the corrupt tail without
// panicking, and resumes seq assignment exactly after the last intact
// record. Trailing garbage and torn records get the same treatment.
func TestR5_CrashRecovery(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 42)
	require.NoError(t, err)

	const n = 10
	for i := 1; i <= n; i++ {
		seq, err := w.Append(context.Background(), NewInsert(insertDoc(i)))
		require.NoError(t, err)
		assert.EqualValues(t, i, seq) // seq numbers start at 1 and are durable on return
	}
	require.NoError(t, w.Close())

	path := segPath(dir, 42, 1)
	require.FileExists(t, path) // data-dir/wal/{shard_id}/{seq}.log layout
	offs := recordOffsets(t, path)
	require.Len(t, offs, n)

	// Inject a bad CRC at record 6: everything from there on was never
	// committed and must be truncated on reopen.
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xFF, 0xFF, 0xFF, 0xFF}, offs[5]+4) // crc32c field
	require.NoError(t, err)
	require.NoError(t, f.Close())

	w2, err := Open(dir, 42)
	require.NoError(t, err)
	got := replayAll(t, w2)
	require.Len(t, got, 5) // records 1..5 recovered; 6..10 discarded
	for i, e := range got {
		assert.EqualValues(t, i+1, e.SeqNo)
		assert.Equal(t, EntryInsert, e.Type)
		assert.Equal(t, insertDoc(i+1), e.Payload) // zstd round-trips byte-exact
	}

	// The WAL continues after the truncation point: the recovered seq 5 is
	// the last committed one, so the next append takes seq 6.
	seq, err := w2.Append(context.Background(), NewInsert(insertDoc(99)))
	require.NoError(t, err)
	assert.EqualValues(t, 6, seq)
	require.NoError(t, w2.Close())

	// Second reopen: recovered prefix plus the new record, no duplicates.
	w3, err := Open(dir, 42)
	require.NoError(t, err)
	got = replayAll(t, w3)
	require.Len(t, got, 6)
	assert.EqualValues(t, 6, got[5].SeqNo)
	assert.Equal(t, insertDoc(99), got[5].Payload)
	require.NoError(t, w3.Close())

	// Trailing garbage after a clean prefix is dropped without panic and
	// the file stays writable.
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = fh.Write([]byte("garbage bytes that are definitely not a wal record"))
	require.NoError(t, err)
	require.NoError(t, fh.Close())

	w4, err := Open(dir, 42)
	require.NoError(t, err)
	got = replayAll(t, w4)
	require.Len(t, got, 6) // garbage tail discarded
	seq, err = w4.Append(context.Background(), NewInsert(insertDoc(100)))
	require.NoError(t, err)
	assert.EqualValues(t, 7, seq)
	require.NoError(t, w4.Close())

	// A torn record (half of a valid append) is treated as uncommitted too.
	fh, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = fh.Write([]byte{0x40, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03}) // len=64 then 3 body bytes
	require.NoError(t, err)
	require.NoError(t, fh.Close())

	w5, err := Open(dir, 42)
	require.NoError(t, err)
	got = replayAll(t, w5)
	require.Len(t, got, 7)
	require.NoError(t, w5.Close())
}

// TestR5_SegmentFlushedSkip verifies the SegmentFlushed marker semantics:
// FlushedUpTo reports the last marker, replay after that point skips the
// already-flushed prefix, and TruncateBefore GC only drops segment files
// fully covered by the flush.
func TestR5_SegmentFlushedSkip(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 3, WithMaxSegmentSize(128))
	require.NoError(t, err)

	const flushed = 5
	// Pre-flush inserts: seq 1..5.
	for i := 1; i <= flushed; i++ {
		_, err := w.Append(context.Background(), NewInsert(insertDoc(i)))
		require.NoError(t, err)
	}
	// Segment publish made records 1..5 durable; the flush worker appends
	// the marker (seq 6) before truncating (design 01 §5).
	_, err = w.Append(context.Background(), NewSegmentFlushed(flushed))
	require.NoError(t, err)
	// Post-flush inserts: seq 7..10 — the recovery path must replay exactly
	// these into the rebuilt memtable.
	for i := 6; i <= 10; i++ {
		_, err := w.Append(context.Background(), NewInsert(insertDoc(i)))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())

	// 11 records with tiny segments must have rolled into several files.
	entries, err := os.ReadDir(filepath.Join(dir, "wal", "3"))
	require.NoError(t, err)
	filesBefore := 0
	for _, e := range entries {
		if !e.IsDir() {
			filesBefore++
		}
	}
	require.GreaterOrEqual(t, filesBefore, 3)

	// Replay spans all segment files in seq order.
	w2, err := Open(dir, 3)
	require.NoError(t, err)
	require.EqualValues(t, flushed, w2.FlushedUpTo())
	got := replayAll(t, w2)
	require.Len(t, got, 11) // 10 inserts + 1 marker
	for i, e := range got {
		if i < flushed {
			assert.EqualValues(t, i+1, e.SeqNo)
			assert.Equal(t, EntryInsert, e.Type)
		}
	}
	assert.Equal(t, EntrySegmentFlushed, got[flushed].Type)
	assert.EqualValues(t, flushed, got[flushed].FlushUpTo())

	// Recovery path: replaying after the flushed prefix yields the marker
	// plus only the five post-flush inserts (the flushed prefix is
	// skipped, so the rebuilt memtable stays deduplicated).
	var post []Entry
	require.NoError(t, w2.Replay(context.Background(), w2.FlushedUpTo()+1, func(e Entry) error {
		post = append(post, e)
		return nil
	}))
	require.Len(t, post, 6) // marker + inserts doc-6..doc-10
	inserts := 0
	for _, e := range post {
		if e.Type == EntryInsert {
			inserts++
			assert.Greater(t, e.SeqNo, types.SeqNo(flushed))
		}
	}
	assert.Equal(t, 5, inserts)

	// TruncateBefore GC: files fully covered by the flush go away, the
	// active file (carrying the marker) always survives.
	require.NoError(t, w2.TruncateBefore(flushed+1))
	entries, err = os.ReadDir(filepath.Join(dir, "wal", "3"))
	require.NoError(t, err)
	filesAfter := 0
	for _, e := range entries {
		if !e.IsDir() {
			filesAfter++
		}
	}
	assert.Less(t, filesAfter, filesBefore)
	assert.GreaterOrEqual(t, filesAfter, 1)
	require.NoError(t, w2.Close())

	// The marker survives GC, so a restart still skips the flushed prefix.
	w3, err := Open(dir, 3)
	require.NoError(t, err)
	assert.EqualValues(t, flushed, w3.FlushedUpTo())
	require.NoError(t, w3.Close())
}

// TestR5_GroupCommit covers spec R5 scenario 3: 100 goroutines append one
// entry each concurrently; every returned seq is unique and the on-disk
// records are contiguous 1..100 in seq order. The enlarged commit window
// makes the batching deterministic: all 100 entries land in a single
// group-commit batch, proven by exactly one fsync.
func TestR5_GroupCommit(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 9, WithCommitWindow(200*time.Millisecond))
	require.NoError(t, err)

	const goroutines = 100
	results := make([]types.SeqNo, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			seq, err := w.Append(context.Background(), NewInsert(insertDoc(i)))
			results[i], errs[i] = seq, err
		}(g)
	}
	close(start)
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		require.NoError(t, errs[i])
	}
	sorted := append([]types.SeqNo(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for i := 0; i < goroutines; i++ {
		assert.EqualValues(t, i+1, sorted[i], "returned seq numbers must be distinct and monotonic")
	}

	// One commit-leader fsync for the whole group: batching works.
	assert.EqualValues(t, 100, w.commitSeq)
	assert.EqualValues(t, 1, w.syncCount.Load())

	// On-disk records are durable and contiguous.
	got := replayAll(t, w)
	require.Len(t, got, goroutines)
	for i, e := range got {
		assert.EqualValues(t, i+1, e.SeqNo)
	}

	// Closed WAL rejects further appends.
	require.NoError(t, w.Close())
	_, err = w.Append(context.Background(), NewInsert(insertDoc(999)))
	assert.ErrorIs(t, err, ErrClosed)
}

// TestR5_EntryEncoding pins the payload layouts of the §3.1 entry types.
func TestR5_EntryEncoding(t *testing.T) {
	doc := []byte(`{"_id":"a","x":1}`)

	ins := NewInsert(doc)
	assert.Equal(t, EntryInsert, ins.Type)
	assert.Equal(t, doc, ins.InsertDoc())

	del := NewDelete("doc-1")
	assert.Equal(t, EntryDelete, del.Type)
	assert.Equal(t, "doc-1", del.DeleteID())

	upd := NewUpdate("doc-1", doc)
	assert.Equal(t, EntryUpdate, upd.Type)
	assert.Equal(t, "doc-1", upd.UpdateID())
	assert.Equal(t, doc, upd.UpdateDoc())

	marker := NewSegmentFlushed(42)
	assert.Equal(t, EntrySegmentFlushed, marker.Type)
	assert.EqualValues(t, 42, marker.FlushUpTo())

	tick := NewMetadataTick([]byte(`{"settings":1}`))
	assert.Equal(t, EntryMetadataTick, tick.Type)

	// The framed payloads survive an Append→Replay round trip byte-exact.
	dir := t.TempDir()
	w, err := Open(dir, 1)
	require.NoError(t, err)
	_, err = w.Append(context.Background(), ins, del, upd, marker, tick)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	w2, err := Open(dir, 1)
	require.NoError(t, err)
	defer w2.Close()
	got := replayAll(t, w2)
	require.Len(t, got, 5)
	for i := range got {
		assert.EqualValues(t, i+1, got[i].SeqNo)
	}
	assert.Equal(t, doc, got[0].InsertDoc())
	assert.Equal(t, "doc-1", got[1].DeleteID())
	assert.Equal(t, "doc-1", got[2].UpdateID())
	assert.Equal(t, doc, got[2].UpdateDoc())
	assert.EqualValues(t, 42, got[3].FlushUpTo())
	assert.Equal(t, []byte(`{"settings":1}`), got[4].Payload)
}

// TestR5_EmptyAndReopen exercises Open on a fresh directory and an
// empty-but-present segment file.
func TestR5_EmptyAndReopen(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 11)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// Reopen with a zero-length segment file (crash before header write).
	segDir := filepath.Join(dir, "wal", "11")
	require.NoError(t, os.WriteFile(filepath.Join(segDir, "1.log"), nil, 0o644))
	w, err = Open(dir, 11)
	require.NoError(t, err)
	seq, err := w.Append(context.Background(), NewInsert(insertDoc(1)))
	require.NoError(t, err)
	assert.EqualValues(t, 1, seq)
	require.NoError(t, w.Close())

	w2, err := Open(dir, 11)
	require.NoError(t, err)
	defer w2.Close()
	require.Len(t, replayAll(t, w2), 1)
}

// FuzzWALReplay feeds arbitrary byte streams as a WAL segment file and
// requires the whole open→replay→append→close→reopen cycle to be
// crash-consistent and panic-free (task T5.4): recovery either rejects the
// input gracefully or truncates to the last intact record, and any record
// that replay counted stays readable after appending more data.
func FuzzWALReplay(f *testing.F) {
	validHeader := func(shard uint64, tail ...byte) []byte {
		h := make([]byte, headerSize)
		copy(h[:8], walMagic)
		binary.LittleEndian.PutUint64(h[8:], shard)
		return append(h, tail...)
	}
	f.Add([]byte(nil))                                                                                                    // empty file
	f.Add([]byte("totally not a wal file"))                                                                               // garbage, no header
	f.Add(validHeader(7))                                                                                                 // valid header only
	f.Add(validHeader(7, 0xEE, 0xFF, 0x00, 0x01, 0x02))                                                                   // header + garbage record bytes
	f.Add(validHeader(7, 0x40, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03))                                                       // header + torn record
	f.Add(validHeader(7, 0x0D, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xAA, 0xBB)) // minimal-length record, bad crc
	f.Add(validHeader(99))                                                                                                // well-formed header of another shard: rejected

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		segDir := filepath.Join(dir, "wal", "7")
		if err := os.MkdirAll(segDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(segDir, "1.log"), data, 0o644); err != nil {
			t.Fatal(err)
		}

		w, err := Open(dir, 7)
		if err != nil {
			return // non-tail corruption is a clean rejection, not a panic
		}
		count := 0
		if err := w.Replay(context.Background(), 0, func(Entry) error { count++; return nil }); err != nil {
			w.Close()
			t.Fatalf("replay on fuzzed wal: %v", err)
		}
		if _, err := w.Append(context.Background(), NewInsert([]byte(`{"_id":"fuzz"}`))); err != nil {
			w.Close()
			t.Fatalf("append after recovery: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		w2, err := Open(dir, 7)
		if err != nil {
			t.Fatalf("reopen after recovery: %v", err)
		}
		defer w2.Close()
		count2 := 0
		if err := w2.Replay(context.Background(), 0, func(Entry) error { count2++; return nil }); err != nil {
			t.Fatalf("replay after reopen: %v", err)
		}
		if count2 != count+1 {
			t.Fatalf("crash-consistency violated: replayed %d records before append, %d after reopen (want %d)",
				count, count2, count+1)
		}
	})
}
