# Relay delivery flow contract

Capacity-carrying handshakes, cumulative `WS2B` and final settlement
(`WS2N`/`WS2S`) are staged behind shared Rust/Go/TypeScript vectors and
socket-free ledgers; current Go/browser connections still use capacity-less
handshakes and 24-byte receive-credit deltas. Mode is never inferred from frame
length or shifted between meanings. Staged entry points: Go
`DeliveryRegisterInit`/`DeliveryJoin`, browser `delivery-handshake-codec.ts`.

Capacity is declared per direction at admission and consumed by per-grant
allowances. Admission reserves both startup allowances before granting either, so
the first frame never waits for feedback. A grant (`WS2W`) reserves destination
capacity immediately; grants, accepted queues and submitted-but-unconsumed data
share capacity, so a grant is not consumption. Feedback reports the complete
consumed `WS2O` prefix and reclaims only newly consumed resources; it never
declares capacity or grants permission. Socket submission never refunds capacity.
Window and batch thresholds are scheduling policy, independent of counter
encoding limits; control frames bypass content credit, and feedback may batch.

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
owner forget; batches adapt to both window axes and drain their below-threshold
tail. Hosts serialize ledger access, retain payloads with receipts, flush sparse
consumption before a bounded deadline, and send ordered controls outside content
credit; delayed callbacks keep their session token.

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
