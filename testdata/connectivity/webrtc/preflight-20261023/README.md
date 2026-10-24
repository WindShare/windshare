# WebRTC implementation preflight — 2026-10-23

The current full file pipeline benefits from two independent connections on this LAN. Four is not an established default: output pressure, startup, shared sockets, and recovery authority change the result. Fix ownership and optional-path failure semantics before adding an adaptive controller.

## Evidence

Published JSON passes through [the evidence exporter](../evidence_export.py). Lab addresses, local paths, credentials, raw logs and connection identifiers are removed or aliased; measurements are unchanged. Raw artifacts remain private under ignored `.tmp`; their measurement-time hashes are retained.

[Results](results.json) retain all 61 runs: 54 passed, seven failed, and 59 transfers passed size/SHA-256/publication checks (five successful runs had two receivers). [Profiles](profiles.json) contain separate instrumented CPU/memory observations. [Identity](identity.json) records frozen sources, binaries, fixture, harness and validation.

Windows sender → Linux Chrome 155 over physical Wi-Fi LAN, selected IPv4 host/UDP pairs verified. Production CLI reads/seals the file; production browser protocol authenticates blocks and writes OPFS. A test activation binds the real output workflow. Research-only page overlays add independent PeerConnections through existing admission/provider limits; production source is unchanged. Same-host controls use Windows Edge and therefore also change receiver hardware/browser.

All speeds are decimal MB/s of unique completed file bytes. MiB denotes fixture size.

| Workflow | Repetitions per connection count | One PC | Two PCs | Four PCs |
|---|---:|---:|---:|---:|
| LAN file, 256 MiB, OPFS | 4 | 16.96 | 25.81 | 27.41 |
| LAN file, 256 MiB, memory sink | 2 | 16.89 | — | 26.05 |
| Same-host file, 256 MiB, OPFS | 2 | 50.54 | — | 50.00 |
| LAN Direct ZIP, one 64 MiB member | 2 | 17.48 | — | 18.53 |
| LAN file, 64 MiB, +125 ms per output write | 2 | 7.30 | — | 6.50 |

Values are medians. The baseline order was 1/2/4, 4/2/1, 1/4/2, 2/4/1. Two beat one in all four adjacent triples; four beat two in only two. The paired geometric change for four versus two was +3.1%; the small-sample bootstrap interval spans −8.0% to +15.5%. These four pairs do not establish general performance. TCP controls varied from 24.9 to 61.5 MB/s around the baseline; the slow one-PC observation remains included.

- **Timing:** ordinary-file throughput starts after requested paths are ready and ends at output close. Setup/catalog and final whole-file hashing are excluded. Activation timings include the probe's ready-all barrier and socket sampling, so they are not UI click latency. ZIP includes job publication and the following CDP metric sample, but excludes extraction/hash.
- **Start immediately:** 128 KiB cold transfers completed at median 516 / 527 ms from activation for one / requested-four paths, three runs each. Extra paths were still being created when the file finished. Waiting for all paths is unsuitable product behavior.
- **Demand changes:** adding paths after one second and retiring one of four during transfer both completed and hashed correctly, at 25.68 / 25.07 MB/s. Each is one scenario, not a recovery-rate estimate.
- **Responsiveness:** during an initial bulk-output pause, a 4 MiB preview completed in 1.14–1.51 s with one PC and 0.77–0.99 s with four (two runs each). Preview cancellation settled 0.8–7.2 ms after the abort request; bulk files remained correct.
- **Competition:** two tabs sharing one sender each downloaded 128 MiB. Aggregate completion throughput was 24.21–27.76 MB/s with one PC each (three runs), and 27.16–31.67 with two each (two runs). Four each hit the admission boundary below; no throughput is assigned to those failures.
- **Holdings:** measured block-buffer reservations peaked at 17 MiB per receiver for large files and returned to zero at completion. This is not total browser/process memory or proof of cross-receiver accounting.

## Failure boundaries

All seven failures are retained, including exploratory smoke failures. They concern research-added paths; they do not imply seven failures of the shipped single-path flow.

1. **Parallel wave reservations (three runs).** Starting four paths together reserves three 120 s waves against the 360 s shared elapsed budget. Trace reports `session-elapsed-budget`; the unavailable callback closes the primary activation, leaving only two extra paths admitted. Serial growth fits the unchanged limits. See browser [wave reservation](../../../../web/src/connectivity/peer-set/wave.ts) and [budget](../../../../web/src/connectivity/peer-set/budget.ts).
2. **Activation-wide failure (one injected callback).** Calling the current unavailable boundary while two authenticated direct paths remain changes active downloads from one to zero. The file had written 10 MiB and did not finish. This deliberately invokes [`#peerUnavailable`](../../../../web/src/connectivity/v2-receiver-policy.ts); it is not a natural ICE-loss measurement.
3. **Shared sender socket capacity (three runs).** Two receivers requesting four PCs each reached two each and timed out at the harness's 30 s ready-all barrier. Two traced repeats show `used=52, requested=13, limit=64`, followed by `resource-deferred` retries. On this host, four paths consume the socket capacity needed for another 13-socket path. Attempt concurrency was already released; it was not the four-attempt limit. Existing healthy paths were present; the probe intentionally waited for all requested paths instead of starting downloads.

