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

Its only constructor, `restore`, takes the share's durable `lifecycle::Record`,
or `None` when storage holds nothing, so no host can start an empty actor over
an existing registration or STOP. Hosts persist the kernel's fixed-layout record
bytes, keyed by relay identity and share:

| Transition | Stored record | Restores as |
| --- | --- | --- |
| Reserve, Released | none; an abort already removed the unresolved record | vacant |
| Publish transaction | `Unresolved` (exact operation), with the descriptor | fenced, returning `Reconcile` |
| Registered, before REGISTERED | `Published` (registration, resume hash, route version) | detached |
| STOP, before STOPPED (next stage) | `Stopped` (instance, stop ID); permanent | stopped |

A crash before REGISTERED's write therefore never revives a fenced attempt.
Restore needs a fresh incarnation and keeps stored operation identities and route
versions; it never derives or extends a deadline. Detached receivers retry and
fresh registrations are refused; a stopped instance is refused with `Stopped`,
and other instances of a stopped share stay occupied.

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

Published-sender disconnect grace, lifecycle RESUME/STOP, reattaching a detached
route (RESUME, or hibernation re-adoption by route version), session forwarding
and both platform adapters remain to be implemented.

`make rust` covers formatting, Clippy, native coverage, doctests and the core Wasm
build. The registration tests use injected time and the real handshake/shared
vectors, covering competing attempts, failure/expiry cleanup, stale callbacks,
uncertain publication, record layout and restoration of every stored record.
See [validation](../../validation.md#rust-relay-checks) for prerequisites and
gate ownership.
