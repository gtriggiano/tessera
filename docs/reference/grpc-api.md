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

`envoy.service.auth.v3.Authorization/Check` decides an HTTP request that Envoy describes. Every request gets the decision of [`policy.default_action`](./configuration.md#policy-default-action), whatever the request.

The call succeeds, with gRPC status `OK`, for both decisions: the decision is in the `status` field of the `CheckResponse`.

| Decision | `CheckResponse` | What Envoy does |
| --- | --- | --- |
| Allow | `status.code` is `OK` (0). `ok_response` is empty: no header to add, set or remove. | Sends the request on, unchanged. |
| Deny | `status.code` is `PERMISSION_DENIED` (7). `denied_response` has the HTTP status `403` and no body and no header. | Answers the client with 403 Forbidden and an empty body. |

The response carries nothing else: no message in the status, no dynamic metadata.

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
