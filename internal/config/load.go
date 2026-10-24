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
	Mode        *string
	Listen      *string
	DataDir     *string
	AdminEnabled *bool
	AdminListen  *string

	// Cloud mode flag overrides.
	Role          *string
	NodeID        *string
	RaftAddr      *string
	RaftPeers     *string // comma-separated "id@addr,..." list
	MetaAddrs     *string // comma-separated "host:port,..." list
	ObjStoreURI   *string // s3://endpoint/bucket
	GrpcAddr      *string
	AdvertiseAddr *string
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
	case "UNISEARCH_ADMIN_ENABLED":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("config: %s: invalid bool %q", key, value)
		}
		cfg.Admin.Enabled = b
	case "UNISEARCH_ADMIN_LISTEN":
		cfg.Admin.Listen = value
	// Cloud mode environment variables.
	case "UNISEARCH_CLOUD_ROLE":
		cfg.Cloud.Role = value
	case "UNISEARCH_CLOUD_NODE_ID":
		cfg.Cloud.NodeID = value
	case "UNISEARCH_CLOUD_RAFT_ADDR":
		cfg.Cloud.RaftAddr = value
	case "UNISEARCH_CLOUD_RAFT_PEERS":
		cfg.Cloud.RaftPeers = splitCSV(value)
	case "UNISEARCH_CLOUD_META_ADDRS":
		cfg.Cloud.MetaAddrs = splitCSV(value)
	case "UNISEARCH_CLOUD_GRPC_ADDR":
		cfg.Cloud.GrpcAddr = value
	case "UNISEARCH_CLOUD_ADVERTISE_ADDR":
		cfg.Cloud.AdvertiseAddr = value
	case "UNISEARCH_OBJSTORE_ACCESS_KEY":
		cfg.Cloud.ObjStore.AccessKey = value
	case "UNISEARCH_OBJSTORE_SECRET_KEY":
		cfg.Cloud.ObjStore.SecretKey = value
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
	if opts.AdminEnabled != nil {
		cfg.Admin.Enabled = *opts.AdminEnabled
	}
	if opts.AdminListen != nil {
		cfg.Admin.Listen = *opts.AdminListen
	}
	// Cloud mode flag overrides.
	if opts.Role != nil {
		cfg.Cloud.Role = *opts.Role
	}
	if opts.NodeID != nil {
		cfg.Cloud.NodeID = *opts.NodeID
	}
	if opts.RaftAddr != nil {
		cfg.Cloud.RaftAddr = *opts.RaftAddr
	}
	if opts.RaftPeers != nil {
		cfg.Cloud.RaftPeers = splitCSV(*opts.RaftPeers)
	}
	if opts.MetaAddrs != nil {
		cfg.Cloud.MetaAddrs = splitCSV(*opts.MetaAddrs)
	}
	if opts.GrpcAddr != nil {
		cfg.Cloud.GrpcAddr = *opts.GrpcAddr
	}
	if opts.AdvertiseAddr != nil {
		cfg.Cloud.AdvertiseAddr = *opts.AdvertiseAddr
	}
	if opts.ObjStoreURI != nil {
		osc, err := parseS3URI(*opts.ObjStoreURI)
		if err == nil {
			cfg.Cloud.ObjStore.Endpoint = osc.Endpoint
			cfg.Cloud.ObjStore.Bucket = osc.Bucket
			cfg.Cloud.ObjStore.UseSSL = osc.UseSSL
		}
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

// splitCSV splits a comma-separated string into a trimmed slice, ignoring
// empty entries.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseS3URI parses an S3 URI like "s3://minio:9000/unisearch" into
// ObjStoreConfig fields. The scheme determines UseSSL: s3:// = false,
// s3s:// = true.
func parseS3URI(uri string) (ObjStoreConfig, error) {
	var osc ObjStoreConfig
	if uri == "" {
		return osc, nil
	}
	// Determine scheme.
	if strings.HasPrefix(uri, "s3s://") {
		osc.UseSSL = true
		uri = uri[6:]
	} else if strings.HasPrefix(uri, "s3://") {
		osc.UseSSL = false
		uri = uri[5:]
	} else {
		return osc, fmt.Errorf("config: objstore URI must start with s3:// or s3s://, got %q", uri)
	}
	// Split endpoint/bucket on the first '/'.
	idx := strings.Index(uri, "/")
	if idx < 0 {
		// No bucket specified — just endpoint.
		osc.Endpoint = uri
		return osc, nil
	}
	osc.Endpoint = uri[:idx]
	osc.Bucket = strings.TrimPrefix(uri[idx:], "/")
	return osc, nil
}
