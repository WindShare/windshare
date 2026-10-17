# Relay delivery flow contract

Handshake capacity, cumulative `WS2B` and final settlement controls have shared
Rust/Go/TypeScript vectors. The Rust kernel binds capacity to each connection
ledger and routes wire feedback into consumption or settlement. This remains
staged: current Go/browser connections use capacity-less handshakes and 24-byte
receive-credit deltas. Never select a mode by frame length
or fall back between meanings. Go `DeliveryRegisterInit` / `DeliveryJoin` and
browser `delivery-handshake-codec.ts` remain explicit staged entry points.

## Four distinct responsibilities

| Contract | Meaning |
| --- | --- |
| Initial capacity | REGISTER / RESUME / JOIN declare connection capacity. Admission reserves both directions' startup allowance before granting either, so the first frame never waits for consumption feedback. |
| `WS2W` sending allowance | The relay grants frame/byte deltas to the source, reserving destination capacity at grant time. Grants, accepted queues and submitted-but-unconsumed data share that capacity. Grants are not consumption. |
| Cumulative `WS2B` | Either endpoint reports the complete inbound `WS2O` prefix its application has consumed. Only newly consumed resources can be reclaimed; feedback neither declares capacity nor grants permission to send. |
| Final settlement (`WS2N` / `WS2S`) | After retirement, revocation and unsent-queue discard, the relay orders a notice behind submitted frames. The destination consumes or discards that prefix and acknowledges exactly the frozen submitted totals. Settlement releases connection capacity without reopening the session. |

Socket submission alone never refunds delivery capacity. Runtime window sizes,
startup reservations and batching thresholds are capacity/scheduling policy,
independent of the cumulative counters' encoding limits. Relay control frames
bypass content credit; feedback is batched without requiring per-frame round trips.

## Client receive ledgers

The staged [Go ledger](../../../transport/relayv2/internal/receiveledger/ledger.go) and
[browser ledger](../../../web/src/transport/relay/receive-ledger/index.ts) serve either
endpoint role without sockets, timers or payload ownership. One connection shares
frame/byte capacity and a retained-session bound. Immutable receipts bind each full
envelope to its local session lifetime and FIFO position. Browser counters use
`bigint`; opaque tokens and weakly held receipts isolate connection generations.
Arrival charges capacity; application handoff and retirement discard are distinct.
A discarded hole stops ordinary consumption's prefix, even if later frames are consumed.

Local retirement retains accounting and accepts in-flight frames until an ordered
`WS2N` exactly matches received totals. Only consuming/discarding that whole prefix
permits `WS2S`; repeated notices cannot refund twice. Metadata stays charged until
the acknowledgement is issued and the owner explicitly forgets the session.

Batch readiness combines pending consumption across sessions and adapts to both
window axes; a ready batch drains fairly, including its below-threshold tail.
The future adapter must serialize ledger access, retain receipts with payloads,
flush sparse consumption at a bounded deadline, and submit returned controls in
order outside content credit. Failed control writes end the connection generation.
No event implicitly admits a session; delayed callbacks retain their original
session token. Current Go/browser runtime handshakes and credit semantics remain
unchanged until coordinated host integration.

## Initial receive capacity

Append two unsigned, network-order 64-bit integers to `WS2R` (both REGISTER and
RESUME) and `WS2J`: `receive_frames`, then `receive_encoded_bytes`.

| Frame | Frames offset | Bytes offset | Exact length |
| --- | --- | --- | --- |
| `WS2R` | 116 | 124 | 132 |
| `WS2J` | 20 | 28 | 36 |

The v2 header and preceding fields stay unchanged. Both limits must be positive;
they are independent bounds, so consumption's envelope-ratio rule does not apply.
Bytes count complete `WS2O` envelopes, excluding control and WebSocket/TLS overhead.
REGISTER and RESUME append these same 16 bytes after challenge expiry in their
signature preimage. STOP has no capacity fields and spends no delivery budget.

Capacity is immutable for a physical connection generation, shared across its
sessions and separate from durable registration identity. RESUME on a replacement
connection may declare different capacity. Hibernation preserves the declaration,
grants and submitted-but-unconsumed occupancy; replay cannot refill the budget.

Effective total capacity is the component-wise minimum of the declaration and
the host's total bound. Every grant also obeys the host's protected-startup and
ordinary-pool limits. Host policy never enlarges a peer's capacity; a small peer
may therefore exhaust its total before either host pool. Startup grants are not
silently reduced: if either destination cannot fund the configured initial grant,
admission rolls back both directions and emits no grant or descriptor. A capacity
below one startup grant is structurally valid but cannot admit a session.

## Cumulative `WS2B`

The fixed 32-byte layout uses network byte order:

| Offset | Width | Field |
| --- | --- | --- |
| 0 | 4 | ASCII `WS2B` |
| 4 | 1 | Wire version `2` |
| 5 | 3 | Reserved, all zero |
| 8 | 8 | Nonzero relay session ID |
| 16 | 8 | Unsigned cumulative frame count |
| 24 | 8 | Unsigned cumulative full encoded byte count |

