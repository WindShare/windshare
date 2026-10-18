# Relay host experiments

These opt-in probes investigate the `ShareRelay::submit` critical section,
checkpoint recovery and the host evidence required by `Input::Closed`.
Production adapters and the Go/browser protocol cutover remain unfinished.

The [checkpoint experiment](checkpoint-recovery.md) now connects the real kernel
to Cloudflare attachments and a SQLite mirror control. It distinguishes normal
hibernation, missing attachments, disconnect callbacks and abrupt reset. Exact
persistence does not solve unreachable-generation settlement.

The [pressure experiment](pressure.md) adds 512 MiB open/close/reset groups,
independent memory controls and real Preview memory/outcome metrics. It supports
continuing Cloudflare work, without proving physical release or a finite TTL.

The [retirement follow-up](retirement.md) reproduces exhausted admission after
consumed data loses feedback, compares explicit risk expiry on real Cloudflare,
and observes complete old bursts after more than ten minutes. Its candidate
policies remain separate from production kernel integration.

## Findings — 2026-10-17

[Initial evidence](evidence/2026-10-17.json) records native and cloud closure runs.
[Recovery follow-up](evidence/2026-10-17-recovery.json) records the Rust/JavaScript
controls and a TLS peer with an 8 KiB OS receive buffer. Both contain source hashes
and pinned versions. Timing observations are not performance requirements.

| Experiment | Observed result | Design consequence |
| --- | --- | --- |
| Native loopback, 1 or 4 healthy receivers, with/without one stalled receiver | Healthy transfers passed byte checks while the stalled writer filled its two-frame queue. `submit` P99 was 1.0–2.5 µs; receive + submit P99 was 3.5–4.2 µs. | Keep synchronous per-share submission; await capacity/socket readiness outside it. These runs do not justify finer locks. |
| Native STOP with a previously issued receipt | Late submission refused; STOP acknowledgment followed joined task cleanup. The paused peer observed TCP reset before closing its own socket. | Join both WebSocket halves and discard queues. The probe uses abortive TCP close; default drop can retain OS send buffers. |
| Real Preview, 128 × 64 KiB to a paused peer | `send()` and `close()` returned, but the five-second Alarm still saw `CLOSING`. A healthy sibling received data and pong. Resuming delivered all 128 frames. | Neither requesting close nor reaching a deadline refunds occupancy. |
| Raw `ctx.abort()` inside a Rust future | The JavaScript constructor ran again, but Rust status requests timed out. Captured error: `closure invoked recursively or after being dropped`. A pure JavaScript reset control recovered. | Corrects the initial attribution: reset happened; the Rust asynchronous callback path failed afterward. |
| Abort in JavaScript after the Rust future and storage barrier finish | Empty and stalled-peer objects recovered; replacement connections worked. Recorded reset + status round trips were 711–891 ms. | The minimal outer bridge avoids the reproduced callback failure. It supplies no buffer-release receipt. |
| Reset with an 8 KiB OS receive buffer, with and without an earlier `close()` | The new object had zero sockets. After resuming, the old peer still received all 8 MiB, then a close frame or abnormal EOF. | Empty `getWebSockets()` does not prove all old transport data disappeared. The experiment cannot locate retained bytes within Cloudflare's forwarding path. |
| Reset fault scope | A healthy connection on the same object closed; a connection on another object survived and answered ping. | Whole-object reset interrupts the whole share. It is unsuitable as routine cleanup of one slow receiver. |

The native run uses actual TCP/WebSocket writers and the real Rust kernel, with
fixture authentication, frozen kernel time and in-memory storage. It measures one
Windows process without TLS, disk, real clients, timer expiry or deployment load.
Its distributions cover completed healthy producers; the blocked producer is
checked separately. Loopback rates are not product throughput estimates.

The original close/reset Worker probe exercises the platform without `ShareRelay`.
The later checkpoint probe uses the kernel; neither establishes peak memory,
CPU cost or full WindShare transfers. Elapsed client
time includes network round trips; SQLite stores bounded metadata, never payloads.
The bounded TLS peer waits without polling, then enlarges its receive buffer to
drain. This is stronger than pausing Node's reader, but is not a platform memory
inspection or proof that all 8 MiB remained inside the Durable Object.

## Integration decision

Keep `Input::Closed` strict until the ownership boundary is explicitly redesigned.
`close()`, close callbacks, `CLOSED`, an Alarm, constructor re-entry and an empty
socket list are not interchangeable with a physical-release receipt. `Restore::restarted`
also cannot turn unknown cleanup into evidence. Repeated reconnects must not reset
old occupancy merely because its socket attachment disappeared.

The kernel now separates STOP's durable revocation and unsubmitted-queue discard
from transport settlement. `STOPPED` cannot recall bytes already handed to a
network or client, and does not refund their occupancy. The native probe now
checks acknowledgement after queue discard and before joining transport tasks;
the historical evidence above records the earlier completion boundary.
Normal `WS2B` consumption, `WS2S` settlement and proven release remain the refund
paths. Unresponsive peers retain responsibility across disconnect/restart.
The [checkpoint follow-up](checkpoint-recovery.md) demonstrates partial recovery with
one sender and two receivers: a lost attachment isolates that receiver while its sibling
continues downloading. Missing live records retain full-budget risk reservations; missing
retired records retain their retirement bound. Replacements use new identities and cannot
settle that uncertainty. The per-event SQLite mirror remains a control. Whole-share reset
still disconnects everyone and supplies no release evidence.

