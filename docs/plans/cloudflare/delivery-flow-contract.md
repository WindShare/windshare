# Relay delivery flow contract

The Go `relayv2` runtime, browser receiver and Rust hosts use capacity-carrying
handshakes, cumulative `WS2B` and final settlement (`WS2N`/`WS2S`). Go process E2E
and browser product fixtures build Go `wind` and Rust `relay-native`; the native
process scenario uses real `DialSender`/`DialReceiver`/`Channel` APIs.
`integration/relayv2` shares the native test host with Go process E2E. Default
server/release entry points and other Go relay fixtures still target the
incompatible legacy Go server. There is no wire-format fallback.

Capacity is declared per direction at admission and consumed by per-grant
allowances. Admission reserves both startup allowances before granting either, so
the first frame never waits for feedback. A grant (`WS2W`) reserves destination
capacity immediately; grants, accepted queues and submitted-but-unconsumed data
share capacity, so a grant is not consumption. Feedback reports the complete
consumed `WS2O` prefix and reclaims only newly consumed resources; it never
declares capacity or grants permission. Socket submission never refunds capacity.
Host windows are scheduling policy, independent of counter encoding limits.
Control frames bypass content credit. Consumption is immediately eligible for the
ordered writer, which coalesces pending prefixes without waiting for more input;
the destination declaration cannot predict the relay's smaller effective window.

## Client receive ledgers

[Go](../../../transport/relayv2/internal/receiveledger/ledger.go) and
[browser](../../../web/src/transport/relay/receive-ledger/index.ts) ledgers are
socket-free and serve either endpoint. A connection shares frame/byte capacity
plus a retained-session bound. Immutable receipts bind each envelope to its
session lifetime and FIFO position; browser counters use `bigint` and opaque
generation tokens. Arrival charges capacity; handoff and retirement discard are
distinct, and a discarded hole stops the consumption prefix even if later frames
are consumed.

Retirement retains accounting until an ordered `WS2N` matches received totals;
only consuming or discarding the whole prefix permits `WS2S`, and repeated
notices cannot refund twice. Metadata stays charged until acknowledgement or
owner forget. Hosts serialize ledger access, retain payloads with receipts, wake
the writer after consumption, and send ordered controls outside content credit;
delayed callbacks keep their session token. Pending prefixes rotate fairly across
sessions, including when only one envelope was consumed.

Go takes its immutable declaration and ledger capacity from
`DialOptions.ReceiveCapacity` (default: 256 frames / 16 MiB per connection).
Both roles wait for explicit `WS2W`, account full envelopes before retaining
payloads, and report consumption on the next available writer turn without a
batch threshold or flush timer. Each turn also offers queued outbound content a
write so continuous consumption cannot starve the opposite direction.
Remote retirement drains accepted frames; owner close discards and joins their
workers. Retired flows retain accounting until the ordered writer issues `WS2S`.

Browser JOIN and its receipt-bearing queue share one immutable `receiveCapacity`
(default: 256 frames / 16 MiB). Stream handoff wakes cumulative feedback outside
content credit, including for single-envelope grants. `WS2N` stops application
sends while accepted frames drain; cancellation discards the remaining prefix.
Owner close submits any pending final control before the WebSocket close, but
cannot invent settlement without a notice. Disconnect ends feedback authority.
The existing 17 MiB browser scenario checks cumulative feedback across the
declared window and verifies the downloaded SHA-256 through the Rust host.

## Initial receive capacity

`WS2R` (REGISTER, RESUME) and `WS2J` append two unsigned network-order u64
values: `receive_frames`, then `receive_encoded_bytes`.

| Frame | Frames offset | Bytes offset | Exact length |
| --- | --- | --- | --- |
| `WS2R` | 116 | 124 | 132 |
| `WS2J` | 20 | 28 | 36 |

Limits are positive and independent, so the envelope-ratio rule does not apply.
Bytes count complete `WS2O` envelopes, excluding control and WebSocket/TLS
overhead. REGISTER/RESUME append the same 16 bytes to their signature preimage
after challenge expiry; STOP carries no capacity and spends no delivery budget.
Capacity is immutable per
physical connection generation, shared across its sessions and separate from
durable registration identity. Replacements may declare differently; hibernation
preserves declaration, grants and submitted occupancy, but replay cannot refill
the budget.

