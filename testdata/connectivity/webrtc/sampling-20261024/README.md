# Connection sampling validation — 2026-10-24

The refactor passed 51 complete, SHA-256-verified cross-host transfers: 47 browser transfers across 45 invocations, plus four native transfers. Every attempt is retained; none failed. Two browser invocations each ran two receivers. No browser page errors or invalid sampling decisions were observed.

## Paired LAN measurements

Windows sender → Linux Chrome, real file reading, sealing, authenticated reception and OPFS publication. Each arm uses frozen sources; **before** restores only the refactor's 27 existing files and removes its nine new files. Other sources are identical. [Build identities](build-identities.json) record the base revision, changed-file hashes and binaries.

Rates are decimal MB/s. Overall timing starts at download-connectivity activation and ends at output close; share/catalog setup and post-close hashing are excluded. Middle timing interpolates the fixed 20%–80% output interval. Browser runs enforce host/host LAN ICE candidates, include connection startup, use 1 MiB blocks unless stated, and run serially except the explicit competition cases. Pair order alternates; there is no throughput warm-up.

| File / policy | Runs | Overall median | Middle median |
|---|---:|---:|---:|
| 256 MiB / before | 6 | 21.79 | 26.06 |
| 256 MiB / current | 6 | 23.07 | 26.51 |
| 1 GiB / before | 3 | 26.59 | 27.68 |
| 1 GiB / current | 3 | 28.39 | 30.21 |
| 1 GiB / fixed three, current | 3 | 31.00 | 31.56 |

Current wins five of six 256 MiB pairs and all three 1 GiB pairs. These are small local samples, not a universal speed guarantee. The extra 1 GiB smoke run is retained but excluded from the paired medians.

Current trials receive representative work: second-path samples completed 40–45 blocks, and third-path samples 25–31, over roughly three seconds in the paired large runs. Forced trial disconnection abandoned the exact old generation; a replacement reusing the lane ID was sampled with its new epoch. No keep/no-gain verdict lacked the required work, and no negative work counters were recorded.

A remaining policy limit is visible: one third-path retry improved useful throughput from 27.76 to 30.24 MB/s (about 8.9%) but missed the existing 10% threshold and was retired. Other third-path trials were retained. Fixed three still wins these local measurements; stable throughput above 30 MB/s is not established. This study does not change the threshold or Windows UDP submission.

## Everyday workflows and faults

| Scenario | Evidence |
|---|---|
| 128 KiB, three runs per arm | All use one PC; completion medians 581 ms before / 511 ms current. No wait for expansion. |
| 64 MiB, 125 ms delay per output write, two per arm | About 9.4–9.5 seconds and one PC in both versions. |
| 64 MiB Direct ZIP, one per arm | Both publish and hash correctly; 4.99 / 4.07 seconds. |
| Two simultaneous 128 MiB receivers, one pair per arm | All four complete; before 8.72 / 9.89 seconds, current 8.59 / 10.38 seconds. |
| 4 MiB preview during a deliberate one-second output pause | Before 1.133 seconds / current 1.142 seconds; completes 116 / 134 ms after output resumes. |
| Cancel that preview | Cancellation resolves in 3.8 / 3.2 ms after abort; the download completes. |
| Close a sampling PC, two current runs | Both complete with correct hashes; old epoch abandoned, replacement preserved. |
| Delay every fetch on the exact sampling lane by 600 ms, two current runs | Both complete; eight successful samples each. One retires, one keeps based on aggregate improvement. |
| 4 MiB blocks / 256 MiB, current | Completes with correct hash; remains on one PC. |

No sampled browser demand exceeded three concurrent paths. The 1 GiB current runs observed at most 8 MiB in flight and 26 MiB queued; these are sampled maxima, not continuous resource peaks. The preview/output-pressure behavior already exists in the baseline. ZIP and competition sample counts are too small for performance claims.

## Native and deterministic checks

Two 1 GiB CLI transfers in each direction passed with matching hashes. Windows → Linux measured 12.80 / 14.52 MB/s including startup; Linux → Windows measured 21.11 / 20.69 MB/s. Default native ICE selected a mixture of host/host and srflx/host pairs, so these rates are separate from the browser LAN comparison. [Native results](native-results.json) preserve selected-pair and trial evidence.

The controller replay checks 1,526 histories / 90,098 decisions against Go and TypeScript, including the 26 shared vectors and 1,500 seeded histories. It checks exact-generation targets and finite nonnegative rates in addition to cross-language agreement. This is controller conformance, not a proof of the entire scheduler.

[Validation results](validation.json) record 25 repeated race runs of the four affected packages, three repeated connection integration runs, E2E, hygiene, and the weekly browser supplement: 60 passed, five skipped. A second cross-browser contract run confirmed all five skips are WebKit OPFS tests because this environment lacks `navigator.storage.getDirectory()`; there were no failures or flaky tests. The full production Web/Go gates also passed during implementation. This study does not establish public-WAN performance, power usage, crash durability or restart recovery.

## Reproduce

Run from the repository root with Go, Rust, Node/pnpm, Chromium and SSH access to the Linux browser host. The existing LAN harness reads `.tmp/webrtc-preflight-20261023/lab.json` for SSH/address settings; credentials are not archived.

- Set `WS_ADAPTIVE_BUILD=webrtc-sampling-stress-20261024` and prepare the frozen current build with `adaptive-20261023/prepare.py`. Clone that snapshot for the before build, restore/remove only the files listed in [build identities](build-identities.json), update its source manifest, and invoke the same preparation module's `build(metadata)` function against it; its normal entry point intentionally rejects sources differing from the current worktree. Use `bottleneck-20261024/prepare-fixture.py` for the 1 GiB fixture.
- `cases.json` defines the large, medium, medium-confirmation and experience matrices. `matrix.mjs` accepts a suite name and either that configuration or a JSON case array; it refuses to overwrite an existing ledger. For a rerun, extract the desired case array and use a new suite name.
- `probe.mjs` freezes each generated driver and route instrumentation. Normal arms leave scheduling policy intact; fixed-three and fault arms are explicit in their configuration.
- `adaptive-20261023/cross-host.py lan --build webrtc-sampling-native-20261024 --size-mib 1024 --repeats 2 --direction forward` runs native transfers; repeat with `reverse`. Its build uses the same current binaries and the 1 GiB fixture as `fixture.bin`.

```powershell
python testdata/connectivity/webrtc/sampling-20261024/conformance.py
node testdata/connectivity/webrtc/sampling-20261024/matrix.mjs large testdata/connectivity/webrtc/sampling-20261024/cases.json
python testdata/connectivity/webrtc/sampling-20261024/analyze.py --archive
python testdata/connectivity/webrtc/sampling-20261024/native-analysis.py
go test -race -count=25 ./core/transfer/lanescheduling ./core/transfer/lanes ./connectivity/throughput ./connectivity/v2peer/peerset
go test -race -count=3 ./integration/v2peer
make e2e
make browser-weekly-supplement
```

[Browser results](results.json) retain all runs, decision context, report/ledger hashes and cleanup acknowledgements. Published evidence removes endpoint addresses, socket ports, machine paths and credentials; connection identities use artifact-local aliases while lane generations and measurements remain intact. Raw reports, block/output timelines, generated drivers and full source manifests remain in the named local `.tmp` builds. Both browser and native harnesses acknowledged remote cleanup.
