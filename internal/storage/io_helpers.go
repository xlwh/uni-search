package storage

import (
	"io"
	"os"
	"path/filepath"
)

// joinSafe joins dir+name defensively. name must not escape dir.
func joinSafe(dir, name string) string {
	return filepath.Join(dir, filepath.Base(name))
}

// openWriteCreate opens path for writing, creating/truncating as needed.
func openWriteCreate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}

// openReadSeek opens path read-only with seeking support.
func openReadSeek(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}

// compile-time assertion: *os.File satisfies io.ReadSeekCloser.
var _ io.ReadSeekCloser = (*os.File)(nil)
