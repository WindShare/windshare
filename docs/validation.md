# Validation

WindShare's ordinary CI is PR-first and unprivileged. The [CI workflow](../.github/workflows/ci.yml)
runs for pull requests, pushes to `main`, and manual dispatches; superseded work for the same PR or
ref is cancelled. A trusted change-range selector may skip the expensive graph for documentation-only
changes, but the stable `CI Required Verdict` is always produced. Windows Firewall and WBEM state
never affect a verdict.

## Local entry points

[Makefile](../Makefile) is a thin native dispatcher. Each gate invokes the local toolchain and owns its
evidence.

| Entry point | Use |
|---|---|
| `make check` | Short developer feedback for root, core, and Web checks. |
| `make integration` | Focused native diagnostics; stability invokes the matching native script once per OS. |
| `make e2e` | Go product E2E; Windows also runs the critical Chromium smoke. |
| `make race` | One complete native root sweep and one core sweep with race instrumentation. |
| `make coverage` | One root and one core coverage sweep, followed by the configured floors. |
| `make browser` | Local browser matrices plus token-free verification of protected network evidence. |
| `make ci` | Serial current-OS composition of the ordinary local gates. |

Coverage remains blocking: core total is at least 90%, root total at least 80%, and every included
package is at least 70%.

## Ordinary CI ownership

The DAG keeps cheap failures ahead of expensive evidence:

1. `changes` binds the event-specific target and comparison range.
2. Linux preflight owns formatting and hygiene, SLOC, workflow lint, Go lint, native root/core
   compile and vet, and the sole `GOWORK=off` released-core consumer build. Windows preflight owns
   native Windows root/core compile and vet.
3. After the matching preflight, Linux owns coverage, vectors, the synthetic core artifact, Web, and
   its native race sweep; Windows owns its native race sweep. Browser preflight runs once after Web,
   then each OS owns its process stack and Windows owns the Chromium smoke.
4. The always-running required verdict accepts either the complete selected graph or an intentional
   documentation-only skip; cancelled, failed, or partially skipped validation fails closed.

There are no separate uninstrumented integration or Go E2E jobs in the ordinary graph. The root
`./...` sweep already contains those packages, so Linux and Windows race each exercise them once
under native race instrumentation, while Linux coverage exercises them once under coverage
instrumentation. The focused `make integration` and `make e2e` targets remain available for
diagnosis without becoming duplicate PR owners.

## Protected Full Browser

[Full Browser](../.github/workflows/browser-full.yml) is isolated from ordinary CI. It accepts only a
scheduled or manual run for a protected default-branch SHA. A GitHub-hosted job prepares and hashes
all executable inputs without OIDC; the protected `browser-network-matrix` environment grants
`id-token: write` only to the one-use network broker on the dedicated self-hosted Linux runner.
The final token-free orchestrator verifies the 45 network identities and runs the local browser
matrix once, then publishes `browser-full-<sha>-<run>-<attempt>`.

This workflow requires repository branch protection, the protected environment, a one-use runner,
and broker runtime configuration bound to `browser-full.yml`. It does not replay the ordinary PR
graph.

## Native integration stability

[Native Integration Stability](../.github/workflows/stability.yml) runs the native integration entry
once on Linux and Windows for one validated default-branch SHA. Artifacts bind the SHA, workflow run,
attempt, job, OS, invocation, authenticated start event, and result.

The explicit evidence contract is:

| Identity | Value |
|---|---|
| Evidence epoch | `windshare.stability-evidence-epoch/v1` |
| Started event | `windshare.stability-integration-started/v2` |
| Result | `windshare.stability-result/v4` |
| Release verdict | `windshare.stability-release-verdict/v7` |
| Finding key | `windshare.stability-finding-key/v2` |

The reducer considers only current-epoch evidence. It seeks 100 valid samples per OS; insufficient
history is reported without blocking, malformed current-epoch evidence fails, two independent
observations reproduce a product finding, and 20 newer passing strict-descendant samples resolve it.

## Exact-SHA release readiness

[Release Readiness](../.github/workflows/release-readiness.yml) is a manual, read-only consumer for
one protected default-branch SHA. It:

1. selects successful `CI Required Verdict`, Full Browser, and both native stability artifacts for
   that exact SHA;
2. re-resolves the immutable selection and verifies producer metadata and artifact content;
3. publishes the stability-history verdict bound to the same target SHA;
4. checks out the target independently in a clean workspace and runs `make core-release`, without
   reusing ordinary CI artifacts or its workspace; and
5. runs an always-settling final verdict that fails unless every independent proof succeeded.

Release readiness uses only GitHub-hosted runners and has no protected environment or OIDC
permission. It cannot pass until ordinary CI, Full Browser, and Native Integration Stability have
matching successful evidence for the target SHA.

Performance publication remains a separate contract in [Performance evidence](performance.md); it
does not schedule correctness tests or grant network access.
