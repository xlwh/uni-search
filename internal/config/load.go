package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// envPrefix namespaces every environment variable recognized by Load.
const envPrefix = "UNISEARCH_"

// Options parameterize Load. Layers merge in this order (later wins):
// built-in defaults < YAML file < environment < flag overrides.
type Options struct {
	// Path of the YAML configuration file; empty skips the file layer.
	Path string

	// Env holds raw "KEY=VALUE" entries scanned for UNISEARCH_* overrides;
	// nil means os.Environ(). An empty non-nil slice isolates the load
	// from the process environment (used by tests).
	Env []string

	// Command-line flag overrides; nil pointers are skipped. These form
	// the highest layer.
	Mode    *string
	Listen  *string
	DataDir *string
}

// Load merges all configuration layers and validates the result. It always
// starts from Default(), so callers never observe zero values.
func Load(opts Options) (*Config, error) {
	cfg := Default()

	if opts.Path != "" {
		raw, err := os.ReadFile(opts.Path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", opts.Path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", opts.Path, err)
		}
	}

	if err := applyEnv(&cfg, opts.Env); err != nil {
		return nil, err
	}
	applyFlags(&cfg, opts)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyEnv scans env entries for UNISEARCH_* overrides. Keys are matched
// case-insensitively, so "unisearch_listen" and "UNISEARCH_LISTEN" are
// equivalent. Unknown UNISEARCH_* variables are ignored so unrelated
// tooling may use the namespace.
func applyEnv(cfg *Config, env []string) error {
	if env == nil {
		env = os.Environ()
	}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		key := strings.ToUpper(name)
		if !strings.HasPrefix(key, envPrefix) {
			continue
		}
		if err := setEnvField(cfg, key, value); err != nil {
			return err
		}
	}
	return nil
}

// setEnvField assigns one environment override to its config field.
// Nested keys are flattened with underscores:
// UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB -> data.memtable.flush_size_mb.
func setEnvField(cfg *Config, key, value string) error {
	switch key {
	case "UNISEARCH_MODE":
		cfg.Mode = value
	case "UNISEARCH_LISTEN":
		cfg.Listen = value
	case "UNISEARCH_DATA_DIR":
		cfg.Data.Dir = value
	case "UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("config: %s: invalid integer %q", key, value)
		}
		cfg.Data.MemTable.FlushSizeMB = n
	case "UNISEARCH_DATA_MEMTABLE_FLUSH_INTERVAL":
		d, err := parseDuration(value)
		if err != nil {
			return fmt.Errorf("config: %s: %w", key, err)
		}
		cfg.Data.MemTable.FlushInterval = Duration(d)
	case "UNISEARCH_STORAGE_SEGMENT_BLOCK_ROWS":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("config: %s: invalid integer %q", key, value)
		}
		cfg.Storage.SegmentBlockRows = n
	case "UNISEARCH_INDEX_HNSW_M":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("config: %s: invalid integer %q", key, value)
		}
		cfg.Index.HNSW.M = n
	case "UNISEARCH_INDEX_HNSW_EF_CONSTRUCTION":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("config: %s: invalid integer %q", key, value)
		}
		cfg.Index.HNSW.EfConstruction = n
	case "UNISEARCH_INDEX_HNSW_QUANTIZATION":
		cfg.Index.HNSW.Quantization = value
	case "UNISEARCH_TENANT_DEFAULT_MODE":
		cfg.Tenant.DefaultMode = value
	}
	return nil
}

// applyFlags overlays the command-line flag overrides, the highest layer.
func applyFlags(cfg *Config, opts Options) {
	if opts.Mode != nil {
		cfg.Mode = *opts.Mode
	}
	if opts.Listen != nil {
		cfg.Listen = *opts.Listen
	}
	if opts.DataDir != nil {
		cfg.Data.Dir = *opts.DataDir
	}
}

// parseDuration accepts duration strings ("30s", "5m") or bare integers
// (nanoseconds, matching Go's native encoding).
func parseDuration(value string) (time.Duration, error) {
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Duration(n), nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", value)
	}
	return d, nil
}
