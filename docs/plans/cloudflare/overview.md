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

This stage ends at publication. Published-sender disconnect grace, lifecycle
RESUME/STOP, durable-state restoration, session forwarding and both platform
adapters remain to be implemented. Construct a vacant lifecycle only after
establishing that no existing durable registration, unresolved transaction or
permanent STOP would be bypassed.

`make rust` covers formatting, Clippy, native coverage, doctests and the core Wasm
build. The registration tests use injected time and the real handshake/shared
vectors, covering competing attempts, failure/expiry cleanup, stale callbacks
and uncertain publication. See [validation](../../validation.md#rust-relay-checks)
for prerequisites and gate ownership.
