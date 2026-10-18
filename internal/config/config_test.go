package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testYAML mirrors the documented configuration surface with non-default
// values (design 00-overview §6.3).
const testYAML = `
mode: embedded
listen: ":8200"
data:
  dir: /tmp/unisearch-yaml
  memtable:
    flush_size_mb: 256
    flush_interval: 45s
storage:
  segment_block_rows: 4096
index:
  hnsw:
    m: 24
    ef_construction: 128
    quantization: sq8
tenant:
  default_mode: dedicated
`

func strPtr(s string) *string { return &s }

// TestR1_ConfigLayeredMerge verifies the layering contract from spec R1:
// defaults < YAML file < environment variables < command-line flags, with
// case-insensitive environment key matching.
func TestR1_ConfigLayeredMerge(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string   // YAML file content; empty means no file layer
		env   []string // explicit environment entries (isolated from os.Environ)
		flags Options
		check func(t *testing.T, cfg *Config)
	}{
		{
			name: "defaults only",
			env:  []string{},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, Default(), *cfg, "defaults must apply untouched")
			},
		},
		{
			name: "yaml overrides defaults",
			yaml: testYAML,
			env:  []string{},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, ModeEmbedded, cfg.Mode)
				assert.Equal(t, ":8200", cfg.Listen)
				assert.Equal(t, "/tmp/unisearch-yaml", cfg.Data.Dir)
				assert.Equal(t, 256, cfg.Data.MemTable.FlushSizeMB)
				assert.Equal(t, Duration(45*time.Second), cfg.Data.MemTable.FlushInterval)
				assert.Equal(t, 4096, cfg.Storage.SegmentBlockRows)
				assert.Equal(t, 24, cfg.Index.HNSW.M)
				assert.Equal(t, 128, cfg.Index.HNSW.EfConstruction)
				assert.Equal(t, "dedicated", cfg.Tenant.DefaultMode)
				// Values absent from the YAML keep their defaults.
				assert.Equal(t, "sq8", cfg.Index.HNSW.Quantization)
			},
		},
		{
			name: "env overrides yaml",
			yaml: testYAML,
			env:  []string{"HOME=/home/tester", "UNISEARCH_LISTEN=:9300"},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, ":9300", cfg.Listen, "UNISEARCH_LISTEN must override the YAML value")
				// Other YAML values still apply.
				assert.Equal(t, "/tmp/unisearch-yaml", cfg.Data.Dir)
			},
		},
		{
			name: "env keys match case-insensitively",
			yaml: testYAML,
			env:  []string{"unisearch_listen=:9400", "Unisearch_Tenant_Default_Mode=isolated"},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, ":9400", cfg.Listen)
				assert.Equal(t, "isolated", cfg.Tenant.DefaultMode)
			},
		},
		{
			name: "env sets nested scalar and duration keys",
			yaml: testYAML,
			env: []string{
				"UNISEARCH_DATA_MEMTABLE_FLUSH_SIZE_MB=64",
				"UNISEARCH_DATA_MEMTABLE_FLUSH_INTERVAL=1m",
				"UNISEARCH_STORAGE_SEGMENT_BLOCK_ROWS=1024",
				"UNISEARCH_INDEX_HNSW_M=32",
			},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, 64, cfg.Data.MemTable.FlushSizeMB)
				assert.Equal(t, Duration(time.Minute), cfg.Data.MemTable.FlushInterval)
				assert.Equal(t, 1024, cfg.Storage.SegmentBlockRows)
				assert.Equal(t, 32, cfg.Index.HNSW.M)
			},
		},
		{
			name: "flags override env",
			yaml: testYAML,
			env:  []string{"UNISEARCH_LISTEN=:9300"},
			flags: Options{
				Mode:    strPtr(ModeEmbedded),
				Listen:  strPtr(":9500"),
				DataDir: strPtr("/data/flag"),
			},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, ":9500", cfg.Listen, "flags must be the highest layer")
				assert.Equal(t, "/data/flag", cfg.Data.Dir)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.flags
			opts.Env = tt.env
			if tt.yaml != "" {
				path := filepath.Join(t.TempDir(), "unisearch.yaml")
				require.NoError(t, os.WriteFile(path, []byte(tt.yaml), 0o644))
				opts.Path = path
			}
			cfg, err := Load(opts)
			require.NoError(t, err)
			tt.check(t, cfg)
		})
	}
}

// TestR1_DumpConfigContainsAllKeys verifies that --dump-config output lists
// every configuration key (design 00-overview §6.3: the binary generates
// the documentation of the full config surface).
func TestR1_DumpConfigContainsAllKeys(t *testing.T) {
	cfg, err := Load(Options{Env: []string{}})
	require.NoError(t, err)

	dump, err := cfg.DumpConfig()
	require.NoError(t, err)

	for _, key := range []string{
		"mode:", "listen:", "data:", "dir:", "memtable:", "flush_size_mb:", "flush_interval:",
		"storage:", "segment_block_rows:", "index:", "hnsw:", "m:", "ef_construction:",
		"quantization:", "tenant:", "default_mode:",
	} {
		assert.Contains(t, dump, key, "dump must list config key %q", key)
	}
	assert.Contains(t, dump, "flush_interval: 30s", "durations dump in human-friendly form")
}

// TestR1_LoadErrors pins the failure modes of Load.
func TestR1_LoadErrors(t *testing.T) {
	t.Run("missing yaml file", func(t *testing.T) {
		_, err := Load(Options{Path: filepath.Join(t.TempDir(), "nope.yaml"), Env: []string{}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config: read")
	})

	t.Run("invalid yaml", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.yaml")
		require.NoError(t, os.WriteFile(path, []byte("listen: [unclosed"), 0o644))
		_, err := Load(Options{Path: path, Env: []string{}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config: parse")
	})

	t.Run("invalid env duration", func(t *testing.T) {
		_, err := Load(Options{Env: []string{"UNISEARCH_DATA_MEMTABLE_FLUSH_INTERVAL=nonsense"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "UNISEARCH_DATA_MEMTABLE_FLUSH_INTERVAL")
	})

	t.Run("validation rejects invalid mode", func(t *testing.T) {
		_, err := Load(Options{Env: []string{"UNISEARCH_MODE=serverless"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config: mode")
	})
}

// TestR1_LoadUsesProcessEnvironment ensures Load consults os.Environ when
// Options.Env is nil (the production path).
func TestR1_LoadUsesProcessEnvironment(t *testing.T) {
	t.Setenv("UNISEARCH_LISTEN", ":9700")
	cfg, err := Load(Options{})
	require.NoError(t, err)
	assert.Equal(t, ":9700", cfg.Listen)
	assert.False(t, strings.HasPrefix(cfg.Data.Dir, "/missing"))
}