The controller needs actual provider capacity, essential connection/recovery reserves, and deferral that does not turn optional exploration into repeated recovery work. A path's failure must be evaluated against remaining eligible paths before terminating an activation. Do not raise limits to make the experiment pass.

## Bottleneck observations and limits

OPFS-only controls around the file baseline exceeded 250 MB/s, and switching the real transfer to memory did not improve its median. The same-host result rules out a universal 20 MB/s application ceiling, but is not a matched network-only intervention. Selected Linux UDP sockets recorded drop deltas of 0–818 packets per baseline run with default buffers; these are local socket drops, not a wire loss-rate measurement.

A [same-fixture recheck](comparison-40mbps.json) reused the historical executable, decoder and 473 MB file: four paced connections measured 29.85 MB/s median, versus the historical 38.48. Removing pacing measured 29.93; isolating browser DNS measured 25.84; the complete product measured 26.46 (three measured transfers per arm).

After the user reported heavy host load during that recheck, an [interleaved repeat](comparison-load-recheck.json) measured **30.73 / 29.20 MB/s** for cached transport / complete product, four measurements plus one warmup each, with external DNS isolated for both. Pre-transfer idle CPU averaged 7.5–7.8% on Windows and 3.9–4.3% on Linux; TCP controls still ranged 28.9–60.1 MB/s. All ten transfers passed integrity checks; one Node startup failure is retained separately. Raw reports and 1 Hz load samples are in `.tmp/webrtc-load-recheck-20261023/`.

The archived harness authenticates/decrypts records but bypasses session envelopes, request scheduling, live source sealing and output. These comparisons do not isolate individual stage costs or quantify the previous background load. The historical 38.48 → initial product 27.41 difference is not an estimate of product overhead or proof of a regression; the later repeat still does not establish sustained 40 MB/s.

The two trace-enabled CPU profiles are diagnostic, excluded from baseline statistics. Go's UDP write stack accounts for roughly a third of sampled sender CPU. Browser `equalBytes` consumes about 0.95–1.07 s per 256 MiB, almost entirely called by canonical-CBOR re-encoding validation. Investigate avoidable copies/re-encoding while retaining strict canonical and authentication semantics. CPU samples do not measure blocked syscall wall time; renderer samples exclude native WebCrypto/network threads. None of this establishes a particular native send optimization's benefit.

Public direct/TURN routes, reversed endpoints, native receivers, resume/crash durability, long-duration stability, and an actual adaptive growth controller remain unmeasured here. Foundational ownership/admission work can proceed; these LAN results do not establish WAN gains.

## Reproduce

Use installed Go/Rust/Node/pnpm prerequisites and web dependencies. Set `WS_LAB_SSH_TARGET`, `WS_LAB_SSH_PORT`, `WS_LAB_SSH_KEY`, `WS_LAB_RECEIVER_HOST`, and `WS_LAB_SENDER_HOST`; optionally store them in ignored `.tmp/webrtc-preflight-20261023/lab.json`. The existing [Linux browser helper](../slowdown-20261022/linux-browser.mjs) owns SSH tunnels, a fresh persistent disk profile and cleanup. Payload traffic uses the verified LAN pair.

```powershell
python testdata/connectivity/webrtc/preflight-20261023/prepare.py
python testdata/connectivity/webrtc/preflight-20261023/prepare-profile.py
foreach ($stage in @('baseline','pressure','small','fault','controls','zip','competition')) {
    & ./testdata/connectivity/webrtc/preflight-20261023/run.ps1 -Stage $stage
}
python testdata/connectivity/webrtc/preflight-20261023/analyze.py
python testdata/connectivity/webrtc/preflight-20261023/profiles.py
```

Preparation freezes tracked working-tree bytes and records hashes; it verifies an existing snapshot, allowing only the resulting plan-document update outside it. Set `WS_RESEARCH_FIXTURE` to reuse at least 256 MiB of fixture data; otherwise preparation reuses the prior local fixture when present or creates random data. Raw reports, generated drivers, traces, profiles and batch ledgers remain under ignored `.tmp/webrtc-preflight-20261023/`. The included JSON files are compact exports; rerunning analysis writes new files under `.tmp/`.

After measurement, 99 relevant browser unit tests, short Go tests in `core/transfer/lanes`, `connectivity/v2peer/peerset`, `core/liveshare`, and `engine`, and `make hygiene` passed. All 57 remote-browser runs acknowledged cleanup; a subsequent SSH audit found none of their processes or profile directories remaining. Four same-host contexts were closed; no matching local study processes remained. `verify.py` rechecks the captured files and validation logs, then exports the three JSON artifacts.
