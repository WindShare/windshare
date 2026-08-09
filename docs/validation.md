# Validation

WindShare uses direct validation gates. The local caller, host, and same-user processes are trusted to run
tests. That trust does not make filesystem state immutable: product code still handles normal concurrent
changes and validates remote input.

## Local gates

[Makefile](../Makefile) dispatches one native script per target. Developers provide Go, Node.js, pnpm,
GNU Make, golangci-lint, gopls, sloc-guard, actionlint, govulncheck, installed Web dependencies, and the
required Playwright browser runtimes. Gates use `GOTOOLCHAIN=local` and never install or update tools,
packages, or browsers. A missing prerequisite fails in the command that needs it. Windows Firewall and
WBEM state are not validation gates.

| Entry point | Direct responsibility |
|---|---|
| `make ci` | Run the ordinary workspace-source gates in their fixed order. |
| `make ci-full` | Run workspace-source CI plus all current-host equivalents of weekly suites. |
| `make check` | Root/core short tests, Web TypeScript checks, and Vitest. |
| `make hygiene` | gofmt verification, `git diff --check`, exact retired-path checks, and production dependency-graph checks. |
| `make sloc`, `make workflow-lint` | Run local sloc-guard and actionlint directly. |
| `make lint` | Run golangci-lint once in each Go module. |
| `make vet` | Vet both Go modules. |
| `make root-release-graph` | Build the root module with `GOWORK=off` against its released core dependency. GitHub owns this merge gate; local use is diagnostic. |
| `make gopls` | Check tracked Go files with the local language server. |
| `make short-go` | Run each Go module once with short, race, and atomic coverage instrumentation, then enforce coverage. |
| `make race`, `make coverage` | Run diagnostic-only short sweeps; `make ci` uses the combined `short-go` gate instead. |
| `make vectors` | Verify the canonical Go-to-TypeScript protocol vectors without modifying the worktree. |
| `make vectors-update` | Explicitly regenerate the canonical protocol vectors for review. |
| `make web` | Run ESLint, the TypeScript/Vite build, and Vitest. |
| `make e2e` | Run the single critical sender/relay/receiver process path. |
| `make browser` | Run the direct current-platform Chromium relay smoke, then every non-periodic browser contract in the `chromium-short` project. |
| `make browser-weekly` | Run `make browser` once, then the Firefox/WebKit contract projects, Chromium periodic contracts, and scheduled product scenarios serially on the current host. |
| `make long-go` | Run named E2E/catalog/output-runtime long suites and native integration packages. |
| `make core-release` | Validate an extracted, independently consumable core module. |

`make ci` runs `short-go vectors web e2e browser hygiene workflow-lint lint vet gopls sloc` serially.
Both local aggregates validate current workspace source and intentionally exclude `root-release-graph`; final
pull requests and `main` validate the published dependency graph in GitHub Actions.
Runtime and protocol failures run first because they carry the highest product risk; gopls and SLOC close
the sweep so late static diagnostics do not delay test feedback. Use `make check` or a focused target while
iterating. `browser-weekly`, `long-go`, `ci-full`, and `core-release` intentionally stay outside ordinary
local CI. The local p95 goal is at most 10 minutes.

Browser component ownership is filename-driven by `web/playwright.contract.config.ts`: `chromium-short`
matches non-periodic `web/test/browser/**/*.spec.ts`, Firefox/WebKit match only `*.cross-browser.spec.ts`,
and `chromium-periodic` matches only `*.periodic.spec.ts`. The platform `browser` script invokes the
Chromium smoke before `test:browser:contract:short`; the weekly supplement invokes the other contract
projects plus the existing progressive, network, interop, and product cross-browser owners.
The contract config accepts `WINDSHARE_CONTRACT_PORT` for isolated concurrent local runs; the platform
scripts reserve separate loopback ports for short, cross-browser, and periodic projects.

A dated, machine-specific diagnostic is available in the
[local test timing snapshot](local-test-timing.md). It records test ownership gaps and outliers,
but does not replace the p95 goal or define a cross-host performance baseline.

