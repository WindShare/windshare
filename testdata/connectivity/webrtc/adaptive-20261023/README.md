# Demand-driven WebRTC throughput

2026-10-23. Implements [the throughput plan](../../../../docs/plans/webrtc-throughput.md).

Engine now owns shared receive and send capacity. Native receivers reserve physical response space before dispatch and allocate distinct queued blocks across lanes. Go and browser PeerSets use the same throughput decisions, with essential attempt/socket reserves, aggregate progress measurements, cooldowns, and retirement. Browser canonical CBOR validation no longer re-encodes opaque content. Native send acceleration remains unadopted.

## Final measurements

[Results](results.json) preserve source/binary hashes, output hashes, selected ICE pairs, bounded decisions, all attempted studies, and raw-report references. The final build is `webrtc-adaptive-20261023-settled`. Earlier builds are retained separately; archived controls and public TURN below used the preceding `observed` build.

| Complete workflow | Runs | Useful MB/s | Timing / behavior |
|---|---:|---:|---|
| Windows → Linux Chrome OPFS, 256 MiB, single PC | 4 | 20.32 median | 13.23 s median completion |
| Same, demand-driven | 4 | 23.65 median | 11.37 s median completion; two PCs |
| Linux CLI → Windows CLI, 256 MiB | 3 | 15.75 median | 17.05 s median completion, including startup |
| Windows CLI → public Linux CLI, 64 MiB | 1 | 1.03 | 65.22 s; verified output |

The four 256 MiB pairs alternate arm order. The median increased 16.4%; first output remained within 0.52–0.70 s across both arms. This is one Windows desktop and one Linux Wi-Fi host. The complete pipeline did **not** reach 40 MB/s.

The final workflow confirmation also passed short files, 125 ms output delays, Direct ZIP, two competing receivers, preview, preview cancellation, and closure of an added PC during transfer. Short/slow-output/ZIP runs kept one PC. Closing the added PC at 6.87 s preserved the download and its final hash. A small fake-clock suite covers gain/loss, changing demand, output pressure, short tails, cooldowns and failed exploration; allocator tests cover distinct probes, waiting for a faster busy lane, priority and physical cancellation settlement.

## Controls and public routes

Four interleaved archived controls used cached encrypted input, four paced PCs and authenticated memory output on the same 472,759,776-byte fixture. Their median was 39.34 MB/s, versus 26.43 MB/s for the `observed` complete workflow (two PCs, 17.90 s median completion). Different connection policies and output work prevent attributing that difference solely to pipeline overhead. No native UDP acceleration was adopted from this comparison.

The public TURN study restricts content eligibility to TURN and disables direct gathering in the probe; credential delegation, renewal, transfer and output are production paths. All five runs (128 KiB, 8 MiB, 64 MiB, 128 KiB, 64 MiB) passed on `observed`, including TURN path replacements. The 64 MiB runs took 73.55–103.32 s (0.65–0.91 MB/s); small-file first output was 5.88–12.35 s. Connected relay pairs showed roughly 0.34–0.50 s RTT. These establish reachability/completion, without establishing throughput gains from optional direct paths. The temporary Cloudflare key was deleted and its absence verified.

The first isolated Pion TURN attempt on the Aliyun host failed before content dispatch. Separate UDP/TCP 3478 probes timed out without reaching the temporary listeners. This failed attempt and cleanup are retained. The configured Korea host rejected SSH authentication, so public native studies use the reachable Aliyun host instead.

Public native short transfers passed in both directions (2.80 / 2.48 s). The final reverse 64 MiB build first exceeded 180 s, then completed in 19.06 s; preimplementation controls completed in 43.61 / 29.39 s. Both successful and timed-out runs selected local host candidates, so candidate type alone does not explain the variation. Its cause and a repeatable public-route gain remain unestablished. Disabling aggregate send capacity did not resolve the slow case; a separate instrumented build completed with optional-path retirement. Diagnostic builds are excluded from performance conclusions.

An earlier public run exposed a real retirement bug: a migrated final could outlive accepted-but-lost fragments and pause the download. Native block operations now retain the authenticated final carrier and retire their exact generation before reassignment. Deterministic race tests verify lost-fragment recovery and preserve rejection of malformed completions. All 12 ordinary validation gates pass, including 3,408 Web tests and 167 Chromium contracts.

## Reproduction and evidence

Published JSON passes through [the evidence exporter](../evidence_export.py): lab addresses, local paths, credentials, raw logs and connection identifiers are removed or aliased. Measurements and hashes are preserved; measurement-time hashes refer to private originals under ignored `.tmp`, and some artifact paths are anonymized.

Use `WS_ADAPTIVE_BUILD` to select a fresh experiment; it resolves to `.tmp/<name>_stignore`, which Syncthing ignores. `prepare.py` freezes sources and builds Windows binaries; cross-compile the same snapshot for `binaries/wind-linux`. The harness requires the lab SSH settings in `.tmp/webrtc-preflight-20261023/lab.json`, existing pinned browser dependencies, and the archived fixture/control binaries identified in `controls.mjs`.

- `matrix.mjs large` runs four interleaved 256 MiB pairs; `confirmation` runs the final workflow checks. `workflows` provides the longer comparison matrix.
- `controls.mjs` repeats the archived cached controls alongside the complete product.
- `cross-host.py ENDPOINT --build NAME --direction reverse|forward --size-mib SIZE --repeats N` runs real CLI processes. SSH carries bootstrap/signaling only; `p2p-only` requires WebRTC content.
- `public-turn.py` accepts a temporary production-format TURN configuration through `WS_STUDY_TURN_CONFIG`, or starts its own temporary Pion service. External credential lifecycle belongs to the caller. Isolated public TURN and reachability probes require `WS_STUDY_SSH_PROFILE` and `WS_STUDY_PUBLIC_HOST`; endpoint values stay in local configuration.
- `analyze.py` validates final source identity and writes the compact evidence. Raw reports remain in the ignored `.tmp` tree; retained references include SHA-256 hashes.

Browser timing starts at content activation, without waiting for another PC. Completion includes output publication; hashes are checked afterwards. Native timing starts at receiver process launch, with remote-clock uncertainty recorded for first-output timestamps. MB/s is decimal; MiB is binary. Experiments run serially without concurrent builds/tests. Final validation logs are referenced in `results.json`.
