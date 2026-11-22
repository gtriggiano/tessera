---
description: "Every Prometheus metric that tessera serve exports on the admin listener, with its type and labels."
---

# Metrics reference

`tessera serve` exports Prometheus metrics on the admin listener, at [`/metrics`](./admin-endpoints.md#metrics). This page lists every metric whose name starts with `tessera_`, with its type and its labels, and names the standard Go and process metrics exported next to them. Metric names, types and labels are part of the [public interface](./versioning.md#public-interface) of **tessera**.

| Metric | Type | Labels |
| --- | --- | --- |
| [`tessera_build_info`](#tessera-build-info) | gauge | `version`, `revision`, `go_version` |
| [`tessera_config_info`](#tessera-config-info) | gauge | `hash` |
| [`tessera_grpc_connections_refused_total`](#tessera-grpc-connections-refused-total) | counter | none |

Each metric has one series, from startup. Scrapes read memory only.

## Build and configuration

### `tessera_build_info`

**Type:** gauge\
**Labels:** `version`, `revision`, `go_version`

Always `1`. The labels identify the binary, with the values that [`tessera version`](./cli.md#tessera-version) prints: `version` is `dev` and `revision` is `unknown` when the build did not set them.

```text
tessera_build_info{go_version="go1.27.4",revision="3c9d5e8f6a7b4c2d1e0f9a8b7c6d5e4f3a2b1c0d",version="{{VERSION}}"} 1
```

### `tessera_config_info`

**Type:** gauge\
**Labels:** `hash`

Always `1`. `hash` is the SHA-256, in lowercase hexadecimal, of what [`tessera config print`](./cli.md#tessera-config-print) writes for the configuration in use. Compare it across replicas to check that they all run the same configuration, or compute it yourself:

```sh
tessera config print --config config.yaml | sha256sum
```

```text
tessera_config_info{hash="66a168e11ca8bdba0135ae646185cab69b3492ea9cb88b4c163b11d838816190"} 1
```

`tessera config print` writes every value that contains an [environment variable substitution](./configuration.md#environment-variables) as `[REDACTED]`, so the hash never depends on a secret, and it does not change when only the value of such a variable changes. A relative socket path in [`server.listen`](./configuration.md#server-listen) is printed as an absolute path, which depends on the directory of the configuration file: compute the hash on the file that tessera reads, where it reads it.

## gRPC listener

### `tessera_grpc_connections_refused_total`

**Type:** counter\
**Labels:** none

The connections to the gRPC listener that tessera closed as soon as it accepted them, because [`server.max_connections`](./configuration.md#server-max-connections) connections were already open. A value that grows means that Envoy needs more connections than the limit allows: raise it, or add replicas. tessera writes no log event for a refused connection.

## Go and process metrics

`/metrics` also exports the standard collectors of the Prometheus Go client:

- `go_*`: the Go runtime, such as `go_goroutines`, `go_threads`, `go_info`, `go_gc_duration_seconds` and the `go_memstats_*` memory statistics;
- `process_*`: the process, such as `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds` and `process_start_time_seconds`. Which `process_*` metrics exist depends on the operating system.

There are no metrics about the scrapes themselves.
