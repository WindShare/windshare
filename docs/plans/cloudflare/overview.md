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
| `connection_usage()` | Report admitted slots, retired generations and outstanding residual frames/bytes separately. |
| `checkpoint_connection(connection)` | Capture one generation's ledger and admissions, even after its socket disappears. |
| `checkpoint_manifest()` | Capture generation reservations, retirement state and the session-ID fence independently of delivery records. |
| `enable_checkpoints(budget, now)` / `flush_checkpoints(now, write)` | Write changed generation records, then the manifest when its membership or header changes. |
| `Checkpoint::from_connections(manifest, records, limits)` | Validate records individually; missing, duplicate or invalid records retain reservations and lose routing authority. |
| `checkpoint()` / `Checkpoint::encode()` | Capture a full image for recovery tools and tests. |

Commands request control sends, entropy, payload discard, exact connection
closure and serialized storage work. Hosts return evidence as inputs, including
discard and physical-close confirmations. They preserve ordering on each
connection and report storage results with the issued work ID. Socket readiness
waits happen before `submit`; its handoff cannot await or cache authorization.
Native hosts leave checkpoint tracking disabled and allocate no change journal.
Forwarding must not write delivery counters to a database.
The [checkpoint experiment](../../../spikes/relay-hosts/checkpoint-recovery.md) uses
an intentionally per-event SQLite mirror as a control, not as production wiring.

Go REGISTER/RESUME/JOIN/STOP and browser JOIN now derive a per-share dial URL with the public
`share_id` query hint. Shared vectors preserve admission-query bytes and relay identity, and reject
preconfigured routing parameters. The future Cloudflare host must validate that hint against the
handshake's share identity; client routing does not resolve host recovery storage.

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
forwarding, persists its intent, waits for unsubmitted-queue discard across all
generations and publication quiescence, then commits before acknowledging.
Submitted occupancy settles independently; old network bytes can still arrive.
`STOPPED` never releases capacity, and stopped checkpoints retain unsettled ledgers.
The aggregate owns
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

Recovery separates ownership/STOP authority, communicating sockets and delivery responsibility.
`Restore::authority` explicitly distinguishes `Known` facts from `Unknown`, which refuses service.
`Responsibility` requires an authoritative checkpoint inventory or proof of complete release;
unknown inventory also refuses recovery. Missing delivery records isolate their connections and
dependent sessions, while healthy ledgers keep their grants, submitted debt and absolute deadlines.
Restart and empty socket lists never prove release.
`Limits::connections` bounds admission through unfinished retirement.
`RetirementPolicy::PhysicalRelease` waits for `Closed` and discard of queues targeting
other connections; `Bounded` permits logical retirement
after revocation, both directions' queue discard and an injected fixed grace (possibly zero).
Retired-generation count and submitted frames/bytes have independent limits. Exhaustion
keeps that generation admitted without interrupting healthy peers; exact settlement or
physical release retries waiting retirement. No timer refunds residual risk.

`Limits::sessions` bounds live admissions. Retired session objects disappear after queue
cleanup; their routes and debt remain in independently bounded
`retained_flows_per_connection` slots until settlement. In particular, a healthy sender
keeps its own outstanding reverse-flow debt. Original deadlines, cumulative totals and
wire-ID high-water marks survive repeated recovery; old feedback cannot affect replacements.
Pending handshakes retain their original ownership; fresh incarnations fence old storage
and entropy callbacks.

Each connection record contains its ledger, handshake and receiver-side admissions.
The independent `CheckpointManifest` retains generation identity, retirement state, a risk
reservation and the session-ID high-water mark. Admission no longer rewrites unrelated
receiver attachments. Missing live records reserve the entire configured connection budget;
missing retired records retain the submitted bound established at retirement. These are
explicit uncertain reservations, not fabricated delivery counters. Old feedback cannot
release them; physical release can. Risk exhaustion blocks further retirement/admission,
without revoking healthy sessions. Repeated recovery preserves fixed grace and loss status.

Hibernating hosts enable tracking after restore and flush after outputs. Flush preflights
changed records and writes a changed manifest last; partial failures retain the batch for
retry. Hosts must omit unavailable records instead of replaying stale credits, and preserve
authoritative lifecycle/reservation facts. Manifest changes cover admission, retirement and
retired-risk release, never ordinary forwarding counters. Generation allocation must be
persisted before accepting a new socket; removed IDs cannot be reused. `CheckpointBudget`
still bounds connection records, including retained flows. Native capacities are unchanged.
See the
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
Follow-up controls reproduced a Rust callback failure after reset and avoided it by
calling abort in JavaScript after the Rust future completes. The object recovered,
but an old peer with an 8 KiB receive buffer still received the complete 8 MiB burst;
a healthy peer on the same object disconnected. Reset is therefore not a verified
buffer-release receipt or per-receiver isolation policy.

The [checkpoint follow-up](../../../spikes/relay-hosts/checkpoint-recovery.md) now
verifies one sender and two receivers in a real Preview. Losing one attachment closes
only that receiver; its sibling continues downloading and the affected receiver rejoins
with a new generation/session. Repeated reconstruction and replayed old settlement retain
loss reservations. Attachment forwarding performs no SQL writes. The per-event SQLite
mirror remains a control. Normal hibernation retains exact ledgers; abrupt reset still
disconnects the whole share. These bounded fixture transfers do not establish that
product flow.

## Testing

`make rust` runs formatting, Clippy, native coverage, doctests and the core Wasm
build. Public aggregate scenarios cover durability barriers, STOP with outstanding
delivery, duplicate STOP, stopped recovery and late callbacks, FIFO
handoff, uncertain results, stale callbacks, cancellation, capacity rollback and
hibernated recovery using injected time without sleeps. Primitive tests retain
focused protocol and state-machine coverage.
See [validation](../../validation.md#rust-relay-checks) for prerequisites.
