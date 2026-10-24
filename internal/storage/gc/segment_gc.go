// Package gc implements segment file lifecycle management via
// persisted reference counts and background garbage collection.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.8
package gc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// SegmentGC manages segment file lifecycle via persisted reference counts.
type SegmentGC struct {
	segDir     string
	interval   time.Duration
	maxOldSegs int
	tmpTTL     time.Duration

	mu        sync.Mutex
	refCounts map[types.SegmentID]*refCountedSeg
	stopCh    chan struct{}
	running   bool
}

type refCountedSeg struct {
	segID    types.SegmentID
	dir      string
	refCount int64 // atomic
	markTime time.Time
}

// refcountFile is the persisted reference count format.
type refcountFile struct {
	SegmentID uint64 `json:"segment_id"`
	RefCount  int64  `json:"ref_count"`
	UpdatedAt string `json:"updated_at"`
}

// NewSegmentGC creates a GC manager for the given segment directory.
func NewSegmentGC(segDir string, interval time.Duration, maxOldSegs int, tmpTTL time.Duration) *SegmentGC {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if maxOldSegs <= 0 {
		maxOldSegs = 3
	}
	if tmpTTL <= 0 {
		tmpTTL = 300 * time.Second
	}
	return &SegmentGC{
		segDir:     segDir,
		interval:   interval,
		maxOldSegs: maxOldSegs,
		tmpTTL:     tmpTTL,
		refCounts:  make(map[types.SegmentID]*refCountedSeg),
		stopCh:     make(chan struct{}),
	}
}

// Register adds a segment to GC tracking. Called when a segment is opened.
func (gc *SegmentGC) Register(segID types.SegmentID, dir string) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if _, ok := gc.refCounts[segID]; !ok {
		rs := &refCountedSeg{segID: segID, dir: dir}
		// Try to load persisted refcount.
		gc.loadPersisted(rs)
		gc.refCounts[segID] = rs
	}
}

// IncRef increments the reference count for a segment (called on Snapshot).
func (gc *SegmentGC) IncRef(segID types.SegmentID) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if rs, ok := gc.refCounts[segID]; ok {
		atomic.AddInt64(&rs.refCount, 1)
		gc.persist(rs)
	}
}

// DecRef decrements the reference count (called on query completion).
// When refCount reaches 0, the segment becomes eligible for collection
// (if also marked).
func (gc *SegmentGC) DecRef(segID types.SegmentID) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if rs, ok := gc.refCounts[segID]; ok {
		atomic.AddInt64(&rs.refCount, -1)
		if atomic.LoadInt64(&rs.refCount) < 0 {
			atomic.StoreInt64(&rs.refCount, 0)
		}
		gc.persist(rs)
	}
}

// MarkForCollection marks a segment as reclaimable (called after merge
// replaces old segments with a new one).
func (gc *SegmentGC) MarkForCollection(segID types.SegmentID) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if rs, ok := gc.refCounts[segID]; ok {
		rs.markTime = time.Now()
	}
}

// Start runs the background GC goroutine.
func (gc *SegmentGC) Start(ctx context.Context) {
	gc.mu.Lock()
	if gc.running {
		gc.mu.Unlock()
		return
	}
	gc.running = true
	gc.mu.Unlock()

	go gc.run(ctx)
}

// Stop signals the background GC goroutine to stop.
func (gc *SegmentGC) Stop() {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if gc.running {
		close(gc.stopCh)
		gc.running = false
	}
}

func (gc *SegmentGC) run(ctx context.Context) {
	ticker := time.NewTicker(gc.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gc.stopCh:
			return
		case <-ticker.C:
			gc.collect()
		}
	}
}

// collect removes segments with refCount=0 that have been marked.
func (gc *SegmentGC) collect() {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	for segID, rs := range gc.refCounts {
		if atomic.LoadInt64(&rs.refCount) == 0 && !rs.markTime.IsZero() {
			// Safe to delete.
			if err := os.RemoveAll(rs.dir); err != nil {
				continue // retry next cycle
			}
			// Remove persisted refcount file.
			os.Remove(filepath.Join(rs.dir, "refcount.json"))
			delete(gc.refCounts, segID)
		}
	}
	gc.cleanTmpDirs()
}

// cleanTmpDirs removes stale .tmp directories.
func (gc *SegmentGC) cleanTmpDirs() {
	entries, err := os.ReadDir(gc.segDir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".tmp" && name[:4] != "seg_" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > gc.tmpTTL {
			os.RemoveAll(filepath.Join(gc.segDir, name))
		}
	}
}

// persist writes the reference count to refcount.json in the segment dir.
func (gc *SegmentGC) persist(rs *refCountedSeg) {
	path := filepath.Join(rs.dir, "refcount.json")
	data, _ := json.Marshal(refcountFile{
		SegmentID: uint64(rs.segID),
		RefCount:  atomic.LoadInt64(&rs.refCount),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	os.WriteFile(path, data, 0644) // best-effort
}

// loadPersisted reads the reference count from refcount.json.
func (gc *SegmentGC) loadPersisted(rs *refCountedSeg) {
	path := filepath.Join(rs.dir, "refcount.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return // no persisted count, start at 0
	}
	var rf refcountFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return
	}
	atomic.StoreInt64(&rs.refCount, rf.RefCount)
}

// Recover loads reference counts from disk for all segments in the directory.
// Called once at startup.
func (gc *SegmentGC) Recover() error {
	entries, err := os.ReadDir(gc.segDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no data dir yet
		}
		return fmt.Errorf("gc: recover: %w", err)
	}
	gc.mu.Lock()
	defer gc.mu.Unlock()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 5 || name[:4] != "seg_" {
			continue
		}
		// Parse segment ID from directory name "seg_<uint64>".
		var segIDNum uint64
		if _, err := fmt.Sscanf(name, "seg_%d", &segIDNum); err != nil {
			continue
		}
		segID := types.SegmentID(segIDNum)
		dir := filepath.Join(gc.segDir, name)
		rs := &refCountedSeg{segID: segID, dir: dir}
		gc.loadPersisted(rs)
		gc.refCounts[segID] = rs
	}
	return nil
}

// Len returns the number of tracked segments.
func (gc *SegmentGC) Len() int {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return len(gc.refCounts)
}

// GetRefCount returns the current reference count for a segment.
func (gc *SegmentGC) GetRefCount(segID types.SegmentID) int64 {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if rs, ok := gc.refCounts[segID]; ok {
		return atomic.LoadInt64(&rs.refCount)
	}
	return 0
}
