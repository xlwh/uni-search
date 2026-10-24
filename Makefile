# UniSearch build entrypoints. CGO is prohibited project-wide
# (docs/go-design/00-overview.md §2.1: single static binary, G3).

GO ?= go
WEB_DIR ?= web/admin
ADMIN_DIST ?= internal/server/admin/dist

.PHONY: build build-frontend build-go test lint clean ci coverage cross-build fuzz proto

build: build-frontend build-go

build-frontend:
	@if command -v npm >/dev/null 2>&1; then \
		echo "==> Building frontend..."; \
		cd $(WEB_DIR) && npm ci && npm run build; \
	else \
		echo "==> npm not found, ensuring placeholder dist..."; \
		mkdir -p $(ADMIN_DIST); \
		if [ ! -f $(ADMIN_DIST)/index.html ]; then \
			echo '<!DOCTYPE html><html><body><h1>Frontend not built. Run: make build-frontend</h1></body></html>' > $(ADMIN_DIST)/index.html; \
		fi; \
	fi

build-go:
	CGO_ENABLED=0 $(GO) build ./...

test:
	CGO_ENABLED=0 $(GO) test ./...

lint:
	$(GO) vet ./...

clean:
	rm -f unisearch uniadm *.test *.out *.prof coverage.txt coverage.out coverage.html
	rm -rf dist/
	rm -rf $(ADMIN_DIST)/assets/

ci: lint test build
	@echo "CI: lint + test + build passed"

coverage:
	CGO_ENABLED=0 $(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

cross-build:
	@bash scripts/build.sh

fuzz:
	@echo "==> Running fuzz targets (60s each)..."
	CGO_ENABLED=0 $(GO) test -fuzz=FuzzParseQuery -fuzztime=60s ./internal/engine/dsl/
	CGO_ENABLED=0 $(GO) test -fuzz=FuzzWALReplay -fuzztime=60s ./internal/storage/wal/
	CGO_ENABLED=0 $(GO) test -fuzz=FuzzSegmentFormat -fuzztime=60s ./internal/storage/segment/
	CGO_ENABLED=0 $(GO) test -fuzz=FuzzQueryString -fuzztime=60s ./internal/engine/dsl/

proto:
	cd api/proto && protoc --go_out=gen --go-grpc_out=gen shard.proto meta.proto
