# Local provider evidence

These fixtures prove bounded transport mechanics, not public connectivity rates.
Pinned Pion versions and source reconstruction are recorded in
`third_party/pion/manifest.json`.

## Historical socket comparison

Reproduce from the repository root:

```sh
go test -race ./transport/webrtc/provider -run '^TestLocalFixedSTUNSocketLifecycleBaseline$' -count=1 -timeout 30s -v
```

`local-fixed-stun-baseline.json` records one Windows run, source hashes, and limits.
The historical arm reconstructs one fixed STUN endpoint and default Pion sockets
owned by each fresh PeerConnection. It is not a saved pre-change binary. The
comparison arm uses the current path-scoped SocketAuthority with the same single
local STUN endpoint. Both arms use the current pinned dependency.

Each arm verifies an 8 KiB payload over two real ICE/DTLS/SCTP attempts, with forced
direct loss between chunks. Only srflx candidates are advertised; actual selected
pairs may include host/prflx paths. STUN source endpoints and selected pairs are
recorded separately. Stable sockets must retain both ports across replacement.
Default sockets may use separate STUN and selected host endpoints.

The fixture uses the production download metric accumulator and export field
names. First-direct timing begins before connection construction and ends after
the fixture's proof payload; it is not ProtocolSession admission timing. Fallback
stall measures pending recovery after forced direct loss; no application relay is
present. The direct fraction covers verified, deduplicated fixture payload only.
Gathering is deliberately completed before candidate delivery for topology
control. Timings and ephemeral ports vary; do not infer a product latency or
Internet success-rate improvement from this comparison. The optional baseline is
skipped by short tests to keep ordinary validation bounded.

## TCP capability

`windows-tcp-host.json` and `windows-tcp-mapped.json` record the exact local
browser/native combinations tested. Reproduce using
`node web/scripts/browser-network-matrix/run-tcp-capability.mjs output.json`
(add `--mapped` for allocated external ports). These results authorize only the
explicit provider profiles implemented by the capability policy.
