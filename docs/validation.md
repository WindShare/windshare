# Validation

WindShare has local validation commands, ordinary GitHub CI, weekly suites, and a manual release workflow.
This document lists those entry points and what they run.

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

Go coverage is blocking:

- core packages total at least 90%;
- non-core packages total at least 80%;
- every included package at least 70%.

## Local commands

Run commands from the repository root. Local gates use the installed Go, Node.js, pnpm, browser, and
analysis tools; they do not install or update prerequisites.

| Command | Runs |
|---|---|
| `make check` | Fast Go and Web feedback. |
| `make ci` | All ordinary gates serially. |
| `make ci-parallel` | The same ordinary gates in bounded runtime, Web, and static-analysis lanes. |
| `make ci-full` | Ordinary gates plus the weekly suites available on the current host. |
| `make short-go` | Core and non-core short tests, race detection, and coverage. |
| `make vectors` | Go-to-TypeScript protocol-vector verification. |
| `make vectors-update` | Regenerate protocol vectors for review. |
| `make web` | ESLint, TypeScript/Vite build, and Vitest. |
| `make e2e` | Critical sender, relay, and receiver process path. |
| `make browser` | Chromium relay smoke and short browser contracts. |
| `make browser-weekly` | Current-host browser suites, including the ordinary browser checks. |
| `make long-go` | Long E2E, catalog, output-runtime, and integration suites. |
| `make hygiene` | Formatting, generated files, repository layout, and dependency-boundary checks. |
| `make workflow-lint`, `make lint`, `make vet`, `make gopls`, `make sloc` | Workflow and source analysis. |

`make ci` and `make ci-parallel` both run this ordinary gate set:

```text
short-go vectors web e2e browser hygiene workflow-lint lint vet gopls sloc
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
| Web | Lint, build, and unit tests. |
| Go E2E | Critical sender/relay/receiver process test. |
| Chromium | Relay smoke and short browser contracts. |
| Windows | Native build, vet, short tests, and compatible-name restoration checks. |

## Weekly CI

[Weekly Product Stability](../.github/workflows/weekly.yml) runs every Sunday at 04:23 UTC and can also be
started manually. It contains:

- Linux and Windows integration tests;
- named long-running Go E2E and catalog tests;
- Linux and Windows durable-output tests;
- progressive catalog, direct/TURN switching, and browser/Pion interoperability scenarios;
- Firefox, WebKit, and periodic Chromium browser tests;
- Windows browser and process smoke tests;
- Linux and Windows native-output release-artifact checks.

## Release workflow

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

Performance measurements are optional local diagnostics; see [Performance diagnostics](performance.md).
