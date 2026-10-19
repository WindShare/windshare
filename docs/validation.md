# Validation

WindShare's validation entry points are local `make` gates, ordinary GitHub CI, weekly suites, and a manual release workflow.

Go process E2E, `integration/relayv2` and browser product scenarios build Rust
`relay-native` once per suite; product scenarios also build Go `wind`. They require
the installed stable Rust toolchain; browser scenarios also need browser prerequisites.
Cargo retains its incremental cache; each suite runs its own executable copy.
Go relay fixtures share `internal/testrelaynative` for isolated SQLite, readiness
and bounded process ownership. `integration/relayv2` also owns relay-set candidate
isolation, exact-lane reconnection, session shutdown and receive recovery with one
download job and output reservation. Process E2E verifies STOP independently on
two relays before and after restart. CLI cancellation tests inject relay cleanup;
short relay-set tests inject dials and clocks. Short Go tests do not build Rust.
Go race instrumentation applies only to Go children.

## Local gates

Make targets operate on the repository root and use the installed Go, Rust, Node.js, pnpm, browser, and analysis tools. Platform targets run `scripts/ci/linux/<target>.sh` or `scripts/ci/windows/<target>.ps1`.

| Command | Runs |
|---|---|
| `make check` | `rust`, then Go short tests, Web typecheck, and Web unit tests. |
| `make ci` | The ordinary gate set, serially. |
| `make ci-parallel` | The same gates in three concurrent lanes: `short-go vectors rust e2e hygiene workflow-lint sloc`, `web browser`, and `gopls lint vet`. |
| `make ci-full` | `ci`, `long-go`, and the weekly browser supplement. |
| `make short-go` | Non-core and core short tests with race detection and coverage verdicts. |
| `make race` | Short tests with race detection. |
| `make coverage` | Short tests with coverage verdicts. |
| `make vectors` | Vector checks in `core/internal/protocolcontract`, `connectivity/v2signal`, and `cmd/wind/internal/runtrace`. |
| `make vectors-update` | Regenerates protocol vectors. |
| `make rust` | Cargo fmt, Clippy, native tests, doctests, coverage, and wasm builds. |
| `make web` | ESLint, TypeScript/Vite build, Vitest. |
| `make e2e` | `TestUserTraceCriticalSenderRelayReceiver` over real CLI processes. |
| `make browser` | Chromium relay smoke and short browser contracts. |
| `make browser-weekly` | `browser` plus Firefox/WebKit contracts, Chromium periodic contracts, progressive catalog, direct/TURN routes, and Pion interop. |
| `make long-go` | Long E2E, integration, catalog, and output-runtime suites. |
| `make hygiene` | gofmt, whitespace, release packaging contracts, pinned-Pion reproduction, generated Unicode tables, dependency boundaries, and package ownership. |
| `make workflow-lint` | actionlint. |
| `make lint` | golangci-lint over production packages. |
| `make vet` | `go vet` over production packages. |
| `make gopls` | gopls diagnostics at hint severity over tracked Go sources. |
| `make sloc` | sloc-guard source-size limits. |

The ordinary gate set shared by `make ci` and `make ci-parallel`:

```text
short-go vectors rust web e2e browser hygiene workflow-lint lint vet gopls sloc
```

### Go packages

The root `go.mod` owns all production Go packages. Core is `./core/...`; non-core is the remainder. Core packages cannot import non-core WindShare packages or concrete networking and transport implementations. Go gates run with `GOWORK=off` and `GOTOOLCHAIN=local`.

Coverage is gated on the core and non-core totals and on each package, with no per-file floor. The thresholds live in `.testcoverage.yml` and `core/.testcoverage.yml`. `internal/perfevidence` and `spikes/webrtc` are separate evidence modules outside the production set.

`make gopls` covers tracked Go sources except the pinned Pion projection, whose source hashes, root replacements, and patches are reproduced first.

### Rust relay

`relay-rust/` is an independent Cargo workspace with build output in `.target/`. `make rust` runs through `rustup run stable` and needs the root Go toolchain because native relay tests build the shared Go client from `relay-rust/tests/_goclient`. Cargo commands use `--locked`.

`make rust` runs:

- `cargo fmt --all -- --check`;
- Clippy over native targets with `-D warnings`, and over the Cloudflare adapter for `wasm32-unknown-unknown`;
- native tests with line-coverage verdicts for the workspace and each package;
- doctests;
- wasm builds of `windshare-relay-core` and `windshare-relay-cloudflare`.

## Ordinary GitHub CI

[CI](../.github/workflows/ci.yml) runs on pull requests and pushes to `main`. A newer run cancels an older in-progress run of the same ref.

| Job | Runs |
|---|---|
| Static analysis | Hygiene, source-size checks, workflow lint, Go lint, and gopls. |
| Non-core Go | Vet, production build, short race tests, and coverage. |
| Core Go | Vet, short race tests, coverage, and protocol vectors. |
| Rust relay (Linux and Windows) | The same `make rust` gate on both hosts. |
| Web | Lint, build, and unit tests. |
| Go E2E | The critical sender/relay/receiver process test. |
| Chromium | Relay smoke, sustained native-size block bursts, and short browser contracts. |
| Windows | Native build, vet, short tests, and compatible-name restoration checks. |

The opt-in [Cloudflare deployment workflow](deploy/cloudflare-relay.md) runs independently
on relay/frontend updates to `main` or manual dispatch. `make hygiene` covers its offline
first-deploy/update/failure-recovery contracts. To run its opt-in live check, install
`age`, `age-keygen`, `worker-build`, Go, pnpm, Web dependencies and the pinned Wrangler, set
`CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ACCOUNT_ID`, then run
`node scripts/deploy/cloudflare/smoke.mjs`. It creates an isolated Worker, imports
its encrypted configuration, verifies frontend routing, registration and permanent
STOP across `full` → `relay-only` → `full` updates, then deletes its Worker and temporary credentials.

## Weekly CI

[Weekly Product Stability](../.github/workflows/weekly.yml) runs every Sunday at 04:23 UTC and can also be started manually. It contains:

- Linux and Windows integration tests;
- named long-running Go E2E and catalog tests;
- Linux and Windows durable-output tests;
- progressive catalog, direct/TURN switching, and browser/Pion interoperability scenarios;
- Firefox and WebKit product scenarios and component contracts (including native-size relay bursts), and periodic Chromium component contracts;
- Windows browser and process smoke tests;
- Linux and Windows native-output release-artifact checks, plus the Linux relay container build/probe.

## Release workflow

[Source and Binary Release](../.github/workflows/release.yml) is started manually with an exact commit SHA and a root `vX.Y.Z` version. Resolution runs on the default branch and binds the version to a default-branch revision.

Linux (ext4) and Windows (NTFS) jobs reconstruct the deterministic source bundle, including pinned Pion nested modules, licenses, manifest, and patches, then run the build, vet, vulnerability, test, and native-output gates. The `wsrelay` asset is built from Rust with a static C runtime; Linux needs the stable `x86_64-unknown-linux-musl` target and `musl-gcc`. After comparing the source bundles byte for byte, the workflow creates the immutable version tag and publishes source and `wind`/`wsrelay` binary ZIPs with SHA-256 checksums.

Local release scripts validate and package but never publish. See [installation](installation.md) for the supported clone, source-bundle, and binary paths.

Performance measurements are optional local diagnostics; see [Performance diagnostics](performance.md).
