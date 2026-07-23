# WindShare Web receiver

The browser receiver is React 19 + TypeScript + Vite. It accepts suite-02
capability links, authenticates a small share descriptor, browses catalog pages
on demand, and reads file-local encrypted ranges over relay or WebRTC lanes.

The protocol authority is [`docs/协议规范.md`](../docs/协议规范.md); product and
phase semantics are in
[`docs/即时分享与文件浏览重构计划.md`](../docs/即时分享与文件浏览重构计划.md).

## Source layout

| Directory | Responsibility |
|---|---|
| `src/catalog/` | Authenticated descriptor/pages, frozen path policy, page storage, and selection rules |
| `src/content/` | File-local geometry, revisions, leases, range broker, and lane scheduling |
| `src/session/`, `src/receiver/` | ProtocolSession runtime and receiver-scoped reconnect supervision |
| `src/transport/`, `src/connectivity/` | v2 relay/WebRTC channels, signaling, and 0/8-second path policy |
| `src/transfer/`, `src/output/` | Progressive jobs, durable checkpoints, FSA/OPFS, single-file, and ZIP output |
| `src/preview/` | Image and MP4 range preview over the shared broker |
| `src/ui/` | Progressive browser UI and immediate capability-fragment erasure |
| `src/unicode/` | Pinned Unicode 15 normalization and full-fold tables |

There is no manifest compatibility path, packed-stream layout, global block
identity, or v1 receiver fallback.

## Local gates

```powershell
pnpm -C web install --frozen-lockfile
pnpm -C web lint
pnpm -C web exec tsc -b --force
pnpm -C web build
pnpm -C web forbidden
pnpm -C web test
```

`make web` runs the same sequence. The forbidden gate walks the production
dependency graph, scans all Web source/tests, and checks the built bundle.

## Browser evidence

Linux CI runs the retained Playwright safety, storage/output, and dedicated
WebRTC adapter suites. Windows real-socket execution must use
`scripts/d5-windows-performance.ps1 -Mode BrowserTests`; direct invocation is
rejected. Chromium is the currently installed matrix. Firefox/WebKit and a new
v2 real-process sender/relay experience suite remain acceptance work and are not
claimed by the unit or component gates.
