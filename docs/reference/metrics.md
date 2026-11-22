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
| [`tessera_decisions_total`](#tessera-decisions-total) | counter | `action`, `enforced`, `rule`, `reason` |
| [`tessera_decision_duration_seconds`](#tessera-decision-duration-seconds) | histogram | `action` |
| [`tessera_audit_matches_total`](#tessera-audit-matches-total) | counter | `rule` |
| [`tessera_rule_evaluation_errors_total`](#tessera-rule-evaluation-errors-total) | counter | `rule`, `cause` |
| [`tessera_policy_audit_mode`](#tessera-policy-audit-mode) | gauge | none |
| [`tessera_decision_logs_dropped_total`](#tessera-decision-logs-dropped-total) | counter | none |
| [`tessera_set_entries`](#tessera-set-entries) | gauge | `set`, `type`, `source` |
| [`tessera_set_available`](#tessera-set-available) | gauge | `set`, `type`, `source` |
| [`tessera_set_last_refresh_success_timestamp_seconds`](#tessera-set-last-refresh-success-timestamp-seconds) | gauge | `set`, `type`, `source` |
| [`tessera_set_refreshes_total`](#tessera-set-refreshes-total) | counter | `set`, `type`, `source`, `result` |
| [`tessera_set_refresh_duration_seconds`](#tessera-set-refresh-duration-seconds) | histogram | `set`, `type`, `source` |

Every series exists from startup, at zero for a counter: the values of the labels come from the configuration and from fixed lists, never from a request, so the number of series depends on the configuration alone. A thousand hosts, paths or client addresses add no series. Scrapes read memory only.

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

## Decisions

### `tessera_decisions_total`

**Type:** counter\
**Labels:** `action`, `enforced`, `rule`, `reason`

The decisions of tessera, one for each request that Envoy asks about and that tessera decides.

| Label | Values |
| --- | --- |
| `action` | `allow` or `deny`: what the policy decided. |
| `enforced` | `false` when [`policy.mode: audit`](./configuration.md#policy-mode) let a denied request through, `true` otherwise. |
| `rule` | The name of the rule that decided, or `default` for [`policy.default_action`](./configuration.md#policy-default-action). |
| `reason` | `rule`: the condition of the rule was true. `default`: no rule decided. `error`: the condition of a `deny` rule could not be evaluated (see [Indeterminate conditions](./expressions.md#indeterminate-conditions)). |

The series are those that the policy can produce: for each rule not in `audit` mode, its action with the reason `rule`, and for a `deny` rule the reason `error` too, and the default action with the reason `default`. A rule in `audit` mode never decides, and has no series here: see [`tessera_audit_matches_total`](#tessera-audit-matches-total).

```text
tessera_decisions_total{action="deny",enforced="true",reason="rule",rule="blocked-networks"} 12
tessera_decisions_total{action="deny",enforced="true",reason="error",rule="blocked-networks"} 1
tessera_decisions_total{action="allow",enforced="true",reason="rule",rule="office"} 4508
tessera_decisions_total{action="deny",enforced="true",reason="default",rule="default"} 97
```

### `tessera_decision_duration_seconds`

**Type:** histogram\
**Labels:** `action`\
**Buckets:** `0.000025`, `0.00005`, `0.0001`, `0.00025`, `0.0005`, `0.001`, `0.0025`, `0.005`, `0.01`, `0.025`, `0.05`, `0.1`

The time of a decision, by `action`, `allow` or `deny`: from when tessera received the call of Envoy to the decision, the reading of the request and of its client address and the evaluation of the rules included. It does not count the recording of the decision, nor the time of the call in the network.

The rate of the sum over both actions, such as `sum(rate(tessera_decision_duration_seconds_sum[5m]))`, is the average number of decisions in progress over that window, as this histogram measures them.

### `tessera_audit_matches_total`

**Type:** counter\
**Labels:** `rule`

The requests for which the condition of a rule in [`audit` mode](./configuration.md#policy-rules-mode) was true, by rule. Each rule in `audit` mode has a series.

### `tessera_rule_evaluation_errors_total`

**Type:** counter\
**Labels:** `rule`, `cause`

The conditions that could not be evaluated and that counted against allowing, by rule and cause (see [Indeterminate conditions](./expressions.md#indeterminate-conditions)). Each rule has a series for each cause:

| `cause` | The condition |
| --- | --- |
| `client_ip_missing` | Read `client.ip`, and the request had no client address. |
| `expression` | Failed otherwise, such as on a missing header, a failed conversion, or the cost limit. |
| `panic` | Made tessera fail while it evaluated the condition, which it recovered (see [`panic_recovered`](./logs.md#panic-recovered)). |
| `set_unavailable` | Looked a value up in a set that was unavailable, with [`on_unavailable: fail`](./configuration.md#sets-name-on-unavailable). |

A failure that `&&` or `||` absorbs does not make the condition indeterminate, and is not counted. A failure of tessera outside the evaluation of a condition is not counted either: the call fails without a decision, and [`panic_recovered`](./logs.md#panic-recovered) reports it (see [Failure modes](./failure-modes.md)). The log event [`evaluation_error`](./logs.md#evaluation-error) reports the same conditions.

### `tessera_policy_audit_mode`

**Type:** gauge\
**Labels:** none

`1` when [`policy.mode`](./configuration.md#policy-mode) is `audit`, `0` otherwise.

### `tessera_decision_logs_dropped_total`

**Type:** counter\
**Labels:** none

The [`decision`](./logs.md#decision) events that tessera dropped instead of writing them: because their queue was full, as when standard output is slower than the decisions, because the output was closed at the end of a shutdown, or because a write to standard output failed. See [The queue of decision events](./logs.md#the-queue-of-decision-events).

## Sets

The metrics of the sets have three labels: `set` is the name of the set, `type` its type, `cidr`, `asn` or `string`, and `source` where tessera reads it from:

| `source` | The set | Series |
| --- | --- | --- |
| `values` | Written in the configuration, in [`sets.<name>.values`](./configuration.md#sets-name-values). It never refreshes. | [`tessera_set_entries`](#tessera-set-entries) and [`tessera_set_available`](#tessera-set-available). |
| `file` | Read from [`sets.<name>.file`](./configuration.md#sets-name-file), and checked every [`sets.<name>.refresh`](./configuration.md#sets-name-refresh), which describes a check. | Every metric of this section. |

Each series that a set has exists from startup. tessera loads every set before it opens its listeners; for a set read from a file, that load counts as a check with the result `updated`. The log events [`set_loaded`](./logs.md#set-loaded), [`set_load_failed`](./logs.md#set-load-failed), [`set_unavailable`](./logs.md#set-unavailable) and [`set_available`](./logs.md#set-available) report the same changes.

### `tessera_set_entries`

**Type:** gauge\
**Labels:** `set`, `type`, `source`

The entries of the copy of the set in use: the values read from its source, a value written twice counted twice, as [`sets.<name>.max_entries`](./configuration.md#sets-name-max-entries) counts them. It changes when a new copy replaces the one in use, and stays the same when a check fails.

### `tessera_set_available`

**Type:** gauge\
**Labels:** `set`, `type`, `source`

`1` when the set is available, `0` when it is not. A set read from a file is unavailable while its last successful check, or the start of the checks before the first one, is older than [`sets.<name>.max_staleness`](./configuration.md#sets-name-max-staleness), and `contains` then answers what [`sets.<name>.on_unavailable`](./configuration.md#sets-name-on-unavailable) says. A set of `values` is always available.

### `tessera_set_last_refresh_success_timestamp_seconds`

**Type:** gauge\
**Labels:** `set`, `type`, `source`

The time of the last successful check of a set read from a file, in seconds since the Unix epoch: a check that found the file unchanged or that replaced the copy, the load at startup included. `time() - tessera_set_last_refresh_success_timestamp_seconds` is the age of the copy in use since that check, which `max_staleness` limits once tessera has checked the file. Before the first check, `max_staleness` counts from the start of the checks, when tessera is ready: until then, this age exceeds the one that `max_staleness` limits by the time that tessera took to start.

### `tessera_set_refreshes_total`

**Type:** counter\
**Labels:** `set`, `type`, `source`, `result`

The checks of the file of a set, the load at startup included, by result. Each set read from a file has a series for each result:

| `result` | The check |
| --- | --- |
| `updated` | Found new valid content, which replaced the copy in use. The load at startup counts here. |
| `unchanged` | Found the file unchanged, or read it and found the content of the copy in use. |
| `rejected` | Found content that is invalid, or that exceeds a ceiling or `max_entries`: the copy in use stays. |
| `error` | Found no file, or one that cannot be opened or read, or that is not a regular file: the copy in use stays. |

`updated` and `unchanged` are successful checks. A check that fails is counted at each `refresh` for as long as the file stays the same, while [`set_load_failed`](./logs.md#set-load-failed) reports the failure once.

### `tessera_set_refresh_duration_seconds`

**Type:** histogram\
**Labels:** `set`, `type`, `source`\
**Buckets:** `0.0001`, `0.001`, `0.01`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, `10`, `30`, `60`

The time of each check of the file of a set that [`tessera_set_refreshes_total`](#tessera-set-refreshes-total) counts, whatever its result, the load at startup included: the reading of the state of the file and, when it has to be read, the reading of the file and the building of the new copy. A check that trusts the state of an unchanged file reads no content.

## Go and process metrics

`/metrics` also exports the standard collectors of the Prometheus Go client:

- `go_*`: the Go runtime, such as `go_goroutines`, `go_threads`, `go_info`, `go_gc_duration_seconds` and the `go_memstats_*` memory statistics;
- `process_*`: the process, such as `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds` and `process_start_time_seconds`. Which `process_*` metrics exist depends on the operating system.

There are no metrics about the scrapes themselves.
