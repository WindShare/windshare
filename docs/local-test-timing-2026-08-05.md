# Local test timing snapshot — 2026-08-05

This is a single warm-cache diagnostic run, not a p95 baseline or a validation budget. Timings from
different hosts, toolchains, cache states, or test ownership layouts are not directly comparable.

Environment: Windows 11 10.0.26200, Intel Core i7-14700KF (28 logical processors), 31.8 GiB RAM,
Go 1.26.5, Node.js 24.16.0, and pnpm 11.8.0.

Counts below use top-level Go tests, Vitest assertions, and Playwright project/test identities. Go package
parallelism means individual test durations are useful for finding outliers but are not additive wall time.

## Ordinary local CI tests

| Owner | Tests | Wall time |
|---|---:|---:|
| Root short Go race/coverage sweep | 465 | 14.81 s + 1.39 s coverage verdict |
| Core short Go race/coverage sweep | 1,269 | 35.37 s + 0.13 s coverage verdict |
| Vitest, 65 files | 450 | 13.51 s |
| Critical process E2E | 1 | 6.59 s |
| Chromium product smoke | 1 | 3.48 s |
| **Test-only total** | **2,186** | **75.30 s** |

The slowest ordinary top-level tests were the progressive liveshare transfer (15.97 s), Windows NTFS
restart recovery (15.69 s), composite runtime durable publication (12.50 s), and output-runtime retirement
cleanup (11.17 s). Vitest assertions consumed 2.41 s in aggregate; most of its 13.51 s wall time was runner
startup, file loading, and transformation.

## Scheduled test owners

| Owner | Tests | Wall time |
|---|---:|---:|
| Long process E2E | 6 | 34.92 s |
| Native integration packages | 8 | 1.10 s |
| Catalog long tests | 4 | 48.05 s |
| Output-runtime long tests | 2 | 3.77 s |
| Browser weekly supplement | 10 | 41.42 s |
| **Additional scheduled total** | **30** | **129.26 s** |

`browser-weekly` also owns the ordinary Chromium smoke, so its complete current-host cost was 44.90 s.

## Browser contracts without a current owner

The seven `web/test/browser/*.spec.ts` files contain 40 logical tests, or 120 identities across Chromium,
Firefox, and WebKit. Existing dormant configurations discover 75 identities; a temporary local-only
configuration measured the remaining 45 and was removed after the run.

| Group | Result | Wall time |
|---|---:|---:|
| R0 storage contract | 7 passed, 2 skipped | 12.15 s |
| Curve25519 fallback | 3 passed | 5.25 s |
| R5 output contracts | 50 passed, 13 skipped | 109.27 s |
| Remaining catalog, preview, and scaffold specs | 45 passed | 29.66 s |
| **Total** | **105 passed, 15 skipped** | **156.34 s** |

R5 dominates this cost: its million-member ZIP, exact portable-boundary, and real recovery scenarios belong
in a scheduled long owner rather than an ordinary short browser gate.

## Validation observation

All suites above passed when timed independently. A later `make ci` run stopped after 39.45 s in the core
short sweep because two Windows placement-guard tests received `Access is denied`. A focused race-enabled
rerun reproduced `TestWindowsV3RecoveryRetainsFullPlacementGuardThroughFinalObservation`; the companion
`TestWindowsResumeDiscardRetainsPlacementThroughFinalRevalidation` passed on focused rerun. Therefore this
snapshot does not establish that the full local CI was green or that its p95 goal was met.
