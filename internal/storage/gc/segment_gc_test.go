package gc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// TestSegmentGCRegisterIncDec verifies basic register/inc/dec lifecycle.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.8
func TestSegmentGCRegisterIncDec(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segs")
	os.MkdirAll(segDir, 0755)
	segPath := filepath.Join(segDir, "seg_1")
	os.MkdirAll(segPath, 0755)

	gc := NewSegmentGC(segDir, 60*time.Second, 3, 300*time.Second)
	gc.Register(types.SegmentID(1), segPath)

	if gc.Len() != 1 {
		t.Fatalf("expected 1 tracked segment, got %d", gc.Len())
	}
	if rc := gc.GetRefCount(types.SegmentID(1)); rc != 0 {
		t.Fatalf("expected refcount 0 after register, got %d", rc)
	}

	gc.IncRef(types.SegmentID(1))
	if rc := gc.GetRefCount(types.SegmentID(1)); rc != 1 {
		t.Fatalf("expected refcount 1 after IncRef, got %d", rc)
	}

	gc.DecRef(types.SegmentID(1))
	if rc := gc.GetRefCount(types.SegmentID(1)); rc != 0 {
		t.Fatalf("expected refcount 0 after DecRef, got %d", rc)
	}
}

// TestSegmentGCMarkAndCollect verifies that marked segments with
// refCount=0 are collected.
func TestSegmentGCMarkAndCollect(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segs")
	os.MkdirAll(segDir, 0755)
	segPath := filepath.Join(segDir, "seg_10")
	os.MkdirAll(segPath, 0755)
	os.WriteFile(filepath.Join(segPath, "data"), []byte("test"), 0644)

	gc := NewSegmentGC(segDir, 50*time.Millisecond, 3, 300*time.Second)
	gc.Register(types.SegmentID(10), segPath)
	gc.MarkForCollection(types.SegmentID(10))

	gc.collect()

	if _, err := os.Stat(segPath); !os.IsNotExist(err) {
		t.Fatalf("expected segment dir deleted after collect, but exists")
	}
	if gc.Len() != 0 {
		t.Fatalf("expected 0 tracked segments after collect, got %d", gc.Len())
	}
}

// TestSegmentGCNoCollectWhileReferenced verifies that segments
// with refCount>0 are NOT collected even if marked.
func TestSegmentGCNoCollectWhileReferenced(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segs")
	os.MkdirAll(segDir, 0755)
	segPath := filepath.Join(segDir, "seg_20")
	os.MkdirAll(segPath, 0755)

	gc := NewSegmentGC(segDir, 50*time.Millisecond, 3, 300*time.Second)
	gc.Register(types.SegmentID(20), segPath)
	gc.IncRef(types.SegmentID(20))
	gc.MarkForCollection(types.SegmentID(20))

	gc.collect()

	if _, err := os.Stat(segPath); err != nil {
		t.Fatalf("expected segment dir to exist (refCount>0), but deleted: %v", err)
	}
	if gc.Len() != 1 {
		t.Fatalf("expected 1 tracked segment, got %d", gc.Len())
	}
}

// TestSegmentGCStartStop verifies the background goroutine starts
// and stops cleanly.
func TestSegmentGCStartStop(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segs")
	os.MkdirAll(segDir, 0755)

	gc := NewSegmentGC(segDir, 50*time.Millisecond, 3, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gc.Start(ctx)
	gc.Stop()
}

// TestSegmentGCRecover verifies loading refcounts from disk.
func TestSegmentGCRecover(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segs")
	os.MkdirAll(segDir, 0755)
	segPath := filepath.Join(segDir, "seg_30")
	os.MkdirAll(segPath, 0755)

	data := []byte(`{"segment_id":30,"ref_count":3,"updated_at":"2026-01-01T00:00:00Z"}`)
	os.WriteFile(filepath.Join(segPath, "refcount.json"), data, 0644)

	gc := NewSegmentGC(segDir, 60*time.Second, 3, 300*time.Second)
	if err := gc.Recover(); err != nil {
		t.Fatalf("Recover failed: %v", err)
	}

	if gc.Len() != 1 {
		t.Fatalf("expected 1 recovered segment, got %d", gc.Len())
	}
	if rc := gc.GetRefCount(types.SegmentID(30)); rc != 3 {
		t.Fatalf("expected recovered refcount 3, got %d", rc)
	}
}

// TestSegmentGCTmpDirCleanup verifies that stale .tmp dirs are cleaned.
func TestSegmentGCTmpDirCleanup(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segs")
	os.MkdirAll(segDir, 0755)

	tmpDir := filepath.Join(segDir, "seg_40.tmp")
	os.MkdirAll(tmpDir, 0755)
	os.WriteFile(filepath.Join(tmpDir, "data"), []byte("x"), 0644)

	oldTime := time.Now().Add(-10 * time.Minute)
	os.Chtimes(tmpDir, oldTime, oldTime)

	gc := NewSegmentGC(segDir, 50*time.Millisecond, 3, 50*time.Millisecond)
	gc.cleanTmpDirs()

	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("expected .tmp dir deleted after cleanTmpDirs")
	}
}

// TestSegmentGCDecRefClampToZero verifies refcount doesn't go negative.
func TestSegmentGCDecRefClampToZero(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segs")
	os.MkdirAll(segDir, 0755)
	segPath := filepath.Join(segDir, "seg_50")
	os.MkdirAll(segPath, 0755)

	gc := NewSegmentGC(segDir, 60*time.Second, 3, 300*time.Second)
	gc.Register(types.SegmentID(50), segPath)

	gc.DecRef(types.SegmentID(50))
	if rc := gc.GetRefCount(types.SegmentID(50)); rc != 0 {
		t.Fatalf("expected refcount clamped to 0, got %d", rc)
	}
}

// TestSegmentGCUnknownSegment verifies operations on unknown segments
// are safe no-ops.
func TestSegmentGCUnknownSegment(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segs")
	os.MkdirAll(segDir, 0755)

	gc := NewSegmentGC(segDir, 60*time.Second, 3, 300*time.Second)

	gc.IncRef(types.SegmentID(999))
	gc.DecRef(types.SegmentID(999))
	gc.MarkForCollection(types.SegmentID(999))
	if rc := gc.GetRefCount(types.SegmentID(999)); rc != 0 {
		t.Fatalf("expected 0 for unknown segment, got %d", rc)
	}
}
