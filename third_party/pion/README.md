# Pinned Pion SCTP patch

WindShare uses official upstream Pion ICE (`v4.4.8`) and WebRTC (`v4.2.26`),
carrying only a minimal source projection of Pion SCTP (`v1.12.0`). Exact
upstream commit, Go checksum and file hashes are in `manifest.json`; upstream
MIT licenses and SPDX notices remain beside the sources. This nested dependency
module is separate from WindShare's single production Go module and coverage set.

The narrow patch in `patches/sctp.patch` adds:

- SCTP RACK recovery of lost retransmissions using the latest send time, with
  minimum-RTT rejection of ambiguous retry acknowledgements. Congestion limits
  and retransmission timeouts are unchanged; [evidence](../../testdata/connectivity/webrtc/stall-20261022/README.md).

Verify the shipped projection without network access:

```sh
go run ./scripts/ci/_piondeps
```

Reproduce it from checksum-verified upstream modules in a temporary directory:

```sh
go run ./scripts/ci/_piondeps -reproduce
go test -race github.com/pion/sctp ./connectivity/socketauthority ./connectivity/pionpeer ./transport/webrtc
```

Reproduction reads the module cache and applies `patches/sctp.patch` only to a
temporary copy. Dependency upgrades must update the reviewed projection, patch
and manifest and revalidate SCTP recovery evidence. The source distribution must
include this nested module until the upstream SCTP fix is merged and adopted.
