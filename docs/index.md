---
layout: home
hero:
  name: tessera
  text: External authz for Envoy.
  tagline: In development. There is no release yet.
  image:
    src: /logo.svg
    alt: tessera
  actions:
    - theme: brand
      text: What is tessera?
      link: /guide/what-is-tessera
    - theme: alt
      text: Versioning
      link: /reference/versioning
---

## Status

**tessera** is an external authorization service for [Envoy](https://www.envoyproxy.io/), in development. [`tessera serve`](./reference/cli.md#tessera-serve) answers Envoy's authorization requests with [ordered rules](./guide/policy-rules.md) on the host, method, path and headers of each request, on the client address, on the location and autonomous system that [enrichments](./guide/enrichments.md) derive from it with MaxMind DB files, on the client software that they recognize in the `user-agent` header, such as the browser, the kind of device or a bot, and on [sets](./guide/sets.md) of networks, AS numbers or strings written in the configuration or read from local files, or of geographic areas read from GeoJSON files. It reloads those files when they change, can add [headers](./reference/configuration.md#upstream-headers) of its own to the requests that it allows, and serves [health checks](./reference/admin-endpoints.md) and [metrics](./reference/metrics.md) on a separate admin listener.

[What is tessera?](./guide/what-is-tessera.md) describes the problem it addresses and what it leaves to other tools, and [How tessera works](./guide/how-tessera-works.md) follows a request through it.
