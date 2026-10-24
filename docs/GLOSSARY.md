# Resource ownership

| Term | Meaning |
|---|---|
| Budget / ledger | Capacity limits and accounting within an owning scope. |
| Reservation | Capacity acquired before work or storage is admitted. |
| Hold / charge | A live claim on capacity that stays charged until released or transferred. |
| Permit | Permission to perform an operation; it does not imply ownership of bytes or sockets. |
| Lease | A domain usage right with a defined lifetime or revocation condition; remote leases may expire and renew. |
| Admission | The decision that work may start under its domain's constraints. |
| Claim | Exclusive authority to settle a particular resource or operation. |
| Grant | The result of an authorization or admission decision. |

`core/capacityledger` implements scalar capacity ownership. Related ledgers and
holds share their domain owner's lock. A copied hold shares one claim; transfer
invalidates old copies without releasing capacity. Split and aggregate growth
preserve accounting. Close stops admission and reports remaining holds; it does
not revoke them or wait for workers.

Owners cancel and join workers before treating retained capacity as a leak.
Receiver cleanup also joins LaneSet-owned hedges before auditing plaintext.
Plaintext, sealed caches, invalidation metadata, and peer-attempt slots use this
scalar ledger. Buffer budgets retain their eviction policies; sealed caches
aggregate entries into one hold per cache. Catalog's multi-resource metering and
revision capacity's reclaim/quarantine state machine remain domain-specific.
