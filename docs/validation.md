# Validation

WindShare's validation entry points are local `make` gates, ordinary GitHub CI, weekly suites, and a manual release workflow.

Go process E2E, `integration/relayv2` and browser product scenarios build Rust
`relay-native` once per suite; product scenarios also build Go `wind`. They require
the installed stable Rust toolchain; browser scenarios also need browser prerequisites.
Cargo retains its incremental cache; each suite runs its own executable copy.
Go relay fixtures share `internal/testrelaynative` for isolated SQLite, readiness
and bounded process ownership. `integration/relayv2` also owns relay-set candidate
isolation, reconnection after lane-slot reuse, session shutdown and receive recovery with one
download job and output reservation. Process E2E verifies STOP independently on
two relays before and after restart. CLI cancellation tests inject relay cleanup;
short relay-set tests inject dials and clocks. Short Go tests do not build Rust.
Go race instrumentation applies only to Go children.

## Local gates

Make targets operate on the repository root and use the installed Go, Rust, Node.js, pnpm, browser, and analysis tools. Platform targets run `scripts/ci/linux/<target>.sh` or `scripts/ci/windows/<target>.ps1`.

| Command | Runs |
|---|---|
| `make check` | `rust` and `desktop-check`, then Go short tests, Web/Consoles typecheck, Consoles ESLint and embedded bundle verification, and Web unit tests. |
| `make ci` | The ordinary gate set, serially. |
| `make ci-parallel` | The same gates in three concurrent lanes: `short-go vectors rust desktop-check e2e hygiene workflow-lint sloc`, `web browser`, and `gopls lint vet`. |
| `make ci-full` | `ci`, `long-go`, and the weekly browser supplement (excludes standalone Cloudflare Worker Miniflare and live checks). |
| `make short-go` | Pinned ICE/SCTP state tests, then non-core and core short tests with race detection and coverage verdicts. |
| `make race` | Short tests with race detection. |
| `make coverage` | Short tests with coverage verdicts. |
| `make vectors` | Shared protocol, signed network directory/authority, signaling, and CLI trace vectors. |
| `make vectors-update` | Regenerates protocol vectors. |
| `make rust` | Cargo fmt, Clippy, native tests, doctests, coverage, and wasm builds. |
| `make desktop-check` | Desktop workspace: Cargo fmt, strict Clippy, non-UI tests (including native host ABI and artifact staging), and native build; requires Node.js, Go, a C compiler, and GPUI prerequisites. |
| `make web` | Web and Consoles ESLint, Consoles typecheck and embedded bundle verification, Web TypeScript/Vite build, and Vitest. |
| `make e2e` | `TestUserTraceCriticalSenderRelayReceiver` over real CLI processes. |
| `make browser` | Chromium relay smoke and short browser contracts. |
| `make browser-weekly` | `browser` plus Firefox/WebKit contracts and product routes, progressive catalog, direct and parallel TURN routes, and Pion interop. |
| `make long-go` | Long E2E, integration, catalog, and output-runtime suites. |
| `make hygiene` | gofmt, whitespace, release packaging contracts, pinned-Pion reproduction, generated Unicode tables, dependency boundaries, and package ownership. |
| `make workflow-lint` | actionlint. |
| `make lint` | golangci-lint over production packages. |
| `make vet` | `go vet` over production packages. |
| `make gopls` | gopls diagnostics at hint severity over tracked and non-ignored new Go sources. |
| `make sloc` | sloc-guard source-size limits. |

The ordinary gate set shared by `make ci` and `make ci-parallel`:

```text
short-go vectors rust desktop-check web e2e browser hygiene workflow-lint lint vet gopls sloc
```

### Web boundaries

`make web` checks resolved TypeScript dependencies, including type imports, re-exports, aliases, and dynamic imports. Application and reusable modules cannot depend on UI/React. Protocol contracts cannot reach runtime implementations; sessions cannot reach connectivity, transport, or storage implementations. UI invokes application commands and diagnostic services; its only output API is `output/model/**`. Models expose immutable facts and pure queries, with no transitive access to output implementations, application, UI/React, or concrete connectivity.

`transfer/**` owns receive intent, lifecycle, output ports, and failure contracts; it cannot reach `output/**`, application composition, or UI, even through shared modules. Browser route coordination lives in `application/browser-receive`; storage execution, journals, and file ownership stay in `output/**`.

