# STUN endpoints

WindShare retains its shipped `stun:stun.l.google.com:19302` endpoint.
The allowlist is build configuration, not a claim of availability or geographic coverage.

`connectivity/icepolicy` and `web/src/connectivity/ice-policy` accept trusted build,
deployment, or explicit local endpoint catalogs. Remote signaling and capability
links cannot supply endpoints. Entries have stable IDs, STUN URLs with explicit
ports, address family, trust, deployment priority, and optional region, provider
and failure-domain facts. Unknown facts remain empty. Enabled entries must be
explicitly allowed: both native and web accept `reviewed` deployment/build
config and `local` entries, while native `connectivity/icepolicy` also accepts
live-verified `community` entries (strictly ranked below `reviewed` and `local`,
bound to the same public host as the community relay); `unreviewed` entries
remain catalog-only.

An attempt snapshots at most two endpoints. A recovery wave contacts at most four,
using distinct known failure domains for the backup. Endpoint-attributed failures
cool down for 30 seconds in the current network generation; unattributable browser
facts remain profile-level. Selection performs no DNS or network requests.
Local candidate pruning preserves slots for late path classes; remote signaling
limits remain enforced.

## STUN service in wsrelay

The Rust native host is shipped as `wsrelay`. Build it using
[installation](installation.md#build-from-source) or use a release binary:

```sh
wsrelay --config node.json
```

The [node configuration](../relay-rust/crates/relay-native/README.md) has a `stun`
object. `udp_addresses` defaults to `["0.0.0.0:3478","[::]:3478"]` and
`admin_address` to `"127.0.0.1:8081"`. IPv6 sockets are IPv6-only, so both families
can bind the same port. Numeric IP:port pairs are required; at most 16 distinct
listeners are accepted. An empty `udp_addresses` list disables STUN;
`admin_address: null` disables only admin. Listener/runtime failures remain
isolated from relay traffic; invalid configuration fails startup.

Each listener allows 1,000 requests/second, 20/source IP/second and 4,096 tracked
IPs per one-second window. Override `stun.limits.requests_per_second`,
`source_requests_per_second` and `max_sources`; values must be positive.
Malformed datagrams spend the same budget. Source ports do not create new quotas,
and a full source table refuses new IPs until the next window.

The UDP profile follows [RFC 8489](https://www.rfc-editor.org/rfc/rfc8489.html)
Binding framing, XOR-MAPPED-ADDRESS and fingerprint rules: requests may omit the
fingerprint; supplied fingerprints must validate and occur last. Malformed packets,
non-Binding requests and datagrams above 1,200 bytes are dropped; unknown required
extensions receive 420. It provides no TURN allocations, authentication, TCP STUN
or legacy RFC 3489 behavior.

Admin `GET/HEAD /healthz` returns 503 if any configured UDP listener is unavailable;
relay health is independent. `GET/HEAD /metrics` exposes received, responded, invalid,
limited, read/write-error counters and health, labelled by `udp-N` listener ID.
Admin has 16 connection slots and a five-second read/write deadline;
UDP writes have a one-second deadline. Terminal read failure retires only that
listener; ICMP peer errors and failed writes leave it usable. Readiness JSON lists
bound addresses; structured lifecycle logs identify listeners and failures.
Ctrl-C and Unix SIGTERM close and join all owned sockets/tasks.

UDP 443 requires explicit provisioning, bind privileges and a free port. It can
improve STUN reachability but does not move peer traffic to port 443.
See [deployment](../scripts/deploy/wsrelay/README.md). Add tested nodes to trusted
endpoint configuration; direct-connect improvement needs real-network measurements.
