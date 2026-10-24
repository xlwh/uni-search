package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/trace"

	"github.com/unisearch-db/unisearch/internal/config"
)

// newRootCommand builds the unisearch root command. Flag values form the
// highest configuration layer and are merged on top of defaults, the YAML
// file, and the environment (see internal/config).
func newRootCommand() *cobra.Command {
	var (
		embedded      bool
		cloud         bool
		dataDir       string
		listen        string
		configPath    string
		dump          bool
		adminFlag     bool
		adminListen   string
		role          string
		nodeID        string
		raftAddr      string
		raftPeers     string
		metaAddrs     string
		objstore      string
		grpcAddr      string
		advertiseAddr string
	)

	root := &cobra.Command{
		Use:          "unisearch",
		Short:        "UniSearch server: fused full-text and vector search database",
		SilenceUsage: true, // runtime errors should not dump the usage block
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := config.Options{Path: configPath}
			if embedded {
				opts.Mode = strPtr(config.ModeEmbedded)
			}
			if cloud {
				opts.Mode = strPtr(config.ModeCloud)
			}
			if dataDir != "" {
				opts.DataDir = &dataDir
			}
			if listen != "" {
				opts.Listen = &listen
			}

			// Admin flag handling: --admin sets enabled, --admin-listen overrides address.
			// --admin=false explicitly disables; omitting the flag leaves config default.
			if cmd.Flags().Changed("admin") {
				opts.AdminEnabled = &adminFlag
			}
			if adminListen != "" {
				opts.AdminListen = &adminListen
			}

			// Cloud mode flag overrides.
			if role != "" {
				opts.Role = &role
			}
			if nodeID != "" {
				opts.NodeID = &nodeID
			}
			if raftAddr != "" {
				opts.RaftAddr = &raftAddr
			}
			if raftPeers != "" {
				opts.RaftPeers = &raftPeers
			}
			if metaAddrs != "" {
				opts.MetaAddrs = &metaAddrs
			}
			if objstore != "" {
				opts.ObjStoreURI = &objstore
			}
			if grpcAddr != "" {
				opts.GrpcAddr = &grpcAddr
			}
			if advertiseAddr != "" {
				opts.AdvertiseAddr = &advertiseAddr
			}

			cfg, err := config.Load(opts)
			if err != nil {
				return err
			}

			if dump {
				out, err := cfg.DumpConfig()
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), out)
				return nil
			}

			initLogger()
			slog.Info("starting unisearch",
				"mode", cfg.Mode,
				"listen", cfg.Listen,
				"data_dir", cfg.Data.Dir,
			)
			if cfg.Mode == config.ModeEmbedded {
				return runEmbedded(cfg)
			}
			if cfg.Mode == config.ModeCloud {
				if cfg.Cloud.Role == "meta" {
					return runMeta(cfg)
				}
				return runShard(cfg)
			}
			return nil
		},
	}

	root.Flags().BoolVar(&embedded, "embedded", false,
		"run in embedded single-node mode (overrides config mode)")
	root.Flags().BoolVar(&cloud, "cloud", false,
		"run in cloud-native cluster mode (overrides config mode)")
	root.Flags().StringVar(&dataDir, "data-dir", "",
		"data directory (overrides config and environment)")
	root.Flags().StringVar(&listen, "listen", "",
		"listen address, e.g. :9200 (overrides config and environment)")
	root.Flags().StringVar(&configPath, "config", "",
		"YAML configuration file path")
	root.Flags().BoolVar(&dump, "dump-config", false,
		"print the merged effective configuration as YAML and exit")
	root.Flags().BoolVar(&adminFlag, "admin", true,
		"enable the web admin console (overrides config and environment)")
	root.Flags().StringVar(&adminListen, "admin-listen", "",
		"admin console listen address, e.g. :9201 (overrides config and environment)")
	// Cloud mode flags.
	root.Flags().StringVar(&role, "role", "",
		"cloud mode role: meta | shard")
	root.Flags().StringVar(&nodeID, "node-id", "",
		"cloud node ID, e.g. meta-1, shard-1")
	root.Flags().StringVar(&raftAddr, "raft-addr", "",
		"Raft transport listen address (meta role), e.g. :9300")
	root.Flags().StringVar(&raftPeers, "raft-peers", "",
		"comma-separated Raft bootstrap peers: id@addr,id@addr,...")
	root.Flags().StringVar(&metaAddrs, "meta-addrs", "",
		"comma-separated meta node HTTP addresses (shard role)")
	root.Flags().StringVar(&objstore, "objstore", "",
		"S3-compatible object storage URI, e.g. s3://minio:9000/unisearch")
	root.Flags().StringVar(&grpcAddr, "grpc-addr", "",
		"gRPC listen address (shard role), e.g. :9300")
	root.Flags().StringVar(&advertiseAddr, "advertise-addr", "",
		"advertised gRPC address for cluster registration, e.g. shard-1:9300")

	return root
}

// initLogger installs the default slog JSON handler with trace_id/span_id
// propagation (design 06 §3: "错误日志必带 trace_id"). Records carry
// ts/level/msg + trace_id/span_id when available from the OTel context.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.1
func initLogger() {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Key = "ts"
			}
			return a
		},
	})
	// Wrap with trace-aware handler that extracts trace_id/span_id
	// from the context and injects them as log fields.
	wrapped := &traceHandler{inner: handler}
	slog.SetDefault(slog.New(wrapped))
}

// traceHandler wraps a slog.Handler to inject trace_id and span_id
// from the OTel context into every log record.
type traceHandler struct {
	inner slog.Handler
}

func (h *traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	// Extract trace_id/span_id from OTel span context.
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		r.AddAttrs(
			slog.String("trace_id", span.SpanContext().TraceID().String()),
			slog.String("span_id", span.SpanContext().SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(name)}
}

func strPtr(s string) *string { return &s }
