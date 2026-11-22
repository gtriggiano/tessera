---
description: "Every command of the tessera binary, with its flags, output and exit status."
---

# CLI reference

The **tessera** binary has a command that answers Envoy's authorization requests, a command that shows how the policy decides a request, commands that check and print a configuration file, and commands that print help and build information.

| Command | What it does |
| --- | --- |
| [`tessera serve`](#tessera-serve) | Answer Envoy's authorization requests |
| [`tessera eval`](#tessera-eval) | Show how the policy decides a request |
| [`tessera config validate`](#tessera-config-validate) | Check a configuration file |
| [`tessera config print`](#tessera-config-print) | Print a configuration file with its defaults applied |
| [`tessera version`](#tessera-version) | Print build information |
| [`tessera`, `tessera help`](#tessera-and-tessera-help) | Print help |

Only `tessera serve` opens sockets. `tessera config print` reads one file, the configuration; `tessera serve`, `tessera eval` and `tessera config validate` read the files of its [sets](./configuration.md#sets) too.

## Synopsis

```text
tessera serve [--config <path>]
tessera eval [--config <path>] [--ip <address>] [--host <host>] [--method <method>] [--path <path>] [--query <query>] [--scheme <scheme>] [--header '<name>: <value>']... [--set '<name>=<value>,<value>']... [--output text|json]
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
| `--config <path>` | `serve`, `eval`, `config` and its subcommands | The configuration file. Overrides `TESSERA_CONFIG`. If repeated, the last value wins. |
| `--output <format>` | `version`, `eval` | `text` (default) or `json`. |
| `-h`, `--help` | Every command | Print the command's help and exit with status 0. |

Only `tessera help` takes positional arguments: the name of a command. An extra argument, including an unknown command name after `tessera` or `tessera config`, fails with `unexpected arguments; see --help`; an unknown flag, or a flag without its value, fails with `invalid command flags; see --help`. Both exit with status 2. Error messages do not repeat the offending argument, which could be a credential pasted by mistake.

### Configuration file

`tessera serve`, `tessera eval`, `tessera config validate` and `tessera config print` read exactly one configuration file:

1. the one named by `--config`, when the flag is present;
2. otherwise, the one named by the `TESSERA_CONFIG` environment variable, when it is set.

With neither, the command fails with `no configuration file; use --config or TESSERA_CONFIG`. An empty value fails with `configuration file path must not be empty`. A path that is missing, unreadable or not a regular file, such as a directory or a named pipe, is an error too, and so is an invalid configuration. No other location is searched. The [configuration reference](./configuration.md#loading) describes how the file is read and checked.

### Output streams

- Standard output carries the result of the command: help, build information, the evaluation of `tessera eval`, `configuration valid`, the printed configuration, and the [log events](./logs.md) of `tessera serve`, its errors included.
- Standard error carries the errors of the command line, one line each: `tessera: <message>`, and the warnings of [`tessera config validate`](#tessera-config-validate): `tessera: warning: <message>`. `tessera serve` writes there only the errors found before it reads its configuration and its sets.

### Exit status

| Status | Meaning |
| --- | --- |
| 0 | Success. For `tessera serve`, a shutdown in which every call finished. |
| 1 | Runtime failure: the output could not be written, or `tessera serve` could not open a listener, a listener failed, or its shutdown was forced (see [Exit status of `serve`](#exit-status-of-serve)). |
| 2 | Usage or configuration error: an unknown command, argument or flag, an invalid flag value, a configuration file that is not selected, missing, unreadable or invalid, or a set that cannot be read or is invalid. |

### Signals

The binary handles SIGINT and SIGTERM itself. `tessera serve` shuts down on the first signal and closes every connection at once on another (see [Signals of `serve`](#signals-of-serve)). The other commands wait on nothing, so they finish normally when a signal arrives.

## `tessera serve`

```sh
tessera serve --config config.yaml
```

Answers Envoy's external authorization requests on the gRPC listener, at [`server.listen`](./configuration.md#server-listen), and serves the health checks and the metrics on the admin listener, at [`admin.listen`](./configuration.md#admin-listen). It decides each request with the rules of [`policy`](./configuration.md#policy). The [gRPC API reference](./grpc-api.md) describes the services and the responses, and the [admin endpoints reference](./admin-endpoints.md) the admin listener. In the container image, `serve` is the default command.

### Startup

1. Reads the [selected file](#configuration-file) and checks it like `tessera config validate`, and loads every [set](./configuration.md#sets), reading its file when it has one. An error here, an invalid set included, prints `tessera: <message>` and exits with status 2, before anything opens.
2. Logs [`starting`](./logs.md#starting), then a [`config_warning`](./logs.md#config-warning) for each warning of the configuration, then a [`set_loaded`](./logs.md#set-loaded) for each set.
3. Opens the gRPC listener, then the admin listener, within 10 seconds for both, the time to resolve their host names included. If one cannot open, or does not open in time, the other is closed, [`startup_failed`](./logs.md#startup-failed) is logged and the exit status is 1. A signal stops a listener that is still resolving its host name in the same way.
4. Logs [`listening`](./logs.md#listening) for each listener, reports ready, on `/readyz` and through the gRPC health service, logs [`ready`](./logs.md#ready), starts checking the file of each set every [`sets.<name>.refresh`](./configuration.md#sets-name-refresh), and starts answering.

### Output

`serve` writes its [log events](./logs.md) to standard output, one line each, in the format of [`log.format`](./configuration.md#log-format), and nothing else. An error before the configuration and its sets are read, which has no format yet, is a plain `tessera: <message>` line on standard error. A standard output that is closed or cannot be written loses the events, and does not stop `serve` or change its exit status.

### Signals of `serve`

SIGTERM or SIGINT starts a graceful shutdown:

1. [`shutdown_started`](./logs.md#shutdown-started) is logged. `/readyz` answers 503 and the gRPC health service `NOT_SERVING`, and [`not_ready`](./logs.md#not-ready) is logged.
2. For [`server.drain_delay`](./configuration.md#server-drain-delay), both listeners keep answering as before, so that Envoy and the orchestrator see the change and stop sending requests.
3. The gRPC listener closes, every client is asked to close its connection, and the calls in progress finish, for at most [`server.shutdown_timeout`](./configuration.md#server-shutdown-timeout). A connection whose client has sent nothing is closed at once.
4. The checks of the files of the sets stop, within the same deadline. A check in progress is cut short, and changes nothing.
5. The admin listener closes, within the same deadline. A connection that has not sent a request is closed at once.
6. The [`decision`](./logs.md#decision) events still in their [queue](./logs.md#the-queue-of-decision-events) are written, within the same deadline.
7. [`shutdown_complete`](./logs.md#shutdown-complete) is logged with `outcome=drained`.

If the deadline passes before the calls, the checks of the sets and the admin requests in progress finish, they are cancelled and every connection is closed: `shutdown_complete` reports `outcome=forced forced_by=timeout`. The same happens when the deadline passes before the queued `decision` events are written: the events left are dropped. Another signal during the shutdown does the same at once, at any step: `shutdown_complete` reports `outcome=forced forced_by=signal`. A listener that fails while tessera runs logs [`listener_failed`](./logs.md#listener-failed) and starts the same shutdown; one that fails during the shutdown is logged at its end, and the exit status is 1. Any other signal received during the startup starts the shutdown as soon as both listeners are open.

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
| Invalid usage or configuration, an invalid set included | None: `tessera: <message>` | 2 |

## `tessera eval`

Shows how the policy of a configuration decides a request that the flags describe, without Envoy: the attributes that the rules read, the outcome of each rule up to the one that decides, and the decision. It loads the [sets](#sets) as `tessera serve` does, writes no [log event](./logs.md), changes no [metric](./metrics.md), and opens no socket.

With the configuration of [A complete example](../guide/write-rules.md#a-complete-example), in a file named `rules.yaml`:

```sh
tessera eval --config rules.yaml \
  --ip 203.0.113.7 --host api.example.com --method POST --path /v1/webhooks \
  --header 'authorization: Bearer k-123' --header 'user-agent: curl/8.0'
```

```text
client.ip                         203.0.113.7
request.headers["authorization"]  [REDACTED]
request.headers["user-agent"]     "curl/8.0"
request.host                      "api.example.com"
request.method                    "POST"
request.path                      "/v1/webhooks"

health-checks      false          The load balancer checks Envoy without X-Forwarded-For.
blocked-networks   false          Networks blocked during an incident.
scanners           false (audit)
admin-from-office  false
admin              false
public-site        false
api                true -> allow

decision: allow (rule api, reason rule)
```

A request to the admin pages from outside the office, by a scanner:

```sh
tessera eval --config rules.yaml \
  --ip 192.0.2.10 --host www.example.com --path /admin/users --header 'user-agent: sqlmap/1.8'
```

```text
client.ip                      192.0.2.10
request.headers["user-agent"]  "sqlmap/1.8"
request.path                   "/admin/users"

health-checks      false         The load balancer checks Envoy without X-Forwarded-For.
blocked-networks   false         Networks blocked during an incident.
scanners           true (audit)
admin-from-office  false
admin              true -> deny

decision: deny (rule admin, reason rule)
response: status 404
```

### The request

| Flag | Attribute | Default |
| --- | --- | --- |
| `--ip <address>` | `client.ip` | Missing. |
| `--host <host>` | `request.host` | Empty. |
| `--method <method>` | `request.method` | `GET` |
| `--path <path>` | `request.path`, without `?` | `/` |
| `--query <query>` | `request.query`, written without `?` | Empty. |
| `--scheme <scheme>` | `request.scheme` | `https` |
| `--header '<name>: <value>'` | `request.headers`; repeat the flag for each header | None. |

- tessera normalizes the request as it normalizes a request from Envoy (see [Attributes](./expressions.md#attributes)): `--host API.example.com:443` gives `request.host == "api.example.com"`, and two `--header` flags with the same name give one value, joined with a comma.
- `--ip` sets `client.ip` directly: [`client_ip`](./configuration.md#client-ip) does not apply. The address has the forms that [`client_ip.from`](./configuration.md#client-ip-from) accepts. Without `--ip`, the request has no client address.
- `request.headers` holds only the headers of the flags, and `request.id` is the value of an `x-request-id` header among them, empty without it.
- A `?` in `--path` fails with `invalid --path; write the query with --query`: write the query with `--query`.

### Sets

`tessera eval` loads the [sets](./configuration.md#sets) of the configuration as `tessera serve` does, their files included: a file that is missing or invalid is a configuration error. `--set '<name>=<value>,<value>'` replaces the content of the set `<name>` for the evaluation, and its file is not read, so that `eval` works without the files of the server:

- the values are separated by commas, and read as the [`values`](./configuration.md#sets-name-values) of the type of the set, with the spaces around them ignored: a value of a `string` set cannot hold a comma;
- `--set '<name>='` makes the set empty;
- `--set` replaces one set, and is given once for each set that you replace.

With this configuration in a file named `sets.yaml`, and the file of its set `blocked`, `blocked.txt`, next to it:

<<< @/examples/sets.yaml{yaml}

<<< @/examples/blocked.txt{text}

```sh
tessera eval --config sets.yaml --ip 198.51.100.7 --host admin.example.com
```

```text
client.ip  198.51.100.7

blocked-networks  true -> deny  Networks blocked during an incident.

decision: deny (rule blocked-networks, reason rule)
response: status 403
```

The same request, with the set `blocked` empty and the network of the client in `office`:

```sh
tessera eval --config sets.yaml --set blocked= --set office=198.51.100.0/24 \
  --ip 198.51.100.7 --host admin.example.com
```

```text
client.ip     198.51.100.7
request.host  "admin.example.com"

blocked-networks  false  Networks blocked during an incident.
internal-hosts    false  The internal hosts, from the office only.

decision: allow (rule default, reason default)
```

### The output

The text output has three blocks:

1. **The attributes** that the conditions of the rules evaluated read, sorted by name, with their values: a string in double quotes, `client.ip` without them, and `missing` for an attribute or a header that the request does not have. A rule that reads the whole map `request.headers`, such as with `exists`, shows every header. The values of `authorization` and `cookie` are shown as `[REDACTED]`.
2. **The rules**, in order, up to the one that decides, each with the outcome of its condition: `true`, `false`, or `indeterminate` followed by its cause (`client_ip_missing`, `expression`, `panic` or `set_unavailable`), then `-> allow` or `-> deny` for the rule that decides, or `(audit)` for a rule in `audit` mode, then its description, on one line.
3. **The decision**: its action, the rule that decided or `default`, and the reason (`rule`, `default`, or `error` followed by the cause), with `not enforced: policy.mode is audit` when [`policy.mode: audit`](./configuration.md#policy-mode) allows a denied request; for a denial, the status of the response.

`--output json` writes one JSON object on one line:

```sh
tessera eval --config rules.yaml \
  --ip 192.0.2.10 --host www.example.com --path /admin/users --header 'user-agent: sqlmap/1.8' \
  --output json
```

```json
{"attributes":[{"name":"client.ip","value":"192.0.2.10"},{"name":"request.headers[\"user-agent\"]","value":"sqlmap/1.8"},{"name":"request.path","value":"/admin/users"}],"rules":[{"name":"health-checks","description":"The load balancer checks Envoy without X-Forwarded-For.","action":"allow","mode":"enforce","result":"false"},{"name":"blocked-networks","description":"Networks blocked during an incident.","action":"deny","mode":"enforce","result":"false"},{"name":"scanners","action":"deny","mode":"audit","result":"true"},{"name":"admin-from-office","action":"allow","mode":"enforce","result":"false"},{"name":"admin","action":"deny","mode":"enforce","result":"true"}],"decision":{"action":"deny","enforced":true,"rule":"admin","reason":"rule","audit_rules":["scanners"],"response":{"status":404,"body":"Not found.","headers":{}}}}
```

| JSON field | Content |
| --- | --- |
| `attributes` | The attributes, as in the text output: each with `name` and `value`, a string, or `missing: true` without `value`. |
| `rules` | The rules evaluated, in order: `name`, `description` (absent when empty), `action`, `mode`, `result` (`true`, `false` or `indeterminate`), and `cause` with `indeterminate`. |
| `decision` | `action`, `enforced`, `rule`, `reason`; `error`, the cause, with the reason `error`; `audit_rules` and `indeterminate_rules`, absent when empty; `response`, for a denial: `status`, `body` and `headers`, by name. |

### Exit status of `eval`

`tessera eval` exits with status 0 whatever the decision, with 2 for a usage or configuration error, and with 1 when it cannot write its output. An invalid `--ip`, `--header`, `--path` or `--output` fails with `invalid --ip; want an IP address`, `invalid --header; want 'name: value'`, `invalid --path; write the query with --query` or `invalid --output; want text or json`, without repeating the value. An invalid `--set` fails without repeating its text either, with one of:

| Error | Cause |
| --- | --- |
| `invalid --set; want name=value,value` | The flag has no `=`. |
| `invalid --set: the configuration declares no set of this name` | No set of [`sets`](./configuration.md#sets) has the name. |
| `invalid --set: the set <name> is given twice` | Two `--set` flags replace the same set. |
| `invalid --set for the set <name>: value <n>: <problem>` | The value in position `<n>`, from 1, is not a value of the type of the set, as for [`sets.<name>.values`](./configuration.md#sets-name-values). |

## `tessera config validate`

```sh
tessera config validate --config config.yaml
```

Reads the [selected file](#configuration-file), checks it as the [configuration reference](./configuration.md) describes, compiles the conditions of the rules, reads the file of every [set](./configuration.md#sets), and prints `configuration valid`. It opens no socket:

```text
configuration valid
```

An invalid configuration, or a set file that is missing or invalid, prints its first error on standard error and exits with status 2:

```text
tessera: policy.default_action (line 2, column 19): expected allow or deny
```

```text
tessera: sets.partners.file (line 9, column 11): line 12 of the file: the value has a control character
```

[Error messages](./configuration.md#error-messages) describes their form.

A warning is a problem that does not make the configuration invalid. `tessera config validate` prints each warning on standard error, in the order of the file, then `configuration valid` on standard output, and exits with status 0. The only warning is a set that no rule reads, which tessera loads and checks all the same:

```text
tessera: warning: sets.office (line 3, column 3): the set is not used by any rule
```

`tessera serve` writes the same warnings as [`config_warning`](./logs.md#config-warning) events.

## `tessera config print`

```sh
tessera config print --config config.yaml
```

Reads and checks the [selected file](#configuration-file) like `tessera config validate`, without reading the files of the sets or printing the warnings, then prints the configuration as YAML, with every default applied and the keys in the order of the [configuration reference](./configuration.md). For a file that sets only `policy.default_action`:

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
  decisions: deny
client_ip:
  from: peer
sets: {}
policy:
  mode: enforce
  default_action: deny
  deny_response:
    status: 403
    body: ""
    headers: {}
  rules: []
```

- A value that contains an [environment variable substitution](./configuration.md#environment-variables) is printed as `'[REDACTED]'`.
- A literal `${` in a value is printed as `$${`, so that the value reads back as written.
- Durations are printed in a normal form: `90s` is printed as `1m30s`, `30m` as `30m0s`, and `500ms` as `500ms`.
- A relative socket path in `server.listen` is printed as an absolute path.
- `client_ip` shows the keys of its source only: `client_ip.from`, with `client_ip.header` or `client_ip.trusted_hops` when the source has one.
- The sets are sorted by name. Each shows the keys of its source only: `type`, `values` and `max_entries` for a set of values, and every key but `values` for a set with a file, whose relative path is printed as an absolute path. A value of `values` is printed as a string, `"64496"` for the integer `64496`.
- Each rule shows `name`, `description`, `action`, `mode` and `when`, in this order and with their defaults, and its `response` with only the keys that the file writes: the others come from `policy.deny_response`.

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
