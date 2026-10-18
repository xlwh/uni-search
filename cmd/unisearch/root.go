package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/unisearch-db/unisearch/internal/config"
)

// newRootCommand builds the unisearch root command. Flag values form the
// highest configuration layer and are merged on top of defaults, the YAML
// file, and the environment (see internal/config).
func newRootCommand() *cobra.Command {
	var (
		embedded   bool
		dataDir    string
		listen     string
		configPath string
		dump       bool
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
			if dataDir != "" {
				opts.DataDir = &dataDir
			}
			if listen != "" {
				opts.Listen = &listen
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
			return nil
		},
	}

	root.Flags().BoolVar(&embedded, "embedded", false,
		"run in embedded single-node mode (overrides config mode)")
	root.Flags().StringVar(&dataDir, "data-dir", "",
		"data directory (overrides config and environment)")
	root.Flags().StringVar(&listen, "listen", "",
		"listen address, e.g. :9200 (overrides config and environment)")
	root.Flags().StringVar(&configPath, "config", "",
		"YAML configuration file path")
	root.Flags().BoolVar(&dump, "dump-config", false,
		"print the merged effective configuration as YAML and exit")

	return root
}

// initLogger installs the default slog JSON handler. Records carry the
// ts/level/msg fields mandated by design 00-overview §6.4 (slog's default
// "time" key is renamed to "ts").
func initLogger() {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Key = "ts"
			}
			return a
		},
	})
	slog.SetDefault(slog.New(handler))
}

func strPtr(s string) *string { return &s }
