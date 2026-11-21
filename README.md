# tessera

**tessera** will be an external authorization service for [Envoy](https://www.envoyproxy.io/).

## Status

tessera is in development and has no release yet. The binary has only the `help` and `version` commands.

## Development

You need Go, at the version in [`go.mod`](go.mod), and a C compiler for the race detector. Inside a Git work tree, `make check` checks formatting and modules, runs `go vet` and the tests with the race detector, and builds the binary; `make build` builds `bin/tessera`.

## License

tessera is released under the [Apache License 2.0](LICENSE).
