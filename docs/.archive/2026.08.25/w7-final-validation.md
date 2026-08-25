# W7 final validation

Date: 2026-08-25

## Decision context

The revised product decision accepts an authority-to-`Published` duration of at most 15 seconds and retains
Windows Chromium FSA at c15 file pipelines, c8 native writers, and one immediately-started claim batch with
c3 ordered destination inspections. Cross-batch preparation is not retained.

The accepted native evidence session is `20260824T224442Z-1e6e29c3`. Its Product min/median/max was
`12,657 / 13,287 / 14,589 ms`, so every measured Product sample is within the revised 15-second ceiling.
The hermetic correctness gates below remain distinct from this native Windows performance evidence.

## Validation sequence

| Command | Outcome | Exact result | Measured wall time |
|---|---|---|---:|
| `pnpm -C web exec vitest run test/output/persistent-tree.test.ts test/output/file-system-access-lifecycle.test.ts test/output/file-system-access-compatible-name.test.ts test/output/fsa-direct-tree-production-chain.test.ts test/output/browser-mutation-scheduler.test.ts test/output/browser-file-writer-mutation.test.ts test/output/persistent-stage-diagnostics.test.ts test/transfer/v2-persistent-execution.test.ts test/ui/fsa-route-activation.test.ts test/diagnostics/performance-summary.test.ts` | PASS | 10/10 files, 179/179 tests; Vitest reported 6.71 s | 7.4516181 s |
| `pnpm -C web run test:browser:contract:short` | PASS | Chromium short: 52/52 tests; Playwright reported 34.9 s | 35.6744877 s |
| `pnpm -C web exec playwright test test/browser/durable-recovery.periodic.spec.ts --config playwright.contract.config.ts --project chromium-periodic` | PASS | Fresh-process DirectTree recovery: 2/2 tests; Playwright reported 6.8 s | 7.5734189 s |
| `make check` | PASS | Production short Go tests, forced TypeScript build, 201/201 Web test files and 1,653/1,653 tests; gate reported `PASS in 01:24` | 00:01:50.7849920 |
| `make ci-parallel` | PASS | Runtime, Web, and static lanes passed; exit 0 | 00:13:14.3402597 |

## Final repository gate detail

- Runtime lane: `short-go` passed in 00:41, vectors in 00:01, and E2E in 00:11.
  Non-core coverage was 81.8% and core coverage was 90.5%; every included package met the 70% floor.
- Web lane: Web build/tests passed in 01:38 with 201/201 files and 1,653/1,653 tests. Browser smoke passed
  1/1 and Chromium short passed 52/52; the combined browser gate passed in 00:44.
- Static lane: hygiene passed in 00:48, workflow lint in 00:00, lint in 00:48 with 0 issues, vet in 00:00,
  gopls in 10:15, and SLOC in 00:00. SLOC checked 1,934 files with 83 advisory warnings and 0 failures.
- Final result: `ci-parallel: all production source gates passed`.

## Worktree integrity

Read-only snapshots were taken before the first validation command, immediately before `make ci-parallel`,
and after it completed.

- HEAD: `8f856d347686150522fe1d2eb4aaf9a2e86007c9` on `main`.
- Pre-existing tracked status: 99 modified, 2 deleted, 0 added; 101 tracked paths total.
- Pre-existing untracked paths: 779.
- Tracked diff stat: 101 files changed, 9,012 insertions, 4,323 deletions.
- The binary tracked-diff hash was
  `d7bdfee6726e21a278bb279be4eb292bff63ff29` at all three snapshots.
- The untracked-path count remained 779 through all validation commands.
- Therefore the focused suites, browser suites, `make check`, and `make ci-parallel` changed no tracked
  file and created no non-ignored untracked artifact. This ignored validation record is the only deliberate
  post-gate worktree addition and is outside product, test, and maintained documentation paths; the final
  non-ignored untracked-path count remains 779.

## Blockers

None.
