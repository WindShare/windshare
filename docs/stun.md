# STUN endpoints

WindShare retains its shipped `stun:stun.l.google.com:19302` endpoint.
The allowlist is build configuration, not a claim of availability or geographic coverage.

`connectivity/icepolicy` and `web/src/connectivity/ice-policy` accept trusted build,
deployment, or explicit local endpoint catalogs. Remote signaling and capability
links cannot supply endpoints. Entries have stable IDs, STUN URLs with explicit
ports, address family, trust, deployment priority, and optional region, provider
and failure-domain facts. Unknown facts remain empty. Enabled entries must be
explicitly allowed (`reviewed` deployment/build config or `local`);
`unreviewed` entries remain catalog-only.

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
wsrelay --database relay.sqlite --listen 0.0.0.0:8484 \
  --stun-udp '0.0.0.0:3478,[::]:3478' --stun-admin 127.0.0.1:8081
```

These are the STUN defaults. IPv6 sockets are IPv6-only, so both families can
bind the same port. Addresses must be numeric IP:port pairs; at most 16 distinct
listeners are accepted. Set `--stun-udp ""` to disable STUN (including admin),
or `--stun-admin ""` to disable only admin. Listener/admin bind and runtime
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
