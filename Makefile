# UniSearch build entrypoints. CGO is prohibited project-wide
# (docs/go-design/00-overview.md §2.1: single static binary, G3).

GO ?= go

.PHONY: build test lint clean

build:
	CGO_ENABLED=0 $(GO) build ./...

test:
	CGO_ENABLED=0 $(GO) test ./...

lint:
	$(GO) vet ./...

clean:
	rm -f unisearch uniadm *.test *.out *.prof coverage.txt coverage.out coverage.html
