#!/usr/bin/env bash
# Quick build & deploy script for UniSearch.
# Compiles the project, deploys the binary + config to a target directory,
# and (re)starts the service.
#
# Usage:
#   ./scripts/deploy.sh                  # build + deploy + restart
#   ./scripts/deploy.sh --build-only     # build only, no deploy
#   ./scripts/deploy.sh --restart-only   # just restart the running service
#   ./scripts/deploy.sh --stop           # stop the service
#   ./scripts/deploy.sh --status         # check service status
#
# Config via environment variables (or defaults below):
#   DEPLOY_DIR   — target directory (default: /Users/zhb/soft/unisearch)
#   LISTEN_ADDR  — listen address (default: :9200)
#   DATA_DIR     — data directory (default: $DEPLOY_DIR/data)
#   LOG_DIR      — log directory (default: $DEPLOY_DIR/logs)
#   EXTRA_ARGS   — extra CLI args appended to the start command
#
set -euo pipefail

# ── Defaults ─────────────────────────────────────────────────────────────
DEPLOY_DIR="${DEPLOY_DIR:-/Users/zhb/soft/unisearch}"
LISTEN_ADDR="${LISTEN_ADDR:-:9200}"
DATA_DIR="${DATA_DIR:-$DEPLOY_DIR/data}"
LOG_DIR="${LOG_DIR:-$DEPLOY_DIR/logs}"
EXTRA_ARGS="${EXTRA_ARGS:-}"

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BINARY_NAME="unisearch"
PID_FILE="$DEPLOY_DIR/$BINARY_NAME.pid"
LOG_FILE="$LOG_DIR/unisearch.log"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; }

# ── Helpers ──────────────────────────────────────────────────────────────

get_pid() {
    if [[ -f "$PID_FILE" ]]; then
        local pid
        pid=$(cat "$PID_FILE" 2>/dev/null || echo "")
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            echo "$pid"
            return 0
        fi
    fi
    # Fallback: pgrep
    pgrep -f "$DEPLOY_DIR/$BINARY_NAME" 2>/dev/null || true
    return 1
}

stop_service() {
    local pid
    pid=$(get_pid) || true
    if [[ -z "$pid" ]]; then
        info "Service is not running."
        return 0
    fi
    info "Stopping service (PID $pid)..."
    kill "$pid" 2>/dev/null || true
    # Wait up to 10 seconds for graceful shutdown
    for i in $(seq 1 10); do
        if ! kill -0 "$pid" 2>/dev/null; then
            break
        fi
        sleep 1
    done
    # Force kill if still running
    if kill -0 "$pid" 2>/dev/null; then
        warn "Graceful shutdown timed out, sending SIGKILL..."
        kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$PID_FILE"
    info "Service stopped."
}

start_service() {
    mkdir -p "$DATA_DIR" "$LOG_DIR"

    info "Starting service..."
    info "  Binary:  $DEPLOY_DIR/$BINARY_NAME"
    info "  Listen:  $LISTEN_ADDR"
    info "  Data:    $DATA_DIR"
    info "  Log:     $LOG_FILE"

    cd "$DEPLOY_DIR"
    nohup "./$BINARY_NAME" \
        --embedded \
        --data-dir "$DATA_DIR" \
        --listen "$LISTEN_ADDR" \
        $EXTRA_ARGS \
        > "$LOG_FILE" 2>&1 &
    disown $! 2>/dev/null || true

    local pid=$!
    echo "$pid" > "$PID_FILE"
    info "Service started (PID $pid)."

    # Wait for health check
    info "Waiting for health check..."
    for i in $(seq 1 30); do
        if curl -sf "http://localhost:${LISTEN_ADDR#:}/_cluster/health" >/dev/null 2>&1; then
            info "Service is healthy!"
            return 0
        fi
        sleep 1
    done
    warn "Health check timed out. Check logs: $LOG_FILE"
    return 1
}

