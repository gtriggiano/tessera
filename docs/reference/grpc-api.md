---
description: "The gRPC listener of tessera serve: Envoy's external authorization service, the responses to Envoy, the health service and the limits."
---

# gRPC API reference

`tessera serve` answers Envoy on the gRPC listener, at [`server.listen`](./configuration.md#server-listen): Envoy's external authorization service, version 3, and the gRPC health service. This page lists the services, the responses that **tessera** gives Envoy, and the limits of the listener. The responses to Envoy are part of the [public interface](./versioning.md#public-interface).

## The listener

- `server.listen` is a TCP address, `:9001` by default, or a Unix socket.
- The listener speaks gRPC over cleartext HTTP/2, without TLS. It has no authentication: let only Envoy reach it, through a private network or a Unix socket.
- A Unix socket is created when tessera starts, with the permissions that the umask of the process allows, and removed when it stops. A file that already exists at its path stops the startup (see [`startup_failed`](./logs.md#startup-failed)).
- The listener offers no server reflection.

## Services

| Service | Methods |
| --- | --- |
| [`envoy.service.auth.v3.Authorization`](#authorization-service) | `Check` |
| [`grpc.health.v1.Health`](#health-service) | `Check`, `Watch`, `List` |

### Authorization service

`envoy.service.auth.v3.Authorization/Check` decides an HTTP request that Envoy describes, with the rules of [`policy`](./configuration.md#policy) (see [Write policy rules](../guide/policy-rules.md)).

The call succeeds, with gRPC status `OK`, for both decisions: the decision is in the `status` field of the `CheckResponse`.

| Decision | `CheckResponse` | What Envoy does |
| --- | --- | --- |
| Allow | `status.code` is `OK` (0). `ok_response` has the [upstream headers](./configuration.md#upstream-headers): in `headers`, those with a value, and in `headers_to_remove`, the names of the others. Without upstream headers, `ok_response` is empty. | Sends the request on, with the headers of `headers` set and those of `headers_to_remove` removed. |
| Deny | `status.code` is `PERMISSION_DENIED` (7). `denied_response` has the status, the body and the headers of the response of the rule that denied the request, or of [`policy.deny_response`](./configuration.md#policy-deny-response): `403`, without a body and without headers, by default. | Answers the client with that status, body and headers. |
| Deny, with [`policy.mode: audit`](./configuration.md#policy-mode) | As for Allow, with the upstream headers too. | As for Allow. |

The response carries nothing else: no message in the status, no dynamic metadata. tessera sends each header of `ok_response` and of a denial with `append` set to `false` and `append_action` set to `OVERWRITE_IF_EXISTS_OR_ADD`, so that Envoy replaces a header of the same name, or adds the header, instead of adding a second one: the value that the client sent for an upstream header does not reach the upstream. Envoy, up to at least version 1.40, reads `append` for these headers, and `append_action` only for `response_headers_to_add`, which tessera does not use: both fields say the same, replace the header of the same name or add it.

An upstream header goes to `headers_to_remove` when its expression gives an empty string, fails, or gives a value that cannot be sent: longer than 4096 bytes, with an ASCII control character other than the horizontal tab, or not valid UTF-8. The decision does not change. tessera lists the headers in the order of their names. With the configuration of the example of [`upstream.headers`](./configuration.md#upstream-headers), the `ok_response` of an allowed request from an address that the databases locate in Italy, then from an address that they do not have, in the text format of Protocol Buffers:

```text
ok_response { headers { header { key: "x-client-asn" value: "64496" } append { value: false } append_action: OVERWRITE_IF_EXISTS_OR_ADD } headers { header { key: "x-client-country" value: "IT" } append { value: false } append_action: OVERWRITE_IF_EXISTS_OR_ADD } }
ok_response { headers_to_remove: "x-client-asn" headers_to_remove: "x-client-country" }
```

When tessera fails while it handles a call, which it recovers (see [Failure modes](./failure-modes.md)), the call ends with the gRPC status `INTERNAL` and the message `internal error`, without a decision: Envoy handles it as it handles any call that fails (see [What Envoy needs](#what-envoy-needs)). A failure while it evaluates a condition or an upstream header is not such a failure: the condition is indeterminate, or the header removed.

Envoy adds to a denial:

- `content-type: text/plain` when the body is not empty and the headers have no `content-type`, and the `content-length` of the body;
- its own `date` header, and by default its own `server` header;
- for a status that its list of statuses does not name, such as `451`, the status as it is, with the reason phrase `Unknown` in HTTP/1.1.

tessera sends the header names in lowercase, those of the upstream headers too.

### Health service

`grpc.health.v1.Health` reports whether tessera is ready, as [`/readyz`](./admin-endpoints.md#readyz) does, for two service names: the empty name and `envoy.service.auth.v3.Authorization`.

| Status | When |
| --- | --- |
| `SERVING` | From the startup, once both listeners are open. |
| `NOT_SERVING` | From the start of a shutdown (see [Signals of `serve`](./cli.md#signals-of-serve)). |

- `Check` answers the status of either name, and `NOT_FOUND` for any other name.
- `Watch` sends the status of the name, then each change. For another name it sends `SERVICE_UNKNOWN`. tessera ends every `Watch` call when its gRPC listener closes during a shutdown.
- `List` answers both names with their status.

## Limits

| Limit | Value |
| --- | --- |
| Open connections | [`server.max_connections`](./configuration.md#server-max-connections), `1024` by default |
| Concurrent calls on one connection | [`server.max_concurrent_streams`](./configuration.md#server-max-concurrent-streams), `1024` by default |
| Size of a request message | 256 KiB, fixed |
| Age of a connection | [`server.max_connection_age`](./configuration.md#server-max-connection-age), no limit by default |
| HTTP/2 handshake of a new connection | 5 s, fixed |

- **Open connections.** The count includes the connections still in their handshake. A connection beyond the limit is closed as soon as tessera accepts it, before any HTTP/2 exchange, and counts in [`tessera_grpc_connections_refused_total`](./metrics.md#tessera-grpc-connections-refused-total). tessera refuses it rather than letting it wait: a connection left in the operating system's backlog would look connected to Envoy, whose connect timeout would not apply, and its calls would wait until the timeout of the filter. Envoy sees the connection close at once, and fails the call as described in [What Envoy needs](#what-envoy-needs).
- **Concurrent calls.** tessera announces the limit to each client when the connection opens, and a client that keeps to it, as Envoy does, starts no more calls than that on one connection. A call beyond the limit is refused with the HTTP/2 error `REFUSED_STREAM`.
- **Message size.** A `CheckRequest` larger than 256 KiB fails with the gRPC status `RESOURCE_EXHAUSTED`, without a decision. Envoy then applies its own behavior for a failed call.
- **Connection age.** At that age, with a random variation of up to 10%, tessera asks the client to close the connection, and lets the calls in progress on it finish.
- **Handshake.** A connection whose client has not completed the HTTP/2 handshake 5 s after it opened is closed.
- **During a shutdown.** When the gRPC listener closes, the connections whose client has sent nothing are closed at once; a connection still in its handshake is closed at the end of its 5 s, or when [`server.shutdown_timeout`](./configuration.md#server-shutdown-timeout) passes, whichever comes first.

## What Envoy needs

Configure Envoy's `ext_authz` HTTP filter to call tessera over gRPC, with version 3 of the protocol, through a cluster that uses HTTP/2:

```yaml [envoy.yaml]
http_filters:
  - name: envoy.filters.http.ext_authz
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthz
      transport_api_version: V3
      failure_mode_allow: false
      grpc_service:
        envoy_grpc: { cluster_name: tessera }
        timeout: 1s
  - name: envoy.filters.http.router
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router

clusters:
  - name: tessera
    type: STRICT_DNS
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
    load_assignment:
      cluster_name: tessera
      endpoints:
        - lb_endpoints:
            - endpoint:
                address:
                  socket_address: { address: tessera.example.com, port_value: 9001 }
```

::: danger Set failure_mode_allow to false
With `failure_mode_allow: true`, Envoy allows every request that it cannot get a decision for: when tessera is unreachable, too slow, or answers a call with an error. With `false`, Envoy answers such requests with the status of `status_on_error`, 403 Forbidden by default.
:::

- tessera does not read the request body: leave `with_request_body` unset, so that a large body cannot exceed the message size limit.
- For gRPC health checks from Envoy or from an orchestrator, use the empty service name or `envoy.service.auth.v3.Authorization`.
- tessera reads the client address of a request from the address of the connection to Envoy, unless [`client_ip`](./configuration.md#client-ip) declares another source. [Set the client address](../guide/client-address.md) describes the Envoy settings for each source.

### Header names with underscores

With [`upstream.headers`](./configuration.md#upstream-headers), set [`headers_with_underscores_action`](https://www.envoyproxy.io/docs/envoy/v1.40.2/api-v3/config/core/v3/protocol.proto#envoy-v3-api-enum-config-core-v3-httpprotocoloptions-headerswithunderscoresaction) in the `common_http_protocol_options` of the HTTP connection manager, so that a client cannot send a header whose name differs from an upstream header only by underscores:

```yaml [envoy.yaml]
- name: envoy.filters.network.http_connection_manager
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
    stat_prefix: ingress
    common_http_protocol_options:
      headers_with_underscores_action: REJECT_REQUEST
```

- `REJECT_REQUEST` refuses a request with such a header: Envoy answers an HTTP/1 request with 400, and resets an HTTP/2 stream.
- `DROP_HEADER` removes such a header from the request before the HTTP filters, `ext_authz` included, see it.

Envoy's [edge proxy best practices](https://www.envoyproxy.io/docs/envoy/v1.40.2/configuration/best_practices/edge) set `REJECT_REQUEST` too.

::: danger The default lets a header with underscores stand for an upstream header
With the default, `ALLOW`, Envoy forwards a header such as `x_client_country`, which tessera neither replaces nor removes, since it is not the upstream header `x-client-country`. An upstream that reads the headers as CGI meta-variables, which turn `-` into `_` ([RFC 3875](https://www.rfc-editor.org/rfc/rfc3875#section-4.1.18)), as CGI applications and WSGI servers ([PEP 3333](https://peps.python.org/pep-3333/#environ-variables)) do, can read both as `HTTP_X_CLIENT_COUNTRY`, and take the value of the client for that of tessera.
:::

### Request paths

tessera receives the path of a request as Envoy sends it, and does not decode or normalize it. Let Envoy normalize the path in the HTTP connection manager, as its [edge proxy best practices](https://www.envoyproxy.io/docs/envoy/v1.40.2/configuration/best_practices/edge) recommend, so that the path that tessera receives is the path that Envoy routes and sends to the upstream:

```yaml [envoy.yaml]
- name: envoy.filters.network.http_connection_manager
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
    stat_prefix: ingress
    normalize_path: true
    merge_slashes: true
    path_with_escaped_slashes_action: UNESCAPE_AND_REDIRECT
```

- `normalize_path` normalizes the path as RFC 3986 describes, and resolves its `.` and `..` segments: `/admin/../public` reaches tessera as `/public`.
- `merge_slashes` merges adjacent slashes: `//admin` reaches tessera as `/admin`.
- `path_with_escaped_slashes_action` acts on the encoded slashes `%2F` and `%5C`, in either case. `UNESCAPE_AND_REDIRECT` redirects the client to the path with them decoded; `REJECT_REQUEST` answers 400 instead, which suits gRPC clients and other clients whose paths never contain them.
