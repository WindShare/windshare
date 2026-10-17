# Validation

WindShare has local validation commands, ordinary GitHub CI, weekly suites, and a manual release workflow.
This document lists those entry points and what they run.

Production wiring is checked through behavior: `make e2e` exercises the real CLI processes, and the
smoke test in `make browser` exercises the browser UI, preview, downloaded contents, and Chromium's
retained ZIP and extensionless original-file exports, including same-page repetition with a stopped sender
and exports after reloading and going offline. Longer recovery
and connectivity scenarios belong to `make long-go` and the weekly browser suites; unit and component
contracts cover individual decisions and failure paths. Component contracts use the lightweight
`test/browser/contract-host.html` page and load production modules; storage, reloads, and browser-process
recovery stay real. Single-file
reload contracts use production inventory and reopen authority with empty and partial checkpoints, and
verify reuse of the retained object and committed bytes. Source invalidation checks preserve partial
bytes across reload, forbid continuation, and verify cleanup; the smoke also verifies that the next
download succeeds and the source rejection is not attributed to a checkpoint failure. Startup and
diagnostics contracts use the application entry point, including same-document share links and portal anchors.
UI harnesses load their required styles. Assertions protect state, actions, accessibility, and usable
layouts; incidental copy, exact palette values, and DOM nesting are not contracts. Test presentation
mappings in unit tests and use browser tests for interactions and representative viewport/theme combinations.
The short Chromium contracts also build and serve the native-storage capability probe under a
same-origin Worker CSP, catching bundling failures hidden by the development server. Production ZIP
startup contracts abort the emitted Worker request and verify repeated retries, reload recovery,
capacity admission, and explicit discard of the original task.
Cross-browser storage contracts keep persistence permission unresolved while real OPFS file and ZIP
workspaces activate, pause, and discard; Firefox also exercises its native permission request.
Cross-browser crypto contracts open version-3 sealed blocks from an independent signer, reject
identity/content/signature substitutions, and exercise native capability failures. Shared Ed25519
vectors require identical Go, native-browser and portable acceptance of exceptional encodings.
Unit contracts cover verifier reuse, bounded backend fallback, authentication failures and mutable
caller buffers; object commitments hash concurrently and reuse one snapshot through decryption.
Native descriptor bootstrap retains one checked sender verifier for catalog, content, control and
lane authentication across reconnects. Teardown releases references without invalidating surviving
receivers. `BenchmarkOpenBlock` measures the production receive path without sealing costs.
Static architecture gates protect dependency and capability boundaries, not
historical filenames, symbol names, or required internal module lists.

Browser product suites build `wind` and `wsrelay` once in Playwright global setup, using a private
invocation directory and a 180-second preparation budget. Workers reuse the completed binaries;
product-stack startup does not compile Go packages or download dependencies. The suite's global budget
includes preparation, while individual product-test timeouts remain unchanged. Global teardown
removes the binaries after the scenarios have stopped their processes.

## Go package checks

The root `go.mod` owns all production Go packages. Validation obtains the full package set with
`go list ./...`, obtains the core set with `go list ./core/...`, and treats the exact remainder as
non-core. The package-set check rejects overlap, omissions, and duplicates.

The dependency check keeps `core/**` network-free: core packages cannot import non-core WindShare
packages or concrete networking and transport implementations. `internal/perfevidence` and
`spikes/webrtc` are separate evidence modules and are not part of the production package set. Go gates
use `GOWORK=off` so a developer's ambient workspace does not change what is tested.

`make gopls` checks every existing tracked Go source at hint severity, except the exact pinned Pion
projection after source hashes, root replacements, and upstream patch reproduction pass. Both host
launchers use the same selector; adapters, CI helpers, evidence modules, and tests remain included.
Pion is compiled through the root native dependency graph and exercised by the provider/socket race
tests. Its standalone upstream module graph and JavaScript/Wasm provider view are not supported targets.
Stage new Go files before final validation so the tracked-source gate includes them.
Both hosts use one owned gopls session for all selected files, including inferred platform and nested
module views. Sources are grouped by module, with current-platform files first, so ordinary files do
not pay for additional platform views before they are needed. Foreign and tagged files remain included.
Small batches wait for explicit diagnostic completion before files close; retained files keep each
build view loaded. Progress reports include elapsed time. Each invocation checks fresh sources.

