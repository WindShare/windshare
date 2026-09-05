# WindShare Web receiver

The browser receiver is React 19 + TypeScript + Vite. It accepts suite-02
capability links, authenticates a small share descriptor, browses catalog pages
on demand, and reads file-local encrypted ranges over relay or WebRTC lanes.

Receive actions name the final artifact: `original-file`, `directory-tree`, or
`zip-archive`. The receiver-local `ReceiveIntentV2` binds that artifact to a legal
destination/workspace plan without adding a sender or relay wire field. Directory
output may retain successful files; ZIP uses `store` encoding and `complete-only`
publication. `browser-handoff` ends at `download-started`, never `published`.

Named output keeps `requestedName`, `logicalReservedName`, and `physicalName`
distinct. Ordinary output keeps the logical and physical names equal. Browser FSA
uses a compatible physical name only after the expected non-creating native lookup
itself throws `TypeError`; names, extensions, messages, browser brands, and operating
systems are not classifiers. The current restoration template is proven only on
Windows, so other platforms fail before creating a compatible target.

Windows compatible output includes an owned PowerShell script and sidecar. The UI
qualifies the result as completed or partial with compatible names only after workers,
output admissions, transactions, and the sidecar projector are quiescent and the
terminal footer is validated. A failed final catch-up retains local pending state and
does not publish qualified success. Browser resume follows the persisted physical-name
ledger until the user runs the restoration script; restoration intentionally ends
browser resumability for that output.

Browsing uses the established relay without starting ICE. A preview or download
activation makes relay content eligible immediately and starts one bounded P2P
recovery supervisor in the background; direct-path delay or exhaustion never gates
the healthy relay.

The protocol overview is [`docs/协议规范.md`](../docs/协议规范.md); product
semantics are in [`docs/重构功能变化.md`](../docs/重构功能变化.md), and the historical
closure record is archived in
[`docs/.archive/2026.08.06/即时分享与文件浏览重构收尾计划.md`](../docs/.archive/2026.08.06/即时分享与文件浏览重构收尾计划.md).
The browser artifact and recovery decisions are recorded in
[`docs/browser-output-ux-refactor-plan.md`](../docs/.archive/2026.08.07/browser-output-ux-refactor-plan.md).
Validation entry points are in [`docs/validation.md`](../docs/validation.md), and
local performance diagnostics are in
[`docs/performance.md`](../docs/performance.md).

## Source layout

| Directory | Responsibility |
|---|---|
| `src/catalog/` | Authenticated descriptor/pages, frozen path policy, page storage, and selection rules |
| `src/content/` | File-local geometry, revisions, leases, range broker, and lane scheduling |
| `src/session/`, `src/receiver/` | ProtocolSession runtime and receiver-scoped reconnect supervision |
| `src/transport/`, `src/connectivity/` | v2 relay/WebRTC channels, signaling, phased peer admission, and bounded direct recovery |
| `src/transfer/projection/`, `src/transfer/discovery/` | Epoch-fenced selection proof and authenticated progressive discovery |
| `src/transfer/job/`, `src/transfer/settlement/` | Plan-specific content execution, durable evidence, and settlement |
| `src/output/planning/`, `src/output/capability/` | Pure artifact offers/binding and thin browser authority acquisition |
| `src/output/persistent-tree/`, `src/output/file-system-access/` | Prefix-visible `directory-tree` transactions and FSA reservation/settlement |
| `src/output/workspace/`, `src/output/origin-private/` | Manifests, budgets, lifecycle/recovery, OPFS materialization, package, and publication |
| `src/output/zip-layout/`, `src/output/streams/`, `src/output/portable/` | Exact store-ZIP policy/writer and bounded browser handoff |
| `src/output/browser/`, `src/output/resume/` | IndexedDB v8 repository, operation leases, compatible-name ledger, and retained-operation inventory |
| `src/preview/` | Image and MP4 range preview over the shared broker |
| `src/ui/` | Progressive browser UI and immediate capability-fragment erasure |
| `src/unicode/` | Pinned Unicode 15 normalization and full-fold tables |

There is no silent artifact fallback, partial ZIP, global block identity, or v1
resume path. Old app-owned records are cleanup-only and cannot create a recoverable
operation. A sender handle released after resume grace can reopen with a new lease and
the same revision when source evidence is unchanged, so durable ranges remain usable.
A different stored revision is an item-local `revision-conflict`; ownership and invalid
binding remain `owned-object-unknown` and `checkpoint-invalid`, and siblings continue.
For `resumable-receive` and `resumable-package`, Continue invokes an output-owned
executor that revalidates exact preparation or sealed package evidence; the UI never
reconstructs authority. Save, redownload, discard, and expiry remain live.

## Local gates

The developer installs Web dependencies and Playwright browser runtimes outside
the gates. The commands below consume that environment without changing it:

```powershell
pnpm -C web lint
pnpm -C web build
pnpm -C web test
make web
make browser
pnpm -C web test:browser:contract:short
```

On Windows, run the production restoration primitive contract with
`powershell.exe -NoProfile -File .\scripts\ci\windows\browser-compatible-name-restoration.tests.ps1`
from the repository root.

`make web` runs ESLint, the TypeScript/Vite production build, and Vitest.
`make browser` runs the direct current-platform Chromium product smoke followed by the
`chromium-short` component contracts. The focused package command is useful while iterating;
the Make target remains the ordinary CI entry point. Set `WINDSHARE_CONTRACT_PORT` when running
multiple contract projects concurrently on one host.

## Browser validation commands

| Command | Coverage |
|---|---|
| `pnpm -C web test:browser:smoke` | Relay-only CLI-to-Chromium micro-directory flow and exact downloaded bytes. |
| `pnpm -C web test:browser:progressive` | Progressive catalog paging across the authenticated page boundary. |
| `pnpm -C web test:browser:network` | Authenticated direct and TURN peer adoption after relay loss. |
| `pnpm -C web test:browser:interop` | Direct D1/D2 browser/Pion adapter interoperability. |
| `pnpm -C web test:browser:cross` | The relay smoke plus native peer hot-switch or a relay-only route when peer capability is unavailable in Firefox and WebKit. |
| `pnpm -C web test:browser:contract:short` | Chromium short component contracts for browser storage, output, catalog, crypto, and media behavior. |
| `pnpm -C web test:browser:contract:cross` | The `*.cross-browser.spec.ts` component contracts in Firefox and WebKit. |
| `pnpm -C web test:browser:contract:periodic` | Chromium-only periodic scale and full-recovery component contracts. |

Ordinary GitHub CI runs `make browser` on Linux, which includes the Chromium smoke and
`chromium-short` contracts. The weekly workflow owns progressive, network, interop,
product cross-browser routes, Firefox/WebKit component contracts, and Chromium periodic
contracts; its Windows job keeps the existing Chromium process smoke. Manual dispatch is
only a diagnostic convenience.
