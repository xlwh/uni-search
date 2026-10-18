package meta

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/unisearch-db/unisearch/internal/errors"
)

// Store is the M0 single-node persistence for index metadata: one JSON file
// per index under a root directory (00-overview §5 layout, `meta/` under the
// data dir). Saves are atomic (write temp + fsync + rename + directory
// fsync), so a crash leaves either the previous or the new file, never a
// torn one. M2 replaces this with the Raft state machine; the types in this
// package stay unchanged.
type Store struct {
	dir string
}

// OpenStore opens (creating if needed) the metadata directory.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("meta: empty store directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("meta: create store dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Save persists m atomically: marshal → temp file → fsync → rename →
// directory fsync. The rename is the commit point; the directory fsync
// afterwards makes the rename itself durable.
func (s *Store) Save(m *IndexMeta) error {
	if err := validIndexName(m.Name); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("meta: encode index %q: %w", m.Name, err)
	}
	final := s.path(m.Name)
	tmp, err := os.CreateTemp(s.dir, "."+m.Name+".json.tmp-*")
	if err != nil {
		return fmt.Errorf("meta: create temp file for %q: %w", m.Name, err)
	}
	tmpName := tmp.Name()
	// Best effort cleanup; a leftover temp file never shadows the real one.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("meta: write temp file for %q: %w", m.Name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("meta: fsync temp file for %q: %w", m.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("meta: close temp file for %q: %w", m.Name, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("meta: publish index %q: %w", m.Name, err)
	}
	if err := fsyncDir(s.dir); err != nil {
		return fmt.Errorf("meta: fsync store dir for %q: %w", m.Name, err)
	}
	return nil
}

// Load reads the persisted metadata for the given index name. A missing
// file surfaces as a 404 index_not_found_exception APIError.
func (s *Store) Load(name string) (*IndexMeta, error) {
	if err := validIndexName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path(name))
	if os.IsNotExist(err) {
		return nil, errors.NewIndexNotFound(name)
	}
	if err != nil {
		return nil, fmt.Errorf("meta: read index %q: %w", name, err)
	}
	var m IndexMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("meta: decode index %q: %w", name, err)
	}
	return &m, nil
}

// Delete removes the persisted metadata for the given index name.
func (s *Store) Delete(name string) error {
	if err := validIndexName(name); err != nil {
		return err
	}
	err := os.Remove(s.path(name))
	if os.IsNotExist(err) {
		return errors.NewIndexNotFound(name)
	}
	if err != nil {
		return fmt.Errorf("meta: delete index %q: %w", name, err)
	}
	if err := fsyncDir(s.dir); err != nil {
		return fmt.Errorf("meta: fsync store dir for %q: %w", name, err)
	}
	return nil
}

// List returns all persisted index metas ordered by name. Entries that
// fail to decode are skipped rather than failing the whole listing.
func (s *Store) List() ([]*IndexMeta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("meta: list store dir: %w", err)
	}
	var metas []*IndexMeta
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue // hidden files are in-flight temp files from Save
		}
		m, err := s.Load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		metas = append(metas, m)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Name < metas[j].Name })
	return metas, nil
}

// path joins the store directory and the index file name; the caller has
// already validated the name.
func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name+".json")
}

// validIndexName enforces the safe subset shared with ES: lowercase
// alphanumeric plus `-`, `_`, `.`; it must not be empty, must not start
// with a dot/underscore/dash, must not end with a dash, no `..` runs, and
// ASCII only. This also keeps names safe as file names (no separators, no
// traversal).
func validIndexName(name string) error {
	if name == "" {
		return errors.NewIllegalArgument("index name must not be empty")
	}
	if len(name) > 255 {
		return errors.NewIllegalArgument("index name must not exceed 255 bytes")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return errors.NewIllegalArgument(
				"index name must contain only lowercase letters, digits, '-', '_' and '.'")
		}
	}
	if c := name[0]; c == '-' || c == '_' || c == '.' {
		return errors.NewIllegalArgument("index name must not start with '-', '_' or '.'")
	}
	if strings.HasSuffix(name, "-") {
		return errors.NewIllegalArgument("index name must not end with '-'")
	}
	if strings.Contains(name, "..") {
		return errors.NewIllegalArgument("index name must not contain '..'")
	}
	return nil
}

// fsyncDir flushes directory entries so a preceding rename survives a
// power loss on POSIX filesystems.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
