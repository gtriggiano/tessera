---
description: "What tessera does when data is missing, an expression fails, it is overloaded or unreachable, and how to change it."
---

# Failure modes

**tessera** is built to fail closed: when it cannot evaluate a condition, the condition counts against allowing, and missing data never turns a denial into an allow unless the configuration says so. This page lists what tessera does in each situation that it handles, and the key or the rule that changes it.

| Situation | What tessera does | How to change it |
| --- | --- | --- |
| No rule decides the request | It applies [`policy.default_action`](./configuration.md#policy-default-action). | The key is required: every configuration states it. |
| The client address is not available | Every condition whose result depends on `client.ip` is indeterminate: a `deny` rule denies, an `allow` rule does not match. [`tessera_rule_evaluation_errors_total`](./metrics.md#tessera-rule-evaluation-errors-total) counts it, with `cause="client_ip_missing"`, and [`evaluation_error`](./logs.md#evaluation-error) reports it. | A rule with `has(client.ip)` before them: see [Handle a missing client address](../guide/write-rules.md#handle-a-missing-client-address). |
| A header that a condition reads is missing | The condition is indeterminate, as above. | Test the header first: `"x-api-key" in request.headers && …`. |
| An expression fails: a conversion, an overflow, a division by zero | The condition is indeterminate when its result depends on the failure. | Write the condition so that `&&` or `\|\|` decides first: see [Indeterminate conditions](./expressions.md#indeterminate-conditions). |
| An evaluation exceeds the cost limit | It stops, and the condition is indeterminate, also inside `\|\|` or `&&`. | Simplify the condition: see [Limits](./expressions.md#limits). |
| tessera fails inside the evaluation of a condition | It recovers, the condition is indeterminate, and tessera goes on deciding. The event [`panic_recovered`](./logs.md#panic-recovered) reports it with its stack. | — |
| tessera fails elsewhere while it handles a call | It recovers, and the call ends with the gRPC status `INTERNAL`, without a decision: Envoy handles it as a call that failed. [`panic_recovered`](./logs.md#panic-recovered) reports it, with an empty `rule`. | Set `failure_mode_allow` to `false`, so that Envoy denies the request: see [What Envoy needs](./grpc-api.md#what-envoy-needs). |
| A rule is in audit mode | The rule never decides; an indeterminate condition does not deny. | [`policy.rules[].mode`](./configuration.md#policy-rules-mode). |
| The policy is in audit mode | Every request is allowed; a denial that is not enforced has `enforced=false` in its [`decision`](./logs.md#decision) event and in [`tessera_decisions_total`](./metrics.md#tessera-decisions-total). | [`policy.mode`](./configuration.md#policy-mode). |
| Standard output is slow or blocked | Decisions never wait for it: a `decision` event that does not fit its queue is dropped and counted. | See [The queue of decision events](./logs.md#the-queue-of-decision-events). |
| Too many concurrent calls on a connection from Envoy | The calls beyond the limit are refused. | [`server.max_concurrent_streams`](./configuration.md#server-max-concurrent-streams). |
| Too many connections from Envoy | The connections beyond the limit are closed at once. | [`server.max_connections`](./configuration.md#server-max-connections). |
| tessera is unreachable, too slow, or answers with an error | Envoy decides, as its `failure_mode_allow` says. | Set it to `false`: see [What Envoy needs](./grpc-api.md#what-envoy-needs). |
