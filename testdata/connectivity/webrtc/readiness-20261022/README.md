# Admission readiness and probe deadlines

2026-10-22. [Results](results.json), [snapshots](snapshots.json), and [transport timelines](transport.json) retain 36 successful, hash-checked 256 MiB transfers. Raw reports remain at their recorded local paths.

The browser receive window now announces blocked-to-available transitions. New requests, released consumer capacity, cancellation and route/lane changes still wake admission independently. Allocation returns either dispatchable work, an event-blocked state or the next eligible probe deadline; admission owns one disposable timer. Probe deadlines require otherwise eligible queued work, so idle lanes do not poll. Existing dispatch observations retain purpose, lane identity, wait duration and capacity context.

## Measurements

Frozen before (`86446a6a`) and after browser snapshots use the same production Windows sender, input bytes, 1 MiB blocks, Chromium 155, OPFS output and verified UDP LAN pair. Both arms count admission calls. Setup and final SHA-256 are excluded. Snapshot differences outside the four changed production modules and two test files are line endings only.

| Eight adjacent pairs, ABBA repeated four times | Before | After |
|---|---:|---:|
| Median admission drains | 5,665 | 1,400 |
| Median transfer-only CDP ScriptDuration, s | 0.554 | 0.419 |
| Median first output, ms | 197.9 | 198.5 |
| Median complete-file throughput, MB/s | 17.18 | 16.75 |

Drains fell 75% and ScriptDuration fell 24%. Complete-file throughput wins split 4/8; paired geometric change is -0.48%, with bootstrap 95% interval -13.88% to +18.19%. Median paired throughput change is -6.12%. First-output wins also split 4/8. Scheduling work decreased, but these noisy measurements establish neither throughput equivalence nor a speedup. ScriptDuration is not total CPU time.

Four preview rounds per version completed alongside paused bulk output: medians 1.20/1.12 s, ranges 0.98–2.99/1.05–2.09 s. Two cancellation rounds per version settled in 3.4–4.3/3.4–3.6 ms; bulk output completed with the correct hash. These small samples do not establish a fixed preview-latency gain.

## Transport boundary

Eight separate profiled rounds add 20 ms Linux UDP socket sampling and the existing 200 ms sender SCTP overlay. The selected sockets accumulated 80–358 drops in short bursts, with fast retransmissions, but none reproduced a T3 timeout. Throughput still ranged 12.92–20.94 MB/s. Thus T3 stalls observed in the earlier investigation cannot explain every slow round. Lost SCTP sequence numbers and the earlier timeout causes remain unlocalized; no transport tuning is justified by this sample. Socket queue bytes are kernel accounting, and cross-host time bounds include SSH clock-sampling uncertainty.

## Validation and reproduction

Passed `make web browser hygiene sloc`: 3,333 unit tests, three Chromium product scenarios and 167 browser contracts. Eight added fast tests cover readiness edges, time-only probe progress, cooldowns, capacity/route changes, cancellation, lane removal and reentrant timer cleanup. Existing tests retain preview fairness, shared ownership, retry/rescue and replacement coverage.

Requires the retained [lab setup](../slowdown-20261022/README.md), binaries and `.tmp/webrtc-interleaved-20261022/fixture.bin`. `run.ps1` freezes/verifies snapshots before each stage. Run stages `baseline`, `interaction`, `preview`, then `profile` twice; regenerate summaries with `analyze.py` and `transport.py`. These optional probes do not enter default gates. WAN behavior and packet-level loss attribution remain outside this evidence.
