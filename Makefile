GO ?= go
GOFMT ?= $(shell $(GO) env GOROOT)/bin/gofmt
BUILD_DIR ?= bin
# Build identity for make build: the commit and its date, in UTC. Override on
# the command line only (make build VERSION=...), not from the environment.
VERSION = dev
REVISION = $(shell git rev-parse --verify --quiet HEAD 2>/dev/null || echo unknown)
BUILD_TIME = $(shell TZ=UTC git log -1 --no-show-signature --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd 2>/dev/null)
LDFLAGS = -X 'main.version=$(VERSION)' -X 'main.revision=$(REVISION)' -X 'main.buildTime=$(BUILD_TIME)'

.PHONY: help build fmt fmt-check vet test mod-check check

help:
	@echo "make check           Check Go formatting, modules, vet, tests and build"
	@echo "make build           Build tessera in bin/"
	@echo "make fmt             Format Go sources"

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/tessera ./cmd/tessera

fmt:
	@git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "make fmt: not inside a Git work tree; run gofmt -w on the Go files instead" >&2; exit 1; }
	git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 "$(GOFMT)" -w

fmt-check:
	@git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "make fmt-check: not inside a Git work tree; run gofmt -l on the Go files instead" >&2; exit 1; }
	@files=$$(git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 "$(GOFMT)" -l) || exit $$?; if [ -n "$$files" ]; then echo "Go files need formatting:"; echo "$$files"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -race -count=1 ./...

mod-check:
	$(GO) mod tidy -diff

check: fmt-check mod-check vet test build
