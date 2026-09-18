# WindShare core

`github.com/windshare/windshare/core/...` is WindShare's network-free package
subtree inside the single production Go module. It owns capability links,
catalog and content contracts, authenticated session state, transfer
orchestration, and root-confined filesystem adapters. The native application
[engine](../engine/) owns task lifecycles and composes core with signaling and
concrete transports outside this package boundary.

An explicit dependency-graph gate prevents core packages from importing
non-core WindShare packages or concrete networking and transport capabilities.
The project may reconsider a separate core module after real external consumers
exist and need an independent compatibility and release lifecycle; the current
pre-v1 package API makes no compatibility promise for that possible split.

## File sources and ownership

`liveshare.PrepareSender` consumes a `FileSourceFactory`. Each acquired source
owns one share's selection and combines selected-root metadata,
`catalog.DirectoryScanner`, `content.RevisionSource`, and `Close`. The native
`osfs.SelectedFileSource` adapter accepts paths; other adapters can use provider
object IDs. `catalog.SourceReference` stores bounded opaque private references;
only the source adapter interprets them. Catalog names and protocol identities
remain independent of host lookup. Native selections stay bound to their acquired
root handles and cannot expand a selected file's authority to its siblings.

Acquisition reads selected-root metadata without traversing descendants or
reading file content. Catalog and content work finish before the sender closes
the source; each `content.StableFile` owns its own handle lifetime. Adapters must
preserve optional `content.RevisionContinuitySource` semantics, including sources
that prove stability only while a handle remains open. `catalog.SourceError`
distinguishes missing objects, access denial, unsupported capability, and stale
evidence while retaining the underlying error.

The application supplies the revision coordinator, process catalog account, and
process content-cache budget. Shares retain their own quotas and release their
own reservations; the native engine owns the aggregate budgets across tasks.

## Validate

From the repository root:

```sh
GOWORK=off go test ./core/...
make ci
```

`testvectors/` is the single canonical Go↔TypeScript protocol-vector
inventory and is included in the root module release archive.

Licensed under Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
