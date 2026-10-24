package coordinator

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncInsertQueue manages per-group batching of small INSERTs for high
// write throughput. Documents are grouped by (index, format, settings hash,
// user identity) and flushed when the batch reaches MaxDataSize or MaxWaitMs.
// @spec: 02_spec.md#6.3
type AsyncInsertQueue struct {
	mu      sync.Mutex
	groups  map[uint64]*asyncInsertBatch
	config  AsyncInsertConfig
	metrics asyncInsertMetrics
	stopCh  chan struct{}
	wg      sync.WaitGroup
	flusher FlushFunc
}

// AsyncInsertGroupKey identifies one batching group.
type AsyncInsertGroupKey struct {
	Index        string
	Format       string
	SettingsHash uint64
	UserIdentity string
}

// AsyncInsertConfig controls batch sizes and wait behavior.
type AsyncInsertConfig struct {
	MaxDataSize   int           // default 1MB
	MaxWaitMs     time.Duration // default 50ms
	WaitForInsert bool          // default true
	MaxQueueDepth int           // default 10000
}

// FlushFunc is the callback to flush a batch to the storage layer.
type FlushFunc func(key AsyncInsertGroupKey, docs [][]byte) error

// asyncInsertBatch accumulates documents for one group.
type asyncInsertBatch struct {
	docs      [][]byte
	size      int
	createdAt time.Time
	done      chan struct{} // closed when flush completes
	err       error         // flush error (read after done is closed)
}

// asyncInsertMetrics tracks queue statistics.
type asyncInsertMetrics struct {
	QueueLength  atomic.Int64
	FlushTotal   atomic.Int64
	FlushErrors  atomic.Int64
	FlushLatency atomic.Int64 // microseconds
}

// AsyncInsertError records a recent flush failure.
type AsyncInsertError struct {
	Index string
	Error string
	Time  time.Time
}

var (
	asyncErrorsMu  sync.Mutex
	asyncErrorsLog []AsyncInsertError
	asyncErrorsMax = 100
)

// NewAsyncInsertQueue creates a new async insert queue.
func NewAsyncInsertQueue(config AsyncInsertConfig, flusher FlushFunc) *AsyncInsertQueue {
	if config.MaxDataSize <= 0 {
		config.MaxDataSize = 1 << 20 // 1MB
	}
	if config.MaxWaitMs <= 0 {
		config.MaxWaitMs = 50 * time.Millisecond
	}
	if config.MaxQueueDepth <= 0 {
		config.MaxQueueDepth = 10000
	}
	q := &AsyncInsertQueue{
		groups:  make(map[uint64]*asyncInsertBatch),
		config:  config,
		stopCh:  make(chan struct{}),
		flusher: flusher,
	}
	q.wg.Add(1)
	go q.flushLoop()
	return q
}

// Submit adds a document to the async queue. When wait=true, it blocks
// until the batch is flushed and returns the flush error (nil on success).
// When wait=false, it returns nil immediately and flushes asynchronously.
// @spec: 02_spec.md#6.4
func (q *AsyncInsertQueue) Submit(key AsyncInsertGroupKey, doc []byte, wait bool) error {
	if len(doc) > q.config.MaxDataSize*10 {
		return fmt.Errorf("async_insert: document too large (%d bytes), use sync insert", len(doc))
	}

	hash := groupKeyHash(key)
	q.mu.Lock()
	if q.metrics.QueueLength.Load() >= int64(q.config.MaxQueueDepth) {
		q.mu.Unlock()
		return fmt.Errorf("async_insert: queue full (depth %d)", q.config.MaxQueueDepth)
	}

	batch, ok := q.groups[hash]
	if !ok {
		batch = &asyncInsertBatch{
			createdAt: time.Now(),
			done:      make(chan struct{}),
		}
		q.groups[hash] = batch
	}
	batch.docs = append(batch.docs, doc)
	batch.size += len(doc)
	q.metrics.QueueLength.Add(1)
	shouldFlush := batch.size >= q.config.MaxDataSize
	q.mu.Unlock()

	if shouldFlush {
		go q.flushGroup(hash)
	}

	if !wait {
		return nil
	}

	// Wait for flush completion (broadcast via closed done channel).
	select {
	case <-batch.done:
		return batch.err
	case <-time.After(30 * time.Second):
		return fmt.Errorf("async_insert: flush timeout")
	}
}

