# Interleaved throughput diagnosis

2026-10-22. [Results](results.json), [CPU/transport samples](profiles.json), and [verified snapshots](snapshots.json) retain 28 successful, hash-checked 256 MiB transfers. Raw timelines and CPU profiles remain at the recorded local report paths.

## Comparison

Frozen browser commits: old `197fe6f4`, new `86446a6a`; every tracked web file was checked against its Git blob. Both use the same production Windows sender binary, identical input bytes, 1 MiB blocks, Linux Chromium 155, OPFS output and one verified UDP LAN candidate pair. Browser external DNS is isolated. Timing excludes setup and final SHA-256; the existing test settlement adapter remains in use.

| Eight rounds per version, ABBA repeated four times | Old | New |
|---|---:|---:|
| Complete-file median, MB/s | 17.10 | 18.52 |
| First output median, ms | 195 | 199 |
| Transfer-only CDP ScriptDuration median, s | 0.328 | 0.541 |
| Transfer-only CDP TaskDuration median, s | 7.175 | 7.234 |
| Cumulative output write median, s | 1.887 | 2.026 |

The new version wins 3/8 adjacent pairs. Median paired change is -1.45%; geometric mean change is +5.40%, with a paired bootstrap 95% interval of -4.36% to +18.40%. Direction depends on the summary because large slow rounds affect both versions. Neither a stable regression nor an improvement is established; a small regression remains possible.

The earlier 14.94 MB/s round spent only 1.66 s writing output, less than the faster new-version rounds. Its original trace lacks sender SCTP counters, so the earlier 2.9% median drop cannot be assigned an exact cause retrospectively.

## Confirmed scheduling cost

Receipt calls `BlockReceiveWindow.#changed()` on every fragment. That wakes `ContentAdmissions` even while a lane remains blocked or already available; pending requests are scanned and sorted again. CPU profiles place 191-214 ms of inclusive sampled time in the new admission drain.

A served-module experiment wakes on receipt only when lane availability changes from blocked to available; close notifications remain intact. Four interleaved pairs against the unmodified new version produced:

| Same 256 block requests | New | Readiness notification experiment |
|---|---:|---:|
| Median admission drain calls | 5,643 | 1,449.5 |
| Median CDP ScriptDuration, s | 0.527 | 0.380 |
| Complete-file median, MB/s | 19.90 | 20.27 |

Drain calls fell 74% and ScriptDuration fell 28%; every pair reduced both. This establishes avoidable scheduling work. Throughput remains noisy, so the experiment does not establish an end-to-end speedup. It is isolated probe code, without production adoption or multi-lane correctness claims. CDP ScriptDuration is not all JavaScript CPU time; profile samples and overlapping per-block waits must not be added as serial critical-path costs.

## Transport stalls

Four separate profiled runs use the existing sender diagnostic overlay and are excluded from the main throughput comparison. The three slower rounds (17.14 / 16.43 / 15.49 MB/s) show 1 / 1 / 2 SCTP T3 timeouts; the 19.74 MB/s round has none. Bulk-progress samples taken every 200 ms have nonempty send queues throughout.

Timeout windows retain the decisive state: about 0.6 MiB pending while the congestion window falls from about 2.6 MB to 1,191 bytes; another run holds 1 MiB pending during repeated zero-progress samples and two startup timeouts. First output in that run takes 3.58 s. These stalls occur below application supply. Socket drops and fluctuating TCP controls also occur, but the origin of every loss/stall is not isolated.

## Reproduction

Requires this lab's retained [browser/SSH setup](../slowdown-20261022/README.md), installed web dependencies, and sender binaries recorded in the results. The main and wake stages reuse `.tmp/single-connection-implementation/baseline`; profiling uses the existing `native-stats` diagnostic binary.

Run `./testdata/connectivity/webrtc/interleaved-20261022/run.ps1 -Stage baseline`, then stages `profile` and `wake`. The runner creates isolated web snapshots and verifies them before testing. Regenerate summaries with `analyze.py` and `profile.py` in this directory.

Validation: all 28 transfers published with the same SHA-256, 256 dispatched blocks, one direct connection, correct selected endpoints and acknowledged browser cleanup. Production code was not changed by this diagnosis.
