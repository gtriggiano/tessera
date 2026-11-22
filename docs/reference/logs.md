---
description: "The log events of tessera serve: their format, their levels, and every event with its fields."
---

# Log reference

`tessera serve` writes log events to standard output, one event per line. This page describes the format of the lines, the levels, and every event that **tessera** writes, with its fields. Event names and fields are part of the [public interface](./versioning.md#public-interface).

## Format

[`log.format`](./configuration.md#log-format) selects the format. A run of `tessera serve` that a signal stops, in the default `logfmt` format:

```text
ts=2026-11-22T10:15:00.124Z level=info msg=starting version={{VERSION}} revision=3c9d5e8f6a7b4c2d1e0f9a8b7c6d5e4f3a2b1c0d config_path=/etc/tessera/config.yaml
ts=2026-11-22T10:15:00.126Z level=info msg=listening listener=grpc address=[::]:9001
ts=2026-11-22T10:15:00.126Z level=info msg=listening listener=admin address=[::]:9090
ts=2026-11-22T10:15:00.126Z level=info msg=ready
ts=2026-11-22T11:02:41.518Z level=info msg=shutdown_started reason=signal
ts=2026-11-22T11:02:41.518Z level=info msg=not_ready
ts=2026-11-22T11:02:46.521Z level=info msg=shutdown_complete outcome=drained
```

The same first event in the `json` format:

```json
{"ts":"2026-11-22T10:15:00.124Z","level":"info","msg":"starting","version":"{{VERSION}}","revision":"3c9d5e8f6a7b4c2d1e0f9a8b7c6d5e4f3a2b1c0d","config_path":"/etc/tessera/config.yaml"}
```

- **One event per line.** `logfmt` writes `key=value` pairs, `json` one JSON object.
- **Fixed start.** Every event starts with `ts`, `level` and `msg`, followed by its fields in the order of this page.
- **Time.** `ts` is the time of the event in UTC, in RFC 3339 with milliseconds.
- **Name.** `msg` is the name of the event: a stable name in `snake_case`, not a sentence.
- **Values.** In `logfmt`, a value that contains a space, `=`, `"` or a control character is quoted and escaped, so that it never starts a new line.

Standard output carries nothing but these events, and once the configuration is read tessera writes nothing to standard error: it discards the messages that its gRPC and HTTP servers would write there. The exception is a crash: the Go runtime writes the report of a panic or of a fatal error to standard error, in its own format, as the process ends. An error found before the configuration is read, such as an invalid file, is a `tessera: <message>` line on standard error (see [Output streams](./cli.md#output-streams)).

If standard output is closed or cannot be written, the events are lost and tessera goes on serving.

## Levels

Each event has a level: `debug`, `info`, `warn` or `error`. [`log.level`](./configuration.md#log-level) sets the lowest level written, `info` by default. tessera writes no `debug` events.

## Events

| Event | Level | When |
| --- | --- | --- |
| [`starting`](#starting) | `info` | `tessera serve` has read a valid configuration. |
| [`listening`](#listening) | `info` | Both listeners are open: one event for each. |
| [`ready`](#ready) | `info` | tessera reports ready. |
| [`not_ready`](#not-ready) | `info` | tessera stops reporting ready, at the start of a shutdown. |
| [`shutdown_started`](#shutdown-started) | `info` | A shutdown starts. |
| [`shutdown_complete`](#shutdown-complete) | `info` or `warn` | The shutdown ends, as the last event before the process exits. |
| [`startup_failed`](#startup-failed) | `error` | A listener could not open. |
| [`listener_failed`](#listener-failed) | `error` | A listener stopped while tessera was running or shutting down. |

[Signals of `serve`](./cli.md#signals-of-serve) describes when each event of a shutdown happens.

### `starting`

The first event of `tessera serve`, written before any listener opens. It is the only event that names the version.

| Field | Value |
| --- | --- |
| `version` | The release, as [`tessera version`](./cli.md#tessera-version) prints it: `dev` for a binary not built for a release. |
| `revision` | The commit the binary was built from, or `unknown`. |
| `config_path` | The path of the configuration file, as `--config` or `TESSERA_CONFIG` gave it. |

### `listening`

One event for the gRPC listener, then one for the admin listener, once both are open.

| Field | Value |
| --- | --- |
| `listener` | `grpc` or `admin`. |
| `address` | The address the listener is bound to: `host:port`, such as `[::]:9001` for the default `:9001`, or `unix:` followed by the path of the socket. |

### `ready`

tessera reports ready: [`/readyz`](./admin-endpoints.md#readyz) answers 200, and the [gRPC health service](./grpc-api.md#health-service) `SERVING`. It follows the `listening` events. No fields.

### `not_ready`

tessera stops reporting ready: `/readyz` answers 503, and the gRPC health service `NOT_SERVING`. It follows `shutdown_started`. No fields.

### `shutdown_started`

| Field | Value |
| --- | --- |
| `reason` | `signal`: SIGTERM or SIGINT. `listener_failed`: a listener failed, as [`listener_failed`](#listener-failed) reported. |

### `shutdown_complete`

The last event of `tessera serve`. Its level is `info` when every call finished, and `warn` when the shutdown was forced.

| Field | Value |
| --- | --- |
| `outcome` | `drained`: every call and admin request in progress finished within [`server.shutdown_timeout`](./configuration.md#server-shutdown-timeout). `forced`: the remaining ones were cancelled and their connections closed. |
| `forced_by` | With `outcome=forced` only. `timeout`: `server.shutdown_timeout` passed. `signal`: another signal arrived during the shutdown. |

The exit status is 0 after `outcome=drained` and 1 after `outcome=forced` (see [Exit status of `serve`](./cli.md#exit-status-of-serve)).

### `startup_failed`

A listener could not open, and `tessera serve` exits with status 1. When it is the admin listener, the gRPC listener, already open, is closed first.

```text
ts=2026-11-22T10:15:00.126Z level=error msg=startup_failed listener=admin address=:9090 error="address already in use"
```

| Field | Value |
| --- | --- |
| `listener` | `grpc` or `admin`. |
| `address` | The address of the listener, as the configuration writes it. |
| `error` | Why it could not open: the reason that the operating system gives, such as `address already in use`, `permission denied` or `cannot assign requested address`; `cannot resolve the host` when the host name has no address; `timed out` when opening both listeners took more than 10 seconds; `interrupted by a signal` when a signal arrived while the host name was being resolved; otherwise `cannot open`. |

A file that already exists at the path of a Unix socket fails with `address already in use`.

### `listener_failed`

A listener stopped accepting connections, and `tessera serve` exits with status 1. While tessera serves, [`shutdown_started`](#shutdown-started) with `reason=listener_failed` follows. During a shutdown, the event comes once the listeners are closed, before `shutdown_complete`.

| Field | Value |
| --- | --- |
| `listener` | `grpc` or `admin`. |

## What is never logged

- Request data: tessera writes no event per request.
- Configuration values, except the addresses of the listeners and the path of the configuration file.
