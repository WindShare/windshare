# Cloudflare checkpoint recovery experiment

The [evidence](evidence/2026-10-17-checkpoints.json) uses a real Rust
`ShareRelay`, Workers WebSockets and SQLite in an isolated Preview. One sender
and one receiver exchange bounded binary frames. Authentication uses canonical
fixtures and a frozen kernel clock; this is not the production relay or a
WindShare file-transfer test.

## Storage choices

Cloudflare documents that [attachments last only while their WebSocket survives](https://developers.cloudflare.com/durable-objects/best-practices/websockets/#websocketserializeattachment).
Both modes persist the lifecycle record, complete manifest and connection-generation
allocator in the `checkpoint_image` SQLite row. Each event calls
`flush_checkpoints`; the attachment binding receives a raw `Uint8Array`, with
round-trip verification and the kernel's attachment budget.

| Mode | Live generation records | Detached generation records |
| --- | --- | --- |
| `attachments` | WebSocket attachments; ordinary forwarding performs no SQL writes. | Saved to SQLite when a live runtime observes disconnect. A missing attachment during cold restoration has no fallback. |
| `sqlite_mirror` | Attachments plus a complete SQLite image after each event. | The same SQLite image, independent of sockets. |

The mirror is an experimental control, **not the production persistence design**.
It writes a pending marker before mutation and atomically replaces the coherent
manifest/record image afterward. A failed event cannot knowingly fall back to
an older clean image. Two single-frame events caused four application SQL writes.
This measures application writes, not billed rows or transactions. Payloads never
enter SQLite.

## What the runs establish

- Natural hibernation reruns the constructor while both sockets remain connected.
  Exact outstanding counts and unused grants survive; forwarding, `WS2B`
  consumption and ordered `WS2N/WS2S` settlement continue.
- Erasing the receiver's attachment and reconstructing Rust fails with
  `MissingResponsibility` in attachment mode. The same object's healthy sender
  then cannot progress. The mirror restores the missing record and keeps the sender usable.
- A disconnect callback processed by a live runtime can preserve its detached
  ledger in either mode. The healthy sender remains usable; its settlement cannot
  refund the detached receiver's outstanding bytes.
- A replacement receiver gets a new connection generation and wire session.
  It can transfer while the old ledger remains charged; replaying the old session's
  settlement from that replacement cannot release the old generation.
- Abrupt object reset loses both sockets. Attachment mode refuses restoration;
  the mirror retains the old submitted occupancy across repeated reconstruction
  without renewing its deadline. The healthy sender is disconnected too.
- Persisting the old ledger supplies no new delivery receipt. After socket loss,
  the present wire protocol has no authenticated old-generation settlement path.
  No test sends `Input::Closed` or treats reset as release evidence.

Natural hibernation, controlled Rust reconstruction with surviving sockets, and
`ctx.abort()` are separate cases. The bridge finishes the Rust future before
aborting, using the existing host-probe workaround. Forced reset is a failure
injection, not a per-receiver cleanup strategy.

## Design consequence

The proposed ordering is useful, but connecting `flush_checkpoints` and `restore`
does not resolve the resource model. Exact persistence beyond all socket lifetimes
needs an independent durable update; attachments alone cannot provide it.
Moreover, durable counters do not make a disconnected receiver able to settle.

Before production integration, separate live connection admission, application-owned
queues, and already-submitted delivery responsibility. Define the authority and
bound for retiring an unreachable generation without claiming remote bytes were
physically erased. Keeping unreachable ledgers forever consumes the existing finite
connection/session limits; a timeout or restart is not a release receipt.
The shared kernel's strict contract remains intact in this experiment. Neither
tested mode meets all three original requirements, so the next task is this
ownership decision, before the full client flow-control cutover.

## Reproduce

From `cloudflare/`, follow the [Preview runbook](../../docs/skills/cloudflare-worker-previews-skill.md).
Use the URL returned by the **latest** deploy or secret update; secret updates
create a new immutable deployment URL.

```powershell
cargo fmt --check
cargo clippy --target wasm32-unknown-unknown --locked -- -D warnings
node checkpoint-probe.mjs <Preview-URL> all
```

The client reads `PROBE_TOKEN` or ignored `.evidence/token`, creates fresh object
names, asserts the outcomes and saves source hashes plus snapshots under
`.evidence/`. Scenarios may also be selected individually: `hibernate`,
`attachment-loss`, `disconnect`, or `reset`. Local Wrangler runs cover the same
behavior, but only Preview evidence establishes actual Cloudflare hibernation.
Cleanup deletes only the Preview created for the run.
