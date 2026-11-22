---
description: "Every command of the tessera binary, with its flags, output and exit status."
---

# CLI reference

The **tessera** binary has commands that print help and build information, and commands that check and print a configuration file. It has no command that serves requests yet.

| Command | What it does |
| --- | --- |
| [`tessera`, `tessera help`](#tessera-and-tessera-help) | Print help |
| [`tessera version`](#tessera-version) | Print build information |
| [`tessera config validate`](#tessera-config-validate) | Check a configuration file |
| [`tessera config print`](#tessera-config-print) | Print a configuration file with its defaults applied |

No command opens a network connection. The `config` commands read one file, the configuration, and nothing else.

## Synopsis

```text
tessera [--help]
tessera help [command]
tessera version [--output text|json]
tessera config validate [--config <path>]
tessera config print [--config <path>]
```

## Common behavior

### Flags and arguments

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--config <path>` | `config` and its subcommands | The configuration file. Overrides `TESSERA_CONFIG`. If repeated, the last value wins. |
| `--output <format>` | `version` | `text` (default) or `json`. |
| `-h`, `--help` | Every command | Print the command's help and exit with status 0. |

Only `tessera help` takes positional arguments: the name of a command. An extra argument, including an unknown command name after `tessera` or `tessera config`, fails with `unexpected arguments; see --help`; an unknown flag, or a flag without its value, fails with `invalid command flags; see --help`. Both exit with status 2. Error messages do not repeat the offending argument, which could be a credential pasted by mistake.

### Configuration file

`tessera config validate` and `tessera config print` read exactly one file:

1. the one named by `--config`, when the flag is present;
2. otherwise, the one named by the `TESSERA_CONFIG` environment variable, when it is set.

With neither, the command fails with `no configuration file; use --config or TESSERA_CONFIG`. An empty value fails with `configuration file path must not be empty`. A path that is missing, unreadable or not a regular file, such as a directory or a named pipe, is an error too, and so is an invalid configuration. No other location is searched. The [configuration reference](./configuration.md#loading) describes how the file is read and checked.

### Output streams

- Standard output carries the result of the command: help, build information, `configuration valid` and the printed configuration.
- Standard error carries errors, one line each: `tessera: <message>`.

### Exit status

| Status | Meaning |
| --- | --- |
| 0 | Success. |
| 1 | Runtime failure: the output could not be written. |
| 2 | Usage or configuration error: an unknown command, argument or flag, an invalid flag value, or a configuration file that is not selected, missing, unreadable or invalid. |

### Signals

The binary handles SIGINT and SIGTERM itself. The current commands wait on nothing, so they finish normally when a signal arrives.

## `tessera` and `tessera help`

```sh
tessera
tessera help config validate
```

`tessera`, `tessera --help` and `tessera help` print the general help. `tessera help <command>` prints the help of a command, like `tessera <command> --help`. An unknown command after `help` fails with `unknown command; see tessera --help` and exit status 2. `tessera config` without a subcommand prints the help of `config`.

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
