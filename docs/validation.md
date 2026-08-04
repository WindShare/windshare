# Validation

WindShare uses direct validation gates. The local caller, host, and same-user processes are trusted to run
tests. That trust does not make filesystem state immutable: product code still handles normal concurrent
changes and validates remote input.

## Local gates

[Makefile](../Makefile) dispatches one native script per target. Developers provide Go, Node.js, pnpm,
GNU Make, golangci-lint, gopls, sloc-guard, actionlint, govulncheck, installed Web dependencies, and the
current-platform Chromium runtime. Gates use `GOTOOLCHAIN=local` and never install or update tools,
packages, or browsers. A missing prerequisite fails in the command that needs it. Windows Firewall and
WBEM state are not validation gates.

| Entry point | Direct responsibility |
|---|---|
| `make ci` | Run the ordinary local gates in their fixed order. |
| `make check` | Root/core short tests, Web TypeScript checks, and Vitest. |
| `make hygiene` | gofmt verification, `git diff --check`, and retired-v1 production-reference scans. |
| `make sloc`, `make workflow-lint` | Run local sloc-guard and actionlint directly. |
| `make lint` | Run golangci-lint once in each Go module. |
| `make vet` | Vet both Go modules and build the released-core consumer with `GOWORK=off`. |
| `make gopls` | Check tracked Go files with the local language server. |
| `make short-go` | Run each Go module once with short, race, and atomic coverage instrumentation, then enforce coverage. |
| `make race`, `make coverage` | Run diagnostic-only short sweeps; `make ci` uses the combined `short-go` gate instead. |
| `make vectors` | Regenerate and compare the canonical Go-to-TypeScript protocol vectors. |
| `make web` | Run ESLint, the TypeScript/Vite build, and Vitest. |
| `make e2e` | Run the single critical sender/relay/receiver process path. |
| `make browser` | Run the direct current-platform Chromium micro-directory smoke. |
| `make long-go` | Run named E2E/catalog/output-runtime long suites and native integration packages. |
| `make core-release` | Validate an extracted, independently consumable core module. |

`make ci` runs `hygiene sloc workflow-lint lint vet short-go vectors web e2e browser gopls` serially.
Use `make check` or a focused target while iterating. `long-go` and `core-release` intentionally stay
outside ordinary local CI. The local p95 goal is at most 10 minutes.

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
| `browser-chromium` | The relay-only Linux Chromium micro-directory product smoke once. |
| `windows-native` | Windows vet/build and root/core short tests, without duplicate coverage. |

Every job has a 10-minute hang fuse, no dependency on another job, and reports its own result directly.
Hosted jobs prepare their own current toolchains and frozen project dependencies. The ordinary CI p95
goal is at most 6 minutes, measured from native GitHub Actions timestamps.

## Automatic weekly suites

The [weekly workflow](../.github/workflows/weekly.yml) runs automatically every Sunday at 04:23 UTC;
manual dispatch is retained only for diagnosis. Its ten independent jobs own the expensive product
coverage that does not belong in ordinary CI:

- Linux and Windows integration stability;
- named Go E2E/catalog long suites and Linux/Windows native output durability;
- Chromium progressive catalog paging and separate direct/TURN relay-cut switching;
- D1/D2 browser/Pion interoperability, Firefox/WebKit smoke, and one Windows Chromium process smoke.

Each job has a 10-minute hang fuse. Long Go tests use native `testing.Short()` boundaries and stable
`TestLong...` names, so every short-mode skip has an automatic owner rather than a manual-only suite.

## Core candidate release

The [core candidate workflow](../.github/workflows/core-release.yml) is separate from ordinary CI. A
push of the candidate tag `core-candidate/vX.Y.Z/<candidate>` resolves the exact commit. Extracted-core
checks run on Linux/ext4 and Windows/NTFS in parallel before creating `core/vX.Y.Z`. Publication is
idempotent:
an absent tag is created, the same commit succeeds, and a different existing commit fails without
moving the tag. Only the publish job receives `contents: write`; manual dispatch runs diagnostics and
does not publish.

## Product safety boundary

Validation simplification does not relax E2EE, capability links, remote-input validation, root
confinement, file revision/lease semantics, resumable and crash-recoverable output, no-replace atomic
publication, native output identity and ancestry revalidation, or relay/WebRTC switching. Stable
session, operation, and scenario identifiers and structured milestone logs remain part of the product
and test diagnostics.

Performance measurements are ordinary local diagnostics described in
[Performance benchmarks](performance.md); they do not gate correctness or release.
