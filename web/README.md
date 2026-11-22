# WindShare Web

> This page outlines the Web app's shape and validation entry points. Code under [`src/`](src/) and [`docs/browser-downloads.md`](../docs/browser-downloads.md) are the sources of truth for specifics.

React 19 + TypeScript + Vite landing portal and browser receiver. It opens suite-02 capability links, authenticates the share descriptor, browses catalog pages on demand, and previews or downloads file ranges over relay or WebRTC lanes.

See [`docs/protocol.md`](../docs/protocol.md), [`docs/browser-downloads.md`](../docs/browser-downloads.md), [`docs/validation.md`](../docs/validation.md), and [`docs/performance.md`](../docs/performance.md).

## Source layout

| Directory | Responsibility |
|---|---|
| `src/document/` | Portal/receiver document lifecycle and runtime loading (`App.tsx`) |
| [`src/application/`](src/application/README.md) | Browser receive/share assembly, task commands, and output-route coordination |
| `src/crypto/`, `src/protocol/` | Suite-02 links/key hierarchy, sealed-object auth, canonical CBOR/text, control payloads |
| `src/contracts/`, `src/session/` | `FrameChannel` contracts and protocol session runtime |
| `src/catalog/`, `src/content/` | Progressive catalog paging/cache, revisions, range brokering, and lane scheduling |
| `src/transport/`, `src/connectivity/`, `src/receiver/` | Relay/WebRTC channels, signaling, peer/TURN adoption, and reconnect supervision |
| `src/network/` | Signed node directory and network authority verification/caching |
| `src/transfer/` | Receive intent, progressive discovery, job lifecycle, checkpoints, and settlement |
| `src/output/` | Artifact authority, FSA/OPFS/portable output, and recovery persistence |
| `src/diagnostics/`, `src/security/` | Bounded tracing/incident export and capability redaction |
| `src/preview/`, `src/ui/` | Image/MP4 range preview, landing portal, and browser receiver UI |
| `src/unicode/` | Pinned Unicode 15 path normalization and case folding |

## Development

Install Web dependencies (`pnpm -C web install --frozen-lockfile`) and Playwright runtimes outside the gates.
On Windows, run `.\dev.ps1` from the repository root to start Vite and a local relay.

```sh
pnpm -C web dev
pnpm -C web lint
pnpm -C web build
pnpm -C web test
make web
make browser
```

See [`package.json`](package.json) for browser test scripts and [`docs/validation.md`](../docs/validation.md) for gate and CI details.
