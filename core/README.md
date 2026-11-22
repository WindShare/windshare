# WindShare core

> This page outlines the `core` subtree's shape and boundaries. Package sources, [`docs/protocol.md`](../docs/protocol.md), and [`docs/validation.md`](../docs/validation.md) are the sources of truth for specifics.

`github.com/windshare/windshare/core/...` is WindShare's network-free package subtree inside the root production Go module. It owns protocol identities, sealed object formats, directory/authority verification, catalog and content contracts, session state, transfer orchestration, and shared capacity and observability primitives. Concrete transports ([`connectivity/`](../connectivity/), [`transport/`](../transport/)) and native filesystem implementations ([`osfs/`](../osfs/), [`adapters/filesystem/`](../adapters/filesystem/)) live outside `core` and are composed by [`engine/`](../engine/) and hosts.

The pre-v1 package API makes no compatibility promise; a separate module would only be reconsidered when external consumers require an independent release lifecycle.

## Package layout

| Packages | Responsibility |
|---|---|
| `link/`, `urlhost/`, `networkendpoint/` | Capability links, IDNA host normalization, relay/STUN/WebSocket endpoint normalization |
| `nodedirectory/`, `serviceauthority/` | Signed network directory, account ticket, and node descriptor verification (no I/O) |
| `senderobject/`, `senderauth/` | Sealed sender-object envelopes and canonical Ed25519 signature acceptance |
| `catalog/` | Directory generations/pages, durable catalog backend and spill, path policy, source errors/omissions |
| `content/`, `content/records/` | File revisions, range leases, key tree, authenticated records, single-lock handle pool |
| `framechannel/`, `session/` | Transport-neutral channels, `sendersession`/`receiversession` runtimes, `catalogflow`/`contentflow`, peer path controls |
| `transfer/` | Receive contracts (`receivecontract/`, `ordinaryoutput/`), job lifecycle, `BlockBroker`, `lanes/` scheduling, `fault/` |
| `liveshare/` | Composition of sender and receiver runtimes (`PrepareSender`, `PrepareReceiver`, `ReceiveCapacity`) |
| `capacityledger/`, `observationstream/`, `diagnostics/`, `diagnosticerror/`, `downloadmetrics/` | Scalar capacity ledgers, bounded observation queues, source diagnostics, download metrics |
| [`testvectors/`](testvectors/), `internal/` | Go/TypeScript/Rust interoperability vectors (`inventory.txt`), HKDF, pinned Unicode 15, contract harnesses |

## Ownership and boundaries

- **Dependency gates**: CI (`_coreboundary`) blocks non-core WindShare imports in production (allowing `osfs` only in test-graph deltas) and concrete networking or transport capabilities, and enforces isolation across `session/` role packages (`sendersession`, `receiversession`, `sessioncore`, `observation`).
- **File sources**: `liveshare.PrepareSender` consumes an injected `FileSourceFactory` (natively `osfs.SelectedFileSource` via `adapters/filesystem`). Acquisition reads selected-root metadata on demand without traversing descendants; `catalog.SourceReference` stays private to the adapter, and `content.StableFile` owns handle lifetimes.
- **Budgets**: The application supplies process-wide sender budgets (`content.HandlePool`, `catalog.BudgetAccount`, `contentflow.ProcessCacheBudget`) and receiver capacity (`liveshare.ReceiveCapacity`); shares and receivers retain and release their own reservations.
- **Transfer**: `transfer` owns receive jobs, `BlockBroker`, and output settlement contracts; `transfer/lanes` owns lane admission, block attempts, cancellation, and lane settlement. Brokers consume `BlockFetcher`, `receiversession` binds `downloadmetrics.Metrics` to both producers, and both layers normalize closed failures in `transfer/internal/failurestate`.

## Validate

From the repository root (see [`docs/validation.md`](../docs/validation.md) for full `make ci` / `make ci-parallel` gates):

```sh
GOWORK=off go test ./core/...
make short-go
make vectors
make hygiene
```

[`testvectors/`](testvectors/) is included in the root module release archive. Licensed under AGPL-3.0-or-later; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
