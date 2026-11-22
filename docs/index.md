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

**tessera** is an external authorization service for [Envoy](https://www.envoyproxy.io/), in development. [`tessera serve`](./reference/cli.md#tessera-serve) answers Envoy's authorization requests with [ordered rules](./guide/write-rules.md) on the host, method, path and headers of each request and on the client address, and serves [health checks](./reference/admin-endpoints.md) and [metrics](./reference/metrics.md) on a separate admin listener. Lists, location data and the client software are not available yet. Each feature gets its page in this documentation when it lands.

[What is tessera?](./guide/what-is-tessera.md) describes the problem it addresses and what it leaves to other tools.
