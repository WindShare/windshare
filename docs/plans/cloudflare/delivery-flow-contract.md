# Relay delivery flow contract

The cumulative `WS2B` codec and shared vectors are implemented in Rust, Go and
TypeScript. This is a staged contract: current Go/browser connections still use
24-byte receive-credit deltas. Activate the new contract only with handshake
capacity, settlement controls and the coordinated relay/client runtime cutover;
then delete the old delta codec. Do not infer a protocol mode from frame length
or add a fallback between the two meanings.

## Four distinct responsibilities

| Contract | Meaning |
| --- | --- |
| Initial capacity (handshake integration pending) | The destination declares bounded connection capacity. Admission reserves both directions' startup allowance from that budget before granting either, so the first frame never waits for consumption feedback. |
| `WS2W` sending allowance | The relay grants frame/byte deltas to the source, reserving destination capacity at grant time. Grants, accepted queues and submitted-but-unconsumed data share that capacity. Grants are not consumption. |
| Cumulative `WS2B` | Either endpoint reports the complete inbound `WS2O` prefix its application has consumed. Only newly consumed resources can be reclaimed; feedback neither declares capacity nor grants permission to send. |
| Final settlement (wire integration pending) | After retirement, revocation and unsent-queue discard, the relay orders a notice behind submitted frames. The destination consumes or discards that prefix and acknowledges exactly the frozen submitted totals. Settlement releases connection capacity without reopening the session. |

Socket submission alone never refunds delivery capacity. Runtime window sizes,
startup reservations and batching thresholds are capacity/scheduling policy,
independent of the cumulative counters' encoding limits. Relay control frames
bypass content credit; feedback is batched without requiring per-frame round trips.

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
