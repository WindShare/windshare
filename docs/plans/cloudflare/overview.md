# Rust relay implementation

`relay-rust/` is an independent Cargo workspace. `windshare-relay-core` is a
`no_std + alloc` kernel with no I/O or async runtime; native and Cloudflare
adapters consume the same `share::ShareRelay` instead of assembling lifecycle,
session and delivery rules themselves.

## Shared boundary

One `ShareRelay` coordinates share authority, sessions and cross-connection queue
cleanup. Its `generations` module owns each connection generation's admission,
exact ledger or conservative reservation, retirement and collection. Transitions
update recovery tracking and deadlines together; ledger edits also track retired
risk changes. Exact records and manifest reservations remain separate persistence
projections. Platform capacity is injected; payloads, sockets, entropy, storage
and scheduling remain outside.

| Entry point | Responsibility |
| --- | --- |
| `restore(...)` | Validate surviving connections, sessions, ledgers and pending handshakes against authoritative facts. |
| `handle(input, now)` | Expire due state, apply the input, return ordered commands, refusals and the next deadline. |
| `submit(receipt, now, handoff)` | Recheck authority and FIFO order, charge submission, hand off the payload under one exclusive borrow. |
| `connection_usage()` | Report admitted slots, retired generations and residual frames/bytes separately. |
| `checkpoint_connection` / `checkpoint_manifest` | Capture a generation's ledger and admissions vs. the independent generation/session-ID fence. |
| `enable_checkpoints` / `flush_checkpoints` | Write changed records, then a changed manifest. |
| `Checkpoint::from_connections` | Validate records; missing, duplicate or invalid ones retain reservations but lose routing authority. |
| `checkpoint()` / `Checkpoint::encode()` | Full image for recovery tools and tests. |

Commands request control sends, entropy, discard, exact closure and serialized
storage work; hosts return evidence as inputs in order, echo work IDs, and never
cache authorization across socket readiness waits. Native hosts leave checkpoint
tracking disabled. Forwarding never writes counters to a database; the
[checkpoint experiment](../../../spikes/relay-hosts/checkpoint-recovery.md) uses
a per-event SQLite mirror only as a control.

Go REGISTER/RESUME/JOIN/STOP and browser JOIN derive a per-share dial URL with a
public `share_id` hint validated against handshake identity; Cloudflare also
binds the Durable Object and stored endpoint. `observation::Observer` is the
single observation interface.

## Authority and durability

Strict sender-key validation and control authentication use shared
`ed25519-acceptance.json` and `v2-session.json` vectors; descriptor
authorization checks the signed digest without parsing E2E contents.

Publication and RESUME stay `Activating` until the exact ownership commit is
confirmed; only then may the lifecycle permit REGISTERED, JOIN and forwarding.
Owner loss enters a fixed grace, and RESUME binds to the ownership observed
before its challenge. STOP revokes forwarding immediately, persists intent after
earlier writes, waits for unsubmitted-queue discard across all generations, then
commits before acknowledging; `STOPPED` never releases capacity.

REGISTER atomically creates ownership and descriptor in one SQL write, with
conditional replacements serialized behind it. Uncertain work retains its exact
obligation, stale completions cannot advance it, and an already-applied write
succeeds without overwriting another owner. Committed ownership survives a lost
receipt by re-adopting the surviving owner or entering grace. Durable records
hold ownership, grace, stopping and permanent stopped facts; pending activation
stays runtime state.

## Delivery and recovery

Each ledger enforces its immutable declaration, injected host policy and FIFO
submission. JOIN allocates a non-reused wire ID and reserves both directions
before issuing either grant. Provisional sessions carry the E2E handshake under
an absolute admission deadline; active sessions have no relay-idle timeout.
Submitted occupancy survives cancellation until consumption, settlement or
confirmed physical closure; failed sends do not refund. The
[delivery contract](delivery-flow-contract.md) stages the codecs separately from
the current Go/browser runtime; socket-free receive ledgers already exist on both
clients, and runtime integration awaits the coordinated cutover.

Retirement revokes unused credit and requests queue discard before ordered
settlement: `WS2N` freezes submitted totals and `WS2S` confirms consumption or
discard. Cleanup deadlines never renew, and settling one receiver does not close
its shared sender or siblings. Lifecycle, handshake, liveness, session and
settlement deadlines share one indexed schedule. Hosts inject
`RetirementPolicy`: `PhysicalRelease` waits for `Closed` and discard of queues
targeting other connections; `Bounded` permits logical retirement after
revocation, both queue discards and a fixed grace (possibly zero).

Clients send increasing `WS2H` nonces; the relay emits only `Control::ProbeAck`
(`WS2A`), prioritized outside content credit and coalescible to the newest nonce.
Only accepted inbound controls and content refresh a connection's 45-second
silence deadline; silence revokes routing and requests closure without claiming
physical release, never renewing handshake, admission, settlement or recovery
grace. Hosts check `Turn::deadline` before new inputs and synchronous submission.

