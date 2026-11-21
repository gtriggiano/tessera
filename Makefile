GO ?= go
GOFMT ?= $(shell $(GO) env GOROOT)/bin/gofmt
BUILD_DIR ?= bin
# Build identity for make build: the commit and its date, in UTC. Override on
# the command line only (make build VERSION=...), not from the environment.
VERSION = dev
REVISION = $(shell git rev-parse --verify --quiet HEAD 2>/dev/null || echo unknown)
BUILD_TIME = $(shell TZ=UTC git log -1 --no-show-signature --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd 2>/dev/null)
LDFLAGS = -X 'main.version=$(VERSION)' -X 'main.revision=$(REVISION)' -X 'main.buildTime=$(BUILD_TIME)'
GOVULNCHECK_VERSION := v1.9.0
ACTIONLINT_VERSION := v1.7.12
# Keep equal to the version input of zizmor-action in .github/workflows/ci.yml.
ZIZMOR_VERSION := 1.32.0
UVX ?= uvx
FUZZ_TIME ?= 10s

.PHONY: help build fmt fmt-check vet test fuzz mod-check check ci vuln workflows-check workflows-audit

help:
	@echo "make check           Check Go formatting, modules, vet, tests and build"
	@echo "make ci              Run Go checks, fuzzing, vulnerability scan and workflow lint"
	@echo "make build           Build tessera in bin/"
	@echo "make fmt             Format Go sources"
	@echo "make fuzz            Run each fuzz target for $(FUZZ_TIME) (none yet)"
	@echo "make vuln            Scan Go code using pinned govulncheck"
	@echo "make workflows-check Validate GitHub Actions workflows"
	@echo "make workflows-audit Audit workflows and Dependabot configuration using pinned zizmor (needs uv)"

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

# Each fuzz target gets one line, as in:
#	$(GO) test ./internal/<package> -run='^$$' -fuzz=Fuzz<Name> -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2
fuzz:
	@echo "make fuzz: no fuzz targets yet"

mod-check:
	$(GO) mod tidy -diff

check: fmt-check mod-check vet test build

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

workflows-check:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -shellcheck= .github/workflows/*.yml

workflows-audit:
	@command -v $(UVX) >/dev/null 2>&1 || { echo "make workflows-audit: $(UVX) not found; install uv from https://docs.astral.sh/uv/" >&2; exit 1; }
	$(UVX) zizmor@$(ZIZMOR_VERSION) --offline .

ci: check fuzz vuln workflows-check
