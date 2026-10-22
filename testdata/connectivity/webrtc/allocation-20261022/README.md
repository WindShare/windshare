# Allocation and bounded prefetch evidence

2026-10-22. [Results](results.json) contain 14 runs / 26 successful transfers, hashes, timings, CPU counters and selected endpoint pairs. Raw bounded protocol/dispatch/output traces remain at the recorded local report paths. The [interleaved follow-up](../interleaved-20261022/README.md) investigates the apparent throughput regression.

The production receiver now admits an eligible work/lane pair together with its capacity. Receipt replenishes network supply while authentication, shared content, consumer copies and output retain their own reservations. Browser preview gets one supported block within existing budgets; reclaimable cache may borrow that capacity. Direct ZIP reads one bounded range across member blocks while preserving ordered writes and checkpoint cuts.

## Complete workflows

Windows sender → Linux Chromium 155, one UDP host-candidate connection. Both selected endpoints were checked before and after each transfer; external DNS was isolated in the test browser. Sender binaries were built before the refactor and reused for every round (hashes in results). Sender, send watermarks, Chromium receive-buffer optimization and Pion dependencies were unchanged. Receiver modules came from each checkout state.

| Workflow | Observed result |
|---|---|
| 256 MiB file, 1 MiB blocks, before → after | Median 19.26 → 18.71 MB/s; first payload 180–203 → 193–224 ms |
| 256 MiB file, 4 MiB blocks, after | Median 15.82 MB/s; first payload 560–661 ms |
| 64 MiB Direct ZIP, 1 MiB blocks, three rounds | Median 16.53 MB/s; first payload 254–262 ms; published archive 67,109,368 bytes |
| 128 KiB file | First payload 91–100 ms; closed in 95–105 ms |
| 64 MiB file, 125 ms per output write | 7.41 MB/s; correct final hash |
| 4 MiB preview alongside paused/slow bulk output | 1.75 / 1.10 s; bulk completes correctly |
| Cancel that preview | 3.2 / 3.8 ms from abort to settlement; bulk continues |

The file probe measures read/seal/transfer/authentication/OPFS close, excluding connection/catalog setup and final hash. It uses the existing test plan-settlement adapter. The ZIP probe uses production activation, writer, journal and published settlement, with a test picker choosing OPFS. ZIP extraction/hash are outside timing; first payload excludes ZIP headers.

At 4 MiB geometry, three interleaved reserve-enabled / reserve-zero pairs gave medians 16.11 / 15.78 MB/s. Enabled rounds ranged 14.87–20.25; disabled rounds 15.70–17.32. No repeatable throughput penalty was detected in this sample; this does not establish zero cost. The zero-reserve control replaces only served test-module code, with no production flag.

After-refactor 256 MiB / 1 MiB rounds spent 1.66–2.15 s inside output writes and recorded 0.56–0.63 s CDP ScriptDuration / 7.72–8.54 s TaskDuration. CDP includes imports, setup and verification, so these are coarse counters, not a transport bottleneck profile. Overall throughput gains are unproven. Portable batching and native acceleration remain unadopted.

## Reproduction

Use the existing [Linux browser setup](../slowdown-20261022/linux-browser.mjs) and its test-host SSH configuration. Point the binary directory at the recorded production binaries or fresh binaries built by the product fixture.

```powershell
$env:WS_RESEARCH_BINARY_DIRECTORY = (Resolve-Path '.tmp/single-connection-implementation/baseline').Path
$env:WS_RESEARCH_ISOLATE_BROWSER_DNS = '1'
$env:WS_RESEARCH_LABEL = 'allocation-after-zip'
$env:WS_ALLOCATION_WORKFLOW = 'zip'
node --import ./testdata/connectivity/webrtc/slowdown-20261022/loader.mjs ./testdata/connectivity/webrtc/allocation-20261022/workflows.mjs baseline 64 3 0 none 1 lan-isolated
python testdata/connectivity/webrtc/allocation-20261022/summarize.py
```

Use workflow `file` or `reserve-zero` with `256 1 0 none 4 lan-isolated` for reserve controls; the measured order was enabled, disabled, disabled, enabled, enabled, disabled. The base probe also accepts `preview` / `cancel-preview`, output delay and block size. Changing a served fixture contract fails closed in the wrapper.

## Correctness and recovery

Passed: `make short-go web vet vectors e2e browser lint gopls hygiene sloc`, plus the periodic Chromium recovery suite. Web: 3,325 unit tests; browser: 3 product scenarios and 167 contracts; cross-process recovery: 2 scenarios. Focused Go race tests passed after the final ownership changes.

Regression tests cover public admission, absent routes, preview under stalled output, cached/shared frontier ordering, cancellation ownership, receipt before authentication and bounded ZIP prefetch. Existing lane tests cover mixed routes, retry/rescue and replacement.

Browser production tests verify ZIP partial-member resume, changed-revision rollback, interrupted checkpoint promotion, target authority and final archive payloads. The periodic recovery suite launches fresh browser processes and forcibly terminates Chromium for DirectTree recovery. These are real OPFS/IndexedDB recovery tests, separate from the LAN throughput measurement. WAN and integrated native acceleration remain unmeasured.
