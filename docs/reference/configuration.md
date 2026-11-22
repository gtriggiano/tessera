---
description: "Every key of the tessera configuration file, with its type, default and allowed values, and how the file is read."
---

# Configuration reference

**tessera** reads its configuration from one YAML file. This page describes how the file is read and lists every key with its type, its default and the values it allows. The [CLI reference](./cli.md) describes the commands that read the file: `tessera serve`, which reads it once at startup, and the commands that check and print it.

## JSON Schema

The schema of the latest release is at `https://gtriggiano.github.io/tessera/config.schema.json`, and the schema of each version at `https://gtriggiano.github.io/tessera/v<major>.<minor>/config.schema.json`:

<<< @/examples/minimal.yaml{yaml}

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
| expression | A string that holds an expression of the [expression language](./expressions.md). Write it in a literal block (`\|`), which YAML never reads otherwise, or in quotes, when YAML would read it as something else: when it starts with a character that YAML reserves (`!`, `&`, `*`, `\|`, `>`, `'`, `"`, `%`, `@`, a backtick, `#`, `,`, `[`, `]`, `{`, `}`, or `-`, `?` or `:` followed by a space), when it contains `: ` or ` #`, or when it is `true` or `false`. | `true` (a boolean), `!has(client.ip)` (a tag) |
| list | A YAML sequence of mappings. | A mapping, a string |
| mapping of strings | A YAML mapping from strings to strings. | A list, a value that is not a string |

No key accepts a negative duration.

### Environment variables

`${NAME}` in a string or duration value is replaced with the value of the environment variable `NAME`. `NAME` starts with a letter or an underscore, followed by letters, digits or underscores.

