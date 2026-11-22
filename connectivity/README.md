# WindShare connectivity

`github.com/windshare/windshare/connectivity/...` owns native peer negotiation, demand-driven multipath orchestration, ICE/gateway/socket path policies, and relay registration/admission outside [`core`](../core/). Application workflows in [`engine`](../engine/) assemble these packages over [`transport`](../transport/) channels.

## Package layout

| Package | Responsibility |
|---|---|
| `v2signal/` | Canonical E2E WebRTC signaling schema, codec, and validation |
| `v2peer/`, `v2peer/peerset/` | Peer negotiation/admission and demand-driven `PeerSet` orchestration |
| `throughput/` | Deterministic path growth and retirement decisions from aggregate progress |
| `nativepeer/`, `pionpeer/` | Native resource ownership and immutable Pion `PeerConnection` adaptation |
| `networkstate/` | Address and egress route snapshots with generation tracking |
| `socketauthority/` | Bounded, independent per-path UDP/TCP socket ownership and leases |
| `reachability/`, `reachability/gateway/` | Gateway port-mapping leases and reachability refresh |
| `iceconfig/`, `icepolicy/` | Immutable ICE inputs and candidate/endpoint selection policy |
| `relayplan/` | Bounded deterministic relay candidate selection |
| `relayset/` | Per-relay sender/receiver registration, recovery, and coordinated multi-relay session admission |

## Validate

```sh
GOWORK=off go test ./connectivity/...
make ci
```