### Go packages

Transport production dependencies cannot reach `connectivity/**`, including through wrappers; `make hygiene` checks Linux, Windows, and macOS graphs. Pion connection setup belongs to `connectivity/pionpeer`; frame delivery stays in `transport/webrtc`.

The root `go.mod` owns all production Go packages. Its `ignore ./desktop` directive keeps the independent Cargo workspace and local reference fixtures outside Go package discovery; `make desktop-check` owns desktop validation. Core is `./core/...`; non-core is the remainder. Core production packages cannot import non-core WindShare packages or concrete networking and transport implementations. Native `osfs/**` fixtures are allowed only in the core test graph delta. Production `cmd/**` and `host/**` files cannot import `connectivity/**`, `transport/**`, or `relay/**`; the engine boundary gate follows exported aliases, fields, methods, and factory signatures in every public `engine/**` package including wrappers in core, to reject native types in the application API. Shared engine code cannot import `osfs/**` or `adapters/**`; hosts assemble these implementations. Engine and production hosts (`cmd/wind`, `cmd/windshare-host`, `host/**`) cannot import test-support packages; e2e milestones are derived from ordinary observation traces. Go gates run with `GOWORK=off` and `GOTOOLCHAIN=local`.

Coverage is gated on the core and non-core totals and on each package, with no per-file floor. Verdicts stage only profile-listed sources to avoid scanning build output. The thresholds live in `.testcoverage.yml` and `core/.testcoverage.yml`. `internal/perfevidence`, `spikes/webrtc`, and `testdata/connectivity/sctp-rack-public-20261030` are explicitly approved evidence modules outside the production set.

`make gopls` checks tracked and non-ignored new Go sources in sequential, fixed platform/tag sessions. Hints cover selected files; compile errors also cover active sibling files in their packages. Additional tag profiles live in `scripts/ci/gopls/profiles.json`; uncovered sources fail with their path. Real-gopls contracts verify cross-platform tags, sibling diagnostics, and environment isolation. Pinned Pion sources are excluded only after hash, replacement, and patch reproduction checks.

### Rust relay

`relay-rust/` is an independent Cargo workspace with build output in `.target/`. `make rust` runs through `rustup run stable` and needs the root Go toolchain because native relay tests build the shared Go client from `relay-rust/tests/_goclient`. Cargo commands use `--locked`.

`make rust` runs:

- `cargo fmt --all -- --check`;
- Clippy over native targets with `-D warnings`, and over the Cloudflare relay and account authority for `wasm32-unknown-unknown`;
- native tests with line-coverage verdicts for the workspace and each package;
- doctests;
- wasm builds of `windshare-relay-core`, `windshare-relay-cloudflare` and `windshare-network-authority`.

## Ordinary GitHub CI

[CI](../.github/workflows/ci.yml) runs on pull requests and pushes to `main`. A newer run cancels an older in-progress run of the same ref.

| Job | Runs |
|---|---|
| Static analysis | Hygiene, source-size checks, workflow lint, Go lint, and gopls. |
| Non-core Go | Vet, production build, short race tests, coverage, and production output on the release ext4 fixture. |
| Core Go | Vet, short race tests, coverage, and protocol vectors. |
| Rust relay (Linux) | Full `make rust` gate. |
| Rust relay (Windows) | `cargo test -p windshare-relay-native --locked`: native unit and process integration tests, without workspace coverage or Wasm builds. |
| Rust desktop (Windows) | `make desktop-check`, then package the executable with its Go library and verify authenticated host startup and joined shutdown from the relocated package. |
| Web | Lint, build, and unit tests (Web and Consoles). |
| Go E2E | The critical sender/relay/receiver process test. |
| Chromium relay smoke | Relay smoke, sustained native-size block bursts, and session isolation. |
| Chromium browser contracts | Short component contracts, independently of native toolchains and smoke tests. |
| Windows | Native build, vet, and short tests. |
| FAT/exFAT (Linux) | Disposable image remounts verify partial reuse, publication, and owned cleanup after native IDs change. |
| macOS (arm64 and amd64) | Each runner builds its native CLI and relay, runs the output/recovery suites, and captures APFS evidence for CLI file/folder downloads and SIGKILL recovery. |

