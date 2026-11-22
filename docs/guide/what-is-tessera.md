---
description: "The problem tessera is being built to solve, what it leaves to other tools, and how it compares with the alternatives."
---

# What is tessera?

**tessera** is an external authorization service for [Envoy](https://www.envoyproxy.io/), in development. This page describes the problem it is being built to solve, what it leaves to other tools, and how it compares with the alternatives.

::: warning In development
tessera has no release yet. [`tessera serve`](../reference/cli.md#tessera-serve) decides Envoy's authorization requests with [ordered rules](./write-rules.md) on the host, method, path and headers of each request, on the client address, and on [sets](./sets.md) of networks, AS numbers or strings written in the configuration or read from local files, which it reloads when they change. Remote lists, the location data and the client software described on this page do not work yet. Each feature gets its own page when it lands.
:::

## The problem

Teams that run an ingress based on Envoy often need to answer questions like these before a request reaches the application:

- Does this call really come from a partner's network?
- Is this address on the block list that the security team updates during an incident?
- Can this page be served in this country?
- Does this traffic come from a hosting provider, and so probably from a program rather than a person?

Envoy's own filters answer part of this: the RBAC filter, for example, matches addresses and headers against lists written in Envoy's configuration, so updating a list means updating that configuration. General-purpose policy engines can answer all of it, but they ask you to learn a language, to source the data and to build the rest yourself.

tessera is being built to sit in between. Envoy will ask it about each HTTP request through the `ext_authz` filter, and tessera will decide from who sends the request (address, network, location, client software) and what it asks for (host, method, path), with ordered rules declared in one YAML file.

## What it leaves to other tools

- **Authentication.** tessera does not verify tokens, sessions or user certificates: Envoy has filters for that.
- **Rate limiting.** Envoy has a dedicated rate limit service.
- **Request bodies.** tessera does not inspect them: it is not a web application firewall.
- **Application permissions.** tessera decides on the origin and the shape of a request, not on what a user may do inside the application.
- **A control plane.** No Kubernetes custom resources, no operator, no user interface: one configuration file.
- **Proxies other than Envoy.** tessera is built for Envoy's `ext_authz` protocol.

## Compared with the alternatives

| Alternative | What it offers | How tessera is meant to differ |
| --- | --- | --- |
| Envoy's RBAC filter | Rules on addresses and headers, in Envoy's configuration | The policy lives in one file of its own, with lists that refresh without touching Envoy's configuration, and with country and network data built in |
| OPA with its Envoy plugin | A general language and any data | No Rego to learn and no data distribution to build; in exchange, tessera does only this |
| Application authorization services, such as Cerbos and OpenFGA | Who may do what on which resource | A different problem: tessera works earlier, on the origin of the request |
| oauth2-proxy and similar tools | User authentication | A different problem |

## The name

> **tessera** · *Latin and Italian, plural* tesserae · stressed on the first syllable: *TESS‑eh‑ra*
>
> 1. A small square of stone, glass or ceramic, set with many others into a mosaic.
> 2. A token shown to be let in: in Rome, the wooden tablet that carried the night's watchword through the army camp; in Italian today, the card you show at the door, from a membership card to a season pass.

The word probably comes from the Greek for *four*, after the four corners of the little block. Romans used tesserae for both of its jobs: they paved floors and walls with them, and they handed them out as tokens of trust, from the army's watchword to the tokens that let a guest prove who they were or a citizen collect a share of grain.

tessera takes its name from both meanings. Like the token at the gate, it decides who gets through. Like the tiles of a mosaic, its rules are small and plain on their own, and set side by side they make the whole access policy.

The name is always written in lowercase, even at the start of a sentence.

## Releases

The [versioning policy](../reference/versioning.md) lists the interfaces that releases will keep compatible, and the [changelog](../reference/changelog.md) lists the changes of each release.
