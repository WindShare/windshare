# Pion fork follow-up

Updated: 2026-10-30. Pinned baselines: ICE `v4.4.8`, WebRTC `v4.2.26`, TURN
`v5.1.3`, SCTP `v1.12.0`. Exact commits and checksums live in
[the manifest](../third_party/pion/manifest.json).

The local upgrade is separate from upstream contribution work. No upstream issue
or PR was submitted as part of this upgrade; earlier proposals still have no
submission links recorded here.

## Changes absorbed from upstream

- Universal UDP mux initialization now completes before the reader starts; the
  local initialization patch has been removed.
- TURN provides context-aware allocation and transaction cleanup. The local
  allocation-method copy has been removed. WindShare still interrupts blocked
  control writes and deletes allocations before closing their transport.
- ICE owns gathering generations and peer-reflexive STUN check priorities. Local
  mapped candidates and TURN routes carry the captured generation; provider
  interface preferences compose with the upstream priority calculation.
- Shared UDP writes now have a cancellation grace period. Exclusive idle refresh
  interrupts immediately so socket handoff does not inherit that delay.

## Remaining contribution candidates

| Area | Local difference | Next upstream work |
| --- | --- | --- |
| ICE/WebRTC TCP srflx | Preserve TCP type and socket address through parsing and conversion | Separate ICE fix, then WebRTC conversion fix |
| STUN socket ownership | Local-endpoint-scoped gathering and explicit cancellable cache refresh | Discuss small socket/context APIs; keep ownership and leases in WindShare |
| ICE priorities | Frozen base-address order without losing TCP direction or prflx type preference | Discuss an interface-preference extension point |
| Initial ICE checks | Initial exploration window independent of connected failure detection | Propose ICE option, then WebRTC setting |
| External mappings | Verified external IP **and port**, bound to the real socket and gathering generation | Discuss endpoint model; retain candidate-type/network filtering |
| TURN routing and cleanup | Injected cancellable egress, allocation ownership, bounded release | Keep route/capacity policy in WindShare; isolate reusable lifecycle fixes |
| TURN binding recovery | Binding expiry, stable channel numbers, bounded retries, peer-scoped fallback and observations | Separate protocol recovery fixes from product observation hooks |
| ICE STUN writes | Bounded per-candidate writers keep permission waits outside the agent loop | Prepare a minimal blocked-permission reproduction |
| SCTP RACK | Lost retransmissions remain eligible for recovery; ambiguous retry ACKs are rejected | Submit focused recovery fix with [recorded evidence](../testdata/connectivity/webrtc/stall-20261022/README.md) |

## Maintenance

Before proposing a change, check upstream main and existing issues/PRs. Submit
one problem per branch, with a minimal reproduction and upstream-native tests.
Do not include dependency upgrades or WindShare policy in a protocol fix.

For local changes, update source, patch and manifest together. Run
`go run ./scripts/ci/_piondeps -reproduce`, affected race tests and
`make ci-parallel`; dependency upgrades also rerun socket/selected-pair payload
and Chromium/Pion evidence. The [provider fixtures](../testdata/provider-capabilities/README.md)
prove local mechanics, not public connectivity rates.

After an upstream fix reaches the adopted release, remove its local patch and
refresh provenance. Record actual submission/merge links here; a merged PR alone
does not remove the shipped maintenance burden.