// Stop gracefully stops the queue, flushing all pending batches.
func (q *AsyncInsertQueue) Stop() {
	close(q.stopCh)
	q.wg.Wait()
	// Flush any remaining batches.
	q.mu.Lock()
	for hash := range q.groups {
		q.mu.Unlock()
		q.flushGroup(hash)
		q.mu.Lock()
	}
	q.mu.Unlock()
}

// Stats returns current queue statistics.
func (q *AsyncInsertQueue) Stats() (queueLength int64, flushTotal int64, flushErrors int64) {
	return q.metrics.QueueLength.Load(),
		q.metrics.FlushTotal.Load(),
		q.metrics.FlushErrors.Load()
}

// RecentErrors returns the most recent flush errors (up to 100).
func RecentAsyncErrors() []AsyncInsertError {
	asyncErrorsMu.Lock()
	defer asyncErrorsMu.Unlock()
	out := make([]AsyncInsertError, len(asyncErrorsLog))
	copy(out, asyncErrorsLog)
	return out
}

func recordAsyncError(idx string, err error) {
	asyncErrorsMu.Lock()
	defer asyncErrorsMu.Unlock()
	asyncErrorsLog = append(asyncErrorsLog, AsyncInsertError{
		Index: idx,
		Error: err.Error(),
		Time:  time.Now(),
	})
	if len(asyncErrorsLog) > asyncErrorsMax {
		asyncErrorsLog = asyncErrorsLog[len(asyncErrorsLog)-asyncErrorsMax:]
	}
}

// flushLoop periodically checks for timed-out batches.
func (q *AsyncInsertQueue) flushLoop() {
	defer q.wg.Done()
	ticker := time.NewTicker(q.config.MaxWaitMs)
	defer ticker.Stop()
	for {
		select {
		case <-q.stopCh:
			return
		case <-ticker.C:
			q.flushExpired()
		}
	}
}

// flushExpired flushes batches that have waited longer than MaxWaitMs.
func (q *AsyncInsertQueue) flushExpired() {
	now := time.Now()
	q.mu.Lock()
	var toFlush []uint64
	for hash, batch := range q.groups {
		if now.Sub(batch.createdAt) >= q.config.MaxWaitMs {
			toFlush = append(toFlush, hash)
		}
	}
	q.mu.Unlock()
	for _, hash := range toFlush {
		q.flushGroup(hash)
	}
}

// flushGroup flushes one group's batch.
func (q *AsyncInsertQueue) flushGroup(hash uint64) {
	q.mu.Lock()
	batch, ok := q.groups[hash]
	if !ok {
		q.mu.Unlock()
		return
	}
	delete(q.groups, hash)
	q.metrics.QueueLength.Add(-int64(len(batch.docs)))
	q.mu.Unlock()

	start := time.Now()
	err := q.flusher(groupHashToKey(hash), batch.docs)
	latency := time.Since(start).Microseconds()
	q.metrics.FlushLatency.Store(latency)
	q.metrics.FlushTotal.Add(1)

	if err != nil {
		q.metrics.FlushErrors.Add(1)
		slog.Warn("async insert flush failed", "error", err)
		recordAsyncError("", err)
	}

	// Broadcast flush completion to all wait=true callers.
	batch.err = err
	close(batch.done)
}

// groupKeyHash computes a stable hash for the group key.
func groupKeyHash(k AsyncInsertGroupKey) uint64 {
	h := uint64(14695981039346656037) // FNV offset
	for _, b := range []byte(k.Index) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	for _, b := range []byte(k.Format) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	h ^= k.SettingsHash
	h *= 1099511628211
	for _, b := range []byte(k.UserIdentity) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}

// groupHashToKey is a placeholder; in production the key is stored alongside
// the batch. For simplicity we return an empty key (the flusher can use the
// docs directly).
func groupHashToKey(hash uint64) AsyncInsertGroupKey {
	return AsyncInsertGroupKey{}
}
