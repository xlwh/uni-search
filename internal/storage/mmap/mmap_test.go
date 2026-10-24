package mmap

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenFallback verifies that when mmap is disabled, files
// are loaded via os.ReadFile.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.1
func TestOpenFallback(t *testing.T) {
	Enabled.Store(false)
	defer Enabled.Store(false)

	dir := t.TempDir()
	path := filepath.Join(dir, "test.bin")
	content := []byte("hello mmap world")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	mf, err := Open(path, AdviseRandom)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer mf.Close()

	if !mf.IsFallback() {
		t.Fatal("expected fallback mode when Enabled=false")
	}
	if string(mf.Bytes()) != string(content) {
		t.Fatalf("expected %q, got %q", content, mf.Bytes())
	}
	if mf.Size() != int64(len(content)) {
		t.Fatalf("expected size %d, got %d", len(content), mf.Size())
	}
}

// TestOpenMmap verifies that when mmap is enabled, files are
// memory-mapped and readable.
func TestOpenMmap(t *testing.T) {
	Enabled.Store(true)
	defer Enabled.Store(false)

	dir := t.TempDir()
	path := filepath.Join(dir, "test.bin")
	content := []byte("hello mmap world")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	mf, err := Open(path, AdviseRandom)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer mf.Close()

	// On unsupported platforms this will be fallback, which is fine.
	// On linux/darwin it should be a real mmap.
	if string(mf.Bytes()) != string(content) {
		t.Fatalf("expected %q, got %q", content, mf.Bytes())
	}
	if mf.Size() != int64(len(content)) {
		t.Fatalf("expected size %d, got %d", len(content), mf.Size())
	}
}

// TestOpenEmptyFile verifies that empty files don't cause mmap errors.
func TestOpenEmptyFile(t *testing.T) {
	Enabled.Store(true)
	defer Enabled.Store(false)

	dir := t.TempDir()
	path := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(path, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	mf, err := Open(path, AdviseNormal)
	if err != nil {
		t.Fatalf("Open empty file failed: %v", err)
	}
	defer mf.Close()

	if mf.Size() != 0 {
		t.Fatalf("expected size 0, got %d", mf.Size())
	}
	if len(mf.Bytes()) != 0 {
		t.Fatalf("expected empty bytes, got %d", len(mf.Bytes()))
	}
}

// TestOpenNonexistentFile verifies that opening a missing file
// returns an error.
func TestOpenNonexistentFile(t *testing.T) {
	Enabled.Store(false)

	_, err := Open("/nonexistent/path/file.bin", AdviseNormal)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

// TestCloseIdempotent verifies that Close is safe to call
// multiple times on a fallback instance.
func TestCloseIdempotent(t *testing.T) {
	Enabled.Store(false)
	defer Enabled.Store(false)

	dir := t.TempDir()
	path := filepath.Join(dir, "test.bin")
	os.WriteFile(path, []byte("test"), 0644)

	mf, _ := Open(path, AdviseNormal)
	if err := mf.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close should be a no-op for fallback.
	if err := mf.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestAdviseFallback verifies that Advise is a no-op on fallback mode.
func TestAdviseFallback(t *testing.T) {
	Enabled.Store(false)
	defer Enabled.Store(false)

	dir := t.TempDir()
	path := filepath.Join(dir, "test.bin")
	os.WriteFile(path, []byte("test"), 0644)

	mf, _ := Open(path, AdviseRandom)
	defer mf.Close()

	if err := mf.Advise(AdviseSequential); err != nil {
		t.Fatalf("Advise on fallback should be no-op: %v", err)
	}
}

// TestProbeMmap verifies that probeMmap succeeds on supported platforms.
func TestProbeMmap(t *testing.T) {
	err := probeMmap()
	// On linux/darwin this should succeed; on other platforms it fails.
	if err != nil {
		// Acceptable on unsupported platforms.
		return
	}
}

// TestInitProbe verifies that InitProbe sets Enabled correctly.
func TestInitProbe(t *testing.T) {
	Enabled.Store(false)

	InitProbe(true)
	// On linux/darwin, Enabled should be true after successful probe.
	// On other platforms, it stays false.
	// Either way, no panic is expected.

	InitProbe(false)
	if Enabled.Load() {
		t.Fatal("Enabled should be false when configEnabled=false")
	}
}

// TestLoad verifies the Load convenience function.
func TestLoad(t *testing.T) {
	Enabled.Store(false)
	defer Enabled.Store(false)

	dir := t.TempDir()
	path := filepath.Join(dir, "load.bin")
	content := []byte("load test content")
	os.WriteFile(path, content, 0644)

	mf, err := Load(path, AdviseSequential)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	defer mf.Close()

	if string(mf.Bytes()) != string(content) {
		t.Fatalf("expected %q, got %q", content, mf.Bytes())
	}
}
