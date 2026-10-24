#!/usr/bin/env bash
# Cross-compilation build script (design 06 §6).
# Produces static binaries for linux/amd64, linux/arm64, darwin/arm64.
set -euo pipefail

VERSION="${VERSION:-dev}"
DIST_DIR="${DIST_DIR:-dist}"
mkdir -p "$DIST_DIR"

TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/arm64"
)

for target in "${TARGETS[@]}"; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  output="$DIST_DIR/unisearch-${GOOS}-${GOARCH}"
  echo "Building $output ..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags "-X main.version=${VERSION}" -o "$output" ./cmd/unisearch
done

echo "Cross-build complete: ${TARGETS[*]}"