Coverage is blocking: core total >=90%, root total >=80%, and every included Go package >=70%. Product
packages are not excluded and thresholds are not lowered to make a gate faster.

## Ordinary GitHub CI

The [CI workflow](../.github/workflows/ci.yml) runs on every pull request and push to `main`, cancels
superseded work for the same ref, and starts seven fixed independent jobs:

| Job | Owner |
|---|---|
| `static` | Hygiene, SLOC, workflow lint, Go lint, and gopls. |
| `go-root` | Root vet, released-core consumer build, one Linux short race/coverage sweep, and its coverage verdict. |
| `go-core` | Core vet/build, one Linux short race/coverage sweep, coverage verdict, and vectors. |
| `web` | Frozen Web install followed by lint, build, and Vitest. |
| `go-e2e` | The critical Linux process E2E once. |
| `browser-chromium` | The relay-only Linux Chromium smoke and `chromium-short` component contracts once. |
| `windows-native` | Windows vet, released-core consumer build, and root/core short tests, without duplicate coverage. |

Every job has a 10-minute hang fuse, no dependency on another job, and reports its own result directly.
Hosted jobs prepare their own current toolchains and frozen project dependencies. Static analysis resolves
each Go validation tool's `@latest` version on every run and reuses binaries only for an exact OS,
architecture, Go version, and resolved tool set. The ordinary CI p95 goal is at most 6 minutes, measured
from native GitHub Actions timestamps.
Branch rules must require the ordinary CI results before merge; the push-to-`main` run detects regressions
but cannot prevent an unprotected merge.

## Automatic weekly suites

The [weekly workflow](../.github/workflows/weekly.yml) runs automatically every Sunday at 04:23 UTC;
manual dispatch is retained only for diagnosis. Its eleven independent jobs own the expensive product
coverage that does not belong in ordinary CI:

- Linux and Windows integration stability;
- named Go E2E/catalog long suites and Linux/Windows native output durability;
- Chromium progressive catalog paging and separate direct/TURN relay-cut switching;
- D1/D2 browser/Pion interoperability, Firefox/WebKit product smoke/hot-switch and component contracts,
  Chromium periodic component contracts, and one Windows Chromium process smoke.

Each job has a 10-minute hang fuse. Long Go tests use native `testing.Short()` boundaries and stable
`TestLong...` names, so every short-mode skip has an automatic owner rather than a manual-only suite.
`make browser-weekly` provides current-host reproduction of the browser scenarios without pretending to
reproduce GitHub's Linux/Windows matrix. `make ci-full` combines that coverage with ordinary CI and
`make long-go`, deduplicating the Chromium smoke and short contracts already owned by ordinary CI.

## Core candidate release

The [core candidate workflow](../.github/workflows/core-release.yml) is separate from ordinary CI. A
push of the candidate tag `core-candidate/vX.Y.Z/<candidate>` resolves the exact commit. Extracted-core
checks run on Linux/ext4 and Windows/NTFS in parallel before creating `core/vX.Y.Z`. Publication is
idempotent:
an absent tag is created, the same commit succeeds, and a different existing commit fails without
moving the tag. Only the publish job receives `contents: write`; manual dispatch runs diagnostics and
does not publish.

A root change that consumes a new pre-v1 core API uses two commits. First, push a candidate tag at the
coherent source commit and wait for its immutable `core/vX.Y.Z` tag. Then update the root `go.mod` to
that released version. The workspace validates current source before publication; the `GOWORK=off`
`root-release-graph` merge gate validates the released graph afterward. Creating a candidate tag is
release authority because the exact commit need not be merged into `main`, so that permission must be
restricted. Local `replace` directives and placeholder
versions are not substitutes for either boundary.

## Product safety boundary

Validation simplification does not relax E2EE, capability links, remote-input validation, root
confinement, file revision/lease semantics, resumable and crash-recoverable output, no-replace atomic
publication, native output identity and ancestry revalidation, or relay/WebRTC switching. Stable
session, operation, and scenario identifiers and structured milestone logs remain part of the product
and test diagnostics.

Performance measurements are ordinary local diagnostics described in
[Performance benchmarks](performance.md); they do not gate correctness or release.
