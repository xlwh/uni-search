//go:build darwin

// macOS-specific mmap implementation. syscall.Mmap/Munmap are available
// but syscall.Madvise is not; madvise calls are no-ops on macOS.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.1

package mmap

import (
	"fmt"
	"os"
	"syscall"
)

// openMmap maps the file at path into memory with the given advice.
// On macOS the advice hint is accepted but not applied (no-op).
func openMmap(path string, advice MmapAdvice) (*MmapFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mmap: open %s: %w", path, err)
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap: stat %s: %w", path, err)
	}
	size := fi.Size()
	if size == 0 {
		f.Close()
		return &MmapFile{data: nil, fd: -1, size: 0, fallback: true}, nil
	}

	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap: map %s: %w", path, err)
	}

	// On macOS, madvise is not available via syscall; skip advice.
	// The kernel's default page-in behavior is reasonable for dev/test.

	return &MmapFile{data: data, fd: int(f.Fd()), size: size}, nil
}

// closeMmap unmaps the region and closes the file descriptor.
func closeMmap(m *MmapFile) error {
	var firstErr error
	if err := syscall.Munmap(m.data); err != nil {
		firstErr = fmt.Errorf("mmap: munmap: %w", err)
	}
	if m.fd >= 0 {
		if err := syscall.Close(m.fd); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("mmap: close fd: %w", err)
		}
	}
	m.data = nil
	m.fd = -1
	return firstErr
}

// adviseMmap is a no-op on macOS (syscall.Madvise unavailable).
func adviseMmap(m *MmapFile, advice MmapAdvice) error {
	return nil
}

// probeMmap tests whether syscall.Mmap is functional.
func probeMmap() error {
	tmp, err := os.CreateTemp("", "mmap-probe-*")
	if err != nil {
		return fmt.Errorf("mmap: probe create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write([]byte{0}); err != nil {
		tmp.Close()
		return fmt.Errorf("mmap: probe write: %w", err)
	}
	tmp.Close()

	f, err := os.Open(tmpName)
	if err != nil {
		return fmt.Errorf("mmap: probe open: %w", err)
	}
	defer f.Close()

	data, err := syscall.Mmap(int(f.Fd()), 0, 1, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap: probe mmap: %w", err)
	}
	_ = syscall.Munmap(data)
	return nil
}

// InitProbe probes mmap availability and sets Enabled accordingly.
func InitProbe(configEnabled bool) {
	if !configEnabled {
		Enabled.Store(false)
		return
	}
	if err := probeMmap(); err != nil {
		Enabled.Store(false)
		return
	}
	Enabled.Store(true)
}
