package objstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B2
func TestLogStore_AppendChannelFull(t *testing.T) {
	s3 := NewMemS3Client()
	ls := NewLogStore(t.TempDir(), s3, types.ShardID(1), 1)

	// Append more items than channel capacity (128) to force
	// the synchronous upload path in the default branch.
	const n = 200
	for i := 0; i < n; i++ {
		_, err := ls.Append(context.Background(), []byte(fmt.Sprintf("log-%d", i)))
		require.NoError(t, err, "Append should not fail even when channel is full")
	}

	ls.Close()

	// All n items should eventually be in S3 — none should be dropped.
	// The synchronous path ensures items beyond channel capacity are
	// uploaded immediately, not silently discarded.
	keys, err := s3.ListObjects(context.Background(), "wal/1/1/")
	require.NoError(t, err)
	assert.Equal(t, n, len(keys), "all %d segments must be uploaded, none silently dropped", n)
}

// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#B2
func TestLogStore_NoSilentDrop(t *testing.T) {
	s3 := NewMemS3Client()
	// Use a tiny channel to force overflow quickly.
	ls := &LogStore{
		localDir: t.TempDir(),
		s3:       s3,
		shardID:  types.ShardID(1),
		gen:      1,
		uploadCh: make(chan *logSegment, 2), // capacity 2
	}
	ls.wg.Add(1)
	go func() {
		defer ls.wg.Done()
		ls.uploadLoop()
	}()

	// Rapidly append 10 items — far exceeding channel capacity.
	for i := 0; i < 10; i++ {
		_, err := ls.Append(context.Background(), []byte("data"))
		require.NoError(t, err, "Append must not drop data when channel is full")
	}

	ls.Close()

	// Wait briefly for async uploads to settle, then verify.
	time.Sleep(100 * time.Millisecond)
	keys, err := s3.ListObjects(context.Background(), "wal/1/1/")
	require.NoError(t, err)
	assert.Equal(t, 10, len(keys), "all 10 segments must be uploaded, none silently dropped")
}
