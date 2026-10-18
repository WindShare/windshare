# Rust relay implementation

`relay-rust/` is an independent Cargo workspace. `windshare-relay-core` is a
`no_std + alloc` kernel with no I/O or async runtime; native and Cloudflare
adapters consume the same `share::ShareRelay` instead of assembling lifecycle,
session and delivery rules themselves.

## Shared boundary

`ShareRelay` coordinates share authority, sessions and cross-connection queue
cleanup; `generations` owns each connection generation's admission, ledger or
conservative reservation, retirement and collection. Transitions update recovery
tracking and deadlines together, and exact records and manifest reservations stay
separate persistence projections. Platform capacity is injected; payloads,
sockets, entropy, storage and scheduling remain outside.

| Entry point | Responsibility |
| --- | --- |
| `restore(...)` | Validate connections, sessions, ledgers and pending handshakes against authoritative facts. |
| `handle(input, now)` | Expire due state, apply the input, return ordered commands, refusals and the next deadline. |
| `submit(receipt, now, handoff)` | Recheck authority and FIFO order, charge submission, hand off under one exclusive borrow. |
| `connection_usage()` | Report admitted slots, retired generations and residual frames/bytes separately. |
| `checkpoint_connection` / `checkpoint_manifest` | Capture ledger and admissions against the independent generation/session-ID fence. |
| `enable_checkpoints` / `flush_checkpoints` | Write changed records, then a changed manifest. |
| `Checkpoint::from_connections` | Validate records; missing, duplicate or invalid ones retain reservations but lose routing authority. |
| `checkpoint()` / `Checkpoint::encode()` | Full image for recovery tools and tests. |

Commands request control sends, entropy, discard, closure and serialized storage
work; hosts return evidence in order and never cache authorization across socket
waits. Native hosts disable checkpoint tracking. Forwarding never writes counters
to a database; the
[checkpoint experiment](../../../spikes/relay-hosts/checkpoint-recovery.md) uses
a per-event SQLite mirror only as a control.

Go REGISTER/RESUME/JOIN/STOP and browser JOIN derive a per-share dial URL with a
public `share_id` hint validated against handshake identity; Cloudflare also
binds the Durable Object and stored endpoint. `observation::Observer` is the
single observation interface.

## Authority and durability

Strict sender-key validation and control authentication share `ed25519-acceptance.json`
and `v2-session.json` vectors; descriptor authorization checks the signed digest
without parsing E2E contents. Publication and RESUME stay `Activating` until the
exact ownership commit is confirmed; only then may the lifecycle permit
REGISTERED, JOIN and forwarding. Owner loss enters a fixed grace, and RESUME
binds to the ownership observed before its challenge. STOP revokes forwarding
immediately, persists intent after earlier writes, waits for unsubmitted-queue
discard across all generations, then commits before acknowledging; `STOPPED`
never releases capacity.

REGISTER atomically creates ownership and descriptor in one SQL write, with
conditional replacements serialized behind it. Uncertain work keeps its exact
obligation and stale completions cannot advance it; an already-applied write
succeeds without overwriting another owner. Committed ownership survives a lost
receipt by re-adopting the owner or entering grace. Durable records hold
ownership, grace and stopped facts; pending activation stays runtime state.

## Delivery and recovery

Each ledger enforces its immutable declaration, host policy and FIFO submission.
JOIN allocates a non-reused wire ID and reserves both directions before granting
either. Provisional sessions carry the E2E handshake under an absolute admission
deadline; active sessions have no relay-idle timeout. Submitted occupancy
survives cancellation until consumption, settlement or confirmed physical
closure, and failed sends do not refund. The
[delivery contract](delivery-flow-contract.md) is active in the Go `relayv2`
runtime and browser receiver: declaration and local buffers share one capacity, both roles await
explicit grants, sparse consumption wakes the ordered writer immediately, and
settlement follows queue release.
Smaller declarations preserve startup
capacity within their effective host/client bound, including after restore.

Retirement revokes unused credit and requests queue discard before ordered
settlement: `WS2N` freezes submitted totals and `WS2S` confirms consumption or
discard. Cleanup deadlines never renew, and settling one receiver does not close
its sender or siblings. Lifecycle, handshake, liveness, session and settlement
deadlines share one indexed schedule. Hosts inject `RetirementPolicy`:
`PhysicalRelease` waits for `Closed` and discard of queues targeting other
connections. `Bounded` fixes admission grace and risk expiry at first loss.
Revocation and both queue discards precede either exit; risk expiry removes only
the lost destination's reservation, without claiming consumption or buffer release.

Clients send increasing `WS2H` nonces; the relay answers with only
`Control::ProbeAck` (`WS2A`), outside content credit and coalescible to the newest
nonce. Accepted controls and content refresh a connection's 45-second silence
deadline; silence revokes routing and requests closure without claiming physical
release and never renews other deadlines. Hosts check `Turn::deadline` before new
inputs and synchronous submission.

Recovery separates ownership/STOP authority, sockets and delivery responsibility;
unknown authority or inventory refuses service. Missing delivery records isolate
affected connections and sessions; healthy ledgers keep grants, debt and absolute
deadlines, and neither restart nor an empty socket list proves release.
`Limits::business_connections` bounds business admissions through unfinished
retirement; new sockets enter a rate-limited control budget first, and STOP keeps
its control slot and proof until acknowledgement. A full risk pool temporarily
blocks retirement/admission; original expiries automatically free capacity and retry
blocked generations before new admission. Healthy senders retain reverse-flow debt.
Recovery preserves deadlines, totals, wire-ID high-water marks, fixed grace and
pending ownership; fresh incarnations fence old callbacks.

