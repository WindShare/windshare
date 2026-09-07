# Local CI timing snapshot — 2026-09-07

One before/after pair on the same Windows amd64 checkout, using existing caches and installed tools.
This is a local diagnostic, not a p95 baseline or a performance guarantee.

Host: Intel Core i7-14700KF (28 logical processors), 31.8 GiB RAM; Go 1.27.0, gopls 0.23.0,
Node.js 24.16.0, pnpm 11.8.0. Other developer applications remained open.

| Measurement | Before | After |
|---|---:|---:|
| Complete `make ci-parallel` | 708.1 s | 286.4 s |
| gopls within that run | 624.0 s | 278.6 s |
| Web gate | 145 s | 110 s |
| Vitest within the Web gate | 78.64 s | 38.97 s |
| Hygiene gate | 79 s | 16 s |
| Browser gate | 115 s | 97 s |

The complete run saved 421.7 seconds (59.6%). Gate timers are reported by their launchers and do not
always include package discovery; concurrent gate durations must not be added to estimate total time.

Both complete runs passed the same ordinary gates: 92 production Go packages with unchanged race and
coverage requirements, 1,297 maintained Go files across six gopls views, 2,018 Vitest tests in 249 files,
and 86 Chromium smoke/short-contract tests.

The comparison combines three changes:

- Move 86,747 historical experiment files from `tmp/` into `.tmp/legacy-tmp/`, preserving them. Go's
  `./...` traversal skips dot directories but does not read `.gitignore`. Package discovery after the
  move took 1.0 second and returned the identical package set. The native FSA evidence runner now writes
  new browser profiles and materialized trees below `.tmp/fsa-small-file-native/`.
- Reduce the gopls open batch from 64 to 8 files. Each open/close recomputes views over the open set;
  smaller batches bound that work while retaining view witnesses and explicit diagnostic completion.
- Run Vitest with two workers and explicit file isolation instead of one worker. Browser concurrency
  and the three CI lanes are unchanged.

These measurements do not isolate each change's contribution. Raw local logs are in
`.tmp/ci-parallel-before.log`, `.tmp/ci-parallel-after.log`, and `.tmp/ci-parallel-after-summary.json`.
