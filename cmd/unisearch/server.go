// Package main server.go assembles the embedded-mode HTTP server:
// meta + embedded engine + esapi router + http.Server with a
// graceful-shutdown signal handler (design 00 §5).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/unisearch-db/unisearch/internal/config"
	"github.com/unisearch-db/unisearch/internal/embedded"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// runEmbedded boots the full single-node stack: meta store, in-
// memory document store, REST router, HTTP listener, and a signal-
// driven graceful shutdown. Returns the process exit code.
func runEmbedded(cfg *config.Config) error {
	slog.Info("opening embedded engine", "data_dir", cfg.Data.Dir)
	engine, err := embedded.New(cfg.Data.Dir)
	if err != nil {
		return fmt.Errorf("open engine: %w", err)
	}
	defer func() { _ = engine.Close() }()

	srv := esapi.NewServer(engine)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout/WriteTimeout intentionally unset: _bulk can be
		// long. IdleTimeout caps idle keepalive conns.
		IdleTimeout: 60 * time.Second,
	}

	// Signal-driven graceful shutdown (design 00 §5): stop accepting
	// new connections, drain in-flight requests, flush docs, close
	// engine. The 10s hard deadline keeps shutdown bounded.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("http listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		slog.Info("shutdown signal received", "signal", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("http shutdown error", "err", err)
	}
	slog.Info("flushing docs")
	if err := engine.Close(); err != nil {
		slog.Error("flush docs error", "err", err)
	}
	slog.Info("shutdown complete")
	return nil
}
