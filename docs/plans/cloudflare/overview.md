# Rust relay implementation

`relay-rust/` is an independent Cargo workspace. `windshare-relay-core` uses
`no_std + alloc`, with no I/O or async runtime dependency. Native and Cloudflare
adapters consume the same `share::ShareRelay`; they do not assemble lifecycle,
session and delivery rules themselves.

## Shared boundary

One `ShareRelay` owns a share's handshakes, lifecycle, connections, sessions,
wire-session IDs, delivery ledgers and deadline index. Platform capacity is
injected; payloads, sockets, entropy, storage and scheduling remain outside it.

| Entry point | Responsibility |
| --- | --- |
| `restore(identity, limits, state, observer, now)` | Read authoritative lifecycle facts and validate surviving connections, sessions, ledgers and pending handshakes together. |
| `handle(input, now)` | Expire due state, apply the input and return ordered commands, any refusal and the next absolute deadline. |
| `submit(receipt, now, handoff)` | Recheck current authority and FIFO order, charge submission and synchronously hand off the payload under one exclusive borrow. |
| `checkpoint_connection(connection)` | Capture one connection's delivery ledger, handshake and receiver-owned admissions. |
| `enable_checkpoints(budget, now)` / `flush_checkpoints(now, write)` | Validate the host's byte allowance, track changed connections and synchronously replace their attachments at event boundaries. |
| `Checkpoint::from_connections(...)` | Assemble independently decoded attachments for the same kernel recovery validation. |
| `checkpoint()` / `Checkpoint::encode()` | Capture a full image for recovery tools and tests. |

Commands request control sends, entropy, payload discard, exact connection
closure and serialized storage work. Hosts return evidence as inputs, including
discard and physical-close confirmations. They preserve ordering on each
connection and report storage results with the issued work ID. Socket readiness
waits happen before `submit`; its handoff cannot await or cache authorization.
Native hosts leave attachment tracking disabled and allocate no change journal. Neither host writes delivery counters to a database.

`observation::Observer` is the single observation interface for lifecycle,
sessions, delivery and orchestration. Typed decisions carry operation/session
identity and context; adapters add platform fields when exporting them.

## Authority and durability

Strict sender-key validation and control authentication use the shared
`ed25519-acceptance.json` and `v2-session.json` vectors. Descriptor authorization
checks the signed digest without parsing end-to-end object contents.

Publication and RESUME enter `Activating` until the exact ownership commit is
confirmed. The lifecycle alone then permits REGISTERED, JOIN and forwarding;
the aggregate executes writes and retries without a second readiness state.
Owner loss enters a fixed grace; RESUME binds to the ownership observed before
its challenge and replaces only that ownership. STOP immediately revokes
forwarding, persists its intent, waits for exact participant closure and
publication quiescence, then commits before acknowledging. The aggregate owns
revocation fanout and ordered I/O; the lifecycle enforces the original activation
deadline and rejects completions superseded by disconnect, expiry or STOP.

Storage commands serialize publication, conditional record replacement and
operation-scoped cleanup. Uncertain work stays fenced and retries the exact
obligation; stale completion generations cannot advance it. Conditional writes
must treat an already-applied target as success and never overwrite another
record. Durable records retain unresolved publication, published ownership,
grace, stopping or permanent stopped facts.

## Delivery and recovery

REGISTER, RESUME and JOIN bind immutable receive capacity to the connection;
STOP remains independent. Each ledger checks both that declaration and injected
host policy. The [delivery contract](delivery-flow-contract.md) stages these
codecs separately from the current Go/browser runtime. Settlement codecs and the
Rust wire-to-ledger path are implemented.

JOIN allocates a non-reused wire ID and reserves both directions before issuing
either grant. Provisional sessions carry the E2E handshake under an absolute
admission deadline; active sessions have no relay-idle timeout. Ledgers bound
both frames and bytes, protect startup capacity, enforce FIFO submission and
retain submitted occupancy through cancellation. Refunds require consumption,
final settlement or confirmed physical closure; failed sends do not refund.

Retirement revokes unused credit and requests queue discard before ordered
settlement. Cleanup deadlines never renew. Session feedback checks only its two
routes; settling a receiver does not close its shared sender or siblings.
Lifecycle, handshake, session and settlement deadlines share one indexed schedule.
`WS2N` freezes submitted totals; `WS2S` confirms consumption or discard, separately
from `WS2B`. Ordinary feedback cannot acknowledge retirement or bypass queue discard.

Recovery validates lifecycle authority before reinstating sessions and their
delivery obligations. Checkpoints preserve original deadlines, exact connection
generations, receive capacity, cumulative credit, unsettled occupancy and the wire-ID high-water
mark. Pending handshakes replay a bounded transcript and reverify proofs against
their original ownership. Fresh incarnations fence old storage and entropy
callbacks. Missing or invalid ledger state cannot authorize a surviving socket;
restoration failure requires closing those sockets. Retired flows retain their
charges and finish cleanup without recreating sessions or issuing new grants.

Each attachment owns its destination ledger and receiver-side session snapshots. The small
share header is replicated: new wire IDs refresh all attachments, while delivery, feedback
and admission-stage changes refresh only affected connections. This preserves the wire-ID
high-water mark even when the newest receiver disconnects. Recovery rejects mixed headers,
duplicate connections and misassigned admissions before checking lifecycle authority.

Hibernating hosts enable tracking after every restore and flush after applying outputs.
Flush preflights the whole changed batch; partial host failure retains it for retry. The
host must retry while active or close affected sockets before ending the event. The sink
must synchronously replace complete attachments and enforce its actual serialization
limit. `CheckpointBudget` reserves host-envelope space and checks the maximum retained
state before enabling tracking; it does not reduce native capacities. See the
[Cloudflare attachment budget](cloudflare-relay-rust-plan.md#attachment-budget).

Flow metadata uses runtime-bounded, lazily allocated slots. Handshakes allocate
no slots; receiver connections allocate for their own flows, independently of
sender fanout. Buffers grow on admission/recovery and are released when empty.
Stable indices preserve delayed-work identity; checkpoints encode only retained
flows and their indices. Delivery observations include retained flows, allocated
slots and the bound, separately from payload occupancy.

## Host evidence

The [bounded host probes](../../../spikes/relay-hosts/README.md) exercise the real
kernel with native TCP/WebSocket writers and a separate Rust Worker in Cloudflare
Previews. Native slow-writer isolation and STOP cleanup passed on Windows; the
probe joins both transport tasks and uses abortive TCP close before `Closed`.
Cloudflare exposes closure events but no verified buffer-release receipt; resetting
even an empty probe object left bounded status requests timing out. Production
adapters and client cutover remain pending; these experiments do not complete them.

## Testing

`make rust` runs formatting, Clippy, native coverage, doctests and the core Wasm
build. Public aggregate scenarios cover durability barriers, STOP races, FIFO
handoff, uncertain results, stale callbacks, cancellation, capacity rollback and
hibernated recovery using injected time without sleeps. Primitive tests retain
focused protocol and state-machine coverage.
See [validation](../../validation.md#rust-relay-checks) for prerequisites.
