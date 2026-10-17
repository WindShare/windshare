# Relay host experiments

These opt-in probes investigate the `ShareRelay::submit` critical section and the
host evidence required by `Input::Closed`. They are separate Cargo workspaces;
production adapters and the Go/browser protocol cutover remain unfinished.

## Findings — 2026-10-17

[Recorded evidence](evidence/2026-10-17.json) includes source hashes, exact versions
and bounded runs. Measurements are observations, not performance requirements.

| Experiment | Observed result | Design consequence |
| --- | --- | --- |
| Native loopback, 1 or 4 healthy receivers, with/without one stalled receiver | Healthy transfers completed with byte checks while the stalled writer filled its two-frame queue. `submit` P99 was 1.0–2.5 µs; receive + submit P99 was 3.5–4.2 µs. | Keep the per-share synchronous submission boundary. Await capacity/socket readiness outside it; there is no evidence here to justify finer locks. |
| Native STOP with a previously issued receipt | Late submission refused; STOP acknowledgment followed joined task cleanup. The paused peer observed TCP reset without first closing its own socket. | Cancel and join both WebSocket halves and discard queues. This probe uses abortive TCP close; default socket drop can keep sending OS-buffered bytes. |
| Rust Worker on a real Preview, 128 × 64 KiB to a paused peer | `send()` and `close()` returned; the peer remained `CLOSING` when the five-second Alarm fired. A healthy sibling received its eight frames and pong. Resuming the slow peer delivered all 128 frames before closure. | A close request or deadline cannot refund outstanding occupancy. |
| Worker close callbacks and `readyState` | Callbacks reported `CLOSED`; `bufferedAmount` was unavailable. No public buffer-release receipt was observed. | Do not translate these events directly into `Input::Closed`. |
| Worker `ctx.abort()` escalation, paused and empty-object controls | HTTP 500, followed by four bounded status-query timeouts; fresh objects in the same Preview responded. The paused case also closed the healthy sibling. | Reset recovery is unresolved in this workers-rs/Preview setup. The empty control prevents attributing the failure to buffered payloads. Reset is not established as isolated or successful cleanup. |

The native run uses actual TCP/WebSocket writers and the real Rust kernel, but
fixture authentication, frozen kernel time and in-memory storage. It measures
one process on Windows, without TLS, disk, real clients, timer expiry or deployment
load. Its distributions cover completed healthy producers; the blocked producer
is checked separately. Loopback rates are not product throughput estimates.

The Worker probe exercises the platform, without loading `ShareRelay`. It does
not establish attachment recovery, peak memory, CPU cost or WindShare transfers.
Its elapsed client time includes network round trips; Workers' `Date.now()` is
not a CPU timer. SQLite stores only bounded diagnostic metadata, never payloads.

Cloudflare integration still needs a supported termination/release contract, or
an explicit redesign of resource ownership and cleanup. Keep unsettled charges
and STOP fencing until evidence exists; waiting an arbitrary interval is not a
release receipt. This is a platform-contract gap, not proof that Cloudflare
cannot implement the product. Normal `WS2B` consumption and `WS2S` settlement
remain the existing way to reclaim acknowledged occupancy.

## Run locally

From `native/`, with the repository Rust prerequisites installed:

```powershell
cargo fmt --check
cargo clippy --all-targets --locked -- -D warnings
cargo run --release --locked
```

Each native scenario has a 20-second bound. Socket and queue sizes are experimental
settings, not production defaults. The close path uses `SO_LINGER=0`; porting its
release claim to another OS requires checking that OS's teardown semantics.

From `cloudflare/`, install Node dependencies, `wasm32-unknown-unknown`, and the
pinned builder, then follow the [Preview runbook](../../docs/skills/cloudflare-worker-previews-skill.md):

```powershell
npm ci
cargo install worker-build --version 0.8.7 --locked
cargo clippy --target wasm32-unknown-unknown --locked -- -D warnings
npx wrangler whoami
$previewName = 'close-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
npx wrangler preview --name $previewName --json
npx wrangler preview secret put PROBE_TOKEN --name $previewName
```

Supply the same random test token to the clients as `PROBE_TOKEN`, or through the
ignored `cloudflare/.evidence/token` file. A missing Worker token fails closed.
Use the returned Preview URL; each run creates a fresh named object:

```powershell
node probe.mjs <Preview-URL>
node reset-probe.mjs <Preview-URL> empty
node reset-probe.mjs <Preview-URL> paused
npx wrangler preview delete --name $previewName
```

Raw diagnostics go to `.evidence/`; do not commit tokens or deployment logs.
`observed` means the experiment completed, including negative platform results;
it does not mean the physical-release contract passed. Save the evidence before
removing the owned Preview. The 2026-10-17 Preview was deleted after capture.
These probes are excluded from ordinary gates and add no routine test runtime.

## Supporting platform contracts

- [Cloudflare close callback](https://developers.cloudflare.com/durable-objects/api/base/#websocketclose) and [state API](https://developers.cloudflare.com/durable-objects/api/state/): closure state and object reset do not specify a physical buffer-release receipt.
- [Pinned workerd source](https://github.com/cloudflare/workerd/blob/1568dcf0a34da009f0a5c86b31d3a7f832ede97e/src/workerd/api/web-socket.c%2B%2B#L1241): the legacy adapter's release check waits for its send pump independently of `readyState`; this source inspection is not a guarantee about the deployed runtime revision.
- [Windows `closesocket`](https://learn.microsoft.com/en-us/windows/win32/api/winsock2/nf-winsock2-closesocket): zero-time linger requests an abortive close and discards unsent data.