Effective capacity is the component-wise minimum of declaration and host bound;
grants also obey protected-startup and ordinary-pool limits. If either direction
cannot fund the configured startup grant, admission rolls back both and emits
neither grant nor descriptor; a capacity below one startup grant is valid but
cannot admit.

The protected startup pool is capped inside that effective capacity, leaving at
least one startup grant's capacity for ordinary replenishment when it fits.
This prevents existing flows from spending a smaller declaration's startup reserve
without deadlocking a single-window peer. Checkpoint restoration enforces the
same partition. Concurrent full windows still require sufficient shared capacity.

## Cumulative `WS2B`

Fixed 32 bytes, network order:

| Offset | Width | Field |
| --- | --- | --- |
| 0 | 4 | ASCII `WS2B` |
| 4 | 1 | Wire version `2` |
| 5 | 3 | Reserved, zero |
| 8 | 8 | Nonzero relay session ID |
| 16 | 8 | Cumulative frame count |
| 24 | 8 | Cumulative full encoded bytes |

Counts are `u64`/`uint64`/`bigint` spanning `0..2^64-1`. Bytes include the
20-byte `WS2O` header plus 1..65,536 ciphertext bytes, excluding WebSocket/TLS and
control frames; valid pairs obey `21*frames <= bytes <= 65,556*frames` (zero
frames means zero bytes), and validation must not overflow. Counts never wrap or
saturate: an exhausted flow retires and starts a new session.

The authenticated socket plus non-reused session ID select generation and
direction; clients send no competing field. Surviving sockets keep counters
across hibernation, replacements start a new flow, and delayed feedback cannot
create state or affect a sibling. The codec checks framing, identity and possible
envelope totals; the ledger checks submitted totals and prefix progression.
Duplicates and older prefixes refund nothing; regressions, impossible deltas and
claims beyond submission are refused. Decoding is not idempotent, and ordinary
`WS2B` never acknowledges a settlement notice.

## Final settlement

Settlement belongs to session retirement, not an individual download; a live
session can stop one download and keep accounting through ordinary `WS2B`.
`WS2N` (relay → destination) and `WS2S` (destination → relay) reuse the `WS2B`
layout and bounds with distinct magic bytes; bytes 5–7 stay zero, and they carry
the nonzero session ID and frozen totals without direction or generation. The
authenticated socket selects the destination flow; zero totals are valid when
nothing was submitted.

The relay revokes grants, confirms unsent-queue discard, then writes `WS2N`
behind all submitted frames. The client processes or discards that whole prefix
before echoing exact totals in `WS2S`; reading the notice is insufficient while
application work is queued. Repeated notices may repeat the acknowledgement;
counters never reset and the session never reopens.

Premature, mismatched or duplicate acknowledgements release nothing and cannot
touch siblings or reused slots; consumption is subtracted once, and a receipt
after the cleanup deadline cannot undo the close obligation, even after
hibernation. Ordinary `WS2B` proves consumption, never queue discard or notice
receipt: a fully consumed flow may release metadata once discard is confirmed
without waiting for `WS2S`, and the ordered notice remains necessary with an
inert late acknowledgement.

## Permanent STOP

`STOPPED` confirms durable revocation, unsubmitted-queue discard and quiescent
publication writes, but waits for no feedback or transport-buffer release;
submitted frames may still arrive. `WS2Y` is the same 24-byte stop-ID encoding in
Rust, Go and TypeScript. The barrier checks every retained generation, including
old owners and detached queues, from ledger state rather than socket presence.
Submitted occupancy survives acknowledgement, disconnect, timeout and restart
until consumption, settlement or proven release; a stopped record alone cannot
replace a responsibility checkpoint. Duplicate authenticated STOP acknowledges
the same fact without changing budgets or reopening routes.

## Shared evidence

`core/testvectors/v2-session.json` holds independently generated capacity,
consumed and settlement vectors; Rust, Go and TypeScript agree on valid cases and
reject malformed frames. Rust ledger and aggregate tests cover duplicate
feedback, stale generations, retirement ordering, partial consumption, replay,
slot reuse, hibernation deadlines, asymmetric budgets and startup rollback.
