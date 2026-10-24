// Package snapshot provides ES-style snapshot repository management.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.6
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SnapshotInfo holds metadata about one snapshot.
type SnapshotInfo struct {
	Snapshot       string    `json:"snapshot"`
	UUID           string    `json:"uuid"`
	Indices        []string  `json:"indices"`
	State          string    `json:"state"` // SUCCESS, IN_PROGRESS, FAILED
	StartTime      time.Time `json:"start_time"`
	EndTime        time.Time `json:"end_time"`
	DurationMillis int64     `json:"duration_in_millis"`
}

// VerifyRepo checks that the repository is writable and returns the
// verification result.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.6.3
func (r *RepoRegistry) VerifyRepo(repoName string) error {
	r.mu.RLock()
	cfg, ok := r.repos[repoName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("repository %q not found", repoName)
	}
	if cfg.Type != "fs" {
		return fmt.Errorf("unsupported repo type: %s", cfg.Type)
	}
	// Ensure the directory exists (mkdir -p equivalent).
	if err := os.MkdirAll(cfg.Location, 0755); err != nil {
		return fmt.Errorf("repository %q cannot create directory: %w", repoName, err)
	}
	// Write a test file and remove it.
	testPath := filepath.Join(cfg.Location, ".verify-test")
	if err := os.WriteFile(testPath, []byte("verify"), 0644); err != nil {
		return fmt.Errorf("repository %q is not writable: %w", repoName, err)
	}
	os.Remove(testPath)
	return nil
}

// GetSnapshot returns info for one specific snapshot.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.6.2
func (r *RepoRegistry) GetSnapshot(repoName, snapshotName string) (*SnapshotInfo, error) {
	r.mu.RLock()
	cfg, ok := r.repos[repoName]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("repository %q not found", repoName)
	}
	metaPath := filepath.Join(cfg.Location, "snap-"+snapshotName, "snap-meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot %q not found in repo %q", snapshotName, repoName)
	}
	var info SnapshotInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("invalid snapshot metadata: %w", err)
	}
	return &info, nil
}

// RepoConfig holds the configuration for a snapshot repository.
type RepoConfig struct {
	Type     string         `json:"type"` // "fs"
	Location string         `json:"location"`
	Settings map[string]any `json:"settings"`
}

// RepoRegistry manages snapshot repositories.
type RepoRegistry struct {
	mu      sync.RWMutex
	repos   map[string]*RepoConfig
	dataDir string
}

// NewRepoRegistry creates a registry. Repos are persisted to
// {dataDir}/snapshot-repos.json.
func NewRepoRegistry(dataDir string) *RepoRegistry {
	r := &RepoRegistry{
		repos:   make(map[string]*RepoConfig),
		dataDir: dataDir,
	}
	r.Load()
	return r
}

func (r *RepoRegistry) Put(name string, cfg *RepoConfig) {
	// Extract location from settings if not set at top level (ES API format).
	if cfg.Location == "" && cfg.Settings != nil {
		if loc, ok := cfg.Settings["location"].(string); ok {
			cfg.Location = loc
		}
	}
	r.mu.Lock()
	r.repos[name] = cfg
	r.mu.Unlock()
	r.Save()
}

func (r *RepoRegistry) Get(name string) (*RepoConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.repos[name]
	return cfg, ok
}

func (r *RepoRegistry) Delete(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.repos[name]; !ok {
		return false
	}
	delete(r.repos, name)
	r.Save()
	return true
}

func (r *RepoRegistry) List() map[string]*RepoConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*RepoConfig, len(r.repos))
	for k, v := range r.repos {
		out[k] = v
	}
	return out
}

func (r *RepoRegistry) Save() {
	path := filepath.Join(r.dataDir, "snapshot-repos.json")
	data, _ := json.MarshalIndent(r.repos, "", "  ")
	_ = os.WriteFile(path, data, 0644)
}

