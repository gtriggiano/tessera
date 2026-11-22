---
description: "How to choose where tessera reads the client address of a request, and what to configure in Envoy, for clients that reach Envoy directly, through proxies that append to X-Forwarded-For, or through a proxy that sets a header."
---

# Set the client address

**tessera** reads the address of the client that sent each request from one source, which you declare in [`client_ip`](../reference/configuration.md#client-ip). This page helps you choose the source for the way clients reach Envoy, and shows what to configure in Envoy for it.

## Choose a source

| Clients reach Envoy | Source | In Envoy |
| --- | --- | --- |
| Directly, or through a load balancer that sends the PROXY protocol | `peer`, the default | Nothing, or the PROXY protocol listener filter |
| Through proxies that append to `X-Forwarded-For` | `x_forwarded_for` | `use_remote_address: true` |
| Through a proxy that writes the client address in a header of its own | `header` | Nothing |

tessera never falls back on another source: when the source you declare gives no address, the request has no client address. Choose the source that every request comes through.

## Clients connect to Envoy

Keep the default, `peer`: tessera reads the address of the connection that Envoy received the request on, and reads no header, so nothing that a client sends changes the address. You can leave out the `client_ip` block, or write the default:

```yaml [config.yaml]
client_ip:
  from: peer
```

When a load balancer in front of Envoy sends the PROXY protocol, add Envoy's [PROXY protocol listener filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/listeners/listener_filters/proxy_protocol) to the listener: Envoy then reports the client address that the load balancer sends as the address of the connection, and `peer` reads it.

## Clients come through proxies that append to X-Forwarded-For

A proxy that forwards a request appends the address it received the request from to the `X-Forwarded-For` header. The entries at the right end of the header are those of the proxies you run; the entries on the left are whatever the client sent. tessera reads the header from the right end, skipping the entries of the proxies you trust.

1. Check that every proxy between the clients and Envoy appends to `X-Forwarded-For`.
2. Make Envoy append the address of the connection it received the request on: set `use_remote_address: true` in the HTTP connection manager, and leave `skip_xff_append` unset. Set `xff_num_trusted_hops` to the number of proxies in front of Envoy, so that Envoy trusts the same address as tessera:

   ```yaml [envoy.yaml]
   - name: envoy.filters.network.http_connection_manager
     typed_config:
       "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
       stat_prefix: ingress
       use_remote_address: true
       xff_num_trusted_hops: 1
   ```

3. Set [`client_ip.trusted_hops`](../reference/configuration.md#client-ip-trusted-hops) to the number of proxies in front of Envoy:

   ```yaml [config.yaml]
   client_ip:
     from: x_forwarded_for
     trusted_hops: 1
   ```

For example, a client at `203.0.113.10` reaches Envoy through one load balancer at `198.51.100.5`. The load balancer appends `203.0.113.10` to `X-Forwarded-For`, Envoy appends `198.51.100.5`, and the header that tessera receives lists `203.0.113.10` then `198.51.100.5`. With `trusted_hops: 1`, tessera skips `198.51.100.5` and reads `203.0.113.10`. A client that sends its own `X-Forwarded-For: 192.0.2.66` only adds an entry on the left: the header lists `192.0.2.66`, `203.0.113.10` and `198.51.100.5`, and the client address stays `203.0.113.10`.

::: danger Count the proxies exactly
With a `trusted_hops` above the number of proxies in front of Envoy, tessera reads an entry that the client wrote, and the client chooses its address. With one below, tessera reads the address of a proxy. When you add or remove a proxy, change `client_ip.trusted_hops` and `xff_num_trusted_hops` with it.
:::

## Clients come through a proxy that sets a header

Some proxies write the client address in a header of their own. Name that header in [`client_ip.header`](../reference/configuration.md#client-ip-header) only when the proxy sets it on every request, replacing any value that the client sent, and clients cannot reach Envoy without going through the proxy:

```yaml [config.yaml]
client_ip:
  from: header
  header: x-client-address
```

The header must hold exactly one address.

::: warning x-envoy-external-address
With `use_remote_address: true`, Envoy sets `x-envoy-external-address` to the client address it trusts, replacing the value that the client sent, but only for a request that it considers external. A request that it considers [internal](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_conn_man/headers#x-envoy-internal), one without `X-Forwarded-For` from an address that the `internal_address_config` of the HTTP connection manager lists, keeps the value that the client sent, if any. Name this header only when that list has no address that clients connect from.
:::
