// Package config implements UniSearch's layered configuration: built-in
// defaults < YAML file < environment variables < command-line flags
// (design 00-overview §6.3). Every item must declare a default in this
// package so that no zero value ever leaks into runtime behavior.
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Server modes (config key "mode"); the binary picks its process model
// based on this value.
const (
	ModeEmbedded = "embedded"
	ModeCloud    = "cloud"
)

// Duration is a time.Duration that round-trips through YAML and
// environment values in human-friendly form ("30s", "5m"); bare integers
// decode as nanoseconds (Go's native encoding).
type Duration time.Duration

// MarshalYAML emits the textual form when it parses back exactly, keeping
// dumps readable ("flush_interval: 30s").
func (d Duration) MarshalYAML() (any, error) {
	if time.Duration(d) == 0 {
		return int64(0), nil
	}
	text := time.Duration(d).String()
	if parsed, err := time.ParseDuration(text); err == nil && parsed == time.Duration(d) {
		return text, nil
	}
	return int64(time.Duration(d)), nil
}

// UnmarshalYAML accepts duration strings ("45s") or nanosecond integers.
// An explicit null keeps the pre-filled default value.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!null" {
		return nil
	}
	v, err := parseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("config: duration %q: %w", node.Value, err)
	}
	*d = Duration(v)
	return nil
}

// Config is the full effective configuration tree. Field order mirrors the
// YAML layout in design 00-overview §6.3.
type Config struct {
	Mode    string        `yaml:"mode"` // embedded | cloud
	Listen  string        `yaml:"listen"`
	Data    DataConfig    `yaml:"data"`
	Storage StorageConfig `yaml:"storage"`
	Index   IndexConfig   `yaml:"index"`
	Tenant  TenantConfig  `yaml:"tenant"`
	Admin   AdminConfig   `yaml:"admin"`
	Cloud   CloudConfig   `yaml:"cloud"`
	Query   QueryConfig   `yaml:"query"`
}

// DataConfig groups local data placement and MemTable tuning.
type DataConfig struct {
	Dir      string         `yaml:"dir"`
	MemTable MemTableConfig `yaml:"memtable"`
}

// MemTableConfig holds the flush thresholds for the columnar write buffer.
type MemTableConfig struct {
	FlushSizeMB   int      `yaml:"flush_size_mb"`
	FlushInterval Duration `yaml:"flush_interval"`
}

// StorageConfig holds column-store segment tuning.
type StorageConfig struct {
	SegmentBlockRows int                    `yaml:"segment_block_rows"`
	MmapAdvice       MmapAdviceConfig       `yaml:"mmap_advice"`
	CacheDir         string                 `yaml:"cache_dir"`
	CacheSizeMB      int                    `yaml:"cache_size_mb"`
	SegmentRetention SegmentRetentionConfig `yaml:"segment_retention"`
}

// MmapAdviceConfig controls mmap usage and per-file-type advice hints.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.1
type MmapAdviceConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Column   string `yaml:"column"`
	Terms    string `yaml:"terms"`
	Postings string `yaml:"postings"`
	Footer   string `yaml:"footer"`
	IDHash   string `yaml:"id_hash"`
	Source   string `yaml:"source"`
}

// SegmentRetentionConfig controls old segment file GC.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.8
type SegmentRetentionConfig struct {
	MaxOldSegments int      `yaml:"max_old_segments"`
	TempDirTTL     Duration `yaml:"temp_dir_ttl"`
}

// QueryConfig holds query engine tuning.
type QueryConfig struct {
	FilterCache FilterCacheConfig `yaml:"filter_cache"`
}

// FilterCacheConfig controls the proxy filter cache.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.2
type FilterCacheConfig struct {
	MaxSize int      `yaml:"max_size"`
	TTL     Duration `yaml:"ttl"`
}

// IndexConfig groups index-engine tuning.
type IndexConfig struct {
	HNSW HNSWConfig `yaml:"hnsw"`
}

// HNSWConfig holds graph construction parameters and the quantization
// policy for vector indexes.
type HNSWConfig struct {
	M              int    `yaml:"m"`
	EfConstruction int    `yaml:"ef_construction"`
	Quantization   string `yaml:"quantization"`
}

// TenantConfig holds multi-tenancy defaults.
type TenantConfig struct {
	DefaultMode string `yaml:"default_mode"`
}

// AdminConfig holds the web admin console settings.
type AdminConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// CloudConfig holds cloud-native mode parameters (design 05 §8).
// Only relevant when Mode == "cloud".
type CloudConfig struct {
	Role          string         `yaml:"role"`           // "meta" | "shard"
	NodeID        string         `yaml:"node_id"`        // e.g. "meta-1"
	RaftAddr      string         `yaml:"raft_addr"`      // Raft transport listen, e.g. ":9300"
	RaftPeers     []string       `yaml:"raft_peers"`     // bootstrap peers "id@addr"
	MetaAddrs     []string       `yaml:"meta_addrs"`     // meta HTTP addresses (shard nodes)
	ObjStore      ObjStoreConfig `yaml:"objstore"`       // S3-compatible object storage
	GrpcAddr      string         `yaml:"grpc_addr"`      // gRPC listen (shard nodes), e.g. ":9300"
	AdvertiseAddr string         `yaml:"advertise_addr"` // broadcast gRPC addr, e.g. "shard-1:9300"
}

