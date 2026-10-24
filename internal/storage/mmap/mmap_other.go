//go:build !linux && !darwin

// Fallback for platforms without mmap support (always uses os.ReadFile).
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.1

package mmap

// openMmap always returns an error on unsupported platforms,
// forcing the fallback path.
func openMmap(path string, advice MmapAdvice) (*MmapFile, error) {
	return nil, errMmapUnsupported
}

// closeMmap is unreachable on this platform.
func closeMmap(m *MmapFile) error { return nil }

// adviseMmap is unreachable on this platform.
func adviseMmap(m *MmapFile, advice MmapAdvice) error { return nil }

// probeMmap always fails on unsupported platforms.
func probeMmap() error { return errMmapUnsupported }

// InitProbe always disables mmap on unsupported platforms.
func InitProbe(configEnabled bool) {
	Enabled.Store(false)
}

var errMmapUnsupported = &mmapUnsupportedError{}

type mmapUnsupportedError struct{}

func (e *mmapUnsupportedError) Error() string { return "mmap: unsupported platform" }
