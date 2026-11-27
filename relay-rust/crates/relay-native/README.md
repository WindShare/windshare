# Native relay host

Run `wsrelay --config node.json`. Copy the [example](../../../deploy/wsrelay/node.example.json),
set the public WebSocket endpoint and browser origins, and terminate TLS
at a reverse proxy. Relative database and identity paths resolve beside the config.
A wildcard listen address requires an explicit endpoint. Copy `relay_url` from
the startup JSON or `/.well-known/windshare` into `wind relay add <url>`.
On HTTPS deployments, open the one-time initialization link, set the admin password
and approve the device code. Guests can use `wind relay add <url> --anonymous`.
For headless HTTP deployments, the first start prints the owner access key.

Back up the Ed25519 identity, relay SQLite database and its sibling `.console.sqlite`
using a consistent SQLite backup. The console stores sessions, per-device installation
receipts and revocations. Owner-approved devices share owner priority. Devices approved
through a service account use that account's allowance and receive a key scoped to this node. The console can issue a service key for a separate principal. Revoking a key
also stops its active shares on this node. One host
owns each database; its endpoint identity and permanent STOP history cannot be reset
by changing configuration. JSON readiness goes to stdout; traces go to stderr.
`RUST_LOG=debug` includes kernel decisions.

## Configuration

- `listen`, `endpoint`, `database`, `identity_file`: listener and durable identity.
- `origins`: `{"kind":"native"}`, `{"kind":"public"}`, or an `allowlist` with
  exact HTTP(S) `origins` and optional `allow_localhost`. Origin policy is separate
  from sender authorization.
- `proxy`: optional `{"peers":["127.0.0.1","::1"],"header":"CF-Connecting-IP"}`.
  Only these socket peers may supply one source IP; malformed trusted headers fail.
- `connections`: `business` (256), `controls` (32), `business_per_source` (128),
  `controls_per_source` (8). Public load leaves owner capacity; anonymous admission
  is refused before authenticated admission. STOP has separate control capacity.
- `stun`: `udp_addresses`, optional `admin_address`, and `limits`;
  see [STUN](../../../docs/stun.md).
- `node` sets services, locality hints, STUN endpoints, optional monthly billing
  budget and up to 16 untrusted `bootstrap_nodes`. The monthly budget counts all
  WebSocket egress on the UTC billing cycle. Anonymous admission defaults off
  for `cn` and on for `global`.
- Self-hosted contribution lives in SQLite. Edit it in the native console.
  The defaults allow 10 GiB per UTC day, 25 Mbps and 64 public business connections.
  `node.ceiling` can bound `daily_bytes`, `bytes_per_second` and `connections`.
  Owner traffic bypasses public pacing and the public daily budget.
- `node.access`: local credentials/owner principals, trusted issuer certificates,
  `shares_per_principal` and `owner_reserved_shares`.
- `node.authority`: official usage endpoint and issuer; see
  [Authority setup](../network-authority/README.md). Local owners require no authority.

Node settings reload from the same file. Listener, storage, identity and reporting
authority changes require restart. Unknown fields and invalid policies fail validation.

## Runtime

`/.well-known/windshare` publishes the five-minute signed node descriptor. `/discovery/v1/peers` exchanges separately signed, one-day gossip records. `/healthz` reports
HTTP liveness independently of relay capacity; UDP STUN has its own private health
endpoint. Ctrl-C or SIGTERM joins relay, reporter and STUN teardown.

A node-wide governor gives owners strict priority and shares remaining capacity
across public classes (weighted toward standard so lowest keeps a small
signaling/progress share without starving), then fairly across principals and
sessions. Idle capacity is shared. Bounded flow queues preserve order; scheduling
and writer capacity are obtained before synchronous forwarding authorization.
Bandwidth-pressure controls let clients back off without losing downloaded progress.

Actual forwarded account bytes are split by UTC day. Durable signed minute batches
retry idempotently, independently of zero-byte entitlement refreshes. Expired or spent
allowance lowers scheduling priority without dropping account attribution. Blocks and
credential revocations independently stop registration, join, resume and active
forwarding. Local owner credentials remain independent of official account policy.

REGISTER acknowledgements follow committed SQLite receipts. STOP retains its permanent
intent through cleanup; process restart recovers committed ownership and grace.
Schema version 5 rejects older databases. Unsubmitted frames are discarded on closure;
already submitted bytes may still arrive. Slow socket writes cannot block share actors.

`make rust` covers protocol, persistence, fair scheduling, admission, usage, HTTP/STUN,
fault-injected teardown and Go-client process interop. From `relay-rust/`, run
`cargo test -p windshare-relay-native tcp_contention -- --ignored --nocapture`
to measure public egress with an idle, bulk or receiver-limited owner.