Go coverage is blocking:

- core packages total at least 60%;
- non-core packages total at least 60%;
- every included package at least 50%;
- no per-file minimum.

## Rust relay checks

`relay-rust/` is an independent Cargo workspace. Its `windshare-relay-core` crate uses `no_std + alloc`,
with no I/O or async runtime dependencies. Sender-key and signature checks consume the shared
Go/browser Ed25519 acceptance vectors; REGISTER/RESUME/STOP frames, preimages, proofs and refusals
consume `v2-session.json`. The per-connection handshake and single-share lifecycle cover
reservation, authenticated publication, stale-operation fencing, uncertain host results, owner-loss
grace and ownership-bound RESUME. The lifecycle's activation barrier keeps JOIN, forwarding and
REGISTERED behind final persistence, including delayed confirmation, disconnect and STOP races.
Aggregate JOIN tests preserve absent/stopped refusals. STOP tests cover ordered persistence/queue discard/final commit,
acknowledgement with unsettled occupancy, duplicate STOP, uncertain writes, registration/RESUME races
and late callbacks. Stopped recovery retains charges and deadlines without sockets; only consumption,
settlement or proven release refunds them. Restoration covers every durable record,
surviving owners and interrupted STOP cleanup. Receiver-session tests cover fixed admission deadlines,
pre-admission E2E forwarding, quiet active sessions, STOP/RESUME/disconnect revocation, stale queued
permits and callbacks, and sibling isolation. Delivery-ledger tests cover startup reservations,
frame/byte bounds, FIFO submission, cancellation/discard/settlement ordering, cumulative feedback,
stale generations and fixed cleanup deadlines using injected time. WS2O/WS2M codecs consume shared
vectors and reject malformed framing. Public `ShareRelay` scenarios exercise final durability
barriers, serialized retries, synchronous handoff, stale callbacks, capacity rollback and sibling
isolation. Hibernation tests restore sessions, pending handshakes, delivery credit and unsettled
occupancy together without renewing deadlines. Cumulative WS2B codecs in Rust, Go and TypeScript
consume shared count-boundary and malformed-frame vectors, independently of runtime windows.
Handshake capacity and distinct WS2N/WS2S settlement controls also consume shared
Rust/Go/TypeScript vectors. Aggregate wire tests cover retirement/discard ordering,
exact totals after partial consumption, replay, slot reuse and hibernated expiry.
Metadata regressions cover lazy handshake allocation, asymmetric sender/receiver
fanout, runtime slot bounds and sparse checkpoint identity/settlement recovery. Incremental
attachment tests cover source liveness and destination accounting writes, admission transitions, no-op suppression,
partial write retries, close/high-water preservation, host byte budgets and joint recovery.
Recovery tests distinguish unknown authority/inventory from lost delivery records. Missing or
invalid receiver records isolate dependent sessions, retain conservative risk and preserve sibling
budgets/deadlines. They cover repeated recovery, risk exhaustion, new generations, old-feedback
isolation, sender-loss fanout and permanent STOP despite delivery-record loss.
Retirement scenarios cover repeated reconnects beside a healthy download, bidirectional queue
discard, independent metadata/frame/byte limits, retained reverse-flow debt and host-injected grace.
Connection-liveness tests consume shared WS2H/WS2A vectors and cover monotonic nonces, content-credit
bypass, accepted inbound activity, silence isolation and expiry before handoff. Repeated hibernation
preserves nonces and absolute deadlines; heartbeats cannot extend handshake, admission or settlement.
All use injected time; no live Cloudflare retention interval is inferred from them.
Go and browser socket-free receive-ledger tests cover shared connection bounds, FIFO
receipts, frame/byte-aware batching across sessions, sparse flush, counter exhaustion,
retirement settlement, replay and stale-generation isolation without timers or sockets.
The native lifecycle host now runs REGISTER/RESUME/STOP and heartbeats on real
WebSockets with SQLite persistence. Its Go-codec scenario generates fresh keys and
proofs, restarts the binary after registration, resumes, stops independently and
restarts again to verify permanent refusal. Connection tests block actual WebSocket
writes over bounded in-memory I/O while reading inbound controls, verify joined
task/queue release, terminal reply draining, heartbeat priority and shutdown.
Real kernel receipts cover queue bounds and STOP fencing at synchronous writer handoff.
Fault-injected host tests hold commits,
lose receipts and race STOP with ownership publication. JOIN/forwarding, client
runtime cutover remain unimplemented. The Cloudflare lifecycle adapter now uses the
same authority-row transitions, real Worker entropy/time and SQLite. Injected host
tests cover incomplete commits, permanent STOP, hibernation, partial attachment
writes, lost-attachment isolation and absolute deadline/nonce preservation. Its
[opt-in Preview scenario](../relay-rust/crates/relay-cloudflare/README.md#validation)
uses the shared Go client and a separate fault-injection entry.
Opt-in [host probes](../spikes/relay-hosts/README.md) exercise real native writers and
Cloudflare Preview closure/recovery separately, including Rust/JavaScript abort
controls and a TLS client with a bounded OS receive buffer. The
[checkpoint probe](../spikes/relay-hosts/checkpoint-recovery.md) adds real-kernel
hibernation, two-receiver attachment-loss isolation, detached risk reservations and replacement
generations with an explicit SQLite mirror control. Their Cargo workspaces
and cloud runs are outside `make rust`; use the documented commands when changing
host assumptions. A successful object restart is not proof of buffer release.
Run Cargo inside `relay-rust/`; its `.cargo/config.toml` places build output in `.target/` so Go's
repository-wide package discovery skips the generated tree. The Make gate uses the same directory.

`make rust` checks formatting, denies Clippy warnings across all native targets, runs native tests
with **60% workspace and 50% per-package line-coverage minimums**, runs doctests, and builds the shared core
and Cloudflare adapter for `wasm32-unknown-unknown`, with Wasm Clippy for the adapter.
`cargo-llvm-cov` measures production code, excluding test directories and
`*_tests.rs` files. An empty scaffold reports no coverable lines; test or tool failures still fail the
gate. Cloudflare host orchestration is measured through native tests with injected
I/O; platform bindings are checked by Wasm Clippy/build and opt-in Preview execution.
Coverage runs the native tests once in `.target/coverage/`, preserving compiled dependencies
while clearing stale workspace coverage. Doctests do not contribute to coverage.
Package coverage sums production-file line counts from the same report using Cargo workspace
membership and manifest directories; nested packages own their own sources. Measured source files
must live inside their package directory. There is no per-file minimum or extra test run per package.
Cargo.lock is committed and build/test gates use `--locked`. The Wasm builds check
core portability and the Cloudflare adapter, including its platform bindings;
Worker bundling and real deployment remain opt-in. GitHub enforces the same gate on Linux and Windows, installing a prebuilt
coverage tool to avoid compiling it during CI.

The Rust gate also requires the Go toolchain from `go.mod` for native-host interoperability.

Install prerequisites explicitly before running local gates:

```sh
rustup toolchain install stable --profile minimal --component rustfmt --component clippy --component llvm-tools-preview --target wasm32-unknown-unknown
cargo install --locked cargo-llvm-cov
```

The gate uses `rustup run stable` without installation, regardless of the default toolchain.

## Local commands

Run commands from the repository root. Local gates use the installed Go, Rust, Node.js, pnpm, browser, and
analysis tools; they do not install or update prerequisites.

Keep local experiment workspaces, browser profiles, and generated file trees in `.tmp/` or the system
temporary directory. Go's `./...` traversal ignores dot directories but does not read `.gitignore`;
large artifacts under `tmp/` slow package discovery and every gopls build view.

Browser storage formats are pre-release. Incompatible capacity schemas are rejected without migration
or deleting accounting records. After a format change, close old tabs and clear the test site's storage
(IndexedDB and OPFS, not just the HTTP cache) before testing the new build.
File-set lifecycle snapshots include verified retained bytes for incomplete files; snapshots without
that field are incompatible with the current pre-release format.

| Command | Runs |
|---|---|
| `make check` | Fast Go, Rust, and Web feedback. |
| `make ci` | All ordinary gates serially. |
| `make ci-parallel` | The same gates in three bounded lanes; gopls starts immediately and progress streams live. |
| `make ci-full` | Ordinary gates plus the weekly suites available on the current host. |
| `make short-go` | Core and non-core short tests, race detection, and coverage. |
| `make vectors` | Go-to-TypeScript protocol-vector verification. |
| `make vectors-update` | Regenerate protocol vectors for review. |
| `make rust` | Rust formatting, Clippy, native coverage (60% workspace / 50% per package), doctests, and core/Cloudflare Wasm checks. |
| `make web` | ESLint with two workers, TypeScript/Vite build, and Vitest with two isolated workers. |
| `make e2e` | Critical sender, relay, and receiver process path. |
| `make browser` | Chromium relay smoke and short browser contracts with two workers. |
| `make browser-weekly` | Current-host browser suites, including the ordinary browser checks. |
| `make long-go` | Long E2E, catalog, output-runtime, and integration suites. |
| `make hygiene` | Formatting, generated files, repository layout, and dependency-boundary checks. |
| `make workflow-lint`, `make lint`, `make vet`, `make gopls`, `make sloc` | Workflow and source analysis. |

`make ci` and `make ci-parallel` both run this ordinary gate set:

```text
short-go vectors rust web e2e browser hygiene workflow-lint lint vet gopls sloc
```

Use a focused target or `make check` while iterating. Run `make ci-parallel` after final code changes.

## Ordinary GitHub CI

[CI](../.github/workflows/ci.yml) runs for pull requests and pushes to `main`. A newer run replaces an
older in-progress run for the same ref.

| Job | Runs |
|---|---|
| Static analysis | Hygiene, source-size checks, workflow lint, Go lint, and gopls. |
| Non-core Go | Vet, production build, short race tests, and coverage. |
| Core Go | Vet, short race tests, coverage, and protocol vectors. |
| Rust relay (Linux and Windows) | The same `make rust` gate on both hosts. |
| Web | Lint, build, and unit tests. |
| Go E2E | Critical sender/relay/receiver process test. |
| Chromium | Relay smoke, sustained native-size block bursts, and short browser contracts. |
| Windows | Native build, vet, short tests, and compatible-name restoration checks. |

## Weekly CI

[Weekly Product Stability](../.github/workflows/weekly.yml) runs every Sunday at 04:23 UTC and can also be
started manually. It contains:

- Linux and Windows integration tests;
- named long-running Go E2E and catalog tests;
- Linux and Windows durable-output tests;
- progressive catalog, direct/TURN switching, and browser/Pion interoperability scenarios;
- Firefox, WebKit (including native-size relay bursts), and periodic Chromium browser tests;
- Windows browser and process smoke tests;
- Linux and Windows native-output release-artifact checks.

## Release workflow

Release is optional and scheduled when external distribution is intended. Development milestones and
subsequent work depend on implemented capabilities and the relevant validation, not on a version tag
or a published release.

[Source and Binary Release](../.github/workflows/release.yml) is started manually with an exact commit SHA
and root `vX.Y.Z` version. Linux and Windows reconstruct a deterministic complete source bundle,
including pinned Pion nested modules, licenses, manifest and patches. Each extracted bundle reproduces
the pinned dependencies and passes the existing build, vet, vulnerability, test and native-output
certification gates. Production package ownership and coverage thresholds above remain unchanged.

After comparing source-bundle bytes across both hosts, the workflow creates the immutable version tag
and publishes source and `wind`/`wsrelay` binary ZIPs with checksums. Binaries are built before tests can
modify extracted sources. Local release scripts validate and package but never publish.
See [installation](installation.md) for supported clone/source-bundle and binary paths. Canonical Go
proxy ZIPs and module-version `go install` are unsupported with nested local replacements.

Exact browser executable hashes, OS builds, and filesystem profiles belong to reproducible release
evidence. Runtime admission uses observable capabilities and operation ownership checks; a release
evidence row is not a user's required machine identity. Basic safe output, source revision continuity,
restart recovery, and crash cleanup are validated independently.

Performance measurements are optional local diagnostics; see [Performance diagnostics](performance.md).