```yaml [config.yaml]
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
- The path of an item of a list has its index, from 0: `policy.rules[2].name`. An error about a key of a mapping of strings, such as a header name, names the mapping and has the position of the key.
- Messages never repeat a configured value. They name keys, and the variable of a failed substitution. The one exception is an error in an expression, which can name an identifier or a type of the expression, such as `undefined field 'hots'`, but never a literal of it: a syntax error has only its position (see [`policy.rules[].when`](#policy-rules-when)).

## `server`

**Type:** mapping\
**Default:** `{}`

The gRPC listener for Envoy. The [gRPC API reference](./grpc-api.md) describes what it serves.

| Key | Type | Default | Allowed |
| --- | --- | --- | --- |
| [`server.listen`](#server-listen) | string | `":9001"` | `host:port`, or `unix:` followed by a path |
| [`server.max_connections`](#server-max-connections) | integer | `1024` | `1` to `65536` |
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
- `unix:` followed by a path listens on a Unix socket: `unix:/run/tessera/grpc.sock`. A relative path is [relative to the configuration file](#relative-paths). The path has no control characters, does not start with `@`, since abstract sockets are not supported, and once absolute is at most 107 bytes long on Linux and 103 on macOS. tessera creates the socket when it starts and removes it when it stops; a file that already exists at the path stops the startup.

On TCP, the address must not compete for a socket with [`admin.listen`](#admin-listen): the two are an error when their ports are equal and their hosts are equal, either host is empty or unspecified (`0.0.0.0`, `::`), or both hosts are loopback (`localhost`, `127.0.0.1`, `::1`).

### `server.max_connections`

**Type:** integer\
**Default:** `1024`\
**Allowed:** `1` to `65536`

The largest number of open connections from Envoy to the gRPC listener, those still in their handshake included. tessera refuses the excess instead of letting it wait: a connection beyond the limit is closed as soon as it is accepted, before any HTTP/2 exchange. Envoy sees the connection close at once and fails the call as it fails any call that cannot reach tessera (see [What Envoy needs](./grpc-api.md#what-envoy-needs)), and [`tessera_grpc_connections_refused_total`](./metrics.md#tessera-grpc-connections-refused-total) counts the refusal.

Envoy opens about one HTTP/2 connection to tessera per worker thread, so a replica of tessera needs about the number of worker threads of all the Envoy instances that reach it, with room for the connections of Envoy instances that start while others stop.

### `server.max_concurrent_streams`

**Type:** integer\
**Default:** `1024`\
**Allowed:** `1` to `4294967295`

The largest number of concurrent calls on one connection from Envoy. tessera announces it to Envoy when the connection opens, and refuses a call beyond it (see [Limits](./grpc-api.md#limits)).

### `server.max_connection_age`

**Type:** duration\
**Default:** `0s`\
**Allowed:** zero or positive

The age at which a connection from Envoy is closed gracefully, so that Envoy opens a new one and the load spreads across replicas. tessera adds a random variation of up to 10%, asks Envoy to close the connection, and lets the calls in progress on it finish. `0s` sets no limit.

### `server.drain_delay`

**Type:** duration\
**Default:** `5s`\
**Allowed:** zero or positive

At shutdown, the time between reporting not ready and closing the gRPC listener. During this time tessera keeps answering on both listeners, so that Envoy and the orchestrator see that it is not ready and stop sending it requests. See [Signals of `serve`](./cli.md#signals-of-serve).

### `server.shutdown_timeout`

**Type:** duration\
**Default:** `20s`\
**Allowed:** positive

At shutdown, the longest time to finish the calls in progress, counted from the end of [`server.drain_delay`](#server-drain-delay). It bounds the admin requests in progress too. When it passes, tessera cancels what is left, closes every connection and exits with status 1.

## `admin`

**Type:** mapping\
**Default:** `{}`

The listener of the health checks and the metrics. The [admin endpoints reference](./admin-endpoints.md) describes what it serves.

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
**Default:** `{}`

The log events, written to standard output. The [log reference](./logs.md) lists every event.

| Key | Type | Default | Allowed |
| --- | --- | --- | --- |
| [`log.level`](#log-level) | string | `info` | `debug`, `info`, `warn`, `error` |
| [`log.format`](#log-format) | string | `logfmt` | `logfmt`, `json` |
| [`log.decisions`](#log-decisions) | string | `deny` | `none`, `deny`, `all` |

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

### `log.decisions`

**Type:** string\
**Default:** `deny`\
**Allowed:** `none`, `deny`, `all`

The decisions that tessera reports with a [`decision`](./logs.md#decision) event:

| Value | Decisions logged |
| --- | --- |
| `none` | None. |
| `deny` | Every denial, also one that [`policy.mode: audit`](#policy-mode) does not enforce, and every allow in which a rule in `audit` mode with `action: deny` matched. |
| `all` | Every decision. |

[`log.level`](#log-level) applies too: a `decision` event has the level `info` when its action is `allow`, and `warn` when it is `deny`, so `log.level: warn` leaves out the allows.

## `client_ip`

**Type:** mapping\
**Default:** `{}`

Where tessera reads the address of the client that sent a request. tessera reads it from one source, the one that [`client_ip.from`](#client-ip-from) selects, and never from another: when that source gives no address, the request has no client address, even when another source has one. Conditions read the address as [`client.ip`](./expressions.md#attributes), absent when the request has none. [Set the client address](../guide/client-address.md) explains which source to choose for where Envoy runs.

| Key | Type | Default | Allowed |
| --- | --- | --- | --- |
| [`client_ip.from`](#client-ip-from) | string | `peer` | `peer`, `header`, `x_forwarded_for` |
| [`client_ip.header`](#client-ip-header) | string | required when `client_ip.from` is `header` | a header name; only when `client_ip.from` is `header` |
| [`client_ip.trusted_hops`](#client-ip-trusted-hops) | integer | required when `client_ip.from` is `x_forwarded_for` | zero or positive; only when `client_ip.from` is `x_forwarded_for` |

### `client_ip.from`

**Type:** string\
**Default:** `peer`\
**Allowed:** `peer`, `header`, `x_forwarded_for`

The source of the client address:

| Value | The client address is |
| --- | --- |
| `peer` | The address of the connection that Envoy received the request on, which Envoy sends in the check request. tessera reads no header. |
| `header` | The value of the header that [`client_ip.header`](#client-ip-header) names. |
| `x_forwarded_for` | An entry of the `X-Forwarded-For` header, counted from the right end: see [`client_ip.trusted_hops`](#client-ip-trusted-hops). |

Each source gives an address in one of these forms, with any spaces or tabs around it:

- an IPv4 or IPv6 address: `203.0.113.10`, `2001:db8::10`;
- an IPv4 address with a port: `203.0.113.10:4711`;
- an IPv6 address in brackets, with or without a port: `[2001:db8::10]`, `[2001:db8::10]:443`.

tessera drops the port, and reads an IPv4-mapped IPv6 address, such as `::ffff:203.0.113.10`, as the IPv4 address `203.0.113.10`. Anything else gives no address, for example an empty value, a host name, `unknown`, an obfuscated identifier such as `_hidden`, the form of the `Forwarded` header `for=203.0.113.10`, an IPv4 address with leading zeros such as `203.0.113.010`, an IPv4 address in brackets, an IPv6 address with a zone such as `fe80::1%eth0`, and the unspecified addresses `0.0.0.0` and `::`.

### `client_ip.header`

**Type:** string\
**Default:** required when `client_ip.from` is `header`\
**Allowed:** a header name; only when `client_ip.from` is `header`

The header that holds the client address, such as `x-client-address`. tessera compares the name without regard to case. The name is an HTTP field name: letters, digits and the characters ``!#$%&'*+-.^_`|~``; a pseudo-header such as `:authority` is an error.