func (r *RepoRegistry) Load() {
	path := filepath.Join(r.dataDir, "snapshot-repos.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	json.Unmarshal(data, &r.repos)
}

// CreateSnapshot creates a snapshot of the given indices in the repo.
func (r *RepoRegistry) CreateSnapshot(repoName, snapshotName string, indices []string, indexDataFn func(idx string) ([]byte, error)) (*SnapshotInfo, error) {
	r.mu.RLock()
	cfg, ok := r.repos[repoName]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("repository %q not found", repoName)
	}
	if cfg.Type != "fs" {
		return nil, fmt.Errorf("unsupported repo type: %s", cfg.Type)
	}
	snapDir := filepath.Join(cfg.Location, "snap-"+snapshotName)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot dir: %w", err)
	}
	now := time.Now()
	info := &SnapshotInfo{
		Snapshot:  snapshotName,
		UUID:      snapshotName,
		Indices:   indices,
		State:     "SUCCESS",
		StartTime: now,
		EndTime:   time.Now(),
	}
	// Store index data (metadata) for each index.
	for _, idx := range indices {
		idxDir := filepath.Join(snapDir, "indices", idx)
		os.MkdirAll(idxDir, 0755)
		data, err := indexDataFn(idx)
		if err != nil {
			info.State = "PARTIAL"
			continue
		}
		os.WriteFile(filepath.Join(idxDir, "meta.json"), data, 0644)
	}
	info.DurationMillis = time.Since(now).Milliseconds()
	// Write snapshot metadata.
	meta, _ := json.MarshalIndent(info, "", "  ")
	os.WriteFile(filepath.Join(snapDir, "snap-meta.json"), meta, 0644)
	return info, nil
}

// ListSnapshots returns all snapshots in a repo.
func (r *RepoRegistry) ListSnapshots(repoName string) ([]SnapshotInfo, error) {
	r.mu.RLock()
	cfg, ok := r.repos[repoName]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("repository %q not found", repoName)
	}
	entries, err := os.ReadDir(cfg.Location)
	if err != nil {
		return nil, nil // empty repo
	}
	var snaps []SnapshotInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 5 || name[:5] != "snap-" {
			continue
		}
		metaPath := filepath.Join(cfg.Location, entry.Name(), "snap-meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var info SnapshotInfo
		if json.Unmarshal(data, &info) == nil {
			snaps = append(snaps, info)
		}
	}
	return snaps, nil
}

// DeleteSnapshot removes a snapshot from the repo.
func (r *RepoRegistry) DeleteSnapshot(repoName, snapshotName string) error {
	r.mu.RLock()
	cfg, ok := r.repos[repoName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("repository %q not found", repoName)
	}
	snapDir := filepath.Join(cfg.Location, "snap-"+snapshotName)
	return os.RemoveAll(snapDir)
}

// RestoreSnapshot restores a snapshot to a target index.
func (r *RepoRegistry) RestoreSnapshot(repoName, snapshotName, targetIndex string, applyMetaFn func(idx string, meta []byte) error) error {
	r.mu.RLock()
	cfg, ok := r.repos[repoName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("repository %q not found", repoName)
	}
	snapDir := filepath.Join(cfg.Location, "snap-"+snapshotName)
	// Read snapshot metadata.
	metaData, err := os.ReadFile(filepath.Join(snapDir, "snap-meta.json"))
	if err != nil {
		return fmt.Errorf("snapshot %q not found in repo %q", snapshotName, repoName)
	}
	var info SnapshotInfo
	if err := json.Unmarshal(metaData, &info); err != nil {
		return fmt.Errorf("invalid snapshot metadata: %w", err)
	}
	// Restore each index's metadata.
	for _, idx := range info.Indices {
		idxMeta, err := os.ReadFile(filepath.Join(snapDir, "indices", idx, "meta.json"))
		if err != nil {
			continue
		}
		target := idx
		if targetIndex != "" {
			target = targetIndex
		}
		if err := applyMetaFn(target, idxMeta); err != nil {
			return fmt.Errorf("failed to restore index %q: %w", target, err)
		}
	}
	return nil
}
