# Native relay host

This host runs REGISTER/RESUME, JOIN, bidirectional opaque forwarding, cumulative
consumption, settlement, independent STOP and heartbeats through `ShareRelay` on
real WebSockets. It requires the capacity-bearing handshake and cumulative feedback;
production Go/browser clients still await the coordinated cutover. The Go relay remains
the product server until client integration and remaining operational interfaces are ready.

From `relay-rust/`:

```sh
cargo run --locked -p windshare-relay-native --bin relay-native -- --database relay.sqlite --listen 127.0.0.1:8787
```

For a reverse proxy, add `--public-endpoint wss://relay.example/v2/ws` (a canonical
full WebSocket URL, including any base path). TLS terminates at that proxy.
The database is bound to this endpoint identity and exclusively leased by one host.
Keep the SQLite database and its WAL together; deleting storage deletes STOP history.
The parent directory must exist. JSON readiness goes to stdout; structured traces
go to stderr, with kernel decisions enabled by `RUST_LOG=debug`.

`GET /healthz` returns `200 ok`; `HEAD /healthz` returns the same status without a
body. This is HTTP liveness, independent of share capacity, at the root path even
when the public WebSocket endpoint has a base path. Unknown paths return 404 and
unsupported methods return 405. HTTP bodies are rejected; ordinary replies close
the connection. HTTP ingress has 64 concurrent slots, a 5-second read/write deadline,
and a 16 KiB / 64-header limit, separate from relay admission.

