# SCTP retransmission stall investigation

[Results and raw-file checksums](results.json) and [normal-comparison binary hashes](production-builds.json) preserve the evidence. Raw reports and bounded packet traces remain under `.tmp/webrtc-stall-20261022/runs/`. This matrix contains 20 successful transfers with identical SHA-256 and three retained harness failures. The public ledger keeps failure presence, cleanup status, and raw-file checksums; machine paths and full error messages stay in local reports.

## Finding

Pion SCTP v1.11.1 excludes previously retransmitted chunks from both RACK loss scans. When a retransmission is lost, newer acknowledgements cannot schedule its recovery through RACK; it can remain outstanding until T3 expires.

A natural timeout in `pause100-default-2` occurred about 526 ms **before** the diagnostic receiver pause. Fourteen chunks lost both their original and first retry. A newer SACK arrived about 1 ms before T3, but those chunks remained outstanding for roughly 968 ms. All 402 missing outbound DATA records matched receiver socket-drop counts and corresponding loss bursts. T3 then reduced the congestion window to one packet.

The patch in [sctp.patch](../../../../third_party/pion/patches/sctp.patch) admits retransmissions to the existing RACK scans using their latest send time. Retry ACKs younger than the measured minimum RTT are rejected as ambiguous delivery evidence; Karn's RTT-sampling rule remains separate. This follows the RACK rationale in [RFC 8985 §6.2](https://datatracker.ietf.org/doc/html/rfc8985#section-6.2), a TCP algorithm adapted by Pion to SCTP.

## Controlled results

Windows sender → Linux Chrome 155, one verified LAN UDP peer, 256 MiB file, 1 MiB blocks, OPFS output. Setup and final hashing are outside throughput timing.

| Measurement | Upstream | Repair |
|---|---:|---:|
| Lost-retry recovery, three runs | 994 / 984 / 1,004 ms | 42 / 49 / 28 ms |
| T3 timeouts in those runs | 1 / 1 / 1 | 0 / 0 / 0 |
| Normal-transfer median, four runs each | 17.74 MB/s | 18.03 MB/s |
| Normal first-write median | 199.8 ms | 190.9 ms |

Fault runs deliberately dropped the original and first retry of one DATA chunk. Both arms requested a 4 MiB receiver socket buffer to suppress unrelated local overflow; measured kernel drops were zero, though some runs still had path losses. This buffer is diagnostic only.

Normal runs used ABBA × 2, with ordinary receiver buffers and no packet hooks, faults, or sender counters. The repair won two of four adjacent comparisons. There is no demonstrated stable throughput gain or regression; eight noisy LAN runs cannot prove equivalence. Missing counters are recorded as null, never zero.

## Scope and cost

The production patch changes loss recovery, without changing pacing, congestion-window limits, receive-buffer sizes, RTO values, file scheduling, preview priority, or cancellation. It adds no user settings or polling. Recovery can send another retry earlier when delivery evidence establishes loss; the minimum-RTT guard limits ambiguous-ACK mistakes. The maintenance cost is one additional pinned Pion source projection and a narrow patch to revalidate on upgrades.

Kernel receive overflow is one established loss source here, but enlarging the diagnostic buffer did not eliminate every path loss. Their upstream network causes, WAN behavior, and the historical 2.9% throughput difference remain unresolved.

## Reproduce

Requires Windows Go/Rust/Node/pnpm dependencies and the owned Linux Chrome/SSH lab described in [the earlier probe](../slowdown-20261022/README.md). Set `WS_LAB_SSH_TARGET`, `WS_LAB_SSH_PORT`, `WS_LAB_SSH_KEY`, `WS_LAB_RECEIVER_HOST`, and `WS_LAB_SENDER_HOST`.

From the repository root:

```powershell
python testdata/connectivity/webrtc/stall-20261022/prepare.py
python testdata/connectivity/webrtc/stall-20261022/prepare.py --repair-rack
./testdata/connectivity/webrtc/stall-20261022/run.ps1 -Packets -ReceiveBuffer 4194304 -DropAt 3000 -Label double-loss-before
./testdata/connectivity/webrtc/stall-20261022/run.ps1 -Packets -ReceiveBuffer 4194304 -DropAt 3000 -RepairRack -Label double-loss-after
python testdata/connectivity/webrtc/stall-20261022/analyze.py
```

For ordinary throughput, run `build-production.py`, then alternate `run.ps1 -Production` with `run.ps1 -Production -RepairRack`. Both builds explicitly choose upstream or repaired SCTP even after production adopts the patch. Browser sources are frozen at `eb1300d7`; binary hashes identify each build. Packet instrumentation exists only in isolated copies and an owned Chrome preload library. Per-run logs archive `.agents/debug.log`; owned browser profiles/processes are cleaned up.

## Validation and retained failures

- Six new regression assertions failed against upstream; the candidate passed. The shipped suite has 22 subcases covering both scanners, repeated retries, recent/queued/acknowledged chunks, and cumulative/selective/ambiguous ACKs.
- Full upstream SCTP suite with race detection passed (41.1 s). The shipped association source is byte-identical to the uninstrumented measured candidate.
- Pinned-source patch replay, release/module-boundary tests, `make short-go`, `make hygiene`, `make sloc`, and Chromium/Pion interop (3 tests) passed.
- The first short-Go run timed out in an in-memory peer handshake test unrelated to SCTP. Its targeted 10-repeat run and the complete gate rerun passed; no test deadline was relaxed.
- One early harness run completed the file but failed on a shared debug-log file lock. Two others crashed Chrome because the initial preload probe called `getsockname` from a sandboxed process. Removing that probe-only syscall fixed the crashes. All three reports remain in the ledger and are excluded from performance comparisons.

These opt-in packet experiments do not run in default CI. Only the fast SCTP state tests join the existing short-Go gate.