The header holds exactly one address, in a form that [`client_ip.from`](#client-ip-from) lists. A header that is missing or empty gives no client address, and so does a header that a request repeats: Envoy joins the values of a repeated header with commas, and a list is not one address.

::: danger Trust only a header that a proxy sets
tessera takes the address in the header as it arrives. Name a header that Envoy, or a proxy in front of Envoy that every request passes through, sets on every request, replacing any value that the client sent. A header that the client can write lets the client choose its address.
:::

### `client_ip.trusted_hops`

**Type:** integer\
**Default:** required when `client_ip.from` is `x_forwarded_for`\
**Allowed:** zero or positive; only when `client_ip.from` is `x_forwarded_for`

The number of entries at the right end of `X-Forwarded-For` that proxies you trust appended after the client address. tessera splits the header at its commas, skips this number of entries from the right end, and reads the next entry; it never reads the entries further left, which the client can write. `0` reads the last entry.

With `trusted_hops: 1`, the header `192.0.2.66, 203.0.113.10, 198.51.100.5` gives the client address `203.0.113.10`.

A header that is missing, or that has no more than `trusted_hops` entries, gives no client address, and so does an entry in that position that is not an address in a form that [`client_ip.from`](#client-ip-from) lists: tessera does not look for another entry. A request with several `X-Forwarded-For` headers has one list, since Envoy joins their values with commas.

## `policy`

**Type:** mapping\
**Default:** required

How tessera decides. tessera evaluates the rules in the order of [`policy.rules`](#policy-rules): the first whose condition is true decides, and a request that no rule decides gets [`policy.default_action`](#policy-default-action). [Write rules](../guide/write-rules.md) explains how to build a policy, and the [expression language reference](./expressions.md) describes the conditions.

| Key | Type | Default | Allowed |
| --- | --- | --- | --- |
| [`policy.mode`](#policy-mode) | string | `enforce` | `enforce`, `audit` |
| [`policy.default_action`](#policy-default-action) | string | required | `allow`, `deny` |
| [`policy.deny_response`](#policy-deny-response) | mapping | `{}` |  |
| [`policy.deny_response.status`](#policy-deny-response-status) | integer | `403` | `400` to `599` |
| [`policy.deny_response.body`](#policy-deny-response-body) | string | `""` | at most 8 KiB |
| [`policy.deny_response.headers`](#policy-deny-response-headers) | mapping of strings | `{}` | header names and values |
| [`policy.rules`](#policy-rules) | list | `[]` | at most 1000 rules |
| [`policy.rules[].name`](#policy-rules-name) | string | required | lowercase letters, digits and hyphens; unique, at most 63 characters, not `default` |
| [`policy.rules[].description`](#policy-rules-description) | string | `""` | at most 1024 bytes |
| [`policy.rules[].action`](#policy-rules-action) | string | required | `allow`, `deny` |
| [`policy.rules[].mode`](#policy-rules-mode) | string | `enforce` | `enforce`, `audit` |
| [`policy.rules[].when`](#policy-rules-when) | expression | `"true"` | a condition of at most 8192 characters |
| [`policy.rules[].response`](#policy-rules-response) | mapping | `{}` | only when `action` is `deny` |
| [`policy.rules[].response.status`](#policy-rules-response-status) | integer | inherited from `policy.deny_response.status` | `400` to `599` |
| [`policy.rules[].response.body`](#policy-rules-response-body) | string | inherited from `policy.deny_response.body` | at most 8 KiB |
| [`policy.rules[].response.headers`](#policy-rules-response-headers) | mapping of strings | inherited from `policy.deny_response.headers` | header names and values |

### `policy.mode`

**Type:** string\
**Default:** `enforce`\
**Allowed:** `enforce`, `audit`

`enforce` applies the decisions. `audit` allows every request, while tessera decides each one as it would in `enforce` mode and reports a denial that it did not enforce with `enforced=false`, in the [`decision`](./logs.md#decision) event and in [`tessera_decisions_total`](./metrics.md#tessera-decisions-total). Try a policy on real traffic this way before you enforce it: see [Audit a policy](../guide/write-rules.md#audit-a-policy). [`tessera_policy_audit_mode`](./metrics.md#tessera-policy-audit-mode) is `1` in `audit` mode.

### `policy.default_action`

**Type:** string\
**Default:** required\
**Allowed:** `allow`, `deny`

The decision for a request that no rule decides: `allow` lets Envoy send the request on, `deny` makes Envoy answer with [`policy.deny_response`](#policy-deny-response). It has no default, so that every configuration states whether such requests are allowed or denied.

### `policy.deny_response`

**Type:** mapping\
**Default:** `{}`

What Envoy answers the client when tessera denies a request, unless the rule that denies it has a [`response`](#policy-rules-response) of its own. The [gRPC API reference](./grpc-api.md#authorization-service) describes how tessera sends it to Envoy.

### `policy.deny_response.status`

**Type:** integer\
**Default:** `403`\
**Allowed:** `400` to `599`

The HTTP status of a denial. Envoy sends any status from `400` to `599`, also one that HTTP does not define, such as `451`.

### `policy.deny_response.body`

**Type:** string\
**Default:** `""`\
**Allowed:** at most 8 KiB

The body of a denial, at most 8192 bytes. When the body is not empty and [`policy.deny_response.headers`](#policy-deny-response-headers) has no `content-type`, Envoy sends `content-type: text/plain`.

### `policy.deny_response.headers`

**Type:** mapping of strings\
**Default:** `{}`\
**Allowed:** header names and values

The headers of a denial, from each name to its value:

```yaml [config.yaml]
policy:
  default_action: deny
  deny_response:
    status: 403
    body: '{"error": "forbidden"}'
    headers:
      content-type: application/json
      cache-control: no-store
```

- A name is an HTTP field name, in any case: letters, digits and the characters ``!#$%&'*+-.^_`|~``. tessera sends it in lowercase. Two names that differ only in case are an error.
- A value has no control character other than the horizontal tab.
- These headers are not allowed, because Envoy sets them itself or they describe the connection rather than the response: `connection`, `content-length`, `keep-alive`, `proxy-connection`, `te`, `trailer`, `transfer-encoding`, `upgrade`.

Envoy adds headers of its own to a denial, such as `date`, and by default replaces `server` with its own.

### `policy.rules`

**Type:** list\
**Default:** `[]`\
**Allowed:** at most 1000 rules

The rules, in the order in which tessera evaluates them. Each rule has a name, optionally a description, an action, optionally a mode, a condition and, for a `deny` rule, a response. The examples of this documentation write the keys of a rule in that order:

```yaml [config.yaml]
policy:
  default_action: deny
  rules:
    - name: blocked-networks
      action: deny
      when: client.ip.inCidr("198.51.100.0/24")
    - name: office
      description: The office network reaches every host.
      action: allow
      when: has(client.ip) && client.ip.inCidr("203.0.113.0/24")
```

A policy has at most 1000 rules, so that the work of one decision stays bounded. A rule that matches every request, without `when` or with `when: "true"`, and in `enforce` mode, decides every request that reaches it: the rules after it would never be evaluated, so they are an error. Such a rule can only be the last one, or in `audit` mode.

### `policy.rules[].name`

**Type:** string\
**Default:** required\
**Allowed:** lowercase letters, digits and hyphens; unique, at most 63 characters, not `default`

The name of the rule, which the decisions report. `default` names the decision of `policy.default_action`, and no rule can take it.

### `policy.rules[].description`

**Type:** string\
**Default:** `""`\
**Allowed:** at most 1024 bytes

A description of the rule, for the people who read the configuration.

### `policy.rules[].action`

**Type:** string\
**Default:** required\
**Allowed:** `allow`, `deny`

What the rule does with a request whose condition is true. A condition that cannot be evaluated counts against allowing: a `deny` rule denies, an `allow` rule does not match. See [Indeterminate conditions](./expressions.md#indeterminate-conditions).

### `policy.rules[].mode`

**Type:** string\
**Default:** `enforce`\
**Allowed:** `enforce`, `audit`

`enforce` lets the rule decide. A rule in `audit` mode never decides: when its condition is true, tessera reports the match, in `audit_rules` of the [`decision`](./logs.md#decision) event and in [`tessera_audit_matches_total`](./metrics.md#tessera-audit-matches-total), and goes on to the next rule; when its condition cannot be evaluated, tessera counts it in [`tessera_rule_evaluation_errors_total`](./metrics.md#tessera-rule-evaluation-errors-total) and goes on too. See [Audit a rule](../guide/write-rules.md#audit-a-rule).

### `policy.rules[].when`

**Type:** expression\
**Default:** `"true"`\
**Allowed:** a condition of at most 8192 characters

The condition of the rule: an expression whose result is a bool, written in the [expression language](./expressions.md). A rule without `when` matches every request.

tessera compiles every condition when it reads the configuration: a syntax error, an unknown attribute or a wrong type is an error at its position in the file, as in `policy.rules[2].when (line 14, column 31): undefined field 'hots'`. A syntax error reports `syntax error in the expression` and its position only, since the parser would quote the text where it stopped, which can be a string of the expression. When the file does not hold the expression as it is, because it is in a folded block (`>`), spans lines outside a literal block (`|`), contains escapes or takes text from an environment variable, the position is that of the value, followed by the position in the expression. An expression that takes text from an environment variable reports `invalid expression`, without the message of the expression language, which could repeat that text.

### `policy.rules[].response`

**Type:** mapping\
**Default:** `{}`\
**Allowed:** only when `action` is `deny`

The response of a `deny` rule. Each of its keys that the rule does not write takes the value of the same key in [`policy.deny_response`](#policy-deny-response). On an `allow` rule, `response` is an error.

```yaml [config.yaml]
policy:
  default_action: allow
  deny_response:
    body: Forbidden.
  rules:
    - name: admin-from-outside
      action: deny
      when: request.path.startsWith("/admin") && !client.ip.inCidr("192.0.2.0/24")
      response:
        status: 404
```

A request denied by `admin-from-outside` gets the status `404` and the body `Forbidden.`.

### `policy.rules[].response.status`

**Type:** integer\
**Default:** inherited from `policy.deny_response.status`\
**Allowed:** `400` to `599`

The HTTP status of a denial by this rule, as in [`policy.deny_response.status`](#policy-deny-response-status).

### `policy.rules[].response.body`

**Type:** string\
**Default:** inherited from `policy.deny_response.body`\
**Allowed:** at most 8 KiB

The body of a denial by this rule, as in [`policy.deny_response.body`](#policy-deny-response-body). `body: ""` sends no body, whatever `policy.deny_response.body` is.

### `policy.rules[].response.headers`

**Type:** mapping of strings\
**Default:** inherited from `policy.deny_response.headers`\
**Allowed:** header names and values

The headers of a denial by this rule, with the rules of [`policy.deny_response.headers`](#policy-deny-response-headers). They replace `policy.deny_response.headers` as a whole, and are not merged with them: write every header that the denial needs, or `headers: {}` for none.
