# Versioning

From version 1.0.0, **tessera** follows [Semantic Versioning 2.0.0](https://semver.org/), applied to the public interface below. Before you move to a new major version, read the **⚠ BREAKING CHANGES** section of the [changelog](./changelog.md): each breaking change there says how to migrate.

## Public interface

These are the public interface. An incompatible change to any of them is a breaking change:

- **The configuration file**: keys, types, defaults and allowed values. A stricter validation that rejects a file accepted before is a breaking change. A breaking change never gives an existing key a new meaning: it renames or removes the key, so that tessera rejects a file written for the old format instead of reading it differently.
- **The expression language** of the rule conditions: attributes, types and functions.
- **The CLI**: commands, flags, the `TESSERA_CONFIG` environment variable and exit statuses.
- **The responses to Envoy**: the decision, the status and content of denials, and the headers added to or removed from the request.
- **The admin endpoints** and their responses.
- **Metrics and log events**: metric names, labels and buckets, and event names and fields, which alerts and dashboards depend on.
- **The container image**: entrypoint, user, default configuration path, exposed ports and platforms.
- **The names of the release assets**, which installation scripts depend on.

These are not: the Go packages in the repository, which are internal, the wording of the documentation, performance, and the exact text of error messages on standard error.

## Release candidates

Before some releases, a release candidate `X.Y.Z-rc.N` lets you try the next version. A release candidate is a GitHub pre-release with its own image tag, such as `1.0.0-rc.1`. It never becomes the latest release, never moves the floating image tags, and is not for production. Its documentation is under `/tessera/next/` until the version is released.

## Supported versions

Only the latest release receives fixes, as a new patch release. Published releases never change: a defect found after a release is fixed in a new version, never by replacing the files or the image of a published one.
