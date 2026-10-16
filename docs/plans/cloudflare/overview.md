# Rust relay implementation

`relay-rust/` is an independent Cargo workspace. Its `windshare-relay-core` crate is
`no_std`, with no allocator, I/O or async runtime requirement. Strict sender-key
validation, control codecs and proof authentication consume the shared
`ed25519-acceptance.json` and `v2-session.json` vectors. Descriptor authorization
checks the signed digest without parsing end-to-end object contents.

`handshake::ControlHandshake` owns one connection's challenge and absolute
`UnixMillis` deadline. Fresh admission consumes a lifecycle reservation bound to
the relay, connection generation, registration and original deadline. Its
authenticated upload retains that attempt identity.

`lifecycle::ShareLifecycle` implements fresh registration:
vacant → reserved → publishing → published. Hosts feed it serialized events and
execute both the requested action and any expiration effect. Publication requests
borrow bounded descriptor bytes; the host owns any copy and all storage/socket
resources. JOIN readiness and REGISTERED follow only after the host confirms
persistence and connection preparation.

A published route has one owner connection. `OwnerDisconnected` moves it to
grace with an absolute deadline (60 s); receivers retry and fresh registrations
are refused until the sender resumes or the route lapses back to vacant. RESUME
is admitted before its challenge and bound to the ownership it observed,
including an absent route, so only an authenticated sender learns `NotFound`.
Its commit replaces a live or lost owner with the resuming connection and names
the replaced owner for cleanup. Another publication, resume or restore in
between makes it `ResumeStale`; the observed owner merely disconnecting does not.

Its only constructor, `restore`, takes the share's durable `lifecycle::Record`,
or `None` when storage holds nothing, so no host can start an empty actor over
an existing registration or STOP. Hosts persist the kernel's fixed-layout record
bytes, keyed by relay identity and share:

| Transition | Stored record | Restores as |
| --- | --- | --- |
| Reserve, Released, resume admission | none; an abort already removed the unresolved record | vacant |
| Publish transaction | `Unresolved` (exact operation), with the descriptor | fenced, returning `Reconcile` |
| Registered or Resumed, before REGISTERED | `Published` (registration, resume hash, owner connection) | published if the owner survived, else grace returning `Detached` |
| Detached | `Grace` (registration, resume hash, absolute deadline) | grace with the stored deadline |
| Lapsed | none | vacant |
| STOP intent, before cleanup | `Stopping` (instance, stop ID, operation, exact cleanup); irreversible | stopping, returning `Stop(Retire)` |
| STOP cleanup complete, before STOPPED | `Stopped` (instance, stop ID); permanent | stopped |

A crash before REGISTERED's write therefore never revives a fenced attempt.
Restore needs a fresh incarnation and keeps stored operation identities.
`surviving` lists connections that outlived the previous incarnation, such as
hibernated sockets, so the stored owner is re-adopted without a write; after a
process restart it is empty. Only a restore that finds the owner gone derives a
grace deadline, and the host persists it, so later restores never extend it.
A stopped instance is refused with `Stopped`, and other instances of a stopped
share stay occupied.

Authenticated STOP immediately fences JOIN, registration and RESUME, including
an absent route or an unfinished registration. Its explicit host steps are
`Persist → Retire → Commit`: store the stop fact and cleanup obligation, retire
the exact connection/operation and quiesce its writes, then store the final
record. Only confirmed completion of all three permits STOPPED. Failed or
uncertain work stays fenced and retries the same step; stale publication
callbacks cannot reopen it. Host writes/deletes are conditional on exact records,
so late publication or rollback cannot overwrite or remove STOP evidence.

A stopping record restores into cleanup even when old sockets survived. A
completed STOP retry acknowledges the same stop ID on the retrying connection
generation; a pending retry gets a bounded retry hint without retaining waiters.
Structured STOP observations include operation, stop ID, phase and cleanup
identity.

Receiver JOIN now carries its exact connection and a 30-second absolute admission
deadline from the handshake. `Action::Joined` creates a move-only
`sessions::Session`: awaiting receiver → awaiting sender → active → retired.
Provisional sessions forward the opaque E2E handshake in both directions;
`WS2M` from the exact sender admits only after a receiver frame. Traffic never
extends admission, and active sessions have no relay-idle timeout.

`ForwardPermit` binds relay/share/instance, wire session, actor/admission
generation, direction, protocol version and both connection generations.
`Session::check_forward` borrows the live session and lifecycle through submission;
queued frames and delayed callbacks must check again. STOP fences permits before
persistence, RESUME replaces ownership, and owner loss enters grace immediately.
Their exact-owner revocations produce session cleanup obligations; STOP executes
physical cleanup in its durable Retire step. Receiver failure or admission expiry
retires only that session. Retirement never refunds submitted bytes.

`flow::ConnectionLedger` accounts for one destination connection generation.
Granted-but-unused, received-but-unsubmitted and submitted-but-unconsumed frames
and encoded bytes share its host-configured total. Initial grants through
`Session::check_route` can use a protected startup pool and spare ordinary capacity
before any frame arrives; later grants use only ordinary capacity. FIFO receipts
and cumulative consumption release each allocation once. Forwarding checks use constant-time
slot lookup and a cached absolute cleanup deadline.

Retirement revokes unused grants immediately, retains queued payload charges until
the host confirms discard, then returns an ordered settlement notice. Submitted
occupancy survives cancellation and socket writes until consumption, exact final
settlement or confirmed physical closure. Duplicate/old feedback cannot affect
replacement slots or connection generations. Cleanup retries and partial
consumption never renew deadlines; expiry requests closure without an early refund.

Hosts own payloads, scheduling, bounded storage sizes and non-reused wire IDs.
The core traces resource transitions/refusals without retaining ciphertext.
These are kernel contracts: bidirectional `WS2B`, handshake startup capacities and
settlement wire controls still need coordinated Go/browser codecs and vectors.
Hibernated session/ledger restoration and both platform adapters also remain next;
a restored lifecycle currently invalidates previous session permits. A surviving
connection must not restart with an empty delivery ledger.

Failure or expiry before publication releases the reservation. Once host
publication starts, cancellation, expiry or an uncertain result fences the
attempt and requests reconciliation. Only confirmation that its record,
resources and outstanding writes are gone releases capacity; late success never
revives it. Operation identities include the actor incarnation, connection
generation and a non-wrapping attempt sequence. Structured observations record
transitions and refusal reasons without carrying descriptor bytes.

Go/browser clients and the Go relay already follow the host-neutral liveness
contract in the [protocol specification](../../协议规范.md): fixed-cadence
`WS2H` probes, coalescible `WS2A` replies and failure only after a full timeout
without accepted inbound frames. Both Rust adapters must keep it.


`make rust` covers formatting, Clippy, native coverage, doctests and the core Wasm
build. The lifecycle tests use injected time and the real handshake/shared
vectors, covering competing attempts, failure/expiry cleanup, stale callbacks,
uncertain publication, owner loss and lapse, resume binding, STOP commit order,
STOP/RESUME races, retries, late callbacks and restoration of every stored record.
Session/flow tests cover provisional routing, fixed admission deadlines, quiet
active sessions, startup protection, independent frame/byte bounds, FIFO receipts,
cancellation with in-flight payloads, cumulative refunds, exact-generation cleanup,
settlement deadlines and sibling isolation. They use injected time without sleeps.
See [validation](../../validation.md#rust-relay-checks) for prerequisites and
gate ownership.
