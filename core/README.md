# WindShare core

`github.com/windshare/windshare/core` is WindShare's network-free Go module. It
owns capability links, catalog and content contracts, authenticated session
state, transfer orchestration, and root-confined filesystem adapters. Signaling
and concrete transports remain in the repository's root module.

The module is pre-v1 and intentionally does not preserve obsolete protocol
models. Applications should pin an exact release.

## Verify a release

From this directory:

```sh
GOWORK=off go mod tidy -diff
GOWORK=off go list ./...
GOWORK=off go build ./...
GOWORK=off go test -race ./...
```

`testvectors/` is the single canonical Go↔TypeScript protocol-vector
inventory. It is included in module releases so the extracted module can run
its complete test suite without a parent repository.

Licensed under Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
