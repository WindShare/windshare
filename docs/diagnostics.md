# Diagnostics

WindShare provides structured diagnostics across native CLI processes and browser receivers to troubleshoot transfers, connectivity, and protocol errors.

## Native Trace (CLI)

Enable trace capture on `wind share` or `wind get`:

```sh
# Trace to a specific file
wind share <path> --verbose --trace trace.ndjson

# Or write to an auto-named file in a directory
wind share <path> --verbose --trace-dir ./traces
```

Traces are recorded in line-delimited JSON (NDJSON). Sensitive secrets (link keys, credentials, payload content) are excluded.

### Event Correlation

Correlate events across the session using:

- `engine_task_id`: Native task identity linking source acquisition, session replacement, admission, and settlement.
- `protocol_session_id`: Global ID linking sender and receiver protocol sessions.
- `runtime_run_id`: Unique ID for a single process execution.
- `protocol_operation_id` + `response_sequence`: Identifies an individual RPC or protocol operation.
- `attempt_sequence`: Tracks retries within an operation.

### Key Events

- `engine_task_observed`: Native task lifecycle, source acquisition, session replacement, and content-admission decisions. Correlate share, receive-operation, job, and previous/current session identities. `task_settled` carries the authoritative `outcome` and `failure_class`; `failure` retains the selected reason, and `cleanup_failure` separately identifies resource-release faults. A stop request or finished lifecycle alone does not imply success.
- `protocol_operation` & `protocol_response_send_returned`: Operation lifecycle, lane selection, and send attempts.
- `protocol_error_received`: Authenticated protocol errors (`scope`, `code`, `retryable`, `retry_after_ms`).
- `sender_session_terminated`: Session shutdown reason, failing component (`source`), error cause chain (`nodes`), and stack trace (`capture_stack`).
- `peer_attempt`: P2P / ICE connection attempts, progression stages, and disconnect causes.
- `relay_availability`: Current ready, configured, and terminal relay counts plus whether any relay was previously ready; separate from active download paths.
- `relay_recovering`: Per-relay attempt state and failure; sender details include connection generation, fast/slow waiting, resume/publication mode, terminal disposition, and next delay.
- `relay_lifecycle`: Connection and channel transitions, including heartbeat probe, acknowledgement, and failure with round, elapsed wait, and timeout.
- `filesystem_output`: Output ownership and settlement decisions. Runtime failures are located by `runtime_decision.component` and `runtime_decision.operation`; `failure.stage` is present only when a native filesystem stage is classified.
- `capabilities.mode`: Filesystem capabilities (`live_only` vs restart-resumable).

Receiver peer and block operations publish their terminal `protocol_operation` after receive and cleanup join.
Content race losers report `receiver_ended / superseded`; peer stops may retain `canceled` or `operation_closed`.
These ordinary ends produce no failure warning. Real protocol or cleanup faults remain `receiver_failed`,
even during a local stop. For peer stops, correlate with `receiver_termination.local_stop_reason`.

## Browser Diagnostics

### Capturing an Issue

Generate a test link that records diagnostics before the receiver's first connection:

```sh
wind share <path> --browser-trace --trace-dir ./traces
```

`--browser-trace` adds `trace=1` to the link's query, before `#<key>`, including with
`--split-key`. Sender tracing remains independently controlled by `--trace` / `--trace-dir`.
You can also add `?trace=1` (or `&trace=1` when a query exists) to an existing link.

1. Open the test link or paste it into the receiver's input, then reproduce the issue.
   The top bar shows recording or retained-failure status.
2. Click **Export diagnostics**. Choose **Share file**, **Save file**, or **Copy log**.
   File sharing appears when supported; some phones receive the same NDJSON content as a `.txt` attachment.
   Clipboard denial exposes selectable text for manual copying.
3. **Stop recording** ends capture without deleting evidence. **Hide notification** then dismisses the top
   bar; the same files remain available from **Connection details → Diagnostics** or the landing page's
   **Diagnostics** entry. Those entries also start recording for ordinary links.

Activation is scoped to the current tab and page path. Reload preserves the original 30-minute
deadline; it does not extend capture or undo a manual stop. The activation query is consumed on entry.
Faults can seal the current evidence while leaving reload capture active until that deadline;
**Stop recording** remains available to turn it off without deleting evidence.

The trace ring is bounded to 4 MiB / 4,096 events. During capture, changed evidence is saved every five
seconds and on page hiding; sealed captures are saved immediately after publication. Unchanged evidence
is not rewritten, and retained events reuse their encoded records. Empty captures are not archived.
After reload, **Previous diagnostics** exports the latest saved capture for that page, with its original
run identity. An empty current run offers no file, so it cannot be mistaken for the saved evidence.
The archive admits up to three captures for 24 hours, at most 12 MiB each / 16 MiB total;
expired records are pruned on access. Startup reads only archive summaries; opening the diagnostics
panel loads the previous file on demand. The archive is separate from transfer storage.
Storage denial leaves live export available. Abrupt browser termination can lose events since the last
successful save. Download names include the timestamp and run ID and use `.ndjson`.

### Console API

The same controls are available through `window.windshareDiagnostics`:

| Method | Description |
|---|---|
| `enable()` | Activates diagnostic recording (30-minute window). |
| `disable()` | Stops recording and prevents capture on subsequent page loads. |
| `export()` | Returns the current run's events and incidents as NDJSON text. |
| `status()` | Prints current capture state, event counts, and expiry deadline. |
| `activation()` | Reports whether reload can resume capture and its original deadline, independently of sealed evidence. |
| `inspectLastFailure()` | Returns the most recent failure incident and stack trace. |
| `clear()` | Clears buffered logs and the current capture's saved snapshot without disabling capture. |

### Key Browser Events

- `content_scheduling`: Dispatched block requests, lane selection, standby probes, and throughput metrics (see [performance](performance.md#content-paths)).
- `operation_recovery`: Operation retry decisions, protocol generation, and available lanes.
- `connection_recovery`: Initial, fast, and slow waiting phases; attempt, generation, retry delay, wait reason (backoff, recovery capacity, or service cooldown), outcome, and failure context.
- `relay_heartbeat`: Per-relay probe round, acknowledgement or failure, buffered bytes, elapsed time, and timeout. Initial events can precede protocol-session identity.
- `protocol_operation`: Frame lifecycle states (`queued`, `sending`, `completed`, `withdrawn`, `failed`).
- `lease_retirement`: Block lease cleanup and timeouts.
- Block timeout `protocol_operation` events include `block_wait`: the receive phase, elapsed wait, and count of earlier-request fragments that extended queue allowance.

## Cross-Runtime Correlation

When debugging issues between a CLI sender and browser receiver, capture both sides simultaneously. Match `protocol_session_id` from the browser diagnostic export with the sender NDJSON trace to align timelines and compare sent vs. received frames. During recovery, correlate the relay identity, connection generation, and attempt before comparing protocol sessions: replacing a failed connection can create a new session while keeping the same download. `NotFound` describes current relay availability; check authentication, stop, file revision, and output failures separately.
