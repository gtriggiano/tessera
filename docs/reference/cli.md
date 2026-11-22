---
description: "Every command of the tessera binary, with its flags, output and exit status."
---

# CLI reference

The **tessera** binary has a command that answers Envoy's authorization requests, commands that check and print a configuration file, and commands that print help and build information.

| Command | What it does |
| --- | --- |
| [`tessera serve`](#tessera-serve) | Answer Envoy's authorization requests |
| [`tessera config validate`](#tessera-config-validate) | Check a configuration file |
| [`tessera config print`](#tessera-config-print) | Print a configuration file with its defaults applied |
| [`tessera version`](#tessera-version) | Print build information |
| [`tessera`, `tessera help`](#tessera-and-tessera-help) | Print help |

Only `tessera serve` opens sockets. The `config` commands read one file, the configuration, and nothing else.

## Synopsis

```text
tessera serve [--config <path>]
tessera config validate [--config <path>]
tessera config print [--config <path>]
tessera version [--output text|json]
tessera help [command]
tessera [--help]
```

## Common behavior

### Flags and arguments

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--config <path>` | `serve`, `config` and its subcommands | The configuration file. Overrides `TESSERA_CONFIG`. If repeated, the last value wins. |
| `--output <format>` | `version` | `text` (default) or `json`. |
| `-h`, `--help` | Every command | Print the command's help and exit with status 0. |

Only `tessera help` takes positional arguments: the name of a command. An extra argument, including an unknown command name after `tessera` or `tessera config`, fails with `unexpected arguments; see --help`; an unknown flag, or a flag without its value, fails with `invalid command flags; see --help`. Both exit with status 2. Error messages do not repeat the offending argument, which could be a credential pasted by mistake.

### Configuration file

`tessera serve`, `tessera config validate` and `tessera config print` read exactly one file:

1. the one named by `--config`, when the flag is present;
2. otherwise, the one named by the `TESSERA_CONFIG` environment variable, when it is set.

With neither, the command fails with `no configuration file; use --config or TESSERA_CONFIG`. An empty value fails with `configuration file path must not be empty`. A path that is missing, unreadable or not a regular file, such as a directory or a named pipe, is an error too, and so is an invalid configuration. No other location is searched. The [configuration reference](./configuration.md#loading) describes how the file is read and checked.

### Output streams

- Standard output carries the result of the command: help, build information, `configuration valid`, the printed configuration, and the [log events](./logs.md) of `tessera serve`, its errors included.
- Standard error carries the errors of the command line, one line each: `tessera: <message>`. `tessera serve` writes there only the errors found before it reads its configuration.

### Exit status

| Status | Meaning |
| --- | --- |
| 0 | Success. For `tessera serve`, a shutdown in which every call finished. |
| 1 | Runtime failure: the output could not be written, or `tessera serve` could not open a listener, a listener failed, or its shutdown was forced (see [Exit status of `serve`](#exit-status-of-serve)). |
| 2 | Usage or configuration error: an unknown command, argument or flag, an invalid flag value, or a configuration file that is not selected, missing, unreadable or invalid. |

### Signals

The binary handles SIGINT and SIGTERM itself. `tessera serve` shuts down on the first signal and closes every connection at once on another (see [Signals of `serve`](#signals-of-serve)). The other commands wait on nothing, so they finish normally when a signal arrives.

## `tessera serve`

```sh
tessera serve --config config.yaml
```

Answers Envoy's external authorization requests on the gRPC listener, at [`server.listen`](./configuration.md#server-listen), and serves the health checks and the metrics on the admin listener, at [`admin.listen`](./configuration.md#admin-listen). Every request gets the decision of [`policy.default_action`](./configuration.md#policy-default-action). The [gRPC API reference](./grpc-api.md) describes the services and the responses, and the [admin endpoints reference](./admin-endpoints.md) the admin listener. In the container image, `serve` is the default command.

### Startup

1. Reads the [selected file](#configuration-file) and checks it like `tessera config validate`. An error here prints `tessera: <message>` and exits with status 2, before anything opens.
2. Logs [`starting`](./logs.md#starting).
3. Opens the gRPC listener, then the admin listener, within 10 seconds for both, the time to resolve their host names included. If one cannot open, or does not open in time, the other is closed, [`startup_failed`](./logs.md#startup-failed) is logged and the exit status is 1. A signal stops a listener that is still resolving its host name in the same way.
4. Logs [`listening`](./logs.md#listening) for each listener, reports ready, on `/readyz` and through the gRPC health service, logs [`ready`](./logs.md#ready) and starts answering.

### Output

`serve` writes its [log events](./logs.md) to standard output, one line each, in the format of [`log.format`](./configuration.md#log-format), and nothing else. An error before the configuration is read, which has no format yet, is a plain `tessera: <message>` line on standard error. A standard output that is closed or cannot be written loses the events, and does not stop `serve` or change its exit status.

### Signals of `serve`

SIGTERM or SIGINT starts a graceful shutdown:

1. [`shutdown_started`](./logs.md#shutdown-started) is logged. `/readyz` answers 503 and the gRPC health service `NOT_SERVING`, and [`not_ready`](./logs.md#not-ready) is logged.
2. For [`server.drain_delay`](./configuration.md#server-drain-delay), both listeners keep answering as before, so that Envoy and the orchestrator see the change and stop sending requests.
3. The gRPC listener closes, every client is asked to close its connection, and the calls in progress finish, for at most [`server.shutdown_timeout`](./configuration.md#server-shutdown-timeout). A connection whose client has sent nothing is closed at once.
4. The admin listener closes, within the same deadline. A connection that has not sent a request is closed at once.
5. [`shutdown_complete`](./logs.md#shutdown-complete) is logged with `outcome=drained`.

If the deadline passes before the calls and admin requests in progress finish, they are cancelled and every connection is closed: `shutdown_complete` reports `outcome=forced forced_by=timeout`. Another signal during the shutdown does the same at once, at any step: `shutdown_complete` reports `outcome=forced forced_by=signal`. A listener that fails while tessera runs logs [`listener_failed`](./logs.md#listener-failed) and starts the same shutdown; one that fails during the shutdown is logged at its end, and the exit status is 1. Any other signal received during the startup starts the shutdown as soon as both listeners are open.

::: tip
Give the orchestrator a grace period longer than `server.drain_delay` plus `server.shutdown_timeout` between its signal and a forced kill: 25 seconds with the defaults.
:::

### Exit status of `serve`

| Situation | Logged event | Exit status |
| --- | --- | --- |
| Shutdown after a signal, every call finished | `shutdown_complete outcome=drained` | 0 |
| Shutdown cut by `server.shutdown_timeout` | `shutdown_complete outcome=forced forced_by=timeout` | 1 |
| Shutdown cut by another signal | `shutdown_complete outcome=forced forced_by=signal` | 1 |
| A listener could not open | `startup_failed` | 1 |
| A listener failed while serving or during the shutdown | `listener_failed` | 1 |
| Invalid usage or configuration | None: `tessera: <message>` | 2 |

## `tessera config validate`

```sh
tessera config validate --config config.yaml
```

Reads the [selected file](#configuration-file), checks it as the [configuration reference](./configuration.md) describes, and prints `configuration valid`:

```text
configuration valid
```

An invalid configuration prints its first error on standard error and exits with status 2:

```text
tessera: policy.default_action (line 2, column 19): expected allow or deny
```

[Error messages](./configuration.md#error-messages) describes their form.

## `tessera config print`

```sh
tessera config print --config config.yaml
```

Reads and checks the [selected file](#configuration-file) like `tessera config validate`, then prints the configuration as YAML, with every default applied and the keys in the order of the [configuration reference](./configuration.md). For a file that sets only `policy.default_action`:

```text
server:
  listen: :9001
  max_connections: 1024
  max_concurrent_streams: 1024
  max_connection_age: 0s
  drain_delay: 5s
  shutdown_timeout: 20s
admin:
  listen: :9090
log:
  level: info
  format: logfmt
policy:
  default_action: deny
```

- A value that contains an [environment variable substitution](./configuration.md#environment-variables) is printed as `'[REDACTED]'`.
- A literal `${` in a value is printed as `$${`, so that the value reads back as written.
- Durations are printed in a normal form: `90s` is printed as `1m30s`, `30m` as `30m0s`, and `500ms` as `500ms`.
- A relative socket path in `server.listen` is printed as an absolute path.

The output is meant for reading. Because of the redacted values, it is not always a valid configuration.

## `tessera version`

```sh
tessera version
tessera version --output json
```

Prints the build information of the binary:

```text
tessera {{VERSION}} (revision 3c9d5e8f6a7b4c2d1e0f9a8b7c6d5e4f3a2b1c0d; go1.27.4; built 2026-11-21T10:15:00Z)
```

```json
{"version":"{{VERSION}}","revision":"3c9d5e8f6a7b4c2d1e0f9a8b7c6d5e4f3a2b1c0d","go_version":"go1.27.4","build_time":"2026-11-21T10:15:00Z"}
```

| JSON field | Content |
| --- | --- |
| `version` | The release, without a leading `v`. `dev` for a binary not built for a release. |
| `revision` | The commit the binary was built from. `unknown` when the build does not set it. |
| `go_version` | The Go version that compiled the binary. |
| `build_time` | The date of that commit, in UTC. Absent when the build does not set it, and then the text form has no `built` part. |

The JSON form is one object on one line. An `--output` value other than `text` or `json` fails with `invalid --output; want text or json` and exit status 2.

## `tessera` and `tessera help`

```sh
tessera
tessera help config validate
```

`tessera`, `tessera --help` and `tessera help` print the general help. `tessera help <command>` prints the help of a command, like `tessera <command> --help`. An unknown command after `help` fails with `unknown command; see tessera --help` and exit status 2. `tessera config` without a subcommand prints the help of `config`.
