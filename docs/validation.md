# Validation

WindShare uses direct validation gates. The local caller, host, and same-user processes are trusted to run
tests. That trust does not make filesystem state immutable: product code still handles normal concurrent
changes and validates remote input.

## Go package boundaries

The root `go.mod` owns every production Go package, including `core/**`. Validation derives the complete
package set with `go list ./...`, derives the core set with `go list ./core/...`, and computes the non-core
set by exact subtraction. The package-set gate rejects overlap, omissions, and duplicates, so new top-level
packages cannot fall outside CI through a maintained directory list.

Core remains a network-free package boundary rather than a separate release unit. Its dependency gate
checks Linux, Windows, and Darwin production graphs independently from test-only dependency deltas. Core
packages may not import non-core WindShare packages or concrete networking and transport capabilities.
`internal/perfevidence` and `spikes/webrtc` remain isolated evidence modules and run only through their
explicit commands. Go gates set `GOWORK=off`, so an ambient user workspace cannot change the graph under
test.

## Local gates

[Makefile](../Makefile) dispatches one native script per target. Developers provide Go, Node.js, pnpm,
GNU Make, golangci-lint, gopls, sloc-guard, actionlint, govulncheck, installed Web dependencies, and the
required Playwright browser runtimes. Windows ordinary CI additionally requires PowerShell 7 (`pwsh`),
Git, `go-test-coverage`, a CGO-capable C compiler, and Chromium. Firefox and WebKit are required only by
weekly browser gates. Gates use the installed toolchain and never install or update tools, packages, or
browsers. A missing prerequisite fails in the command that needs it. Windows Firewall and WBEM state are
not validation gates.

| Entry point | Direct responsibility |
|---|---|
| `make ci` | Run the ordinary source gates in their fixed order. |
| `make ci-parallel` | Run the same ordinary gates once across three bounded local lanes. |
| `make ci-full` | Run ordinary CI plus all current-host equivalents of weekly suites. |
| `make check` | Run fast Go and Web feedback. |
| `make hygiene` | Verify formatting, generated protocol tables, diff hygiene, deterministic browser-evidence contracts, the single-module layout, retired paths, and production dependency boundaries. |
| `make sloc`, `make workflow-lint` | Run local sloc-guard and actionlint directly. |
| `make lint`, `make vet` | Analyze the complete production module. |
| `make gopls` | Check tracked Go files with the local language server. |
| `make short-go` | Validate the disjoint core and non-core sets with short, race, atomic coverage, and Go's local test cache, then enforce coverage. |
| `make race`, `make coverage` | Run diagnostic-only short sweeps; `make ci` uses the combined `short-go` gate. |
| `make vectors` | Verify the canonical Go-to-TypeScript protocol vectors without modifying the worktree. |
| `make vectors-update` | Explicitly regenerate the canonical protocol vectors for review. |
| `make web` | Run ESLint, the TypeScript/Vite build, and Vitest. |
| `make e2e` | Run the critical sender/relay/receiver process path. |
| `make browser` | Run the current-platform Chromium relay smoke and non-periodic browser contracts. |
| `make browser-weekly` | Add Firefox, WebKit, Chromium periodic, and scheduled product scenarios on the current host. |
| `make long-go` | Run named E2E/catalog/output-runtime long suites and native integration packages. |

`make ci` runs `short-go vectors web e2e browser hygiene workflow-lint lint vet gopls sloc` serially.
`make ci-parallel` owns that exact gate set through concurrent runtime (`short-go`, `vectors`, `e2e`), Web
(`web`, `browser`), and static-analysis lanes; each lane preserves its listed order. Runtime and protocol
failures run first in the serial entry point because they carry the highest product risk. Use `make check` or
a focused target while iterating; run `make ci` before handoff. Local `short-go` reuses Go test results when
their code and observed inputs are unchanged; hosted CI and release validation still force fresh execution.
The serial local p95 goal is at most 10 minutes.
Named Go suites discover their selected top-level tests before execution and fail when a selector matches none.

Browser component ownership is filename-driven by `web/playwright.contract.config.ts`. The platform
browser gate runs the Chromium smoke before short component contracts; the weekly supplement owns the
remaining browser projects and product scenarios. A dated, machine-specific diagnostic is available in the
[local test timing snapshot](local-test-timing.md); it does not define a cross-host performance baseline.

