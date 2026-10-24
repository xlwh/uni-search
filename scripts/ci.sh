#!/usr/bin/env bash
# CI entry point — platform-agnostic (design 06 §6).
# Runs: lint → test → build. Exits non-zero on any failure.
set -euo pipefail

echo "=== CI: lint ==="
CGO_ENABLED=0 go vet ./...
if command -v golangci-lint &>/dev/null; then
  golangci-lint run ./...
else
  echo "[warn] golangci-lint not installed, skipping"
fi

echo "=== CI: test ==="
CGO_ENABLED=0 go test -race ./...

echo "=== CI: build ==="
CGO_ENABLED=0 go build ./...

echo "=== CI: all passed ==="