The bridge is a tested reset mechanism for this pinned SDK, not a production
termination policy. Source inspection suggests V8 termination interrupts an active
Wasm callback's cleanup; the exact internal failure mechanism remains an inference.
The control results and captured callback error are direct observations.

## Related upstream reports

Checked 2026-10-17; none is a confirmed fix for this probe.

- [wasm-bindgen #5379](https://github.com/wasm-bindgen/wasm-bindgen/pull/5379)
  (draft, unmerged) describes executor stalls and the same callback error after
  host termination. Its trigger is CPU exhaustion; ours is `ctx.abort()`.
  The [maintainer requests a workers-rs 0.8 retest](https://github.com/wasm-bindgen/wasm-bindgen/pull/5379#issuecomment-6224062714);
  that reporter used 0.7.5, while this probe already uses 0.8.7. A shared cause
  remains unconfirmed.
- [workerd #988](https://github.com/cloudflare/workerd/issues/988) (open) tracks
  missing `WebSocket.bufferedAmount` and backpressure support. It provides no
  buffer-release guarantee.
- [workerd #7595](https://github.com/cloudflare/workerd/issues/7595) and
  [#7566](https://github.com/cloudflare/workerd/issues/7566) (open) report sockets
  returning to `OPEN` after closing and hibernation, or delayed connection closure.
  Their triggers differ from this probe; neither explains the retained burst yet.

Next: check the 0.8.7 reinitialization path against our reset reproduction.
Production host risk policy and a real client download remain the integration work.

## Run locally

The probes are separate Cargo workspaces, excluded from ordinary gates. From
`native/`, with repository Rust prerequisites installed:

```powershell
cargo fmt --check
cargo clippy --all-targets --all-features --locked -- -D warnings
cargo run --release --locked
cargo build --release --locked --features cloud-client --bin cloudflare-slow-peer
```

The default native scenario has a 20-second bound. TLS dependencies are opt-in for
the cloud client. Socket/queue sizes are experimental, not production defaults.
The native close path uses `SO_LINGER=0`; other OSes need their own teardown evidence.

From `cloudflare/`, follow the [Preview runbook](../../docs/skills/cloudflare-worker-previews-skill.md):

```powershell
npm ci
cargo install worker-build --version 0.8.7 --locked
cargo clippy --target wasm32-unknown-unknown --locked -- -D warnings
npx wrangler whoami
$previewName = 'close-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
npx wrangler preview --name $previewName --json
npx wrangler preview secret put PROBE_TOKEN --name $previewName
```

Supply the same random test token as client `PROBE_TOKEN`, or in the ignored
`cloudflare/.evidence/token` file. A missing Worker token fails closed. Each command
below creates a fresh named object; `control` supports the empty case only:

```powershell
node probe.mjs <Preview-URL>
node reset-probe.mjs <Preview-URL> empty control
node reset-probe.mjs <Preview-URL> empty bridge-raw
node reset-probe.mjs <Preview-URL> empty bridge
node reset-probe.mjs <Preview-URL> bounded bridge close
node reset-probe.mjs <Preview-URL> bounded bridge open
npx wrangler preview delete --name $previewName
```

`bridge-raw` reproduces abort inside Rust; `bridge` moves only abort outside Rust.
`direct` additionally bypasses the Rust edge router; `probe` preserves the original
Rust-only routing. `paused` uses Node instead of the bounded native TLS peer.
Raw diagnostics stay in `.evidence/`; do not commit tokens or deployment logs.
`observed` includes negative results, not a passed release contract. Both owned
Previews were deleted after evidence capture; no production deployment was changed.

## Supporting platform contracts

- [State API](https://developers.cloudflare.com/durable-objects/api/state/), [close callback](https://developers.cloudflare.com/durable-objects/api/base/#websocketclose) and [shutdown behavior](https://developers.cloudflare.com/durable-objects/concepts/durable-object-lifecycle/#shutdown-behavior): distinguish connection events, object reset and hibernation; none supplies a physical buffer-release receipt.
- Pinned workerd [abort implementation](https://github.com/cloudflare/workerd/blob/1568dcf0a34da009f0a5c86b31d3a7f832ede97e/src/workerd/api/actor-state.c%2B%2B#L1127) terminates execution; its [socket release check](https://github.com/cloudflare/workerd/blob/1568dcf0a34da009f0a5c86b31d3a7f832ede97e/src/workerd/api/web-socket.c%2B%2B#L1241) waits for the send pump independently of `readyState`. This is not a verified deployed runtime revision.
- [Pinned js-sys future queue](https://github.com/wasm-bindgen/wasm-bindgen/blob/165586f854c852bcbc4d14d8a75a0a0e0d0abc12/crates/js-sys/src/futures/queue.rs) drives Rust futures through a shared callback.
- Windows [socket options](https://learn.microsoft.com/en-us/windows/win32/winsock/sol-socket-socket-options) and [`closesocket`](https://learn.microsoft.com/en-us/windows/win32/api/winsock2/nf-winsock2-closesocket) describe receive buffers and abortive close.