build_project() {
    info "Building UniSearch..."
    cd "$PROJECT_ROOT"

    # Run go vet first
    info "Running go vet..."
    if ! go vet ./... 2>&1; then
        error "go vet failed!"
        return 1
    fi

    # Build for the host platform (native binary, not cross-compiled)
    info "Compiling (native: $(uname -s)/$(uname -m))..."
    CGO_ENABLED=0 go build \
        -ldflags "-X main.version=$(git describe --tags --always 2>/dev/null || echo dev)-$(date +%Y%m%d%H%M%S)" \
        -o "$BINARY_NAME" \
        ./cmd/unisearch/

    if [[ ! -f "$BINARY_NAME" ]]; then
        error "Build failed: binary not found!"
        return 1
    fi

    info "Build successful: $(ls -lh "$BINARY_NAME" | awk '{print $5}') $BINARY_NAME"
}

deploy_files() {
    info "Deploying to $DEPLOY_DIR..."
    mkdir -p "$DEPLOY_DIR" "$DATA_DIR" "$LOG_DIR"

    # Copy binary
    cp "$PROJECT_ROOT/$BINARY_NAME" "$DEPLOY_DIR/$BINARY_NAME"
    chmod +x "$DEPLOY_DIR/$BINARY_NAME"

    # Copy config files if they exist
    for f in config.yaml docker-compose.yml Dockerfile; do
        if [[ -f "$PROJECT_ROOT/$f" ]]; then
            cp "$PROJECT_ROOT/$f" "$DEPLOY_DIR/$f"
        fi
    done

    info "Files deployed."
}

show_status() {
    local pid
    pid=$(get_pid) || true
    if [[ -z "$pid" ]]; then
        info "Service is NOT running."
        return 1
    fi
    info "Service is running (PID $pid)."
    # Health check
    if curl -sf "http://localhost:${LISTEN_ADDR#:}/_cluster/health" >/dev/null 2>&1; then
        info "Health: $(curl -sf "http://localhost:${LISTEN_ADDR#:}/_cluster/health" 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("status","unknown"))' 2>/dev/null || echo 'ok')"
    else
        warn "Health check failed."
    fi
    # Show recent log lines
    if [[ -f "$LOG_FILE" ]]; then
        info "Recent logs:"
        tail -5 "$LOG_FILE" 2>/dev/null || true
    fi
}

show_help() {
    cat << 'EOF'
Usage: ./scripts/deploy.sh [OPTION]

Options:
  (default)       Build → Deploy → Stop old → Start new
  --build-only    Only compile the project (no deploy)
  --restart-only  Only restart the running service (no build)
  --stop          Stop the running service
  --status        Show service status and recent logs
  --help          Show this help message

Environment variables:
  DEPLOY_DIR    Target directory (default: /Users/zhb/soft/unisearch)
  LISTEN_ADDR   Listen address (default: :9200)
  DATA_DIR      Data directory (default: $DEPLOY_DIR/data)
  LOG_DIR       Log directory (default: $DEPLOY_DIR/logs)
  EXTRA_ARGS    Extra CLI args for the service

Examples:
  ./scripts/deploy.sh
  DEPLOY_DIR=/opt/unisearch ./scripts/deploy.sh
  ./scripts/deploy.sh --status
  ./scripts/deploy.sh --stop
EOF
}

# ── Main ─────────────────────────────────────────────────────────────────

case "${1:-}" in
    --help|-h)
        show_help
        ;;
    --build-only)
        build_project
        ;;
    --restart-only)
        stop_service
        start_service
        ;;
    --stop)
        stop_service
        ;;
    --status)
        show_status
        ;;
    "")
        info "=== UniSearch Quick Deploy ==="
        info ""
        # Step 1: Build
        build_project || exit 1
        info ""
        # Step 2: Deploy files
        deploy_files
        info ""
        # Step 3: Stop old service (if running)
        stop_service
        info ""
        # Step 4: Start new service
        start_service || warn "Service may need a moment to start up."
        info ""
        info "=== Deploy complete ==="
        info "Endpoint: http://localhost:${LISTEN_ADDR#:}"
        info "Health:   curl http://localhost:${LISTEN_ADDR#:}/_cluster/health"
        info "Logs:     tail -f $LOG_FILE"
        info "Stop:     ./scripts/deploy.sh --stop"
        info "Status:   ./scripts/deploy.sh --status"
        ;;
    *)
        error "Unknown option: $1"
        show_help
        exit 1
        ;;
esac