The browser FSA acquisition experiment is complete. Shipping retains its compact reviewed support matrix,
schemas, detached digest, and a noninteractive artifact verifier; native picker/browser acquisition source and
raw receipts are not distributed. `make hygiene` checks those frozen artifacts plus the self-contained workspace
threshold derivation. The Web suite separately proves that production grants support only for one exact reviewed
browser, platform, filesystem, feature, artifact-byte, and policy-digest match.

Coverage is blocking: the core package-set total is at least 90%, the non-core package-set total is at
least 80%, and every included Go package is at least 70%. Product packages are not excluded and thresholds
are not lowered to make a gate faster.

## Ordinary GitHub CI

The [CI workflow](../.github/workflows/ci.yml) runs on every pull request and push to `main`, cancels
superseded work for the same ref, and keeps independent owners for:

- static analysis and repository hygiene;
- disjoint core and non-core vet, short race/coverage, and coverage verdicts, plus one all-package build;
- canonical protocol vectors;
- the Web application, critical Go process E2E, Chromium smoke/contracts, and Windows-native Go behavior.

Each Go package set is tested once by its owner, and every Go cache uses the root `go.sum`. Hosted jobs
prepare their own current toolchains and frozen project dependencies. Branch rules must require the
ordinary CI results before merge; the push-to-`main` run detects regressions but cannot prevent an
unprotected merge.

## Automatic weekly suites

The [weekly workflow](../.github/workflows/weekly.yml) runs automatically every Sunday at 04:23 UTC;
manual dispatch is retained for diagnosis. It owns the expensive product evidence that does not belong in
ordinary CI:

- Linux and Windows integration stability;
- named Go E2E/catalog suites and Linux/Windows durable-output long tests;
- hosted Linux/ext4 and Windows/NTFS native output certification;
- Chromium progressive catalog paging and direct/TURN relay-cut switching;
- browser/Pion interoperability, Firefox/WebKit product and component contracts, Chromium periodic
  contracts, and a Windows Chromium process smoke.

Long Go tests use native `testing.Short()` boundaries and stable `TestLong...` names, so every short-mode
skip has an automatic owner. `make browser-weekly` and `make ci-full` reproduce only the applicable
current-host coverage; they do not claim another platform's filesystem or browser evidence.

Native output certification uses the same implementation as release validation. Linux runs an isolated
loop-ext4 fixture with an unprivileged receiver; Windows runs NTFS checks under a fresh standard-user token.
A current-host pass or cross-build proves compilation, not the other platform's filesystem semantics.

## Root production release

The [root release workflow](../.github/workflows/release.yml) is the only production release control plane.
Manual promotion supplies one exact lowercase 40-character commit SHA and one root `vX.Y.Z` version. The
commit must be reachable from the repository's default branch. Read-only Linux and Windows jobs construct
and validate the deterministic root module archive from that commit, including tidy, dependency
verification, the core dependency boundary, vet, build, vulnerability scan, tests, CLI installation, and
native filesystem certification. Release authority and the sole write-token publisher run from fresh
default-branch checkouts, so the requested revision cannot replace the promotion control plane.

Publication creates the immutable root `vX.Y.Z` tag only after both platform validations succeed. Repeating
the same commit is idempotent; an existing tag at another commit fails without moving it. There are no
intermediate tags or separately versioned core steps; source and root artifact stay one coherent revision.
Historical tags remain immutable history rather than current release instructions.

For local diagnosis, `scripts/ci/linux/release.sh <version> <commit-sha> [linux-ext4]` and
`scripts/ci/windows/release.ps1 -Version <version> -CommitSHA <commit-sha> [-NativeProfile windows-ntfs]`
validate an exact clean checkout and extracted archive but never publish a tag. Native profiles require the
corresponding Linux/ext4 or elevated Windows/NTFS host capabilities.

## Product safety boundary

Validation simplification does not relax E2EE, capability links, remote-input validation, root
confinement, file revision/lease semantics, atomic no-replace publication, crash cleanup, native output
identity/ancestry revalidation, or relay/WebRTC switching. Native ordinary output is resumable only when
all four capability facts hold, live-only only when safe publication plus crash cleanup hold, and otherwise
fails before content. Unknown local, network, FUSE, cloud-placeholder, reparse, and nested-mount profiles
cannot inherit the NTFS/ext4 verdict. Stable session, operation, and scenario identifiers and structured
milestone logs remain part of product and test diagnostics.

Performance measurements are local diagnostics described in
[Performance benchmarks](performance.md); they do not gate correctness or release.
