# STUN endpoints

WindShare retains its existing shipped `stun:stun.l.google.com:19302` endpoint.
This allowlist is build configuration, not a claim of availability or geographic
coverage. No new public node is enabled by this change.

`connectivity/icepolicy` and `web/src/connectivity/ice-policy` accept trusted build,
deployment, or explicit local endpoint catalogs. Remote signaling and capability
links cannot supply endpoints. Catalog entries have stable IDs, STUN URLs with
explicit ports, address family, trust, deployment priority, and optional known
region, provider, and failure-domain facts. Unknown facts remain empty.
An enabled entry must be explicitly allowed (`reviewed` shipped/deployment config
or `local`); `unreviewed` entries remain catalog-only.

An attempt snapshots at most two endpoints. A recovery wave contacts at most four,
using distinct known failure domains for the backup. Endpoint-attributed failures
cool down for 30 seconds in the current network generation; unattributable browser
facts remain profile-level. Selection performs no DNS or network requests.
Local candidate pruning preserves slots for late path classes; remote signaling
limits remain enforced.

## STUN service in wsrelay

Build `go build -o wsrelay ./relay/cmd/wsrelay`, then run the single relay process:

```sh
wsrelay -listen :8484 -stun-udp :3478 -stun-admin 127.0.0.1:8081
```

The same binary serves application relay traffic and UDP STUN Binding requests.
STUN uses independent listeners, health, per-listener and source IP rate limits,
and metrics; it never allocates TURN relays. STUN bind, configuration, admin, or
runtime failures are logged and leave application relay traffic available.
Shutdown closes and joins all owned listeners.

The private STUN admin listener serves `/healthz` and `/metrics`. Its health fails
if any configured UDP listener is unavailable, including bind failure; metrics
retain each configured listener ID. Application relay `/healthz` is independent.
Set `-stun-udp ""` to disable STUN, or `-stun-admin ""` to disable its admin HTTP.
Defaults are 1,000 requests/second/listener, 20/source IP/second, and 4,096 tracked
source IPs per one-second window; datagrams are bounded to 1,200 bytes. Override
with `-stun-requests-per-second`, `-stun-source-requests-per-second`, and
`-stun-max-sources` (zero uses the default; negative values disable STUN listeners
with a configuration error).

Use `-stun-udp :3478,:443` only where UDP 443 is explicitly provisioned and free of
listener conflicts. UDP 443 improves STUN reachability; it does not make peer
traffic use port 443. No UDP 443 listener or privileged bind is attempted by default.

The unified [wsrelay deployment](../scripts/deploy/wsrelay/README.md) includes STUN.
After checking a deployed node, add its real hostname, port, and known metadata
to trusted endpoint configuration. No startup directory scan or probe is required.
Real-network direct-connect improvement remains unmeasured until samples are collected.

## Native Rust host

`relay-native` now provides independent UDP STUN; Go remains the product entry
point pending the coordinated relay/client cutover. From `relay-rust/`:

```sh
cargo run --locked -p windshare-relay-native -- --database relay.sqlite --stun-udp '0.0.0.0:3478,[::]:3478' --stun-admin 127.0.0.1:8081
```

These are the STUN defaults. IPv6 sockets are IPv6-only, so both families can
bind the same port. Addresses must be numeric IP:port pairs; at most 16 distinct
listeners are accepted. Set `--stun-udp ""` to disable STUN (including admin),
or `--stun-admin ""` to disable only admin. All listener/admin bind or runtime
failures are isolated from relay traffic; invalid CLI configuration fails startup.

Each listener allows 1,000 requests/second, 20/source IP/second and 4,096 tracked
IPs per one-second window. Override with `--stun-requests-per-second`,
`--stun-source-requests-per-second` and `--stun-max-sources`; values must be positive.
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
limited, read/write-error counters and health, labelled only by `udp-N` listener ID.
Admin has 16 independent connection slots and a five-second read/write deadline;
UDP writes have a one-second deadline. Terminal read failure retires only that
listener; ICMP peer errors and failed writes leave it usable. Readiness JSON lists
bound addresses; structured lifecycle logs identify listeners and failure reasons.
Ctrl-C and Unix SIGTERM close and join all owned sockets/tasks.
