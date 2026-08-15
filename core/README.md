# WindShare core

`github.com/windshare/windshare/core/...` is WindShare's network-free package
subtree inside the single production Go module. It owns capability links,
catalog and content contracts, authenticated session state, transfer
orchestration, and root-confined filesystem adapters. Signaling and concrete
transports remain outside this package boundary.

An explicit dependency-graph gate prevents core packages from importing
non-core WindShare packages or concrete networking and transport capabilities.
The project may reconsider a separate core module after real external consumers
exist and need an independent compatibility and release lifecycle; the current
pre-v1 package API makes no compatibility promise for that possible split.

## Validate

From the repository root:

```sh
GOWORK=off go test ./core/...
make ci
```

`testvectors/` is the single canonical Go↔TypeScript protocol-vector
inventory and is included in the root module release archive.

Licensed under Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
