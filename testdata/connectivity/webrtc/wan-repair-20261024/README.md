# Native recovery and completion repair — 2026-10-24

Production changes address the scheduler wait and false completion failure identified in the [original study](../wan-stall-20261024/README.md). The laboratory base revision is `c6419c5885c01a70e1c2d85d7732a8cef12c300c` (mapped to the anonymized public history in [verification.json](verification.json)); [results.json](results.json) binds each working-tree variant to source, binary and trace hashes and retains all 16 transfer attempts, including two failed intermediate attempts.

## Behavior and ownership

- [RouteSupply](../../../../core/transfer/lanes/route_supply.go) represents an owner provisioning permitted routes. A forbidden relay never promises recovery. Queued reads survive connection replacement while that owner lives; closing the owner wakes them with route exhaustion. Existing per-read attempt limits remain enforced.
- [Content recovery](../../../../connectivity/v2peer/peerset/content_recovery.go) observes failed, route-blocked immutable reads independently of throughput growth. The existing PeerSet loop retires the exact admitted lane generation before replacement, including admission racing cancellation. Recovery retains its path identity and existing capacity/rate budgets.
- [Output settlement](../../../../core/transfer/job_output.go) separates confirmed publication from remote revision cleanup. Cleanup retains its deadline and bounded diagnostics but cannot turn published output into a failed file. Authentication, file commit and output finalization remain authoritative.
- Typed `content_recovery_requested` and `revision_cleanup_settled` events retain the decision context without invalidating CLI traces. Recovery uses the existing sampling interval; active slow reads and consumer backpressure do not themselves request reconnection.

## Real transfer evidence

All transfers use one 64 MiB fixture, Linux sender → Windows receiver over physical Wi-Fi. The injected fault drops UDP datagrams larger than 500 bytes for 25 seconds after 64 large datagrams. Receiver startup and cleanup are included in timings; source preparation and final SHA-256 verification are excluded. Transfers run serially without concurrent builds or tests.

| Variant | Cases | Result |
|---|---|---|
| Initial recovery candidate | 2 normal, 2 faulty `p2p-only`, 2 faulty `auto` | 4/6 passed. Both P2P cases exposed a missing adopted-channel retirement; the negotiation attempt alone did not own that channel. |
| Explicit admitted-lane retirement | Same six cases | 6/6 passed; faulty P2P completed in 53.969 / 55.641 s. |
| Late-admission cancellation handling | 1 faulty P2P, 1 faulty auto | 2/2 passed, 40.266 / 8.016 s. |
| Final recovery trace attribution | 1 faulty P2P, 1 faulty auto | 2/2 passed, 38.813 / 8.141 s. |

The final ten runs all exit 0 with SHA-256-correct output. All four faulty `auto` runs retain a lease-cleanup deadline diagnostic while completing successfully. The final P2P trace records the original attempt, network generation and direct-route context before recovery. All remote cleanup reports are complete and no study-owned local transfer processes remain.

These are diagnostic recovery tests, not throughput benchmarks. In `auto`, application-relay content crosses the harness's SSH tunnel. Public path selection, very slow fragment trickling that avoids inactivity timeout, browser behavior, and the remaining cross-machine throughput limit are not established by this repair. No VPN blacklist or global buffer increase was introduced.

## Validation and reproduction

`make short-go hygiene vectors lint vet sloc` and `make e2e` pass. Targeted race tests and hygiene/lint/vet also pass after the final trace-attribution adjustment. Deterministic tests cover forbidden routes, absent recovery owners, replacement admission, cancellation, consumer backpressure, stale generations, late attachment, cleanup timeout and real output-finalization failure; virtual clocks avoid network-length waits in unit tests.

Use the original study's LAN SSH prerequisites and fresh build names. Adjust the interface in [final-cases.json](final-cases.json) for the test host.

```powershell
$env:WS_ADAPTIVE_BUILD = 'wan-repair-new'
python testdata/connectivity/webrtc/adaptive-20261023/prepare.py
$study = 'testdata/connectivity/webrtc/wan-stall-20261024'
$repair = 'testdata/connectivity/webrtc/wan-repair-20261024'
python "$study/prepare-diagnostic.py" --base wan-repair-new --name wan-repair-new-diag --interface-filter
python "$study/matrix.py" "$repair/final-cases.json" --build wan-repair-new-diag --name recovery --public-endpoint unused
python "$repair/export.py" wan-repair-new-diag --output "$repair/new-results.json"
```

The evidence exporter retains intermediate failures and independently hashes published output. Original snapshots, overlays, ledgers and traces remain under ignored `.tmp/webrtc-wan-repair-*`; [verification.json](verification.json) binds the gate logs and final source verification.
