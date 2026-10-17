# Cloudflare checkpoint recovery experiment

The [partial-recovery evidence](evidence/2026-10-17-partial-recovery.json) uses the
real Rust `ShareRelay`, Workers WebSockets and SQLite in an isolated Preview.
Authentication uses canonical fixtures and a frozen kernel clock. These are
bounded binary-frame transfers, not production clients or a WindShare file download.

## Recovery boundary

SQLite retains authoritative lifecycle facts, the generation allocator and a
`CheckpointManifest` of generation reservations and the session-ID high-water mark.
Live delivery counters stay in bounded binary WebSocket attachments. A lost or
invalid record closes only its connection and revokes dependent sessions. Its
reservation survives independently, without inventing exact submitted counts.

The probe uses three admitted connections and two retired generations. Its experimental
zero-grace policy allows retirement only after revocation and queue discard. Retired
risk is bounded by two connection budgets. Missing live records reserve the full
connection capacity; records lost after retirement reserve the submitted bound
persisted at retirement. Neither reservation expires or proves physical release.
This profile demonstrates isolation, not a production retention policy.

| Mode | Live records | After a socket disappears |
| --- | --- | --- |
| `attachments` | WebSocket attachments; ordinary forwarding performs no SQL writes. | No durable delivery ledger. Missing records become conservative reservations. |
| `sqlite_mirror` | Attachments plus a complete SQLite image per event. | Exact records remain recoverable as an experimental control. |

The mirror uses a pending marker and atomic row replacement. Two single-frame
events cause four application SQL writes; attachment mode causes zero. Payloads
never enter SQLite. The mirror is not the production persistence design.

## Observed behavior

- Normal Cloudflare hibernation preserves grants, outstanding counts and absolute
  deadlines; forwarding, `WS2B` consumption and ordered `WS2N/WS2S` settlement resume.
- With one sender and two receivers, erasing one receiver's attachment closes only
  that receiver. The sender and sibling keep their sockets; the sibling's unchanged
  grants and outstanding counts survive reconstruction and its download continues.
- A second reconstruction preserves the missing generation's reservation. The affected
  receiver rejoins as `c-2-2` with session 3 while session 2 continues. Replaying session
  1 settlement cannot refund either the old reservation or the replacement's debt.
- Live disconnect can establish a smaller retirement reservation before the exact
  record disappears. A reconstruction keeps that bound; sender settlement cannot
  refund the missing receiver.
- Abrupt whole-object reset still disconnects every socket. Recovery retains old
  responsibility and the original persisted deadline. It is not per-receiver cleanup.
- The SQLite mirror restores the erased record without forcing that receiver to
  reconnect. It remains useful as a control, at the cost of forwarding-path writes.

The [earlier complete-checkpoint experiment](evidence/2026-10-17-checkpoints.json)
rejected attachment loss with `MissingResponsibility`, blocking the healthy sender.
The kernel now isolates delivery-record loss; missing or uncertain authority and
reservation inventory still refuse recovery. No run synthesizes `Input::Closed`.

## Reproduce

From `cloudflare/`, follow the [Preview runbook](../../docs/skills/cloudflare-worker-previews-skill.md).
Use the latest deployment URL; updating secrets creates a new immutable URL.

```powershell
cargo fmt --check
cargo clippy --target wasm32-unknown-unknown --locked -- -D warnings
node checkpoint-probe.mjs <Preview-URL> all
```

The client reads `PROBE_TOKEN` or ignored `.evidence/token`, creates fresh objects,
checks payload bytes and writes source hashes plus snapshots under `.evidence/`.
Individual scenarios: `hibernate`, `attachment-loss`, `disconnect`, `reset`.
Local Wrangler runs cover the same behavior; only Preview runs establish real
Cloudflare hibernation. Delete only the Preview created for the run.