// ObjStoreConfig holds S3-compatible object storage connection params.
type ObjStoreConfig struct {
	Endpoint  string `yaml:"endpoint"` // e.g. "minio:9000"
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
	UseSSL    bool   `yaml:"use_ssl"`
	Region    string `yaml:"region"`
}

// Default returns the built-in configuration. Every field must be non-zero
// here; zero-value implicit behavior is forbidden (design 00-overview §6.3).
func Default() Config {
	return Config{
		Mode:   ModeEmbedded,
		Listen: ":9200",
		Data: DataConfig{
			Dir: "./data",
			MemTable: MemTableConfig{
				FlushSizeMB:   512,
				FlushInterval: Duration(30 * time.Second),
			},
		},
		Storage: StorageConfig{
			SegmentBlockRows: 8192,
			MmapAdvice: MmapAdviceConfig{
				Enabled:  true,
				Column:   "random",
				Terms:    "random",
				Postings: "sequential",
				Footer:   "willneed",
				IDHash:   "random",
				Source:   "sequential",
			},
			CacheDir:    "",
			CacheSizeMB: 1024,
			SegmentRetention: SegmentRetentionConfig{
				MaxOldSegments: 3,
				TempDirTTL:     Duration(300 * time.Second),
			},
		},
		Index: IndexConfig{
			HNSW: HNSWConfig{
				M:              16,
				EfConstruction: 200,
				Quantization:   "sq8",
			},
		},
		Tenant: TenantConfig{
			DefaultMode: "shared-shard",
		},
		Admin: AdminConfig{
			Enabled: true,
			Listen:  ":9201",
		},
		Query: QueryConfig{
			FilterCache: FilterCacheConfig{
				MaxSize: 10000,
				TTL:     Duration(60 * time.Second),
			},
		},
		Cloud: CloudConfig{
			Role:     "shard",
			RaftAddr: ":9300",
			GrpcAddr: ":9300",
		},
	}
}

// Validate rejects structurally invalid configurations with a message that
// names the offending config key.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeEmbedded, ModeCloud:
	default:
		return fmt.Errorf("config: mode must be %q or %q, got %q", ModeEmbedded, ModeCloud, c.Mode)
	}
	if c.Listen == "" {
		return fmt.Errorf("config: listen must not be empty")
	}
	if c.Data.Dir == "" {
		return fmt.Errorf("config: data.dir must not be empty")
	}
	if c.Data.MemTable.FlushSizeMB <= 0 {
		return fmt.Errorf("config: data.memtable.flush_size_mb must be positive, got %d", c.Data.MemTable.FlushSizeMB)
	}
	if c.Data.MemTable.FlushInterval <= 0 {
		return fmt.Errorf("config: data.memtable.flush_interval must be positive, got %s", time.Duration(c.Data.MemTable.FlushInterval))
	}
	if c.Storage.SegmentBlockRows <= 0 {
		return fmt.Errorf("config: storage.segment_block_rows must be positive, got %d", c.Storage.SegmentBlockRows)
	}
	if c.Index.HNSW.M <= 0 {
		return fmt.Errorf("config: index.hnsw.m must be positive, got %d", c.Index.HNSW.M)
	}
	if c.Index.HNSW.EfConstruction <= 0 {
		return fmt.Errorf("config: index.hnsw.ef_construction must be positive, got %d", c.Index.HNSW.EfConstruction)
	}
	if c.Index.HNSW.Quantization == "" {
		return fmt.Errorf("config: index.hnsw.quantization must not be empty")
	}
	if c.Tenant.DefaultMode == "" {
		return fmt.Errorf("config: tenant.default_mode must not be empty")
	}
	if c.Admin.Enabled && c.Admin.Listen == "" {
		return fmt.Errorf("config: admin.listen must not be empty when admin is enabled")
	}
	if c.Admin.Enabled && c.Admin.Listen == c.Listen {
		return fmt.Errorf("config: admin.listen must not equal listen (got %q)", c.Admin.Listen)
	}
	// Cloud mode validation.
	if c.Mode == ModeCloud {
		if c.Cloud.Role != "meta" && c.Cloud.Role != "shard" {
			return fmt.Errorf("config: cloud.role must be %q or %q, got %q", "meta", "shard", c.Cloud.Role)
		}
		if c.Cloud.NodeID == "" {
			return fmt.Errorf("config: cloud.node_id must not be empty in cloud mode")
		}
		if c.Cloud.Role == "meta" {
			if len(c.Cloud.RaftPeers) == 0 {
				return fmt.Errorf("config: cloud.raft_peers must not be empty for meta role")
			}
			if c.Cloud.RaftAddr == "" {
				return fmt.Errorf("config: cloud.raft_addr must not be empty for meta role")
			}
		}
		if c.Cloud.Role == "shard" {
			if len(c.Cloud.MetaAddrs) == 0 {
				return fmt.Errorf("config: cloud.meta_addrs must not be empty for shard role")
			}
		}
	}
	return nil
}

// DumpConfig renders the final merged configuration as YAML text; it backs
// `unisearch --dump-config` so the binary itself is the source of truth for
// the full configuration surface.
func (c *Config) DumpConfig() (string, error) {
	out, err := yaml.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("config: dump: %w", err)
	}
	return string(out), nil
}
