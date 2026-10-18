# Relay delivery flow contract

Handshake capacity, cumulative `WS2B` and final settlement (`WS2N`/`WS2S`) are
staged behind shared Rust/Go/TypeScript vectors and socket-free ledgers: current
Go/browser connections still use capacity-less handshakes and 24-byte
receive-credit deltas. Never select a mode by frame length or fall back between
meanings. Go `DeliveryRegisterInit`/`DeliveryJoin` and browser
`delivery-handshake-codec.ts` are the explicit staged entry points.

## Responsibilities

| Contract | Meaning |
| --- | --- |
| Initial capacity | REGISTER/RESUME/JOIN declare per-direction capacity. Admission reserves both startup allowances before granting either, so the first frame never waits for feedback. |
| `WS2W` allowance | The relay grants frame/byte deltas to the source, reserving destination capacity at grant time. Grants, accepted queues and submitted-but-unconsumed data share capacity; a grant is not consumption. |
| Cumulative `WS2B` | Each endpoint reports the complete inbound `WS2O` prefix its application consumed. Only newly consumed resources are reclaimed; feedback neither declares capacity nor grants sending permission. |
| Final settlement | After retirement, revocation and unsent-queue discard, the relay orders `WS2N` behind all submitted frames; the destination consumes/discards that prefix, acknowledges frozen totals via `WS2S`, and capacity is released without reopening the session. |

Socket submission never refunds capacity. Window sizes and batch thresholds are
scheduling policy, independent of counter encoding limits. Control frames bypass
content credit and feedback may batch.

## Client receive ledgers

[Go](../../../transport/relayv2/internal/receiveledger/ledger.go) and
[browser](../../../web/src/transport/relay/receive-ledger/index.ts) ledgers are
socket-free and serve either endpoint role. A connection shares frame/byte
capacity plus a retained-session bound. Immutable receipts bind each envelope to
its session lifetime and FIFO position; browser counters use `bigint` and opaque
tokens isolate generations. Arrival charges capacity; handoff and retirement
discard are distinct, and a discarded hole stops the consumption prefix even if
later frames are consumed.

Local retirement retains accounting until an ordered `WS2N` matches received
totals; only consuming/discarding the whole prefix permits `WS2S`, and repeated
notices cannot refund twice. Metadata stays charged until acknowledgement and
explicit owner forget. Batches adapt to both window axes and drain their
below-threshold tail. Hosts must serialize ledger access, retain receipts with
payloads, flush sparse consumption at a bounded deadline, and send controls in
order outside content credit; a failed control write ends the generation.
Delayed callbacks keep their session token, nothing implicitly admits a session,
and runtime semantics stay unchanged until the coordinated cutover.

## Initial receive capacity

`WS2R` (REGISTER and RESUME) and `WS2J` append two unsigned network-order u64
values: `receive_frames`, then `receive_encoded_bytes`.

| Frame | Frames offset | Bytes offset | Exact length |
| --- | --- | --- | --- |
| `WS2R` | 116 | 124 | 132 |
| `WS2J` | 20 | 28 | 36 |

Both limits are positive and independent, so the envelope-ratio rule does not
apply. Bytes count complete `WS2O` envelopes, excluding control and
WebSocket/TLS overhead. REGISTER/RESUME append the same 16 bytes to their
signature preimage after challenge expiry; STOP carries no capacity and spends no
delivery budget. Capacity is immutable per physical connection generation,
shared across its sessions and separate from durable registration identity.
RESUME on a replacement connection may declare different capacity. Hibernation
preserves declaration, grants and submitted occupancy, but replay cannot refill
the budget.

Effective capacity is the component-wise minimum of the declaration and the host
bound; every grant also obeys host protected-startup and ordinary-pool limits.
If either direction cannot fund the configured startup grant, admission rolls
back both and emits neither grant nor descriptor. A capacity below one startup
grant is valid but cannot admit.

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

Counts are `u64`/`uint64`/`bigint` spanning `0..2^64-1`. Bytes include each
20-byte `WS2O` header plus 1..65,536 bytes of ciphertext, excluding WebSocket/TLS
and control frames; a valid pair obeys `21 * frames <= bytes <= 65,556 * frames`
(zero frames means zero bytes) and validation must not overflow. Counts never
wrap or saturate: an exhausted flow retires and starts a new session.

The authenticated socket plus non-reused session ID select the destination
generation and direction, so clients send no competing field. A surviving socket
keeps counters across hibernation; replacements start a new flow, and delayed
feedback cannot create state or affect a sibling. The codec checks framing,
identity and possible envelope totals; the ledger checks submitted totals and
prefix progression. Duplicates and older prefixes refund nothing; regressions,
impossible deltas or claims beyond submission are refused. Decoding twice is not
idempotence, and ordinary `WS2B` never acknowledges a settlement notice.

## Final settlement

Settlement belongs to relay-session retirement, not an individual download; a
live session can stop one download and keep accounting through ordinary `WS2B`.
`WS2N` (relay → destination) and `WS2S` (destination → relay) reuse the `WS2B`
layout and count bounds with distinct magic bytes; bytes 5–7 stay zero, and they
carry the nonzero session ID and frozen frame/byte totals with no direction or
generation. The authenticated socket selects the destination flow; zero totals
are valid when nothing was submitted.

The relay revokes grants, confirms unsent-queue discard, then writes `WS2N`
behind all submitted frames. The client processes/discards that whole prefix
before echoing the exact totals in `WS2S`; merely reading the notice is
insufficient while application work is queued. Repeated notices may repeat the
same acknowledgement; counters never reset and the session never reopens.

`ShareRelay` emits `SettlementNotice` and maps `WS2S` from `Input::Control` to
`Input::Settled`. Premature or mismatched acknowledgements release nothing;
earlier consumption is subtracted once, and duplicate or removed-flow receipts
cannot touch siblings or reused slots. A receipt after the fixed cleanup
deadline cannot undo the close obligation, even after hibernation. Ordinary
`WS2B` proves consumption, never queue discard or notice receipt: a fully
consumed flow may release metadata once discard is confirmed without waiting for
`WS2S`; the ordered notice is still sent and its late acknowledgement is inert.

## Permanent STOP

`STOPPED` confirms durable revocation, discarded unsubmitted queues and
quiescent publication writes, but waits for no feedback or transport-buffer
release; already-submitted frames may still arrive. `WS2Y` is the same 24-byte
stop-ID encoding in Rust, Go and TypeScript. The barrier checks every retained
generation, including old owners and detached queues, from ledger state rather
than socket presence. Submitted occupancy survives acknowledgement, disconnect,
timeout and restart until consumption, settlement or proven release; a stopped
record alone cannot replace a responsibility checkpoint. Duplicate authenticated
STOP acknowledges the same fact without changing budgets or reopening routes.

## Shared evidence

`core/testvectors/v2-session.json` holds independently generated
`relay-delivery-consumed`, `relay-settlement-notice`/`relay-settlement-ack` and
`relay-capacity-*` vectors. All three implementations agree on valid count cases
and reject malformed frames (truncation, trailing bytes, bad prefixes, zero IDs,
old layouts, cross-family headers), pinning zero, envelope limits, values above
runtime windows/u32/JS safe integers and u64 boundaries. Rust ledger/aggregate
tests own duplicate feedback, stale generations, retirement ordering, partial
consumption, wrong totals, replay, slot reuse, hibernation deadlines, asymmetric
budgets and startup rollback.
