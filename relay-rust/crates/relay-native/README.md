# Native relay host

Run `wsrelay --config node.json`. Copy the [example](../../../deploy/wsrelay/node.example.json),
set the public WebSocket endpoint, browser origins and capacity, and terminate TLS
at a reverse proxy. Relative database and identity paths resolve beside the config.
A wildcard listen address requires an explicit endpoint. Copy `relay_url` from
the startup JSON or `/.well-known/windshare` into `wind relay onboard <url>`.
Use the one-time administrator URL printed at startup to set a password. Confirm
the device name and code at the approval URL. Public guests can use `wind relay add`.

The first start creates a durable Ed25519 node identity and prints its owner access
key. Back up the identity, relay SQLite database and its sibling `.console.sqlite`
using a consistent SQLite backup. The console stores sessions, per-device installation
receipts and revocations. Owner-approved devices share owner priority. Devices approved
through a service account use that account?s allowance and receive a key scoped to this node. The console can issue a service key for a separate principal. Revoking a key
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
- `node`: enabled `services`, `zone`, `region`, `stun_urls`,
  `capacity_bytes_per_second`, optional `relay_budget`, and `access`.
  Anonymous admission defaults off for `cn`, on for `global`.
- `node.access`: local credentials/owner principals, trusted issuer certificates,
  `shares_per_principal` and `owner_reserved_shares`.
- `node.authority`: official usage endpoint and issuer; see
  [Authority setup](../network-authority/README.md). Local owners require no authority.

Node settings reload from the same file. Listener, storage, identity and reporting
authority changes require restart. Unknown fields and invalid policies fail validation.

## Runtime

`/.well-known/windshare` publishes the signed v2 node descriptor. `/healthz` reports
HTTP liveness independently of relay capacity; UDP STUN has its own private health
endpoint. Ctrl-C or SIGTERM joins relay, reporter and STUN teardown.

A node-wide governor schedules owner, standard and lowest classes, then principals
and sessions. Idle capacity is shared. Bounded flow queues preserve order; scheduling
and writer capacity are obtained before synchronous forwarding authorization.
Bandwidth-pressure controls let clients back off without losing downloaded progress.

Actual forwarded account bytes are split by UTC day. Durable signed minute batches
retry idempotently, independently of zero-byte entitlement refreshes. Expired or spent
allowance lowers scheduling priority without dropping account attribution. Blocks and
credential revocations independently stop registration, join, resume and active
forwarding. Local owner credentials remain independent of official account policy.

REGISTER acknowledgements follow committed SQLite receipts. STOP retains its permanent
intent through cleanup; process restart recovers committed ownership and grace.
Schema version 4 rejects older databases. Unsubmitted frames are discarded on closure;
already submitted bytes may still arrive. Slow socket writes cannot block share actors.

`make rust` covers protocol, persistence, fair scheduling, admission, usage, HTTP/STUN,
fault-injected teardown and Go-client process interop.
