package objstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// LogStore implements the WAL double-write strategy (D2: NVMe first +
// S3 async追赶). Writes go to local NVMe first (sync fsync), then
// upload to S3 asynchronously. Followers pull from S3 for追赶.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.2
type LogStore struct {
	mu       sync.Mutex
	localDir string
	s3       S3Client
	shardID  types.ShardID
	gen      uint64
	seq      uint64
	uploadCh chan *logSegment
	closed   bool
	wg       sync.WaitGroup
}

type logSegment struct {
	seq  uint64
	data []byte
	key  string // S3 key: wal/{shard}/{gen}/{seq}.log
}

// NewLogStore creates a double-write LogStore.
func NewLogStore(localDir string, s3 S3Client, shardID types.ShardID, gen uint64) *LogStore {
	ls := &LogStore{
		localDir: localDir,
		s3:       s3,
		shardID:  shardID,
		gen:      gen,
		uploadCh: make(chan *logSegment, 128),
	}
	ls.wg.Add(1)
	go func() {
		defer ls.wg.Done()
		ls.uploadLoop()
	}()
	return ls
}

// Append writes a log entry to local NVMe first, then queues async
// S3 upload. Returns after NVMe fsync confirms.
func (ls *LogStore) Append(ctx context.Context, data []byte) (uint64, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return 0, fmt.Errorf("objstore: logstore closed")
	}
	ls.seq++
	seq := ls.seq

	// 1. Write to local NVMe (sync fsync).
	// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#3.1
	localPath := filepath.Join(ls.localDir,
		fmt.Sprintf("wal/%d/%d/%d.log", ls.shardID, ls.gen, seq))
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return 0, fmt.Errorf("objstore: create wal dir: %w", err)
	}
	if err := os.WriteFile(localPath, data, 0o644); err != nil {
		return 0, fmt.Errorf("objstore: nvme write: %w", err)
	}
	// fsync the file for durability.
	if f, err := os.Open(localPath); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}

	// 2. Queue async S3 upload.
	s3Key := fmt.Sprintf("wal/%d/%d/%d.log", ls.shardID, ls.gen, seq)
	seg := &logSegment{seq: seq, data: data, key: s3Key}
	select {
	case ls.uploadCh <- seg:
	default:
		// Channel full: synchronous upload to avoid data loss.
		// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B2
		ls.s3.PutObject(ctx, s3Key, data)
	}

	return seq, nil
}

// uploadLoop processes async S3 uploads. When the channel is
// closed, the range loop drains remaining buffered items before
// exiting naturally — no early return on ls.closed.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B2
func (ls *LogStore) uploadLoop() {
	for seg := range ls.uploadCh {
		ls.s3.PutObject(context.Background(), seg.key, seg.data)
	}
}

// PullFromS3 fetches log segments from S3 for follower追赶.
func (ls *LogStore) PullFromS3(ctx context.Context, fromSeq uint64) ([][]byte, error) {
	prefix := fmt.Sprintf("wal/%d/%d/", ls.shardID, ls.gen)
	keys, err := ls.s3.ListObjects(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("objstore: list S3 logs: %w", err)
	}
	var segments [][]byte
	for _, key := range keys {
		rc, err := ls.s3.GetObject(ctx, key)
		if err != nil {
			continue
		}
		buf := make([]byte, 0)
		tmp := make([]byte, 4096)
		for {
			n, err := rc.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				break
			}
		}
		rc.Close()
		segments = append(segments, buf)
	}
	return segments, nil
}

// Close shuts down the async upload loop and waits for it to drain.
func (ls *LogStore) Close() {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return
	}
	ls.closed = true
	close(ls.uploadCh)
	ls.mu.Unlock()
	ls.wg.Wait() // wait for uploadLoop to drain remaining items
}