**Private relay verification** checks the embedded console, deployment scripts and local Miniflare
access and managed TURN contracts on relay-related pull requests. On `main`, it runs standalone
when automatic deployment is off. Forks with automatic updates enabled run the checks inside
**Deploy private relay** before publishing a relay. Manual deployment is available on the default
branch. Frontend-only deployments skip relay verification and Rust builds. See the
[Cloudflare deployment guide](deploy/cloudflare-relay.md).

Run the local Worker contracts with `npm run test:access` in `relay-rust/crates/relay-cloudflare`.
`make hygiene` covers first deployment, updates and failure recovery offline. Standalone Worker
checks stay outside `make ci-full` and `make browser-weekly` so core local gates do not require
`worker-build` or Wrangler/Miniflare dependencies. To run the opt-in live check, install Rust
stable with `wasm32-unknown-unknown`, `worker-build`, Go, pnpm, Web dependencies, Playwright
Chromium (`pnpm -C web exec playwright install chromium`), and the pinned Wrangler/Miniflare
dependencies (`npm ci` in `relay-rust/crates/relay-cloudflare`), set
`CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ACCOUNT_ID`, then run
`node scripts/deploy/cloudflare/smoke.mjs`. It creates an isolated relay and companion console,
imports a per-device credential, verifies separate origins, managed-TURN issuance and Chromium
transfer when available, and registration/permanent STOP across `full` → `relay-only` → `full`
updates, then deletes its Workers and temporary files. With the same local prerequisites (minus
Cloudflare credentials), `npm run test:managed-turn:browser` in
`relay-rust/crates/relay-cloudflare` runs the isolated local-Worker discovery, credential
issuance, and Chromium transfer check without a Cloudflare account.

CI calls the reusable [native output workflow](../.github/workflows/native-output.yml) on every pull request and `main` push. It also supports manual reruns. Each macOS architecture caches the Go toolchain from `go.mod` and its Cargo dependencies.

## Weekly CI

[Weekly Product Stability](../.github/workflows/weekly.yml) runs every Sunday at 04:23 UTC and can also be started manually. It contains:

- Linux and Windows integration tests;
- named long-running Go E2E and catalog tests;
- Linux and Windows output and process-restart tests;
- progressive catalog, direct and parallel TURN routes, isolated local-Worker managed TURN browser transfer, and browser/Pion interoperability scenarios;
- Firefox and WebKit product scenarios and component contracts (including native-size relay bursts);
- Windows browser and process smoke tests;
- Linux and Windows native-output release-artifact checks, plus the Linux relay container build/probe.

## Release workflow

[Source and Binary Release](../.github/workflows/release.yml) is started manually with an exact commit SHA and a root `vX.Y.Z` version. Resolution runs on the default branch and binds the version to a default-branch revision.

Linux (ext4) and Windows (NTFS) verification jobs reconstruct the deterministic source bundle directly from commit blobs, including pinned Pion nested modules, licenses, manifest, and patches, then run the build, vet, vulnerability, test, and unprivileged native-output certification gates using precompiled test binaries. Verification jobs cache the resolved `govulncheck` binary and Cargo test builds (via `CARGO_TARGET_DIR`), while Linux owns upstream Pion patch replay and `core/catalog` scale suites and Windows runs `core/catalog` in `-short` mode alongside full native, integration, and E2E suites. In parallel, clean-room packaging jobs reconstruct the same source bundle and build the release `wind` and `wsrelay` binaries, while another parallel job verifies embedded `console-dist/` bundles against a clean rebuild of that exact commit. These exercise the shared output backend and public recovery factory, including process-exit lock release, no-copy publication, timestamps, and preserved download progress. The `wsrelay` asset is built from Rust with a static C runtime; Linux needs the stable `x86_64-unknown-linux-musl` target and `musl-gcc`. The hosted Linux packaging job boots the packaged relay twice under QEMU’s baseline x86-64 CPU to verify SQLite restart on older hosts. After comparing the packaged source bundles byte for byte and verifying their SHA-256 matches both verification jobs, the workflow creates the immutable version tag and publishes source and `wind`/`wsrelay` binary ZIPs with SHA-256 checksums.

Local release scripts validate and package but never publish. See [installation](installation.md) for the supported clone, source-bundle, and binary paths.

Performance measurements are optional local diagnostics; see [Performance diagnostics](performance.md).
