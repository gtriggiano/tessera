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

**tessera** is an external authorization service for [Envoy](https://www.envoyproxy.io/), in development. [`tessera serve`](./reference/cli.md#tessera-serve) answers Envoy's authorization requests, every one with the decision of [`policy.default_action`](./reference/configuration.md#policy-default-action), and serves [health checks](./reference/admin-endpoints.md) and [metrics](./reference/metrics.md) on a separate admin listener. Rules that look at the request are not available yet. Each feature gets its page in this documentation when it lands.

[What is tessera?](./guide/what-is-tessera.md) describes the problem it addresses and what it leaves to other tools.
