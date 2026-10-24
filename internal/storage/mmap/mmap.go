// Package mmap provides a memory-mapped file abstraction with madvise
// hints, falling back to os.ReadFile when mmap is unavailable.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.1
package mmap

import (
	"fmt"
	"os"
	"sync/atomic"
)

// MmapAdvice hints the kernel about expected access patterns.
type MmapAdvice int

const (
	AdviseNormal     MmapAdvice = iota // MADV_NORMAL (default)
	AdviseRandom                       // MADV_RANDOM (disable readahead)
	AdviseSequential                   // MADV_SEQUENTIAL (increase readahead)
	AdviseWillNeed                     // MADV_WILLNEED (prefetch pages)
	AdviseDontNeed                     // MADV_DONTNEED (release pages)
)

// Enabled is the global toggle, set once at startup based on
// config and probeMmap result. When false, all Open calls use
// the os.ReadFile fallback path.
var Enabled atomic.Bool

// MmapFile wraps a memory-mapped file region. When fd == -1 the
// instance is in fallback mode (data loaded via os.ReadFile, no
// mmap region to unmap).
type MmapFile struct {
	data    []byte // mapped region or fallback buffer
	fd      int    // underlying file descriptor (-1 = fallback)
	size    int64  // file size
	fallback bool   // true when using os.ReadFile instead of mmap
}

// Open maps the file at path into memory with the given advice.
// If mmap is disabled (Enabled=false) or fails, falls back to
// os.ReadFile and returns a MmapFile in fallback mode.
func Open(path string, advice MmapAdvice) (*MmapFile, error) {
	if !Enabled.Load() {
		return openFallback(path)
	}

	mf, err := openMmap(path, advice)
	if err != nil {
		// mmap failed for this file — degrade gracefully.
		return openFallback(path)
	}
	return mf, nil
}

// openFallback loads the file via os.ReadFile (no mmap).
func openFallback(path string) (*MmapFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mmap: read fallback %s: %w", path, err)
	}
	return &MmapFile{data: data, fd: -1, size: int64(len(data)), fallback: true}, nil
}

// Bytes returns the mapped byte slice. The returned slice aliases
// internal storage and must not be modified.
func (m *MmapFile) Bytes() []byte { return m.data }

// Size returns the file size in bytes.
func (m *MmapFile) Size() int64 { return m.size }

// IsFallback reports whether this MmapFile is using the os.ReadFile
// fallback path (as opposed to a real mmap region).
func (m *MmapFile) IsFallback() bool { return m.fallback }

// Close releases the mapped region (or frees the fallback buffer)
// and closes the file descriptor. Safe to call on a fallback
// instance (no-op for the buffer; fd is already -1).
func (m *MmapFile) Close() error {
	if m.fallback {
		return nil
	}
	return closeMmap(m)
}

// Advise updates the kernel advice for the mapped region. On
// fallback mode this is a no-op.
func (m *MmapFile) Advise(advice MmapAdvice) error {
	if m.fallback {
		return nil
	}
	return adviseMmap(m, advice)
}

// Load is the unified file loader used by the segment reader: uses
// MmapFile when Enabled, falls back to os.ReadFile when disabled.
// Returns the file contents as a byte slice (mapped or read). The
// caller must call Close on the returned MmapFile when done with
// the data (for mmap mode; safe no-op for fallback).
func Load(path string, advice MmapAdvice) (*MmapFile, error) {
	return Open(path, advice)
}
