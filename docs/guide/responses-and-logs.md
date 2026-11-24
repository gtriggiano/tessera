---
description: "What tessera answers and records once the policy decides, and how to choose the response of a denied request, the headers that the upstream receives, the decision events and their fields, and the secret headers, and see them with tessera eval."
---

# Shape responses and logs

Once the policy decides a request, **tessera** answers Envoy and records the decision. This page shows how to choose what a denied client gets, send headers of your own to the upstream with the requests that Envoy sends on, choose the decisions that write an event and the fields of the events, keep secret headers out of what tessera writes, and see all of it with `tessera eval` before you deploy it.

This is the last step of the [path of a request](./how-tessera-works.md#response): the [policy rules](./policy-rules.md) have decided, tessera turns the decision into an answer for Envoy, counts it in the [metrics](../reference/metrics.md#decisions) and, for the decisions that you select, writes a `decision` event.

## Choose what Envoy answers

A denied client gets the status, body and headers of [`policy.deny_response`](../reference/configuration.md#policy-deny-response), `403` without a body by default. A `deny` rule can have a [`response`](../reference/configuration.md#policy-rules-response) of its own, whose keys replace those of `policy.deny_response`:

```yaml [config.yaml]
policy:
  default_action: deny
  deny_response:
    body: '{"error": "forbidden"}'
    headers:
      content-type: application/json
  rules:
    - name: admin
      action: deny
      when: request.path.startsWith("/admin") && !client.ip.inCidr("203.0.113.0/24")
      response:
        status: 404
        body: '{"error": "not found"}'
```

A request denied by `admin` gets `404`, the body `{"error": "not found"}` and the header `content-type: application/json`; any other denied request gets `403` and the body `{"error": "forbidden"}`.

With [`policy.mode: audit`](../reference/configuration.md#policy-mode), no client gets a denial: Envoy sends every request on, and the decision events report the denials that tessera did not enforce (see [Audit a policy](./policy-rules.md#audit-a-policy)).

## Send headers to the upstream

[`upstream.headers`](../reference/configuration.md#upstream-headers) sets headers of your own on every request that Envoy sends on: an allowed request, and a denied request that `policy.mode: audit` lets through. Each header is the value of an [expression](../reference/expressions.md#upstream-headers) whose result is a string, which tessera computes after the decision, with what the rules read: the same request, the same sets and the same records of the enrichments. Use them to tell the upstream what tessera knows of the client, such as what the [enrichments](./enrichments.md) say of its address:

```yaml [config.yaml]
enrichment:
  geo:
    database: /var/lib/geoip/GeoLite2-City.mmdb
  asn:
    database: /var/lib/geoip/GeoLite2-ASN.mmdb
upstream:
  headers:
    x-client-country: geo.country.code
    x-client-asn: 'asn.found ? string(asn.number) : ""'
```

- An expression that gives an empty string removes the header: `asn.found ? string(asn.number) : ""` sends no `x-client-asn`, rather than `0`, for an address that the database does not have.
- An expression that fails, or that gives a value that cannot be sent, removes the header too, and the decision does not change. [`tessera_upstream_header_errors_total`](../reference/metrics.md#tessera-upstream-header-errors-total) counts it, by header and cause, and no log event reports it.
- Some names are not allowed, such as `host`, `authorization` and the names that start with `x-envoy-`: [`upstream.headers`](../reference/configuration.md#upstream-headers) lists them, with the limits of the headers and of their values.

### Send a header that the upstream can trust

A header that `upstream.headers` declares belongs to tessera: tessera asks Envoy to replace the value that the client sent with its own, or to remove the header of the client when it has no value, so that the value of the client does not reach the upstream under that name (see the [gRPC API reference](../reference/grpc-api.md#authorization-service)). To let the upstream trust such a header:

1. Declare the header in `upstream.headers`, with an expression that gives an empty string when tessera has nothing to say, as `x-client-asn` above.
2. Set `headers_with_underscores_action` to `REJECT_REQUEST` in the HTTP connection manager of Envoy, as [Header names with underscores](../reference/grpc-api.md#header-names-with-underscores) shows. With Envoy's default, a client can send `x_client_country`, which tessera neither replaces nor removes, and an upstream that reads the headers as CGI variables can take its value for that of `x-client-country`.

To be warned when tessera removes a header because its expression fails, alert on the errors:

```text
increase(tessera_upstream_header_errors_total[5m]) > 0
```

## Log the decisions

A [`decision`](../reference/logs.md#decision) event records one decision: the request, the client address, the action, the rule and the reason. [`log.decisions`](../reference/configuration.md#log-decisions) chooses the decisions that write one: by default the denials, and the allows in which a rule in `audit` mode with `action: deny` matched; `all` for every decision, `none` for none. [`log.level`](../reference/configuration.md#log-level) applies too: the event of an allow has the level `info`, and that of a denial `warn`.

[`log.decision_fields`](../reference/configuration.md#log-decision-fields) adds fields of your own to every event, each the value of an expression whose result is a string:

```yaml [config.yaml]
enrichment:
  geo:
    database: /var/lib/geoip/GeoLite2-City.mmdb
log:
  decisions: all
  decision_fields:
    agent: request.headers["user-agent"]
    country: geo.country.code
```

- tessera computes the fields only for the events that it writes, after the decision, with what the rules read.
- A field whose expression fails, such as `agent` for a request without `User-Agent`, is empty.
- A decision field cannot read what tessera never logs: the query, a secret header, and the headers otherwise than by a literal name. tessera rejects such an expression when it reads the configuration (see [Decision fields](../reference/expressions.md#decision-fields) and [What is never logged](../reference/logs.md#what-is-never-logged)).

::: tip A decision never waits for its event
tessera queues the `decision` events and writes them in the background. When standard output cannot keep up, it drops events and counts them in [`tessera_decision_logs_dropped_total`](../reference/metrics.md#tessera-decision-logs-dropped-total) (see [The queue of decision events](../reference/logs.md#the-queue-of-decision-events)). [`tessera_decisions_total`](../reference/metrics.md#tessera-decisions-total) counts every decision.
:::

## Keep secret headers out of the logs

The values of `authorization`, `proxy-authorization` and `cookie` are always secret, and [`secret_headers`](../reference/configuration.md#secret-headers) adds headers of your own, such as an API key:

```yaml [config.yaml]
secret_headers:
  - x-api-key
policy:
  default_action: deny
  rules:
    - name: api-key
      action: allow
      when: '"x-api-key" in request.headers && request.headers["x-api-key"].startsWith("k-")'
```

Conditions and upstream headers read a secret header as any other. What tessera writes keeps its value out: a decision field cannot read it, so no event holds it, and `tessera eval` shows it as `[REDACTED]`, together with the value of an upstream header that reads it. Only the value is secret: what tessera derives from the header is not, such as the attributes of `ua` with `user-agent` among the secret headers (see [`secret_headers`](../reference/configuration.md#secret-headers)).

## See them before you deploy

[`tessera eval`](../reference/cli.md#tessera-eval) decides a request that you describe, without Envoy, and shows what tessera would answer:

- for a denial, the status of the response, and with `--output json` its body and headers;
- for a request that Envoy sends on, each upstream header with its value, or `removed`;
- the values of the secret headers as `[REDACTED]`.

```sh
tessera eval --config config.yaml --ip 192.0.2.10 --header 'x-api-key: k-123'
```

[Upstream headers](../reference/cli.md#upstream-headers) in the CLI reference shows the output. `tessera eval` writes no `decision` event, and so does not show the decision fields: see them in the events of `tessera serve`.
