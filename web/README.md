# WindShare Web receiver

The browser receiver is React 19 + TypeScript + Vite. It accepts suite-02
capability links, authenticates a small share descriptor, browses catalog pages
on demand, and reads file-local encrypted ranges over relay or WebRTC lanes.

The protocol authority is [`docs/协议规范.md`](../docs/协议规范.md); product and
phase semantics are in
[`docs/即时分享与文件浏览重构计划.md`](../docs/即时分享与文件浏览重构计划.md).
Validation entry points are in [`docs/validation.md`](../docs/validation.md), and
manual performance publication is in [`docs/performance.md`](../docs/performance.md).

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
pnpm -C web run test:unit:remainder
```

`make web` runs the same sequence. The forbidden gate walks the production
dependency graph, scans all Web source/tests, and checks the built bundle.

## Browser evidence

`pnpm -C web test:browser:smoke` runs the single Chromium product path used by
the Windows PR gate. `pnpm -C web test:browser` (or `make browser`) runs the full
three-browser main/Pion matrix. Both commands allocate invocation-private
loopback listeners and run under the cross-platform process-tree owner; retries
are disabled, and cleanup evidence is part of the verdict. Direct tests never
inspect Firewall or WBEM state. See the validation document for PR, nightly,
and release scope.
