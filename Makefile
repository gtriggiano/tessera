GO ?= go
GOFMT ?= $(shell $(GO) env GOROOT)/bin/gofmt
NPM ?= npm
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
# Keep equal to the version inputs of goreleaser-action and download-syft in
# .github/workflows/ci.yml and release.yml.
GORELEASER_VERSION := v2.19.0
SYFT_VERSION := v1.55.0
TOOLS_DIR := $(CURDIR)/.cache/tools
GORELEASER := $(TOOLS_DIR)/goreleaser-$(GORELEASER_VERSION)/goreleaser
SYFT_DIR := $(TOOLS_DIR)/syft-$(SYFT_VERSION)
SNAPSHOT_FLAGS ?=
FUZZ_TIME ?= 10s
BENCH_TIME ?= 2s

.PHONY: help build fmt fmt-check vet test test-e2e fuzz bench mod-check check ci vuln workflows-check workflows-audit release-check release-snapshot docs-install docs-audit docs-dev docs-build docs-check docs-preview

help:
	@echo "make check           Check Go formatting, modules, vet, tests and build"
	@echo "make ci              Run Go checks, fuzzing, vulnerability scan, workflow lint, docs build and checks"
	@echo "make build           Build tessera in bin/"
	@echo "make fmt             Format Go sources"
	@echo "make test-e2e        Run tessera in Docker with Envoy and a test upstream (needs Docker)"
	@echo "make fuzz            Run each fuzz target for $(FUZZ_TIME)"
	@echo "make bench           Run the benchmarks of a decision, of the sets, of the areas, of the databases and of the user-agent analysis for $(BENCH_TIME) each"
	@echo "make vuln            Scan Go code using pinned govulncheck"
	@echo "make workflows-check Validate GitHub Actions workflows"
	@echo "make workflows-audit Audit workflows and Dependabot configuration using pinned zizmor (needs uv)"
	@echo "make release-check   Validate the GoReleaser configuration"
	@echo "make release-snapshot Build archives, checksums, SBOMs and images in dist/ without publishing (needs Docker)"
	@echo "make docs-install    Install locked documentation dependencies"
	@echo "make docs-audit      Check documentation dependency advisories"
	@echo "make docs-dev        Serve documentation locally"
	@echo "make docs-build      Build the static documentation site and check its links"
	@echo "make docs-check      Render every built page in Chrome and check its diagrams"
	@echo "make docs-preview    Preview the built documentation site"

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/tessera ./cmd/tessera

fmt:
	@git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "make fmt: not inside a Git work tree; run gofmt -w on the Go files instead" >&2; exit 1; }
	git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 "$(GOFMT)" -w

fmt-check:
	@git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "make fmt-check: not inside a Git work tree; run gofmt -l on the Go files instead" >&2; exit 1; }
	@files=$$(git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 "$(GOFMT)" -l) || exit $$?; if [ -n "$$files" ]; then echo "Go files need formatting:"; echo "$$files"; exit 1; fi

# The end-to-end test builds only with the e2e tag: vet compiles it too.
vet:
	$(GO) vet ./...
	$(GO) vet -tags e2e ./internal/e2e

test:
	$(GO) test -race -count=1 ./...

# Builds an image of tessera from the working tree with the Dockerfile, and
# runs it in Docker with Envoy and a test upstream (internal/e2e).
test-e2e:
	$(GO) test -tags e2e -count=1 -v -timeout 10m ./internal/e2e

# Each fuzz target gets one line, as in:
#	$(GO) test ./internal/<package> -run='^$$' -fuzz=Fuzz<Name> -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2
fuzz:
	$(GO) test ./internal/request -run='^$$' -fuzz=FuzzNormalize -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2
	$(GO) test ./internal/request -run='^$$' -fuzz=FuzzClientIP -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2
	$(GO) test ./internal/sets -run='^$$' -fuzz=FuzzRead -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2
	$(GO) test ./internal/sets -run='^$$' -fuzz=FuzzParseValue -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2
	$(GO) test ./internal/enrichment -run='^$$' -fuzz=FuzzParse -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2
	$(GO) test ./internal/useragent -run='^$$' -fuzz=FuzzAnalyze -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2
	$(GO) test ./internal/area -run='^$$' -fuzz=FuzzReadGeoJSON -fuzztime=$(FUZZ_TIME) -fuzzminimizetime=1x -parallel=2

# The benchmarks of one decision over ten typical rules, alone, after a rule
# on a set of 100,000 and of 1,000,000 networks, after two rules on the
# databases as large as GeoLite2 City and ASN, after a rule on the user-agent
# header with the cache of its analysis warm and cold, after a rule on
# 10,000 geofences, and with upstream headers and decision fields: time,
# memory and allocations per decision, and the percentiles of the time of
# each, from one goroutine and from many at once. Then the lookups in those
# sets, areas and databases, the memory per entry of each type of set and per
# vertex of the areas, and the reading of a set of one million entries, of
# large GeoJSON documents and of those databases, with the memory of one copy
# and the peak during repeated reads; and the analysis of a user-agent
# header with the cache cold and warm, from one goroutine and from many, of
# the slowest headers, and the memory of the cache. One package at a time
# (-p 1), so that their times do not mix.
bench:
	$(GO) test -p 1 ./internal/engine ./internal/sets ./internal/area ./internal/enrichment ./internal/useragent ./internal/source -run='^$$' -bench=. -benchmem -benchtime=$(BENCH_TIME) -count=1

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

# GoReleaser and syft are built once per version into .cache/tools/; GoReleaser
# runs syft from PATH to write the SBOMs of the archives.
$(GORELEASER):
	GOBIN=$(dir $@) $(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

$(SYFT_DIR)/syft:
	GOBIN=$(SYFT_DIR) $(GO) install github.com/anchore/syft/cmd/syft@$(SYFT_VERSION)

release-check: $(GORELEASER)
	$(GORELEASER) check

release-snapshot: $(GORELEASER) $(SYFT_DIR)/syft
	PATH="$(SYFT_DIR):$$PATH" $(GORELEASER) release --snapshot --clean $(SNAPSHOT_FLAGS)

ci: check fuzz vuln workflows-check
	$(MAKE) docs-install
	$(MAKE) docs-audit
	$(MAKE) docs-build
	$(MAKE) docs-check

docs-install:
	$(NPM) --prefix docs ci

docs-audit:
	$(NPM) --prefix docs audit --audit-level=moderate

# The bot names reference is written from the code before VitePress reads the
# pages, with Go (internal/botnames); Git ignores it.
BOT_NAMES_PAGE := docs/reference/bot-names.md

docs-dev:
	$(GO) run ./internal/botnames $(BOT_NAMES_PAGE)
	$(NPM) --prefix docs run docs:dev

docs-build:
	$(GO) run ./internal/botnames $(BOT_NAMES_PAGE)
	$(NPM) --prefix docs run docs:build

docs-check:
	$(NPM) --prefix docs run docs:check

docs-preview:
	$(NPM) --prefix docs run docs:preview
