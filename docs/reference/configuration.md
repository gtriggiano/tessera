---
description: "Every key of the tessera configuration file, with its type, default and allowed values, and how the file is read."
---

# Configuration reference

**tessera** reads its configuration from one YAML file. This page describes how the file is read and lists every key with its type, its default and the values it allows. The [CLI reference](./cli.md) describes the commands that check and print the file.

::: warning In development
No command serves requests yet: `tessera config validate` and `tessera config print` read and check this file, and nothing else uses it.
:::

## JSON Schema

The schema of the latest release is at `https://gtriggiano.github.io/tessera/config.schema.json`, and the schema of each version at `https://gtriggiano.github.io/tessera/v<major>.<minor>/config.schema.json`:

```yaml
# yaml-language-server: $schema=https://gtriggiano.github.io/tessera/config.schema.json
policy:
  default_action: deny
```

The schema does not check the rules between two keys, nor values that contain `${NAME}`: `tessera config validate` does.

## Loading

### The file

- The command line selects the file: see [Configuration file](./cli.md#configuration-file) in the CLI reference. tessera reads it once.
- The file is a regular file of at most 1 MiB, and holds exactly one YAML document, whose root is a mapping. An empty file, or one with only comments, is an error.

### Format

- Keys are plain strings. A key that this page does not list, and a key repeated in the same mapping, are errors.
- YAML aliases (`*name`), merge keys (`<<`) and custom tags (`!tag`) are errors, so anchors (`&name`) have no use. Standard tags such as `!!str` are accepted when they match the type of the key.
- An explicit null (`key: null`, `key: ~`, or `key:` without a value) is an error. Omit the key to use its default.
- Values are not converted between types:

| Type | Accepted | Rejected |
| --- | --- | --- |
| string | A YAML string. Quote a value that YAML reads as another type: `"8080"`. | `8080` (an integer), `true` (a boolean) |
| integer | A decimal YAML integer, without a sign other than `-` and without leading zeros. | `0x10`, `1_000`, `+5`, `007`, `1.0`, `"16"` |
| duration | A string of decimal numbers, each followed by a unit: `ns`, `us` (or `µs`), `ms`, `s`, `m` or `h`. Fractions (`1.5s`, `.5s`) and a leading sign are accepted, and units combine: `1h30m`. Write zero as `0s`. | `5`, `"0"`, `1d` |

No key accepts a negative duration.

### Environment variables

`${NAME}` in a string or duration value is replaced with the value of the environment variable `NAME`. `NAME` starts with a letter or an underscore, followed by letters, digits or underscores.

```yaml
log:
  level: ${LOG_LEVEL:-info}
```

- `${NAME:-default}` inserts `default` when `NAME` is not set or is empty. The default is inserted as written: it cannot contain `}`, and it is not substituted again.
- `$${` writes a literal `${`. Any other `$` is kept as written: `$HOME` stays `$HOME`, and `$$` stays `$$`.
- A variable that is not set, without a default, is an error that names the variable. A variable set to the empty string inserts an empty string. An unclosed `${` and an invalid name are errors.
- Substitution applies to values only, not to keys or integers.
- It happens after YAML parsing: the inserted text is never read as YAML, and is checked as the value of the key.
- `tessera config print` shows every value that contains a substitution as `[REDACTED]`, even when the substitution inserted its default.

### Relative paths

A relative path is relative to the directory of the configuration file, not to the working directory. The only path today is the socket in [`server.listen`](#server-listen).

### Error messages

An invalid configuration stops the command with exit status 2. The message names the key and, when the key is in the file, its position; an error about the whole file names `configuration` or `configuration file` instead of a key:

```text
tessera: server.drain_delay (line 3, column 16): expected a duration with units, such as 5s
```

- tessera reports the first error it finds.
- A required key that is missing has the position of the nearest block that is in the file, or no position: `policy.default_action: required`.
- A YAML syntax error reports the line where the parser stopped, which can be before the mistake, with column 1: `configuration (line 2, column 1): invalid YAML syntax`. Some syntax errors, such as invalid UTF-8, have no line: `configuration: invalid YAML syntax`.
- An unknown key is reported by its path, unless it is not a lowercase name of letters, digits and underscores: then it is reported as `<unknown>`.
- Messages never repeat a configured value. They name keys, and the variable of a failed substitution.

## `server`

**Type:** mapping\
**Default:** every key has its own default

The gRPC listener for Envoy.

| Key | Type | Default | Allowed |
| --- | --- | --- | --- |
| [`server.listen`](#server-listen) | string | `":9001"` | `host:port`, or `unix:` followed by a path |
| [`server.max_concurrent_streams`](#server-max-concurrent-streams) | integer | `1024` | `1` to `4294967295` |
| [`server.max_connection_age`](#server-max-connection-age) | duration | `0s` | zero or positive |
| [`server.drain_delay`](#server-drain-delay) | duration | `5s` | zero or positive |
| [`server.shutdown_timeout`](#server-shutdown-timeout) | duration | `20s` | positive |

### `server.listen`

**Type:** string\
**Default:** `":9001"`\
**Allowed:** `host:port`, or `unix:` followed by a path

The address of the gRPC listener for Envoy.

- `host:port` listens on TCP. The host is empty, for every address, an IP address, such as `127.0.0.1` or `[::1]`, or a DNS name, such as `localhost`. A name whose last label is all digits, such as `127.1`, is an error: write the address in full. The port is a decimal number from `1` to `65535`, without a sign or leading zeros.
- `unix:` followed by a path listens on a Unix socket: `unix:/run/tessera/grpc.sock`. A relative path is [relative to the configuration file](#relative-paths). The path has no control characters.

On TCP, the address must not compete for a socket with [`admin.listen`](#admin-listen): the two are an error when their ports are equal and their hosts are equal, either host is empty or unspecified (`0.0.0.0`, `::`), or both hosts are loopback (`localhost`, `127.0.0.1`, `::1`).

### `server.max_concurrent_streams`

**Type:** integer\
**Default:** `1024`\
**Allowed:** `1` to `4294967295`

The largest number of concurrent calls on one connection from Envoy.

### `server.max_connection_age`

**Type:** duration\
**Default:** `0s`\
**Allowed:** zero or positive

The age at which a connection from Envoy is closed gracefully, so that Envoy opens a new one and the load spreads across replicas. `0s` sets no limit.

### `server.drain_delay`

**Type:** duration\
**Default:** `5s`\
**Allowed:** zero or positive

At shutdown, the time between reporting not ready and closing the connections.

### `server.shutdown_timeout`

**Type:** duration\
**Default:** `20s`\
**Allowed:** positive

At shutdown, the longest time to finish the calls in progress.

## `admin`

**Type:** mapping\
**Default:** every key has its own default

The listener of the health checks and the metrics.

| Key | Type | Default | Allowed |
| --- | --- | --- | --- |
| [`admin.listen`](#admin-listen) | string | `":9090"` | `host:port` |

### `admin.listen`

**Type:** string\
**Default:** `":9090"`\
**Allowed:** `host:port`

The address of the admin listener, in the `host:port` form of [`server.listen`](#server-listen). A Unix socket is not allowed.

## `log`

**Type:** mapping\
**Default:** every key has its own default

The log events, written to standard error.

| Key | Type | Default | Allowed |
| --- | --- | --- | --- |
| [`log.level`](#log-level) | string | `info` | `debug`, `info`, `warn`, `error` |
| [`log.format`](#log-format) | string | `logfmt` | `logfmt`, `json` |

### `log.level`

**Type:** string\
**Default:** `info`\
**Allowed:** `debug`, `info`, `warn`, `error`

The lowest level of the events that are written.

### `log.format`

**Type:** string\
**Default:** `logfmt`\
**Allowed:** `logfmt`, `json`

The format of the events: `logfmt` writes one line of `key=value` pairs per event, `json` one JSON object per line.

## `policy`

**Type:** mapping\
**Default:** required

How tessera decides.

| Key | Type | Default | Allowed |
| --- | --- | --- | --- |
| [`policy.default_action`](#policy-default-action) | string | required | `allow`, `deny` |

### `policy.default_action`

**Type:** string\
**Default:** required\
**Allowed:** `allow`, `deny`

The decision for a request. It has no default, so that every configuration states whether requests are allowed or denied.
