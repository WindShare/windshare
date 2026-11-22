# Pion fork follow-up

Updated: 2026-11-23. Official upstream dependencies: ICE `v4.4.8`, WebRTC
`v4.2.26`, TURN `v5.1.3`. Only SCTP `v1.12.0` remains pinned under
`third_party/pion/sctp`; exact commit and checksums live in
[the manifest](../third_party/pion/manifest.json).

## Retired local forks

- `pion/turn`, `pion/ice`, and `pion/webrtc` forks have been removed.
- Mapped external UDP and passive TCP reachability candidates are emitted at the
  WindShare Trickle ICE boundary (`connectivity/pionpeer`) on top of upstream
  `SetICEUDPMux` and `SetICETCPMux`.
- Routine `io.ErrClosedPipe` shutdown packet drops from upstream `UDPMuxDefault`
  are downgraded in WindShare's injected Pion logger (`connectivity/pionpeer/logging.go`).

## Remaining contribution candidate

| Area | Local difference | Next upstream work |
| --- | --- | --- |
| SCTP RACK | Lost retransmissions remain eligible for recovery; ambiguous retry ACKs are rejected | Submit focused recovery fix to `pion/sctp` with [recorded evidence](../testdata/connectivity/webrtc/stall-20261022/README.md) |

## Maintenance

Do not reintroduce `pion/ice` or `pion/webrtc` forks or log-suppression patches
under `third_party/pion/`; keep product policy and logger filtering in WindShare.

For local SCTP changes, update source, patch and manifest together. Run
`go run ./scripts/ci/_piondeps -reproduce`, `go test -short -race github.com/pion/sctp`,
and `make hygiene`.

Once the upstream SCTP RACK fix reaches an adopted release, remove
`third_party/pion/` and `scripts/ci/_piondeps/` completely.