Independent UDP STUN Binding listeners default to `0.0.0.0:3478` and `[::]:3478`;
their private health/metrics HTTP listener defaults to `127.0.0.1:8081`.
UDP/admin failures leave relay traffic available. Ctrl-C (or SIGTERM on Unix)
joins STUN and relay teardown. See [STUN configuration](../../../docs/stun.md#native-rust-host)
for flags, protocol boundaries, limits and health semantics.

Browser WebSocket access requires `--origins https://app.example,https://other.example`.
Entries are full HTTP(S) origins without paths, credentials, queries or fragments;
host/scheme case and default ports are normalized. Invalid entries fail startup.
Missing Origin is allowed for native clients; empty, `null`, repeated and unlisted
origins are rejected with 403. Origin is browser policy, not client authentication.
`--allow-localhost` additionally allows HTTP(S) origins on `localhost`, `127.0.0.1`
and `[::1]` at any port for development; it is off by default. Neither Host nor
proxy forwarding headers grant origin access.

REGISTER creates ownership and descriptor in one SQLite transaction. The kernel
permits REGISTERED only after that exact write is confirmed; ambiguous results retry
the same write. STOP follows earlier writes, retains its permanent intent through
cleanup and commits the final fact before acknowledgement. Startup recovers committed
ownership even if its receipt was lost, enters grace when the owner is gone and
preserves already-persisted grace deadlines. Schema version 2 rejects older databases.
Generation allocation is durable; challenge entropy comes from the OS.
JOIN reads its sealed descriptor from the confirmed authority cached by the actor,
including after RESUME. Opaque forwarding and consumption never perform SQL.

Validated WebSocket upgrades reserve global and source control capacity before the
101 response; saturation returns 503. REGISTER/RESUME/JOIN initiations atomically
exchange both control charges for business charges. STOP keeps its independent
control capacity when global or source business capacity is full. Each share also
keeps 32 business and 8 control slots. New connections remain rate-limited.

| Connection option | Default |
| --- | --- |
| `--max-business-connections` | 256 |
| `--max-control-connections` | 32 |
| `--max-business-connections-per-source` | 128 |
| `--max-control-connections-per-source` | 8 |

Limits must be positive, with each source limit no larger than its global limit.
Sources are socket IPs by default, without ports; IPv4-mapped IPv6 uses the same
budget as IPv4. Users sharing a NAT share these budgets. Forwarded headers are
ignored unless **both** `--trusted-proxies <comma-separated proxy IPs>` and
`--source-ip-header <header-name>` are configured. Only exact matching socket peers
may supply that header; it must contain exactly one IP, without a port or list.
Missing, repeated or invalid trusted headers return 400, without fallback to the
proxy IP. Other peers still use their socket IP. Health checks require no source header.

For a local Cloudflare Tunnel connector, for example, add:

```sh
--trusted-proxies 127.0.0.1,::1 --source-ip-header CF-Connecting-IP
```

Trust only connector addresses under your control and restrict origin access to
that connector. A general reverse proxy must overwrite the configured header;
client-supplied forwarding chains are not parsed. With Cloudflare, keep Pseudo IPv4
out of `Overwrite Headers` mode so
[`CF-Connecting-IP`](https://developers.cloudflare.com/fundamentals/reference/http-headers/#cf-connecting-ip)
retains the native client address. Worker subrequests can change its meaning.

Health checks and rejected HTTP requests do not spend relay attempt tokens or
allocate share actors. Upgrades transfer the transport and pre-read bytes to the
connection supervisor, releasing the HTTP slot. Supervisor, reader and writer retain
both relay charges until the last resource owner is destroyed; idle source entries
are then removed. Failed admission never partially charges either budget, and closing
does not refund attempt tokens. Structured `connection_admission_refused` traces
include connection identity, source IP and distinct global/source/control/business
or attempt-rate reasons; `connection_policy` records startup limits and proxy trust.

Per-share actors serialize decisions; each connection supervisor owns separate reader
and writer tasks. Slow writes cannot block inbound controls. Output has independent
frame/byte bounds, reserved control capacity and coalesced priority heartbeat replies.
Unsubmitted payloads stay with the actor. The connection foundation rechecks a receipt
through `ShareRelay::submit` and hands it to the bounded writer synchronously; those
bytes are then submitted responsibility and may still arrive after STOP.
Native scheduling maintains a 16-frame window, bounded by the destination declaration
and available ordinary capacity. It reads kernel occupancy instead of duplicating
cumulative counters, rotates flows sharing a destination, and defers grants until
their control writer has space. Consumption replay cannot replenish twice.

Queue discard precedes its kernel receipt; the resulting settlement enters the ordered
writer before closure. Closure aborts both tasks, or gives ordered
terminal replies and the WebSocket close handshake one bounded interval; shutdown
interrupts that drain.
Only joined tasks, dropped queues and abortive TCP teardown justify `Closed`;
shutdown waits until the share actor has processed that receipt.
An independent generation supervisor tracks the actor, connection supervisors,
reader/writer descendants and blocking storage work. Failure, cancellation and idle
eviction retain the registry slot until every tracked task has destroyed its resources;
a closed mailbox alone never permits reconstruction. Each generation has its own mailbox,
so delayed old receipts cannot enter the replacement.

Recovery evidence depends on the boundary:

- A first share requires an authoritative absent record and an empty host inventory.
- Same-host reconstruction requires the old generation's completed cleanup barrier,
  including storage work, before reading authority again.
- Host/process restart requires the exclusive database lease, held by every tracked
  resource owner until teardown, and a new durable connection generation. Native
  transports use abortive TCP close and retain no buffers in an external host.

Previously submitted network bytes may still arrive; release does not mean delivery
or recall. These native ownership guarantees do not apply to Cloudflare buffers.

`make rust` covers STUN Binding with Pion on IPv4/IPv6, malformed/oversized datagrams,
virtual-time limits and I/O deadlines, listener/admin fault isolation and socket release.
It also covers HTTP routing, Origin policy, bounded headers, virtual-time HTTP
read/write deadlines, source isolation, trusted-header validation, admission rollback,
STOP under full business budgets and upgrade handoff, plus blocked-write ingress,
bounded queues, STOP submission fencing,
joined teardown, held writer destruction, actor reconstruction, stale-receipt isolation,
cancelled-supervisor lease retention and fault-injected commit ordering. The existing
Go-codec process scenario transfers 80 mixed-size frames per direction beyond asymmetric
receive windows, replays consumption, leaves submitted debt outstanding for STOP, and
verifies ordered settlement. It also kills/restarts the binary, resumes and transfers
again, then verifies that another restart cannot republish or resume a stopped share.
