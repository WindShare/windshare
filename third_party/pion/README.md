# Pinned Pion provider adaptation

WindShare carries source projections of Pion ICE v4.2.7 and WebRTC v4.2.16. Exact
upstream commits, Go checksums and file hashes are in `manifest.json`; upstream
MIT licenses and SPDX notices remain beside the sources. These nested dependency
modules are separate from WindShare's single production Go module and coverage set.

The narrow patches in `patches/` add:

- A PeerConnection setting for the ICE provider capability snapshot.
- Shared physical socket gathering for host and server-reflexive candidates,
  including explicit local-endpoint selection for multiple interfaces/families.
- Immutable allocated external UDP/TCP IP **and port** candidates bound to the real base.
- Correct TCP type and socket address preservation for mapped server-reflexive candidates.
- Initial checking time independent of connected disconnection/failure timers.
- Local preference applied inside ICE before checklist construction and signaling.
- Explicit STUN cache-bypassing refresh and response transaction validation.
- Complete universal UDP mux initialization before its socket reader starts.

Socket, demand, mapping and retry policy remain in WindShare. Production uses
independent sockets per peer path; cross-path shared remote-tuple demultiplexing
is not supported. TCP is restricted to the exact capability profiles and local
proof scope documented in [provider capability evidence](../../testdata/provider-capabilities/README.md).
Initial gathering uses Pion's default GatherOnce; active TCP is filtered to the
same immutable local address snapshot, so provider regathering cannot escape it.

Verify the shipped projection without network access:

```sh
go run ./scripts/ci/_piondeps
```

Reproduce it from checksum-verified upstream modules in a temporary directory:

```sh
go run ./scripts/ci/_piondeps -reproduce
go test -race ./connectivity/socketauthority ./transport/webrtc/provider
```

Reproduction reads the module cache and applies patches only to temporary copies.
Dependency upgrades must update the reviewed projection, patches and manifest and
rerun the socket, selected-pair and authenticated payload evidence. The source
distribution must include these nested modules; a canonical Go proxy module zip
cannot carry this local replacement build.