Recovery separates ownership/STOP authority, communicating sockets and delivery
responsibility; unknown authority or inventory refuses service. Missing delivery
records isolate their connections and dependent sessions, while healthy ledgers
keep grants, submitted debt and absolute deadlines; restart or an empty socket
list never proves release. `Limits::business_connections` bounds
REGISTER/RESUME/JOIN through unfinished retirement; new sockets first enter a
rate-limited control budget, and only valid business initiation allocates a
ledger. STOP keeps its control slot and proof and closes after acknowledgement.
Exhausted retired generations stay admitted without interrupting healthy peers
until exact settlement or physical release retries them; retained flows occupy
their own bounded slots so a healthy sender keeps its own reverse-flow debt. Risk
exhaustion blocks further retirement/admission without revoking healthy sessions.
Original deadlines, totals and wire-ID high-water marks survive recovery, and
repeated recovery preserves fixed grace and loss status. Pending handshakes keep
their original ownership, while fresh incarnations fence old storage and entropy
callbacks.

Control records hold the handshake and original control deadline; business
records hold the ledger and receiver-side admissions; established connections
also retain last probe nonce, last accepted inbound time and the absolute silence
deadline. The independent `CheckpointManifest` holds generation identity,
retirement state, a risk reservation, attempt-rate history and the session-ID
high-water mark. Missing control records close only their sockets; missing
business or retired records become explicit uncertain reservations (full
configured budget or the submission bound at retirement), never fabricated
counters; old feedback cannot release them, but physical release can.

Hibernating hosts enable tracking after restore and flush after outputs, writing
a changed manifest last and retaining partial batches for retry. They omit
unavailable records instead of replaying stale credits, and persist generation
allocation before accepting a new socket; removed IDs are never reused.
`CheckpointBudget` bounds connection records including retained flows. Flow
metadata uses lazily allocated, runtime-bounded slots: handshakes allocate none,
receiver connections allocate for their own flows, and checkpoints encode only
retained flows and stable indices. See the
[Cloudflare attachment budget](cloudflare-relay-rust-plan.md#attachment-budget).

## Native lifecycle host

[`relay-native`](../../../relay-rust/crates/relay-native/README.md) executes
REGISTER, RESUME, STOP and heartbeat controls on real WebSockets. Per-share
actors consume `ShareRelay` commands; OS entropy, monotonic time, bounded socket
writers and SQLite transactions supply host evidence, with ownership and
permanent STOP acknowledgements behind kernel commit barriers. An exclusive
database lease and persisted connection generation prevent overlapping hosts and
reused lifetimes; startup recovers committed ownership and interrupted STOP, and
a Go-codec scenario verifies recovery and stopped refusal across restarts.

The connection layer owns reader/writer tasks, frame/byte-bounded queues and a
synchronous kernel-to-writer handoff; unsubmitted queues are discarded before
receipts, and transport release follows joined task cancellation and abortive TCP
teardown. A supervisor tracks actor, connection and storage descendants,
retaining registry slots until resources are destroyed and keeping old receipts
bound to old mailboxes. JOIN, forwarding, STUN, operational endpoints and client
integration remain pending; Go is still the product entry point.

## Cloudflare lifecycle host

[`relay-cloudflare`](../../../relay-rust/crates/relay-cloudflare/README.md) runs
the same lifecycle on a Worker and SQLite Durable Object. The Worker routes by
`share_id` and gates fresh registration with a deployment credential; RESUME and
STOP authenticate independently. `authority::apply` supplies the same atomic row
transitions to both SQL hosts. Typed binary attachments preserve pending
handshakes and liveness; the manifest and persisted allocator fence lost
generations, and alarms coalesce without extending deadlines. The lifecycle-only
schema has never acquired payload debt; enabling delivery requires replacing that
recovery assumption. The
[live Preview run](../../../relay-rust/crates/relay-cloudflare/tests/preview-evidence.md)
passed signatures, hibernation, fault RESUME, permanent STOP and Alarm expiry.
JOIN/forwarding and the runtime cutover remain pending.

## Host evidence

The [bounded host probes](../../../spikes/relay-hosts/README.md) exercise the real
kernel with native writers and a Rust Worker in Cloudflare Previews. Native
slow-writer isolation and STOP cleanup passed on Windows. A Cloudflare control
repro showed that after reset an old peer with an 8 KiB receive buffer still
received a complete 8 MiB burst while a healthy peer disconnected, so reset is
not a verified buffer-release receipt or per-receiver isolation policy. The
[checkpoint follow-up](../../../spikes/relay-hosts/checkpoint-recovery.md)
verifies one sender and two receivers: losing one attachment closes only that
receiver, which rejoins with a new generation/session while its sibling keeps
downloading; replayed settlement keeps loss reservations and forwarding writes
no SQL. Normal hibernation retains exact ledgers; abrupt reset still disconnects
the whole share. These fixture transfers do not establish the product flow.

## Testing

`make rust` runs formatting, Clippy, native coverage, doctests and the core and
Cloudflare Wasm builds plus Wasm Clippy. Aggregate scenarios cover durability
barriers, STOP with outstanding delivery, duplicate STOP, stopped recovery, late
callbacks, FIFO handoff, uncertain results, stale callbacks, cancellation,
capacity rollback and hibernated recovery with injected time and no sleeps. See
[validation](../../validation.md#rust-relay-checks) for prerequisites.
