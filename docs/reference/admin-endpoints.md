---
description: "The admin listener of tessera serve: liveness, readiness and metrics, with their responses and fixed limits."
---

# Admin endpoints reference

`tessera serve` runs a second listener, the admin listener, for liveness, readiness and metrics. It listens on [`admin.listen`](./configuration.md#admin-listen), `:9090` by default, and serves cleartext HTTP/1.1: no TLS and no HTTP/2. This page lists its routes, their responses and its fixed limits. The admin endpoints and their responses are part of the [public interface](./versioning.md#public-interface) of **tessera**.

::: danger Keep the admin listener private
The admin endpoints have no authentication. Anyone who reaches them can read your metrics and watch readiness. Bind `admin.listen` to loopback or to a private network, restrict access to it with your network controls, and never expose it to the internet.
:::

## Routes

| Path | Answer |
| --- | --- |
| [`/livez`](#livez) | Liveness: 200 while the process runs. |
| [`/readyz`](#readyz) | Readiness: 200 when ready, 503 with what is missing otherwise. |
| [`/metrics`](#metrics) | Prometheus metrics. |

- **Methods.** Each route answers `GET` and `HEAD`. A `HEAD` gets the headers of the `GET` without the body.
- **Exact paths.** The path must match exactly as sent: `/livez/`, `/LIVEZ` and percent-encoded spellings such as `/%6Civez` are other paths. The query string is ignored.
- **404 Not Found** for any other path, whatever the method, with the body `not found`.
- **405 Method Not Allowed** for another method on one of the three paths, with `Allow: GET, HEAD` and the body `method not allowed`.
- **No caching.** Every response has `Cache-Control: no-store`.

There is no other route: no index page, and no debug or profiling endpoint.

## `/livez`

```text
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Length: 3
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
Date: Sun, 22 Nov 2026 10:50:42 GMT

ok
```

`/livez` answers 200 with the body `ok` for as long as the admin listener runs, throughout a shutdown too. It depends on nothing else.

## `/readyz`

`/readyz` answers 200 with the body `ready`, or 503 Service Unavailable with one line `not ready: <what is missing>` for each condition that is missing, as `text/plain; charset=utf-8` with `X-Content-Type-Options: nosniff`.

```text
not ready: shutting down
```

tessera is ready once both listeners are open, and stops being ready when a shutdown starts. The only condition that `/readyz` can report is therefore:

| Line | When |
| --- | --- |
| `not ready: shutting down` | A shutdown has started (see [Signals of `serve`](./cli.md#signals-of-serve)). |

The admin listener opens after the gRPC listener, and tessera reports ready as soon as both are open, before it answers any request: a probe never gets a 503 during the startup. The [gRPC health service](./grpc-api.md#health-service) reports the same state, as `SERVING` and `NOT_SERVING`, and the [`ready`](./logs.md#ready) and [`not_ready`](./logs.md#not-ready) events record each change.

## `/metrics`

`/metrics` serves every metric in the Prometheus text exposition format, or in the protobuf format when the scraper asks for it in its `Accept` header. OpenMetrics is not offered. The response is compressed with gzip when `Accept-Encoding` allows it.

```text
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Type: text/plain; version=0.0.4; charset=utf-8; escaping=underscores
Vary: Accept
Vary: Accept-Encoding
```

If the metrics cannot be gathered, the endpoint answers 500 Internal Server Error. The [metrics reference](./metrics.md) lists every metric.

## Fixed limits

The admin listener has its own limits, which you cannot configure, so that probes keep answering whatever the load on the gRPC listener:

| Limit | Value |
| --- | --- |
| Reading the request headers | 5 s |
| Reading the whole request | 10 s |
| Writing the response | 10 s |
| Idle keep-alive connection | 10 s |
| Request header size | 8 KiB, request line included, plus an allowance of 4 KiB; larger headers get 431 Request Header Fields Too Large |
| Open connections | 16; further clients wait in the operating system's backlog |

Probes and scrapes read memory only, and requests to the admin listener write no log event.

## During a shutdown

When a shutdown starts, `/readyz` answers 503 while `/livez` and `/metrics` keep answering. The admin listener stays open while the gRPC calls in progress finish, and closes after them, within [`server.shutdown_timeout`](./configuration.md#server-shutdown-timeout): orchestrators can watch the shutdown until its end. When it closes, the connections that are idle or that have not sent a request yet are closed at once. See [Signals of `serve`](./cli.md#signals-of-serve).
