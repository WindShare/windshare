# Relay delivery flow contract

Handshake capacity and cumulative `WS2B` codecs have shared Rust/Go/TypeScript
vectors. The Rust kernel binds capacity to each connection ledger. This remains
staged: current Go/browser connections use capacity-less handshakes and 24-byte
receive-credit deltas. Add settlement controls before the coordinated runtime
cutover, then delete the old codecs. Never select a mode by frame length or fall
back between meanings. Go `DeliveryRegisterInit` / `DeliveryJoin` and browser
`delivery-handshake-codec.ts` are explicit staged entry points.

## Four distinct responsibilities

| Contract | Meaning |
| --- | --- |
| Initial capacity | REGISTER / RESUME / JOIN declare connection capacity. Admission reserves both directions' startup allowance before granting either, so the first frame never waits for consumption feedback. |
| `WS2W` sending allowance | The relay grants frame/byte deltas to the source, reserving destination capacity at grant time. Grants, accepted queues and submitted-but-unconsumed data share that capacity. Grants are not consumption. |
| Cumulative `WS2B` | Either endpoint reports the complete inbound `WS2O` prefix its application has consumed. Only newly consumed resources can be reclaimed; feedback neither declares capacity nor grants permission to send. |
| Final settlement (wire integration pending) | After retirement, revocation and unsent-queue discard, the relay orders a notice behind submitted frames. The destination consumes or discards that prefix and acknowledges exactly the frozen submitted totals. Settlement releases connection capacity without reopening the session. |

Socket submission alone never refunds delivery capacity. Runtime window sizes,
startup reservations and batching thresholds are capacity/scheduling policy,
independent of the cumulative counters' encoding limits. Relay control frames
bypass content credit; feedback is batched without requiring per-frame round trips.

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

## Shared evidence

`core/testvectors/v2-session.json` contains `relay-delivery-consumed`, generated
independently of production codecs. All three implementations encode and decode
the same count cases and reject the same malformed frames. Cases include zero,
envelope limits, totals above runtime windows/u32/JavaScript safe integers,
u64 arithmetic boundaries, every truncated length, trailing bytes, bad prefixes,
zero IDs and the old 24-byte layout. Existing Rust ledger/aggregate tests retain
ownership of duplicate feedback, stale generations and settlement behavior.
The `relay-capacity-*` families pin exact handshake bytes, both signed capacity
axes, independent u64 limits, truncated/old layouts and different RESUME capacity.
Rust aggregate tests cover asymmetric budgets, startup rollback and hibernation
with outstanding grants and submitted data.
