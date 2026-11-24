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

If standard output is closed or cannot be written, the events are lost and tessera goes on serving. The [`decision`](#decision) events wait in a [queue](#the-queue-of-decision-events), so that a slow standard output never slows a decision: a `decision` event can appear after an event that tessera wrote later, such as `shutdown_started`.

## Levels

Each event has a level: `debug`, `info`, `warn` or `error`. [`log.level`](./configuration.md#log-level) sets the lowest level written, `info` by default. tessera writes no `debug` events.

## Events

| Event | Level | When |
| --- | --- | --- |
| [`starting`](#starting) | `info` | `tessera serve` has read a valid configuration and loaded its sets and databases. |
| [`config_warning`](#config-warning) | `warn` | The configuration has a warning: one event for each, after `starting`. |
| [`listening`](#listening) | `info` | Both listeners are open: one event for each. |
| [`ready`](#ready) | `info` | tessera reports ready. |
| [`not_ready`](#not-ready) | `info` | tessera stops reporting ready, at the start of a shutdown. |
| [`shutdown_started`](#shutdown-started) | `info` | A shutdown starts. |
| [`shutdown_complete`](#shutdown-complete) | `info` or `warn` | The shutdown ends, as the last event before the process exits. |
| [`startup_failed`](#startup-failed) | `error` | A listener could not open. |
| [`listener_failed`](#listener-failed) | `error` | A listener stopped while tessera was running or shutting down. |
| [`set_loaded`](#set-loaded) | `info` | A new copy of a set is in use: one event for each set at startup, after `config_warning`, and one for each new content of a file. |
| [`set_load_failed`](#set-load-failed) | `warn` | A check of the file of a set failed, and the copy in use stays. |
| [`set_unavailable`](#set-unavailable) | `warn` | A set became unavailable. |
| [`set_available`](#set-available) | `info` | A set became available again. |
| [`database_loaded`](#database-loaded) | `info` | A new copy of a database is in use: one event for each database at startup, after `set_loaded`, and one for each new database in its file. |
| [`database_load_failed`](#database-load-failed) | `warn` | A check of the file of a database failed, and the copy in use stays. |
| [`decision`](#decision) | `info` or `warn` | tessera decided a request, and [`log.decisions`](./configuration.md#log-decisions) selects it. |
| [`decision_logs_dropped`](#decision-logs-dropped) | `warn` | `decision` events were dropped because their queue was full. |
| [`evaluation_error`](#evaluation-error) | `warn` | Conditions of a rule could not be evaluated. |
| [`panic_recovered`](#panic-recovered) | `error` | tessera recovered from a failure of its own code, while it evaluated a condition or elsewhere while it handled a call. |

[Signals of `serve`](./cli.md#signals-of-serve) describes when each event of a shutdown happens.

### `starting`

The first event of `tessera serve`, written before any listener opens. It is the only event that names the version.

| Field | Value |
| --- | --- |
| `version` | The release, as [`tessera version`](./cli.md#tessera-version) prints it: `dev` for a binary not built for a release. |
| `revision` | The commit the binary was built from, or `unknown`. |
| `config_path` | The path of the configuration file, as `--config` or `TESSERA_CONFIG` gave it. |

### `config_warning`

A problem of the configuration that does not make it invalid, such as a set, a database or the analysis of the user-agent header that no rule, [upstream header](./configuration.md#upstream-headers) or [decision field](./configuration.md#log-decision-fields) reads. `tessera serve` writes one event for each warning, in the order of the file, right after `starting`, and [`tessera config validate`](./cli.md#tessera-config-validate) prints the same warnings.

```text
ts=2026-11-22T10:15:00.124Z level=warn msg=config_warning key=sets.office warning="the set is not used by any rule, upstream header or decision field"
```

| Field | Value |
| --- | --- |
| `key` | The key of the configuration that the warning is about, such as `sets.office`, `enrichment.asn` or `enrichment.user_agent`. |
| `warning` | The problem. It never repeats a configured value. |

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
| `outcome` | `drained`: every call and admin request in progress finished, the checks of the sets and of the databases stopped, and every queued [`decision`](#decision) event was written, within [`server.shutdown_timeout`](./configuration.md#server-shutdown-timeout). `forced`: the remaining ones were cancelled and their connections closed, and the queued `decision` events dropped. |
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

### `set_loaded`

A new copy of a [set](./configuration.md#sets) is in use. tessera writes one for each set at startup, sets of `values` included, in the order of the configuration file, after `starting` and the `config_warning` events and before the [`database_loaded`](#database-loaded) events and `listening`. Then it writes one each time a check of the file of a set finds new valid content, which replaces the copy in use (see [`sets.<name>.refresh`](./configuration.md#sets-name-refresh)). A file whose content is that of the copy in use writes no event.

```text
ts=2026-11-22T10:15:00.125Z level=info msg=set_loaded set=partners set_type=string set_source=file entries=1250 duration_ms=1.873
```

The same event at startup for a set of `values`, in the `json` format:

```json
{"ts":"2026-11-22T10:15:00.124Z","level":"info","msg":"set_loaded","set":"office","set_type":"cidr","set_source":"values","entries":2,"duration_ms":0.012}
```

| Field | Value |
| --- | --- |
| `set` | The name of the set. |
| `set_type` | The type of the set: `cidr`, `asn`, `string` or `area`, as the label `type` of the [metrics of the sets](./metrics.md#sets). |
| `set_source` | Where tessera reads the set from: `values` or `file`, as the label `source` of the metrics of the sets. |
| `entries` | The entries of the new copy: the values read, a value written twice counted twice, or the polygons of an area set, as [`sets.<name>.max_entries`](./configuration.md#sets-name-max-entries) counts them. |
| `duration_ms` | The time of the load, in milliseconds with the precision of a microsecond, as in [`decision`](#decision): for a file, from the check of its state to the new copy, the reading of the file included. |

### `set_load_failed`

A check of the file of a set failed: the file is missing, cannot be read, or has invalid content, and the copy of the set in use stays. tessera writes the event once for each new failure: when the version of the file or the error differs from those of the previous failure. A check that fails again in the same way, at each [`sets.<name>.refresh`](./configuration.md#sets-name-refresh), writes no event, and [`tessera_set_refreshes_total`](./metrics.md#tessera-set-refreshes-total) counts it, with `result="rejected"` or `result="error"`. After a successful check, the next failure is a new one. A file that is missing or invalid when tessera starts writes no event: tessera does not start (see [`sets`](./configuration.md#sets)).

```text
ts=2026-11-22T10:20:00.125Z level=warn msg=set_load_failed set=partners set_type=string set_source=file error="line 12 of the file: the value has a control character"
```

| Field | Value |
| --- | --- |
| `set` | The name of the set. |
| `set_type`, `set_source` | The type and the source of the set, as in [`set_loaded`](#set-loaded). |
| `error` | The problem. Content that is invalid or exceeds a ceiling gives `line <n> of the file: <problem>`, such as `line 12 of the file: more entries than max_entries`, or `the content is larger than 256 MiB`; the GeoJSON document of an area set gives the column too, and the place of the problem in the document, such as `line 3, column 88 of the file: feature 3, polygon 1, ring 2: the ring is not closed: its last position differs from its first`. A file that cannot be read gives `the file does not exist`, `not a regular file`, `the file cannot be opened: permission denied`, `the file cannot be opened` or `the file cannot be read`. It never repeats the content of the file or its path. |

### `set_unavailable`

A set read from a file became unavailable: its last successful check is older than [`sets.<name>.max_staleness`](./configuration.md#sets-name-max-staleness). tessera writes it as the age passes the limit, between two checks too. From then on, `contains` on the set answers what [`sets.<name>.on_unavailable`](./configuration.md#sets-name-on-unavailable) says, until [`set_available`](#set-available).

```text
ts=2026-11-22T10:25:00.125Z level=warn msg=set_unavailable set=partners set_type=string set_source=file reason=stale
```

| Field | Value |
| --- | --- |
| `set` | The name of the set. |
| `set_type`, `set_source` | The type and the source of the set, as in [`set_loaded`](#set-loaded). |
| `reason` | `stale`: the last successful check is older than `max_staleness`. |

### `set_available`

A set that was unavailable is available again, after a successful check of its file: one that found the file unchanged, or that replaced the copy. It follows the [`set_loaded`](#set-loaded) of the new copy, if any.

```text
ts=2026-11-22T10:26:00.125Z level=info msg=set_available set=partners set_type=string set_source=file
```

| Field | Value |
| --- | --- |
| `set` | The name of the set. |
| `set_type`, `set_source` | The type and the source of the set, as in [`set_loaded`](#set-loaded). |

### `database_loaded`

A new copy of a [database](./configuration.md#enrichment) is in use. tessera writes one for each database at startup, `geo` then `asn`, after the [`set_loaded`](#set-loaded) events and before `listening`. Then it writes one each time a check of the file of a database finds a new valid database, which replaces the copy in use (see [`enrichment.geo.refresh`](./configuration.md#enrichment-geo-refresh)). A file whose content is that of the copy in use writes no event.

```text
ts=2026-11-22T10:15:00.125Z level=info msg=database_loaded database=geo database_type=GeoLite2-City build_time=2026-11-17T10:00:00Z duration_ms=61.873
```

```json
{"ts":"2026-11-22T10:15:00.125Z","level":"info","msg":"database_loaded","database":"asn","database_type":"GeoLite2-ASN","build_time":"2026-11-17T10:00:00Z","duration_ms":12.402}
```

| Field | Value |
| --- | --- |
| `database` | `geo` or `asn`, as the label `database` of the [metrics of the enrichments](./metrics.md#enrichments). |
| `database_type` | The type of the database, as its metadata names it, such as `GeoLite2-City`, cut to 128 bytes. tessera does not check it. |
| `build_time` | The build time of the database, from its metadata, in RFC 3339 and UTC: [`tessera_enrichment_database_build_timestamp_seconds`](./metrics.md#tessera-enrichment-database-build-timestamp-seconds) has the same time. A build time after `9999-12-31T23:59:59Z`, which RFC 3339 cannot write, is given as that time, and the database is used all the same. |
| `duration_ms` | The time of the load, in milliseconds with the precision of a microsecond, as in [`decision`](#decision): from the check of the state of the file to the new copy, the reading of the file and the checks of the database included. |

### `database_load_failed`

A check of the file of a database failed: the file is missing, cannot be read, is larger than 512 MiB, is not a valid database or is a database of the other kind, and the copy of the database in use stays. tessera writes the event once for each new failure, as [`set_load_failed`](#set-load-failed) does for a set: when the version of the file or the error differs from those of the previous failure. A check that fails again in the same way writes no event, and [`tessera_enrichment_database_refreshes_total`](./metrics.md#tessera-enrichment-database-refreshes-total) counts it, with `result="rejected"` or `result="error"`. A database that is missing or invalid when tessera starts writes no event: tessera does not start (see [`enrichment`](./configuration.md#enrichment)).

```text
ts=2026-11-22T10:20:00.125Z level=warn msg=database_load_failed database=geo error="the file is not a valid MMDB database"
```

| Field | Value |
| --- | --- |
| `database` | `geo` or `asn`. |
| `error` | The problem, one of those that [`enrichment.geo.database`](./configuration.md#enrichment-geo-database) lists. It never repeats the content of the file or its path. |

### `decision`

tessera decided a request. [`log.decisions`](./configuration.md#log-decisions) selects the decisions that have an event: by default, the denials, and the allows in which a rule in `audit` mode with `action: deny` matched. The level is `info` when `action` is `allow`, and `warn` when it is `deny`, also when `policy.mode: audit` did not enforce the denial; [`log.level`](./configuration.md#log-level) applies.

```text
ts=2026-11-22T10:16:04.512Z level=warn msg=decision request_id=6f1c0f6e-5a1b-4a0c-9e7d-2b8f4c1d3e5a host=api.example.com method=POST path=/v1/webhooks client_ip=198.51.100.7 action=deny enforced=true rule=blocked-networks reason=rule duration_ms=0.041
```

```json
{"ts":"2026-11-22T10:16:04.512Z","level":"warn","msg":"decision","request_id":"6f1c0f6e-5a1b-4a0c-9e7d-2b8f4c1d3e5a","host":"api.example.com","method":"POST","path":"/v1/webhooks","client_ip":"198.51.100.7","action":"deny","enforced":true,"rule":"blocked-networks","reason":"rule","duration_ms":0.041}
```

| Field | Value |
| --- | --- |
| `request_id` | The value of the `x-request-id` header of the request, as [`request.id`](./expressions.md#attributes) has it, which says where Envoy takes it from; empty without the header. |
| `host` | The host of the request, as [`request.host`](./expressions.md#attributes) has it. |
| `method` | The method, as `request.method` has it. |
| `path` | The path, without the query, as `request.path` has it. |
| `client_ip` | The client address, `client.ip`, or empty when the request has none. |
| `action` | `allow` or `deny`: what the policy decided. |
| `enforced` | `false` when [`policy.mode: audit`](./configuration.md#policy-mode) let a denied request through, `true` otherwise. |
| `rule` | The rule that decided, or `default` for [`policy.default_action`](./configuration.md#policy-default-action). |
| `reason` | `rule`: the condition of the rule was true. `default`: no rule decided. `error`: the condition of a `deny` rule could not be evaluated. |
| `audit_rules` | Only when there are some: the rules in `audit` mode whose condition was true, in order, separated by commas. |
| `indeterminate_rules` | Only when there are some: the `allow` rules that did not match because their condition could not be evaluated, in order, separated by commas. |
| `error` | Only with `reason=error`: why the condition could not be evaluated: `client_ip_missing`, `expression`, `panic`, `set_unavailable` or `database_error` (see [`tessera_rule_evaluation_errors_total`](./metrics.md#tessera-rule-evaluation-errors-total)). |
| `duration_ms` | The time of the decision, from when tessera received the call of Envoy: the reading of the request and of its client address, the evaluation of the rules and of the [upstream headers](./configuration.md#upstream-headers). It does not count the recording of the decision, nor the time of the call in the network. In milliseconds with the precision of a microsecond; a number in both formats. |
| The fields of [`log.decision_fields`](./configuration.md#log-decision-fields) | One field for each, by its name, in the order of the names: the value of its expression, a string in both formats, empty when the expression fails. |

- Every field but `audit_rules`, `indeterminate_rules` and `error` is in every `decision` event, in the order of the table. The lists are strings in both formats: rule names contain no comma.
- `request_id`, `host`, `method` and `path` come from the request: tessera cuts each of them to 1024 bytes, at a character boundary, and so it cuts the value of a decision field.
- tessera computes the decision fields only for an event that it writes, before the event enters [its queue](#the-queue-of-decision-events), with what the decision read: the same sets and databases, and the records of the enrichments that a rule computed.
- The event never holds the query of the request, nor a header, unless a decision field reads the header (see [What is never logged](#what-is-never-logged)).
- tessera writes the event after the decision: the event cannot change it.

With `log.decision_fields` set to `agent: request.headers["user-agent"]` and `country: geo.country.code`:

```text
ts=2026-11-22T10:16:04.512Z level=info msg=decision request_id=6f1c0f6e-5a1b-4a0c-9e7d-2b8f4c1d3e5a host=api.example.com method=GET path=/ client_ip=192.0.2.7 action=allow enforced=true rule=default reason=default duration_ms=0.052 agent=curl/8.7.1 country=IT
```

### The queue of decision events

The `decision` events wait in a queue of at most 4096 events and 4 MiB, which tessera writes to standard output in the background. A decision never waits for standard output: when the queue is full, the event is dropped, and counted in [`tessera_decision_logs_dropped_total`](./metrics.md#tessera-decision-logs-dropped-total). The other events are written as they happen.

At the end of a shutdown, after the listeners close and before `shutdown_complete`, tessera writes the events still in the queue, within [`server.shutdown_timeout`](./configuration.md#server-shutdown-timeout). When the deadline passes, or another signal arrives, before they are all written, the shutdown is forced: the events left are dropped and counted, and `shutdown_complete` reports `outcome=forced`. A shutdown forced earlier drops them at once. `shutdown_complete` stays the last event: no `decision` event follows it.

### `decision_logs_dropped`

`decision` events were dropped. tessera writes this event once standard output writes again, at most once every 10 seconds, and when the queue closes at the end of a shutdown.

| Field | Value |
| --- | --- |
| `dropped` | The events dropped since the previous `decision_logs_dropped`. |
| `dropped_total` | The events dropped since tessera started. |

### `evaluation_error`

Conditions of a rule could not be evaluated, and counted against allowing (see [Indeterminate conditions](./expressions.md#indeterminate-conditions)). tessera writes the first one at once, then at most one event every 10 seconds for each rule and cause, and the rest when the shutdown ends.

```text
ts=2026-11-22T10:16:04.513Z level=warn msg=evaluation_error rule=office cause=client_ip_missing count=1
```

| Field | Value |
| --- | --- |
| `rule` | The rule. |
| `cause` | `client_ip_missing`, `expression`, `panic`, `set_unavailable` or `database_error`, as in [`tessera_rule_evaluation_errors_total`](./metrics.md#tessera-rule-evaluation-errors-total). |
| `count` | The conditions of this rule with this cause since the previous `evaluation_error` for them. |

### `panic_recovered`

tessera recovered from a failure of its own code, and went on serving. During the evaluation of the condition of a rule, the condition is indeterminate, unless `&&` or `\|\|` absorbs the failure; the event comes in both cases. During the evaluation of an [upstream header](./configuration.md#upstream-headers), the header is removed, and during that of a [decision field](./configuration.md#log-decision-fields), the field is empty: the event has an empty `rule`. Anywhere else in the handling of a call, the call fails without a decision (see [Failure modes](./failure-modes.md)). tessera writes the first one at once, then at most one event every 10 seconds for each rule, and the rest when the shutdown ends.

| Field | Value |
| --- | --- |
| `rule` | The rule, or empty for a failure outside the evaluation of a condition, as in an upstream header or a decision field. |
| `count` | The failures in this rule, or outside a rule, since the previous `panic_recovered` for it. |
| `error` | What failed, for the first of them: the message of the Go runtime, such as `assignment to entry in nil map`, or the type of the value of the failure. |
| `stack` | The stack of the first of them, at most 16 KiB: report it with the [bug](https://github.com/gtriggiano/tessera/issues). |

## What is never logged

- The query of a request, and the values of its [secret headers](./configuration.md#secret-headers): `authorization`, `proxy-authorization`, `cookie` and those of `secret_headers`. A [decision field](./configuration.md#log-decision-fields) cannot read them. What tessera derives from a secret header, such as the attributes of `ua`, is not secret (see [`secret_headers`](./configuration.md#secret-headers)).
- The other headers of a request, but `x-request-id`, which is `request_id`, unless a decision field reads them.
- Configuration values, except the path of the configuration file, the addresses of the listeners, the keys that `config_warning` names, the names of the rules, of the sets and of the decision fields, and the types of the sets. The names of `secret_headers` and of `upstream.headers` are never logged. The value of a decision field is not a configuration value: it is what its expression computes for the request.
