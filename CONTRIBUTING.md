# Contributing

**tessera** is developed at <https://github.com/gtriggiano/tessera>. Report bugs and propose changes there, as issues and pull requests. Everyone who takes part follows the [Code of Conduct](CODE_OF_CONDUCT.md). To report a vulnerability, follow [SECURITY.md](SECURITY.md): do not open an issue.

A bug report is most useful with the output of `tessera version`, the relevant part of the configuration and the log lines; remove credentials and hostnames you do not want to publish.

A change in behavior comes with tests that cover it and with the documentation updated in the same pull request.

## Requirements

- **Go 1.27.4**, as declared in `go.mod`. An installed Go 1.21 or later downloads this toolchain automatically when it has network access, unless `GOTOOLCHAIN=local` is set.
- **A C compiler on Linux and Windows.** `make check` runs the tests with the race detector, which needs cgo there. On macOS it does not.
- **Make and Git.** The Makefile drives every check. `make fmt` and `make check` list the Go sources with Git, so run them in a Git clone: outside a Git work tree, `make fmt` and `make fmt-check` (part of `make check`) fail with `not inside a Git work tree`.
- **Node.js 24 and npm** for the documentation (`docs/.node-version`). Its build needs Go too: `make docs-build` and `make docs-dev` write the bot names reference from the code.
- **uv**, optional, for `make workflows-audit`, which runs [zizmor](https://docs.zizmor.sh/) with `uvx`. CI runs it too, with zizmor's online audits.
- **Docker with buildx**, for `make release-snapshot` and `make test-e2e` only.
- **Google Chrome or Chromium** for `make docs-check`. It uses `CHROME_PATH` or `CHROME_BIN` when set, and otherwise finds Google Chrome or Chromium in the standard install locations on Linux, macOS and Windows. For any other browser binary, set `CHROME_PATH` or `CHROME_BIN`.

The test suite of `make check` needs no credentials, Docker or external service; `make test-e2e` needs Docker, and network access to pull the images. The first build downloads the Go modules, and `make vuln`, `make workflows-check`, `make workflows-audit`, the first `make release-check` or `make release-snapshot` and the first `make docs-install` download their tools or packages: they need network access.

## Repository layout

```text
cmd/tessera/         Command entry point
internal/            Implementation packages (not a public Go API); see the code map below
docs/                Documentation site: guide/, reference/; examples/ holds the configuration examples
docs/scripts/        Link and diagram checks, and the assembly of the site with versions
docs/.vitepress/     Site configuration, navigation and theme
.github/             CI workflows, Dependabot configuration, issue and pull request templates
.goreleaser.yaml     Release packaging: binaries, archives, checksums, SBOMs, image
Dockerfile           Container image, built from the binaries GoReleaser compiles
AGENTS.md            Instructions for AI coding agents; CLAUDE.md imports it for Claude Code
.agents/skills/      Agent skills for contributors; .claude/skills/ links them for Claude Code
```

### Code map

Each package under `internal/` has one role.

| Package | Role |
| --- | --- |
| `cmd/tessera/` | Entry point. |
| `internal/cli/` | Commands: `serve`, `eval`, `help`, `version`, `config validate` and `config print`; the test that runs the `tessera eval` examples of the CLI reference. |
| `internal/config/` | The configuration file: types, defaults, strict reading, checks and printing; the tests that generate its JSON Schema and compare its documentation with the code. |
| `internal/app/` | `serve`: builds the components from a valid configuration, opens the listeners, and shuts down on a signal. |
| `internal/extauthz/` | The adapter between Envoy and tessera: the gRPC server of the external authorization and health services, from a `CheckRequest` to a request and from a decision to a `CheckResponse`, with its upstream headers. The only package that imports Envoy's types. |
| `internal/request/` | The request that tessera decides, independent of Envoy: its normalization, and its client address, read from the source that `client_ip` declares. |
| `internal/expr/` | The expression environment of the conditions, the upstream headers and the decision fields: the CEL types, the `request`, `client`, `sets`, `geo`, `asn` and `ua` attributes, the functions on addresses and sets, the limits, the reads that a decision field cannot make, compile errors with positions and the causes of indeterminate conditions. |
| `internal/engine/` | Decides a request with the ordered rules of the policy, on one snapshot of the sets and one of the databases: indeterminate conditions, audit mode, the default action, the response of a denial, the upstream headers of a request that Envoy sends on, and the decision fields, with what the decision read. Depends on `internal/expr`, `internal/sets`, `internal/enrichment`, `internal/useragent` and `internal/request` only. |
| `internal/sets/` | The sets that conditions read: their types (`cidr`, `asn`, `string`, `area`), the strict parsers of a value and of a file, with their ceilings, the immutable copies, and the store from which each decision takes one snapshot of every set, with its availability. |
| `internal/area/` | The polygons of an area set: the strict reader of a GeoJSON document, which reads it once, in a stream, with its ceilings and the line and the column of each problem, the polygons in flat arrays, the static R-tree packed along a Hilbert curve and the horizontal bands of the large rings, and the lookup of a point, half-open on the edges, without allocations. |
| `internal/enrichment/` | The MMDB databases that `geo` and `asn` read: the strict reading of a database from its content, in memory, with its ceiling and light checks, the records that tessera decodes, the lookups, and the store from which each decision takes one snapshot of the databases. |
| `internal/useragent/` | The analysis of the user-agent header that `ua` reads: its first 512 bytes, with uasurfer and the list of crawler-user-agents, the tables that name the browsers, the operating systems, the kinds of device and the bots, and the cache of the records, sharded, with a fixed number of entries, which every decision shares. |
| `internal/botnames/` | Documentation only: the program that writes the bot names reference, `docs/reference/bot-names.md`, from the names and the descriptions of the bots of `internal/useragent`, which `make docs-build` and `make docs-dev` run before VitePress. It is not part of the binary. |
| `internal/source/` | Reads the content of a set or of a database from a file, through the open descriptor, with the identity of the file it read, and runs the refreshers that check the file of each set and each database, replace its copy in the store and mark a set unavailable when it is stale. |
| `internal/admin/` | The admin listener: `/livez`, `/readyz` and `/metrics`. |
| `internal/metrics/` | Prometheus metrics on a private registry; the test that compares the metrics reference with the registry. |
| `internal/telemetry/` | Log events, in logfmt or JSON: the `decision` event, with its decision fields, and its bounded queue, and the rate-limited reports of dropped events, indeterminate conditions and recovered panics. |
| `internal/mmdbtest/` | Tests only: a writer of the MaxMind DB format, and databases with the schemas of GeoIP2 City and Country and of GeoLite2 ASN, filled with example data, which the tests and the databases of `docs/examples/` are written from, and generated as large as GeoLite2 City and ASN for the benchmarks. |
| `internal/e2e/` | Tests only, behind the `e2e` build tag: Envoy, a tessera image and a test upstream in Docker (`make test-e2e`). |
| `internal/release/` | Tests only: version references in the documentation and the README. |
| `internal/agentdocs/` | Tests only: consistency of `AGENTS.md`, `CLAUDE.md` and the skills. |

## Design rules

These hold across the code base. A change that weakens one needs the maintainer's agreement first: open an issue before the pull request.

1. **A decision does no I/O**. The request path reads memory only. Networks, disks and databases are touched in the background.
2. **Fail closed**. A condition that cannot be evaluated counts against allowing: a `deny` rule denies, an `allow` rule does not match. Missing data never turns a denial into an allow, unless the configuration declares otherwise for that data.
3. **Configuration is immutable; data refreshes.** Everything in the file is built and validated before any socket opens. Sets, databases and certificates are replaced atomically.
4. **Every resource has a configured ceiling.** tessera refuses excess work instead of queueing it.
5. **Nothing a client writes becomes a metric label** or an unbounded cache key.
6. **No secrets or request data in errors.** Configuration errors never repeat configured values.
7. **`internal/` is not a public API.** The public interfaces are the configuration file, the expression language, the CLI, the responses to Envoy, the admin endpoints, metric names and labels, log events and fields, the container image and the release asset names. Changing one of them is a breaking change (see [Commit messages and pull request titles](#commit-messages-and-pull-request-titles)).
8. **The engine does not know Envoy.** It takes a normalized request and returns a decision. The translation to and from `ext_authz` lives in one adapter package.

## Make targets

| Target | What it does |
| --- | --- |
| `make build` | Builds `bin/tessera`. |
| `make check` | Checks Go formatting and module tidiness, runs `go vet` (on the end-to-end test too), runs the tests with the race detector, and builds the binary. |
| `make test-e2e` | Builds a Linux binary and an image of tessera from the working tree with the `Dockerfile`, and runs it in Docker with Envoy and a test upstream: a request through Envoy is denied with 403 or reaches the upstream, as the policy decides, and tessera stops with status 0 on a signal; a policy with rules on hosts, paths and networks lets the allowed requests reach the upstream, sends the configured responses of the denials, names the rule in every `decision` event, keeps the client address when the client writes `X-Forwarded-For`, and lets every request through in audit mode; a file renamed over the file of a set, in a mounted directory, changes the decisions without a restart, and an invalid one changes nothing and is counted as `rejected`; rules on the country and the autonomous system of the client, read from databases in a mounted directory, decide as expected, a database renamed over the file in use changes the next decisions without a restart, and the upstream receives tessera's values of the upstream headers, never those that the client sent, and no header whose value is empty; a rule on the bots, recognized in the user-agent header that Envoy passes, denies them on some pages, the upstream receives tessera's values of the kind of device and of the name of the bot, and a repeated header reaches the analysis joined with a comma; a deny rule on an area set read from a GeoJSON file in a mounted directory, on the location that a database gives to the client address, denies a location inside an area and neither one outside nor an address without a location, a document renamed over the file in use changes the decisions without a restart, and an invalid one changes nothing and is reported in a `set_load_failed` event. Needs Docker; not part of `make check` or `make ci`. |
| `make fmt` | Formats the Go sources. |
| `make fuzz` | Fuzzes each fuzz target for 10 seconds; set `FUZZ_TIME`, for example `make fuzz FUZZ_TIME=30s`. Each fuzz target has its own line in the Makefile. |
| `make bench` | Runs the benchmarks for 2 seconds each (`BENCH_TIME`), one package at a time: one decision over ten typical rules, decided by the last rule and by an early one, over the same rules after a rule on a `cidr` set of 100,000 and of 1,000,000 networks, after two rules that read `geo` and `asn` in databases as large as GeoLite2 City and GeoLite2 ASN, compared with the same decision without them, after a rule that reads `ua`, with the cache of the analysis warm and cold, after a rule on an area set of 10,000 geofences, and with three upstream headers and two decision fields, with the time, memory and allocations per decision and the 50th, 99th and 99.9th percentiles of the time of each decision, from one goroutine and, for some of them, from as many goroutines as processors and sixteen times as many; the lookups of matching and missing IPv4 and IPv6 addresses in those sets, and in those databases with the decoding of their records; the lookups of points inside, near and outside areas of 10,000 and 100,000 geofences and of 16 large polygons; the reading of sets of one million entries of each type, networks of several shapes among them, with the time, the heap that one copy holds, and the peak of the heap while two copies coexist, as during a refresh; the reading of the GeoJSON documents of those areas, with the same measures and the heap per vertex; the reading of a file of one million networks and of the files of those databases, with the same measures; and the analysis of a user-agent header without a cache, with the cache cold and warm, from one goroutine and from many, of headers written to make it slow, and the heap of a full cache. The networks are generated in `240.0.0.0/4` and `2001:db8::/32`; the areas around real places, and the user-agent headers from those of real browsers and programs. Not part of `make check` or `make ci`: compare its results before and after a change to the engine, the expressions, the sets, the areas, the databases or the analysis of the user-agent header, on the same machine. |
| `make vuln` | Scans the Go code with `govulncheck`, at the version pinned in the Makefile. |
| `make workflows-check` | Lints `.github/workflows/*.yml` with `actionlint`, at the version pinned in the Makefile, without ShellCheck. |
| `make workflows-audit` | Audits the workflows and the Dependabot configuration with `zizmor`, at the version pinned in the Makefile, without its online audits. Needs `uvx`. |
| `make release-check` | Validates `.goreleaser.yaml` with GoReleaser, at the version pinned in the Makefile. |
| `make release-snapshot` | Builds what a release would publish into the ignored `dist/` directory, without publishing it: the binaries and archives for Linux and macOS on amd64 and arm64, the checksums, the SBOMs, and one image per platform, loaded into Docker as `ghcr.io/gtriggiano/tessera:<version>-amd64` and `-arm64`. Needs Docker; `make release-snapshot SNAPSHOT_FLAGS=--skip=docker` builds everything except the images. |
| `make docs-install` | Installs the locked documentation dependencies (`npm ci`). |
| `make docs-audit` | Runs `npm audit` on the documentation dependencies, failing at moderate severity or higher. |
| `make docs-dev` | Writes the bot names reference, then serves the documentation with live reload, on `http://127.0.0.1:5173/tessera/` by default. |
| `make docs-build` | Writes the bot names reference, then builds the site into `docs/.vitepress/dist/` and checks its internal links and anchors. |
| `make docs-check` | Renders every page of the last build, except `404.html`, in headless Chrome and checks its diagrams. |
| `make docs-preview` | Serves the built site, on `http://127.0.0.1:4173/tessera/` by default. |
| `make ci` | Runs `check`, `fuzz`, `vuln` and `workflows-check`, then `docs-install`, `docs-audit`, `docs-build` and `docs-check`. |

Run `make check` for every Go change, and `make test-e2e` as well for a change to `serve`, to the responses to Envoy, to the `Dockerfile` or to `internal/e2e/`. Run `make docs-build` then `make docs-check` for every documentation change, and `make check` as well for a change to the configuration reference, the metrics reference, a configuration snippet or `docs/examples/`, which Go tests read. `make ci` runs the checks of the CI and Documentation workflows on your machine, except the Linux cross-compilation, the tests on the other platforms, the end-to-end test, which `make test-e2e` runs, the workflow audit, which `make workflows-audit` runs without the online audits, and the release dry run, which `make release-snapshot` runs.

The `govulncheck`, `actionlint`, `zizmor`, GoReleaser and `syft` versions are Makefile variables, which Dependabot does not update: review and bump them by hand. The last three are also inputs in `.github/workflows/ci.yml` and `.github/workflows/release.yml` (`version` of `zizmor-action` and `goreleaser-action`, `syft-version` of `download-syft`): change them together. `make release-check` and `make release-snapshot` build GoReleaser and `syft` once per version into the ignored `.cache/tools/`.

To keep the Go and npm caches inside the checkout, for example in a restricted environment, point them at the ignored `.cache/` directory:

```sh
export GOMODCACHE="$PWD/.cache/go-mod"
export GOCACHE="$PWD/.cache/go-build"
export npm_config_cache="$PWD/.cache/npm"
```

## Tests

- Cover each behavior change with a behavioral test of the contract: the decisions tessera returns, failure modes and resource limits. A test of the observable result is worth more than a test of internals.
- Keep the suite passing with the race detector (`make check`).
- Fuzz seed cases run with the ordinary tests; `make fuzz` runs the bounded fuzzing.
- Some tests read the documentation: see [Pages guarded by tests](#pages-guarded-by-tests).
- The default suite tests the gRPC and admin listeners with clients in the same process, on loopback. The end-to-end test, `make test-e2e`, runs the image with a real Envoy in Docker, and is a job of its own in CI.
- Never put production credentials, `.env` files or real hostnames and addresses in fixtures. Use `example.com` hostnames, the documentation IP ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 and 2001:db8::/32) and the reserved AS numbers 64496 to 64511. A benchmark that needs more IPv4 networks than the documentation ranges hold generates them in 240.0.0.0/4, which is reserved for future use. Two exceptions: coordinates and polygons can be those of real places, and a `user-agent` header can be the real header of a browser or a program, URLs included.

## Documentation

The documentation is a [VitePress](https://vitepress.dev/) site in `docs/`.

### Content

- The code is the source of truth. Every statement about behavior, and every diagram, must match what the code does today. Do not document planned behavior.
- Write in US English, in the second person and the present tense, with sentence-case headings and the imperative for steps. The name is tessera, always lowercase: set it in bold at its first mention in the prose of each page, and in code formatting only for the command or the image. No superlatives and no "simply", "just" or "easy". Make no performance claims.
- Use only public interfaces in the user documentation: configuration keys, expression language attributes and functions, CLI commands and flags, responses to Envoy, log events and fields, metric names and labels, admin endpoints. No Go package, type or test names.
- The site follows [Diátaxis](https://diataxis.fr/): `docs/guide/` holds the how-to guides and `docs/reference/` lists every field, command, response, log event and metric. Keep each fact in one home and link to it from the other pages.
- Add a new page to the sidebar in `docs/.vitepress/navigation.ts`.
- A page names the release it documents with the placeholder `{{VERSION}}`, in an image tag `ghcr.io/gtriggiano/tessera:{{VERSION}}`, a download URL or an asset name. The build replaces it with the release that the site documents, so readers see commands they can paste as they are. Do not write the version itself, a `VERSION` shell variable, or an image tag that is `latest` or a minor version. A file included with `<<<` is not replaced: keep version references out of included files.

### Formatting

- Write each paragraph and each list item of a Markdown file on one line, and let your editor wrap it for display: never break a line in the middle of a sentence. Use a hard line break, a `\` at the end of a line, only where the layout needs one, as between the `**Type:**` and `**Default:**` lines of the configuration reference.
- Write a configuration key with its full dotted path at its first mention on a page, and values in code (`30s`, `128MiB`, `true`).
- Link between pages with relative links to `.md` files, to a heading anchor when one exists: `[public interface](../reference/versioning.md#public-interface)`.
- Put commands in `sh` blocks without a prompt, and their output in `text` blocks.
- Use example data only: `example.com` hostnames, the documentation IP ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32), RFC 1918 ranges for proxies and the reserved AS numbers 64496 to 64511. Two exceptions: coordinates and polygons can be those of real places, and a `user-agent` header can be the real header of a browser or a program, URLs included. Never credentials.
- Use `::: tip` for advice, `::: warning` for surprising behavior and `::: danger` for a security impact.

### Pages guarded by tests

`make check` runs Go tests that read the documentation:

- **Configuration reference.** Every configuration key has its own heading in `docs/reference/configuration.md`, the exact key path in backticks, followed by its `**Type:**` and `**Default:**` lines and, when validation narrows the type, an `**Allowed:**` line. The key of an item of a list is written with `[]` after the list, as in `policy.rules[].name`, and the test checks it in a list of one item. The default is a YAML value in backticks, `{}` for a block, `none` for an optional block, which is absent unless the file writes it, `required`, ``required when `<key>` is `<value>` `` for a key that another key's value makes required, or ``inherited from `<key>` `` for a key that takes the value of another when the file omits it; the test checks such a key with the other key set. The allowed line starts, where it applies, with a form that the test checks against the code: a list of values (`` `allow`, `deny` ``), a range (`` `1` to `3` ``), `positive`, `zero or positive` or `nonempty`; other text follows after `; `. Every integer and duration has an allowed line that starts with a form. Each top-level section has a table of its keys, after its description, that repeats these lines. The test fails on a missing or unknown key, on a documented type, default or allowed form that differs from the code, on a required key or a list of values that differs from the configuration schema, on a string that rejects its empty value and has no allowed line, and on a table that differs from the entries. Text after `; `, the allowed line of a string when it is free text, and the prose are not checked: verify them against the code.
- **Metrics reference.** Every metric whose name starts with `tessera_` has its own heading in `docs/reference/metrics.md`, the name in backticks, followed by its `**Type:**` and `**Labels:**` lines (`none`, or the label names in backticks), a `**Buckets:**` line for a histogram (the upper bounds in seconds, in backticks), and a row in the table at the top of the page. `TestMetricsReference` fails on a metric that the code registers and the page does not document, on a documented metric that the code does not export, and on a type, a label, the buckets or a row that differs. The `change-metric` skill describes the procedure.
- **`tessera eval` examples.** `TestDocumentedEval` runs every `sh` block of `docs/reference/cli.md` that starts with `tessera eval` and is followed by a `text` or `json` block, with the file of `docs/examples/` that its `--config` names, and fails when the output differs from that block.
- **Configuration snippets.** A tessera configuration snippet in `docs/` is a fence whose info string is exactly `yaml [config.yaml]`. The test validates every such snippet, with every environment variable unset; a partial fragment is completed with a `policy.default_action`. Write that title nowhere else in `docs/`, not even in prose, and give every other YAML fence another title, such as `yaml [envoy.yaml]`: the test fails on a YAML fence without a title, which would escape validation.
- **Order of the keys of a rule.** Every rule of a `yaml [config.yaml]` snippet, of `docs/examples/` and of `internal/e2e/testdata/` writes its keys in the order `name`, `description`, `action`, `mode`, `when`, `response`, skipping the absent ones, and the keys of `response` in the order `status`, `body`, `headers`. It is the order of the fields of the rule type in the code, which `tessera config print`, the JSON Schema and the configuration reference follow too. `TestRuleKeyOrder` fails on a rule in another order. tessera reads the keys in any order: the order is a convention of the examples.
- **Configuration schema.** `docs/public/config.schema.json`, the JSON Schema that the site publishes for editors and other tools, is generated from the configuration types, the defaults and the notes in `internal/config/json_schema_test.go`. `TestJSONSchema` fails when the file differs: add a note for a new key, then regenerate the file with `go test ./internal/config -run TestJSONSchema -update`. Two more tests check that the schema accepts every example and configuration snippet of the documentation, and that each kind of rule in it rejects a file that tessera rejects too. The schema states only what a file states on its own: leave out a rule that involves two keys or the environment.
- **Examples.** `TestRepositoryExamples` loads and validates every `.yaml` file in `docs/examples/` as a complete configuration, with every environment variable unset, reads the files of its sets (`.txt`, or `.geojson` for an area set) and its databases (`.mmdb`) as `tessera serve` does, fails on such a file that no example reads, and requires the first line of a configuration to be `# yaml-language-server: $schema=../public/config.schema.json`: an editor then checks the example against the schema of the working tree. The databases are written from the example data of the tests: `TestExampleDatabases` fails when one differs, and `go test ./internal/config -run TestExampleDatabases -update` writes them again. Pages include the files with `<<< @/examples/<name>.yaml{yaml}` instead of copying them, and the build replaces that path with the address of the schema in the version of the site it builds (`docs/.vitepress/schema-path.ts`); a relative schema path that does not lead to a file in `docs/public/` fails the build. The release workflow and the release dry run check `docs/examples/minimal.yaml` with `tessera config validate` in the image.

### Diagrams

Diagrams are Mermaid, in ` ```mermaid ` fences.

- Every diagram declares `accTitle` and `accDescr`, or the build fails. The description is not shown on the page: screen readers read it in place of the diagram, so it must summarize the diagram on its own. Introduce each diagram with a sentence before it.
- Use no `style`, `classDef` or colors: the theme provides them. A frontmatter `config` block is allowed for layout settings only, such as the width of a sequence diagram.
- Aim for the 670 px content width: top-down flowcharts of about 10 nodes at most, and sequence diagrams of at most 4 participants with short messages.

### Build and checks

- `make docs-build` fails on a link to a missing page and on a `#fragment` that matches no element id in the target page. It also fails on an address of the published site, `https://gtriggiano.github.io/tessera/...`, written in a page, that is not a file of the build.
- `make docs-check` serves the build and renders every page except `404.html` in headless Chrome, in the light and dark themes, at desktop and mobile widths. It fails when a diagram does not render, on a console or page error, on an HTTP error, and when a page is wider than the viewport. A wide diagram scrolls inside its own frame.
- The bot names reference, `docs/reference/bot-names.md`, lists every name that `ua.bot.name` can give, with its description. `make docs-build` and `make docs-dev` write it before VitePress reads the pages, with `go run ./internal/botnames`, from the tables of `internal/useragent` and the list of crawler-user-agents of `go.mod`, so it follows the code without a change. Git ignores it: do not edit it, and change its text in `internal/botnames`. The tests of `make check` pass with and without it, and the workflows that build the site install Go for it.
- The build also writes the pages for AI agents ([llmstxt.org](https://llmstxt.org/)): a Markdown twin of every page at the path of its source, such as `guide/what-is-tessera.md`, and `llms.txt`, an index of the twins in the order of the sidebar, with the first paragraph of each page as its description. Every HTML page links to both from its `<head>`. `docs/.vitepress/llms.ts` rewrites what only VitePress understands (frontmatter, `<<<` and `<!--@include-->` files, `{{VERSION}}`, the relative schema paths of included files, containers and code groups) and fails the build on a construct it does not know, such as a new container type: teach it there. Give a page `llms: optional` in its frontmatter to list it in the section that an agent can skip, and a `description` when its first paragraph does not describe it.
- The site base is `/tessera/`. Build for another base with `DOCS_BASE`, for example `DOCS_BASE=/ make docs-build`, and run `make docs-check` with the same value.
- The published site has a folder per released version (see [RELEASING.md](RELEASING.md#the-documentation-site)). Two more variables shape a build: `DOCS_SITE_ROOT`, the root of the whole site, which defaults to `DOCS_BASE`, and `DOCS_VERSION`, the release that the build documents, empty for local builds. With `DOCS_VERSION` set, the pages name that version instead of the one in `.release-please-manifest.json`: it must match `.release-please-manifest.json`, unless it is a release candidate, whose documentation is built from `main`. A build whose `DOCS_BASE` is below `DOCS_SITE_ROOT` is an archived version: it is not indexed by search engines, has no "Edit this page" link, and shows a banner when a newer version exists. Check that variant with, for example, `DOCS_SITE_ROOT=/tessera/ DOCS_BASE=/tessera/v0.1/ make docs-build`, then `make docs-check` with the same variables. The version selector reads `versions.json` from the site root; `docs/public/versions.json` is an empty placeholder that the deploy replaces.
- The documentation dependencies are pinned in `docs/package.json` and locked in `docs/package-lock.json`. Commit both together, and install with `make docs-install`. An npm override in `docs/package.json` selects Vite 6.4.6, outside the Vite range that VitePress 1.6.4 declares. Check `make docs-dev`, `make docs-build` and `make docs-preview` whenever you change the override or upgrade VitePress, and remove the override once VitePress no longer needs it.

## Commit messages and pull request titles

Pull requests are merged with squash merge only: each one becomes a single commit on `main`, whose message is the pull request title (GitHub appends the pull request number) and whose body is the pull request description. [release-please](https://github.com/googleapis/release-please) reads these commits to compute the next version and to write `CHANGELOG.md`, so the title follows [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/):

```text
<type>(<optional scope>)<optional !>: <description>
```

The **PR title** check rejects any other title, any type not in this table and a description that starts with an uppercase letter or ends with a period, and the pull request cannot be merged until it passes.

| Type | Use it for | Version bump | Changelog section |
| --- | --- | --- | --- |
| `feat` | A capability users can see: a configuration key, a command, a metric, a header | minor | Features |
| `fix` | A correction of wrong behavior | patch | Bug Fixes |
| `perf` | A performance improvement with no change in behavior | patch | Performance Improvements |
| `deps` | A dependency update that changes the binary or the image: Go modules, the Go version, the base image | patch | Dependencies |
| `revert` | The revert of an earlier change | patch | Reverts |
| `docs` | Documentation only | none | hidden |
| `refactor`, `test`, `build`, `ci`, `chore` | Internal work that changes nothing tessera distributes | none | hidden |

Only the visible types, and breaking changes of any type, make release-please propose a release: a series of `docs:` and `ci:` commits alone does not. Among the commits since the last release, the highest bump wins.

- **Breaking changes.** A change that breaks a public interface (the configuration file, the expression language, the CLI, the responses to Envoy, the admin endpoints, metric names and labels, log events and fields, the container image or the release asset names) takes a `!` after the type or scope, as in `feat(config)!: rename old_name to new_name`. A breaking change to the configuration renames or removes a key, and never gives an existing key a new meaning: tessera then rejects an old file instead of reading it differently. Explain the migration in a footer: end the description with a paragraph that starts with `BREAKING CHANGE: ` followed by the instructions, which appear in the changelog, and write nothing after it.
- **Lines that release-please reads anywhere in the description.** A line that starts with `BREAKING CHANGE:`, or that contains `BREAKING-CHANGE:` anywhere, marks the change as breaking wherever it is, and a paragraph that starts with a type and a colon, such as `fix: `, becomes an extra changelog entry with its own bump. Start such lines only when you mean them.
- **Scopes** are optional and not checked. The suggested ones follow the public areas: `config`, `cli`, `admin`, `metrics`, `logs`, `image`, `docs`.
- **The description** is the changelog entry: write it in the imperative, starting with a lowercase letter and without a final period.
- **Reverts.** GitHub's **Revert** button proposes the title `Revert "fix(cli): …"`, which the check rejects: rename it to `revert: fix(cli): …`.

The pull request template repeats these rules. Dependabot titles already follow them (see [Dependabot](#dependabot)).

## AI coding agents

`AGENTS.md` condenses this guide for AI coding agents: the commands, the checks to run for each kind of change, the design rules and the conventions. Codex, Antigravity, Cursor, GitHub Copilot and other tools read it directly; Claude Code reads it through `CLAUDE.md`, which imports it. Procedures for recurring tasks, such as preparing a pull request, are skills in `.agents/skills/`, in the [Agent Skills](https://agentskills.io/) format. Claude Code does not read that directory, so each skill has a symbolic link in `.claude/skills/` to its directory in `.agents/skills/`. `.claude/settings.json` lets Claude Code run the Make checks without asking and keeps its file tools away from the local credential files.

- **Keep them in step with this guide.** A pull request that changes a rule here also updates `AGENTS.md` and the skills that state it. This guide wins when they disagree.
- **Add a skill** in `.agents/skills/<name>/SKILL.md`, link it for Claude Code with `ln -s ../../.agents/skills/<name> .claude/skills/<name>`, and list it in the table at the end of `AGENTS.md`.
- **Symbolic links on Windows.** Git checks the links out as plain text files unless the clone supports symbolic links: turn on Developer Mode, or work as an administrator, and clone with `git clone -c core.symlinks=true`. Without them Claude Code does not find the skills; the other tools and the tests are not affected.
- **Tests guard them.** `make check` runs the tests in `internal/agentdocs`. They fail when a skill in `.agents/skills/` has no link in `.claude/skills/` or its link leads elsewhere, when `AGENTS.md` does not list a skill, and when `CLAUDE.md` does not import `AGENTS.md`. They also fail when one of these files names something that does not exist: a path under `cmd/`, `internal/`, `docs/`, `.github/`, `.agents/` or `.claude/`, a Go file, a Make target, a test, or a relative link or its anchor. Other names, such as Go identifiers, are not checked.
- **Personal instructions stay local.** Git ignores `CLAUDE.local.md`, `.claude/settings.local.json` and `AGENTS.override.md`.

## Releases

Maintainers release tessera by merging the Release PR that release-please keeps open; the Release workflow then builds, verifies and publishes the archives, the container image and the documentation site. [RELEASING.md](RELEASING.md) describes the process. A contribution needs nothing beyond a correct pull request title: its changelog entry and its effect on the version come from it.

## Continuous integration

| Workflow | Runs on | Jobs |
| --- | --- | --- |
| PR title (`.github/workflows/pr-title.yml`) | Pull requests: opened, edited, reopened, and on every new commit | **PR title**: checks the title against Conventional Commits, the allowed types and the form of the description. It checks out nothing and reads only the title through the API, which the read-only token of a pull request from a fork allows. |
| CI (`.github/workflows/ci.yml`) | Pull requests, pushes to `main`, manual runs | **Go checks**: `make check`, `make fuzz`, a Linux `amd64` and `arm64` cross-compilation without cgo, `make workflows-check`. **Envoy end-to-end**: `make test-e2e`. **Go tests (linux/arm64)** and **Go tests (macOS)**: `make check` on an arm64 Linux runner and on a macOS runner. **Release dry run**: `goreleaser check`, then a snapshot build of every archive, SBOM and per-platform image, and a run of the `linux/amd64` image with `version` and with `config validate` on `docs/examples/minimal.yaml`. **Workflow audit**: `zizmor` on the workflows and the Dependabot configuration, with its online audits; it is not a required check, because a new advisory about an action can turn it red without any change in the pull request, but fix what it reports before merging. **Go vulnerabilities**: `make vuln`. |
| Documentation (`.github/workflows/docs.yml`) | Pull requests, pushes to `main`, manual runs, and the Release workflow | **Build documentation** (pull requests and pushes): `make docs-install`, `make docs-audit`, `make docs-build` and `make docs-check` with the runner's Chrome, the same for an archived version, and a test of `docs/scripts/assemble-site.mjs` on those builds. **Assemble documentation** and **Deploy documentation** (deploys only): the site root built from the latest release, the older versions from the documentation archives of the releases, and the publication to GitHub Pages. |
| Security (`.github/workflows/security.yml`) | Every week, manual runs | **Go vulnerabilities (main)**: `make vuln`. **Latest release**: finds the latest stable release, not counting release candidates. **Go vulnerabilities (latest release)**: `make vuln` on its tag. **Image scan (latest release)**: Grype on its `linux/amd64` image, with the results in the repository's Security tab. Before the first release, only the scan of `main` runs. |
| Scorecard (`.github/workflows/scorecard.yml`) | Pushes to `main`, every week | **Scorecard analysis**: [OpenSSF Scorecard](https://github.com/ossf/scorecard), with the results published for the README badge and in the repository's Security tab. |
| Release (`.github/workflows/release.yml`) | Pushes to `main`, manual runs | **Release PR**: release-please, with the token of the release App. **Release candidate**: manual runs with **candidate** selected. **Prepare**, **Build**, **Documentation archive**, **Verify (linux/amd64)**, **Verify (linux/arm64)**, **Publish** and **Documentation site** run only for a new tag, or for a manual run on a tag. See [RELEASING.md](RELEASING.md). |

The Release workflow publishes the documentation site after each release; the site is never built from `main`. To publish it again, run the Documentation workflow on `main` with **deploy** selected. The site root is `/<repository name>/`, or the repository variable `DOCS_BASE` when it is set.

Actions are pinned to commit SHAs, checkouts do not persist credentials, and every workflow starts with no permissions; each job gets only the ones it needs, read-only unless stated here. In the Release workflow, the jobs that build, upload and publish a release can write the repository contents and the packages, and the build job requests an OpenID Connect token to sign the attestations; the Release PR and the release candidate use the token of the release App. The documentation deploy can write to Pages, and the image scan and Scorecard can upload results to the Security tab; Scorecard also requests an OpenID Connect token to publish its results.

## Dependabot

Dependabot checks the Go modules, the documentation's npm packages (version updates grouped in one pull request) and the GitHub Actions every week, and the base image of the `Dockerfile` and the Envoy image of the end-to-end test (`internal/e2e/testdata/Dockerfile`) every month. It proposes a new version only once it has been published for 7 days; security updates are not delayed. Its titles follow the [commit message format](#commit-messages-and-pull-request-titles): `deps:` for the Go modules and the base image, which make the Release PR propose a patch release, and `docs(deps-dev):` for the documentation packages, `ci:` for the actions and `test:` for the Envoy image, which do not. Review each update and run the checks it affects; commit npm updates together with `docs/package-lock.json`. An update of `github.com/LumenResearch/uasurfer` or `github.com/monperrus/crawler-user-agents` can fail the tests of `internal/useragent`: a new browser, operating system or kind of device of uasurfer needs a name in its tables; a new pattern of the list of crawlers that is not plain text needs a name in `crawlerPatternNames`, the one of the product or of its operator, as its owner writes it, from the entry's description, URL and examples in the list; an entry of `crawlerPatternNames` or `crawlerAliases` whose pattern or name the list no longer has must go; a new name that differs from another bot name, of the list or of uasurfer, only in case, spaces, `-` and `_` needs one spelling, in `crawlerAliases` or `crawlerPatternNames`, or a name of its own when it is another bot; and a name that is not a valid value of a `string` set, such as text with a `#`, which starts a comment in the file of a set, needs a name in `crawlerPatternNames`. A new pattern that is plain text needs nothing else: its text is its name, even when it is a host name or a URL, unless tessera gives it a name of its own in `crawlerPatternNames`, as it does for the bots of well-known platforms. The names of the browsers, of the operating systems and of the bots of uasurfer are listed in the [expressions reference](docs/reference/expressions.md#ua), which a test compares with the tables. The bot names reference is written from the code at each build of the site, so it follows the update without a change.

tessera reads the descriptions of the bots, which `ua.bot.description` gives, from the JSON that `github.com/monperrus/crawler-user-agents` embeds, its unexported variable `crawlersJson`, through a `//go:linkname` directive in `internal/useragent/descriptions.go`: the module gives no other access to them. Neither the compiler nor the linker checks the directive, and a later Go toolchain could refuse a `//go:linkname` to a package outside the standard library, which would show as a failure to link on the pull request that brings it. If a version of the module renames the variable, the directive links to an empty slice without an error; if it changes its type, the build does not fail either. When the variable is renamed, or retyped so that tessera cannot read its bytes as the JSON of the list, tessera starts and analyzes headers all the same, with every description empty, and the tests that read the descriptions fail, `TestCrawlerDescriptions` first of all, so `make check` fails on the Dependabot pull request: that test, which matches the entries of the JSON with the crawlers of the module one to one, in order, is the guard. Then point the directive at the variable that now holds the JSON, a `[]byte`; or, if the module now gives the description of each crawler, read it from there and remove the directive; if neither is possible, close the pull request, which keeps the previous version of the module. A version of the list that rewords a description fails no test of the behavior, which take the descriptions from the list, but fails `TestDocumentedEval` when an `eval` example of `docs/reference/cli.md` shows that description: update the example.

tessera pins revision 1 of the cost model of CEL, in `internal/expr/expr.go`: the [cost limit](docs/reference/expressions.md#limits) is a number of its units, and a later revision, which cel-go uses by default, must not change silently which conditions pass it. `TestCosts`, which fixes the cost of representative conditions, `TestCostLimitBoundary` and `TestCostModelRevision`, which checks the pin, fail on the Dependabot pull request of an update of `cel.dev/cel-go` that changes how tessera counts, by its revision or otherwise. Adopt another revision, or other costs, only deliberately, as a change of the expression language: update the pin, the expected costs and the expressions reference, and mark the change as breaking after the first release when it changes which conditions pass the limit. That holds in either direction, as the [public interface](docs/reference/versioning.md#public-interface) says: a condition beyond the limit is indeterminate, so a lower cost can make a `deny` rule stop denying, and a higher one can make it start. tessera evaluates two calls in place of the interpreter of cel-go, `inCidr` and `contains` on an area set: each records its own cost, if any, in the cost tracker of the evaluation, and answers both when the interpreter executes it in a frame and when it evaluates it with an activation, as it does for the condition of `? :`; a change of that interface fails the build or the tests of `internal/expr`, `TestCosts` and `TestConditionalOnPreparedCalls` among them. Run `make bench` before and after the update: cel-go evaluates every condition.
