# WindShare Web receiver

The browser receiver is React 19 + TypeScript + Vite. It accepts suite-02
capability links, authenticates a small share descriptor, browses catalog pages
on demand, and reads file-local encrypted ranges over relay or WebRTC lanes.

The protocol authority is [`docs/协议规范.md`](../docs/协议规范.md); product and
phase semantics are in
[`docs/即时分享与文件浏览重构计划.md`](../docs/即时分享与文件浏览重构计划.md).
Validation entry points are in [`docs/validation.md`](../docs/validation.md), and
local performance diagnostics are in
[`docs/performance.md`](../docs/performance.md).

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

The developer installs Web dependencies and Playwright browser runtimes outside
the gates. The commands below consume that environment without changing it:

```powershell
pnpm -C web lint
pnpm -C web build
pnpm -C web test
make web
make browser
```

`make web` runs ESLint, the TypeScript/Vite production build, and Vitest.
`make browser` runs the direct current-platform Chromium product smoke.

## Browser product tests

| Command | Product coverage |
|---|---|
| `pnpm -C web test:browser:smoke` | Relay-only CLI-to-Chromium micro-directory flow and exact downloaded bytes. |
| `pnpm -C web test:browser:progressive` | Progressive catalog paging across the authenticated page boundary. |
| `pnpm -C web test:browser:network` | Authenticated direct and TURN peer adoption after relay loss. |
| `pnpm -C web test:browser:interop` | Direct D1/D2 browser/Pion adapter interoperability. |
| `pnpm -C web test:browser:cross` | The relay-only product smoke in Firefox and WebKit. |

Ordinary GitHub CI runs the Chromium smoke on Linux. The weekly Windows job reuses
that scenario entry on Windows; the scheduled workflow owns every other
browser/network suite. Manual dispatch is only a diagnostic convenience.
