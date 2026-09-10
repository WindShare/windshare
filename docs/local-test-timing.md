# Local CI timing snapshot — 2026-09-10

One before/after pair on the same Windows amd64 checkout, using existing caches and installed tools.
This is a local diagnostic, not a p95 baseline or a performance guarantee.

Host: Intel Core i7-14700KF (28 logical processors), 31.8 GiB RAM; Go 1.27.0, gopls 0.23.0,
Node.js 24.16.0, pnpm 11.8.0. Other developer applications remained open.

| Measurement | Before | After |
|---|---:|---:|
| Complete `make ci-parallel` | 290.4 s | 159.7 s |
| gopls within that run | 282.7 s | 144.2 s |
| Web gate | 110 s | 98 s |
| Browser gate | 98 s | 60 s |

The complete run saved 130.7 seconds (45.0%). Concurrent gate durations must not be added to estimate
total time. Both runs passed the same ordinary gates, including race and coverage requirements,
1,340 maintained Go files, 2,107 Vitest tests in 261 files, and 89 Chromium smoke/short-contract tests.

The comparison combines three changes:

- Order Go diagnostics by owning module, checking current-build files before foreign-platform files.
  Opening foreign files early made gopls repeatedly update additional build views for ordinary files.
  Go's build matcher determines order only; no foreign, tagged, test, or nested-module files are removed.
  The same single session, bounded batches, hint severity, and explicit completion barriers remain.
- Run ESLint with two workers, checking fresh sources on every invocation.
- Run browser contracts with two workers. Browser contexts isolate storage; tests within each spec
  remain sequential. The three CI lanes and the single-worker product smoke are unchanged.

Ordering tests cover Windows/Linux selection, nested modules, tagged files, Unicode/spaced paths,
determinism, and unreadable sources. An installed-gopls comparison also preserved compiler errors and
hint diagnostics across 75 fixture files and two modules. The full run above was on Windows only.

Raw local evidence is in `.tmp/ci-speed-before-20260910.log`, `.tmp/ci-speed-after-20260910.log`,
their matching JSON timing summaries, and `.tmp/gopls-order-parity.log`.
