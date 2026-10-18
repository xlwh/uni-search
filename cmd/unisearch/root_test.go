package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestR1_DumpConfigFlag exercises the --dump-config path end to end through
// the cobra command (spec R1: the flag prints the merged config and exits
// successfully).
func TestR1_DumpConfigFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		yaml string
		want []string // substrings expected in the dump
		bad  []string // substrings that must not appear
	}{
		{
			name: "defaults only lists the full config surface",
			args: []string{"--dump-config"},
			want: []string{
				"mode:", "listen:", "data:", "dir:", "memtable:", "flush_size_mb:",
				"flush_interval:", "storage:", "segment_block_rows:", "index:",
				"hnsw:", "quantization:", "tenant:", "default_mode:",
			},
		},
		{
			name: "yaml file layer is honored",
			args: []string{"--dump-config"},
			yaml: "listen: \":8200\"\ndata:\n  dir: /tmp/unisearch-cmd\n",
			want: []string{":8200", "/tmp/unisearch-cmd"},
			bad:  []string{":9200"},
		},
		{
			name: "flags override environment",
			args: []string{"--listen", ":9999", "--dump-config"},
			env:  map[string]string{"UNISEARCH_LISTEN": ":9300"},
			want: []string{"listen: :9999"},
			bad:  []string{":9300"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if tt.yaml != "" {
				path := filepath.Join(t.TempDir(), "unisearch.yaml")
				require.NoError(t, os.WriteFile(path, []byte(tt.yaml), 0o644))
				tt.args = append(tt.args, "--config", path)
			}

			root := newRootCommand()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(tt.args)

			require.NoError(t, root.Execute(), "--dump-config must exit 0")

			dump := out.String()
			for _, want := range tt.want {
				assert.Contains(t, dump, want)
			}
			for _, bad := range tt.bad {
				assert.NotContains(t, dump, bad)
			}
		})
	}
}

// TestR1_InvalidConfigReturnsError ensures config failures surface as
// command errors (non-zero exit) rather than a bogus dump.
func TestR1_InvalidConfigReturnsError(t *testing.T) {
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml")})

	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config: read")
}
