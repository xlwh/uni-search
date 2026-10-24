package coordinator

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// @spec: 02_spec.md#6.8 F5-T01..T06

func TestF5_AsyncInsertBatchSizeTrigger(t *testing.T) {
	var flushCount atomic.Int64
	var flushDocs atomic.Int64
	flusher := func(key AsyncInsertGroupKey, docs [][]byte) error {
		flushCount.Add(1)
		flushDocs.Add(int64(len(docs)))
		return nil
	}
	q := NewAsyncInsertQueue(AsyncInsertConfig{
		MaxDataSize: 100, // 100 bytes triggers flush
		MaxWaitMs:   10 * time.Second,
	}, flusher)
	defer q.Stop()

	key := AsyncInsertGroupKey{Index: "test", Format: "json"}
	// Submit 4 docs (80 bytes) without waiting, then the 5th triggers flush.
	for i := 0; i < 4; i++ {
		err := q.Submit(key, make([]byte, 20), false)
		require.NoError(t, err)
	}
	// 5th doc (100 bytes total) triggers size-based flush.
	err := q.Submit(key, make([]byte, 20), true)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	require.GreaterOrEqual(t, flushCount.Load(), int64(1))
	require.GreaterOrEqual(t, flushDocs.Load(), int64(5))
}

func TestF5_AsyncInsertTimeoutTrigger(t *testing.T) {
	var flushCount atomic.Int64
	flusher := func(key AsyncInsertGroupKey, docs [][]byte) error {
		flushCount.Add(1)
		return nil
	}
	q := NewAsyncInsertQueue(AsyncInsertConfig{
		MaxDataSize: 1 << 20, // 1MB - won't trigger by size
		MaxWaitMs:   50 * time.Millisecond,
	}, flusher)
	defer q.Stop()

	key := AsyncInsertGroupKey{Index: "test"}
	err := q.Submit(key, []byte("hello"), true)
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)
	require.GreaterOrEqual(t, flushCount.Load(), int64(1))
}

func TestF5_AsyncInsertWaitTrueErrorPropagation(t *testing.T) {
	testErr := errors.New("flush failed")
	flusher := func(key AsyncInsertGroupKey, docs [][]byte) error {
		return testErr
	}
	q := NewAsyncInsertQueue(AsyncInsertConfig{
		MaxDataSize: 10,
		MaxWaitMs:   10 * time.Second,
	}, flusher)
	defer q.Stop()

	key := AsyncInsertGroupKey{Index: "test"}
	// First submit: 5 bytes < 10, no flush yet, don't wait.
	err := q.Submit(key, []byte("hello"), false)
	require.NoError(t, err)
	// Second submit: 10 bytes >= 10, triggers flush, wait for result.
	err = q.Submit(key, []byte("world"), true)
	require.Error(t, err)
	require.Equal(t, testErr, err)
}

func TestF5_AsyncInsertWaitFalseNoBlock(t *testing.T) {
	var flushCount atomic.Int64
	flusher := func(key AsyncInsertGroupKey, docs [][]byte) error {
		flushCount.Add(1)
		return nil
	}
	q := NewAsyncInsertQueue(AsyncInsertConfig{
		MaxDataSize: 1 << 20,
		MaxWaitMs:   100 * time.Millisecond,
	}, flusher)
	defer q.Stop()

	key := AsyncInsertGroupKey{Index: "test"}
	start := time.Now()
	err := q.Submit(key, []byte("hello"), false)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Less(t, elapsed, 50*time.Millisecond, "wait=false should return immediately")

	time.Sleep(200 * time.Millisecond)
	require.GreaterOrEqual(t, flushCount.Load(), int64(1))
}

func TestF5_AsyncInsertQueueDepthLimit(t *testing.T) {
	flusher := func(key AsyncInsertGroupKey, docs [][]byte) error { return nil }
	q := NewAsyncInsertQueue(AsyncInsertConfig{
		MaxDataSize:   1 << 20,
		MaxWaitMs:     10 * time.Second,
		MaxQueueDepth: 5,
	}, flusher)
	defer q.Stop()

	key := AsyncInsertGroupKey{Index: "test"}
	for i := 0; i < 5; i++ {
		err := q.Submit(key, []byte("x"), false)
		require.NoError(t, err)
	}
	// 6th should fail (queue full)
	err := q.Submit(key, []byte("x"), false)
	require.Error(t, err)
}

func TestF5_AsyncInsertConcurrentSafe(t *testing.T) {
	var flushCount atomic.Int64
	flusher := func(key AsyncInsertGroupKey, docs [][]byte) error {
		flushCount.Add(1)
		return nil
	}
	q := NewAsyncInsertQueue(AsyncInsertConfig{
		MaxDataSize: 100,
		MaxWaitMs:   50 * time.Millisecond,
	}, flusher)
	defer q.Stop()

	key := AsyncInsertGroupKey{Index: "test"}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = q.Submit(key, []byte("doc"), false)
		}()
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond)
	require.GreaterOrEqual(t, flushCount.Load(), int64(1))
}
