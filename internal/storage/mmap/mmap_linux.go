//go:build linux

// Linux-specific mmap implementation using syscall.Mmap + syscall.Madvise.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.1

package mmap

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// adviceToSyscall maps MmapAdvice to syscall MADV_* constants.
func adviceToSyscall(advice MmapAdvice) int {
	switch advice {
	case AdviseRandom:
		return syscall.MADV_RANDOM
	case AdviseSequential:
		return syscall.MADV_SEQUENTIAL
	case AdviseWillNeed:
		return syscall.MADV_WILLNEED
	case AdviseDontNeed:
		return syscall.MADV_DONTNEED
	default:
		return syscall.MADV_NORMAL
	}
}

// openMmap maps the file at path into memory with the given advice.
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

	// Apply initial madvise hint.
	if advice != AdviseNormal {
		if err := syscall.Madvise(data, adviceToSyscall(advice)); err != nil {
			// Non-fatal: madvise is advisory, not required.
			_ = err
		}
	}

	// Keep fd open for the mmap region lifetime. We store it in the
	// MmapFile so Close can munmap and close it.
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

// adviseMmap updates the kernel advice for the mapped region.
func adviseMmap(m *MmapFile, advice MmapAdvice) error {
	if m.data == nil {
		return nil
	}
	return syscall.Madvise(m.data, adviceToSyscall(advice))
}

// probeMmap tests whether syscall.Mmap is functional by mapping a
// 1-byte temporary file. Returns nil if mmap is available.
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
// Called once at startup. If probe fails, Enabled stays false and
// all subsequent Open calls use the os.ReadFile fallback.
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

// EnsureUnused suppresses "unused import" for unsafe on Linux when
// the compiler optimizes away the pointer cast in future versions.
var _ = unsafe.Pointer(nil)