Control records hold the handshake and control deadline; business records hold
the ledger and admissions; established connections retain the probe nonce, last
inbound time and silence deadline. The independent `CheckpointManifest` holds
generation identity, retirement state, absolute risk expiry, reservation, attempt-rate history and
the session-ID high-water mark. Missing control records close only their sockets;
missing business or retired records become explicit uncertain reservations (full
budget or the submission bound), never fabricated counters; old feedback cannot
release them. Physical release or quiescent policy expiry can end these reservations.

Hibernating hosts enable tracking after restore and flush after outputs, writing
the manifest last and retaining partial batches for retry. They omit unavailable
records rather than replay stale credits, and persist generation allocation
before accepting a socket; removed IDs are never reused. `CheckpointBudget`
bounds connection records including retained flows; checkpoints encode only
retained flows and stable indices. See the
[Cloudflare attachment budget](cloudflare-relay-rust-plan.md#attachment-budget).

## Native host

[`relay-native`](../../../relay-rust/crates/relay-native/README.md) executes
REGISTER, RESUME, JOIN, bidirectional opaque delivery, cumulative consumption,
settlement, STOP and heartbeats on real WebSockets. Per-share actors consume
`ShareRelay` commands; OS entropy, monotonic time, bounded socket writers and
SQLite transactions supply host evidence. An exclusive database lease and
persisted generation prevent overlapping hosts and reused lifetimes; startup
recovers committed ownership and interrupted STOP. Confirmed authority supplies
the sealed JOIN descriptor without another SQL read. Native scheduling replenishes
windows from kernel occupancy and defers grants until control-writer space is available.
The real Go-client process scenario covers asymmetric windows, sparse feedback,
slow-receiver isolation, cancelled-queue settlement, STOP with outstanding delivery
and restart/RESUME. Native HTTP ingress now serves root
`GET/HEAD /healthz` and validates configurable WebSocket origins before relay admission.
Bounded HTTP reads/writes use separate capacity; upgrades preserve pre-read bytes and
existing connection teardown. Independent UDP STUN now supplies IPv4/IPv6 Binding,
bounded per-listener/source rates, fault-isolated health/metrics and joined teardown.
The native process scenario verifies responses with Pion across restarts.
Global and per-source connection budgets now share atomic admission and teardown;
separate control budgets preserve STOP under business saturation. Explicit proxy trust
resolves source IPs without accepting arbitrary forwarding headers.
Go process E2E and browser product fixtures now use Go `wind` and Rust
`relay-native`, covering reconnect, permanent STOP, receiver-crash recovery and
the 17 MiB browser download/hash scenario. Remaining operational interfaces,
default server/release entry points and `integration/` suites have not been switched.

## Cloudflare host

[`relay-cloudflare`](../../../relay-rust/crates/relay-cloudflare/README.md) runs
the same lifecycle and bidirectional delivery on a Worker and SQLite Durable Object.
JOIN reads the descriptor from confirmed authority; event-owned payloads use synchronous
kernel submission, and consumption replenishes windows from restored kernel occupancy.
The Worker routes by
`share_id` and gates fresh registration with a deployment credential; RESUME and
STOP authenticate independently. `authority::apply` shares atomic row transitions
between SQL hosts. Typed attachments preserve pending handshakes, delivery and liveness;
the manifest and persisted allocator fence lost generations, and alarms coalesce
without per-frame reads or extending deadlines. Existing identities require the manifest
even without authority or sockets; only first creation establishes empty responsibility.
Ordered cleanup completes settlement sends before subsequent closure.
The host selects immediate risk expiry after quiescence; a reconnect-rate budget
remains enforced. This is an admission policy, not a physical buffer TTL. The
[live Preview run](../../../relay-rust/crates/relay-cloudflare/tests/preview-evidence.md)
passed bidirectional transfers beyond asymmetric windows, duplicate feedback, hibernation
with unconsumed debt, fault RESUME, ordered STOP and Alarm expiry.
Real Go-client and browser validation against Cloudflare remain pending.

## Host evidence

[Host probes](../../../spikes/relay-hosts/README.md) exercise the real kernel with
native writers and a Rust Worker; native slow-writer isolation and STOP cleanup
passed on Windows. Cloudflare reset is not a buffer-release receipt: after reset
an old peer still received a complete 8 MiB burst while a healthy peer
disconnected. The
[checkpoint follow-up](../../../spikes/relay-hosts/checkpoint-recovery.md) shows
one receiver closing and rejoining while its sibling keeps downloading; replayed
settlement keeps loss reservations and forwarding writes no SQL. Hibernation
retains exact ledgers, but abrupt reset disconnects the whole share. These
fixtures do not establish the product flow.

## Testing

`make rust` runs formatting, Clippy, native coverage, doctests and the core and
Cloudflare Wasm builds plus Wasm Clippy. Aggregate scenarios cover durability barriers, STOP with
outstanding delivery, duplicate STOP, stopped recovery, late callbacks, FIFO
handoff, uncertain results, stale callbacks, cancellation, capacity rollback and
hibernated recovery with injected time. See
[validation](../../validation.md#rust-relay-checks).