Both counts use `u64` / `uint64` / TypeScript `bigint`, spanning `0..2^64-1`.
Bytes include each complete `WS2O` header (20 bytes) and ciphertext (1..65,536
bytes), excluding WebSocket/TLS overhead and relay control frames. A valid pair
obeys `21 * frames <= bytes <= 65,556 * frames`; zero frames requires zero bytes.
Validation must not overflow. Counts never wrap or saturate: an exhausted flow
must retire and use a new session, not reset its counters.

The authenticated socket and non-reused session ID select the destination
connection generation and inbound direction. Clients do not send a second
direction/generation field that could override that authority. A surviving socket
keeps its counters across hibernation; a replacement connection/session starts a
new flow. Delayed feedback cannot create accounting state or affect a sibling.

The codec checks framing, identity and possible envelope totals. The delivery
ledger checks actual submitted totals and prefix progression: duplicates and
older prefixes refund nothing; mixed regressions, impossible deltas or claims
beyond submission are refused. Decoding the same bytes twice is not itself an
idempotence guarantee. Final settlement additionally requires retirement ordering
and exact frozen totals; ordinary `WS2B` does not acknowledge a settlement notice.

## Final settlement

This boundary is relay-session retirement, not cancellation of an individual
download. A download can stop while its relay session continues serving other
work; that live flow still accounts for dequeued frames through ordinary `WS2B`.

`WS2N` (relay → destination) and `WS2S` (destination → relay) use the same 32-byte
layout and complete-envelope count bounds as cumulative `WS2B`, with distinct
magic bytes. Bytes 5–7 stay zero. Both carry the nonzero session ID, final frame
count and final full encoded byte count; they carry no direction or generation.
The authenticated socket selects that exact destination flow. Zero totals are
valid when the relay submitted no frames.

The relay revokes grants, confirms unsent-queue discard, then sends `WS2N` through
the destination's ordered writer behind all submitted frames. It bypasses content
credit but cannot overtake those frames. The client processes or discards that
entire prefix before echoing the exact notice totals in `WS2S`; merely receiving
the notice is insufficient when application work remains queued. Repeated notices
may repeat the same acknowledgement. Counters never reset or reopen the session.

`ShareRelay` emits an encodable `SettlementNotice` and decodes `WS2S` from
`Input::Control` into the existing `Input::Settled` path. Premature or mismatched
acknowledgements release nothing. Earlier consumption is subtracted only once;
duplicate acknowledgements and receipts for removed flows cannot touch siblings
or reused slots. A receipt arriving after the fixed cleanup deadline cannot undo
the connection's close obligation, including after hibernation.

Ordinary `WS2B` proves application consumption, never unsent-queue discard or
receipt of `WS2N`. Fully consumed flows can release their remaining metadata once
queue discard is confirmed, without retaining zero occupancy just to wait for
`WS2S`; the ordered notice is still sent and its eventual acknowledgement is inert.
Host/client runtime integration must preserve these ordering and discard rules.

## Permanent STOP and outstanding delivery

`STOPPED` confirms durable revocation, discarded unsubmitted queues and quiescent
publication writes. It does not wait for `WS2B`, `WS2S` or transport-buffer release;
already-submitted frames can still arrive. Its 24-byte `WS2Y` encoding carries only
the existing stop ID, with the same meaning in Rust, Go and TypeScript.

The STOP barrier checks all retained generations, including old owners and detached
queues. Their settled/discarded state comes from the ledgers, not socket presence.
Submitted occupancy survives acknowledgement, disconnect, timeout and restart until
consumption, final settlement or proven release. A stopped record alone cannot
replace a responsibility checkpoint. Authenticated duplicate STOP acknowledges the
same permanent fact without changing budgets or reopening routes.

## Shared evidence

`core/testvectors/v2-session.json` contains `relay-delivery-consumed`, generated
independently of production codecs. All three implementations encode and decode
the same count cases and reject the same malformed frames. Cases include zero,
envelope limits, totals above runtime windows/u32/JavaScript safe integers,
u64 arithmetic boundaries, every truncated length, trailing bytes, bad prefixes,
zero IDs and the old 24-byte layout. Existing Rust ledger/aggregate tests retain
ownership of duplicate feedback, stale generations and settlement behavior.
`relay-settlement-notice` and `relay-settlement-ack` share those boundaries and
reject each other's headers and `WS2B`. Aggregate tests exercise encoded controls
through retirement ordering, partial consumption, wrong totals, replay, slot reuse
and hibernated deadline expiry.
The `relay-capacity-*` families pin exact handshake bytes, both signed capacity
axes, independent u64 limits, truncated/old layouts and different RESUME capacity.
Rust aggregate tests cover asymmetric budgets, startup rollback and hibernation
with outstanding grants and submitted data.
