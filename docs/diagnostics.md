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

Native traces use NDJSON schema 5. Typed product events in `engine/observation` describe readiness, progress, connectivity summaries and task settlement. Detailed diagnostics travel through an injected `core/diagnostics.Sink`, independently of product events; their source owns the event name and attributes. New diagnostic stages need no engine, wire or CLI registration.

The sink is disabled by default and enabled by `--verbose` or trace capture. Its bounded queue never waits for presentation or disk writes. Hosts close the diagnostic stream after tasks stop; task results remain available through `Wait`, even if diagnostics are dropped or the sink closes.

### Event Correlation

Correlate events across the session using:

- `scope.engine_task_id` / `scope.engine_id`: Diagnostic ownership; matches the task ID in product lifecycle events.
- `source_time`: Source observation time, or sink ingestion time when the source has no timestamp. `time` and `sequence` describe recording order.
- `protocol_session_id`: Global ID linking sender and receiver protocol sessions.
- `runtime_run_id`: Unique ID for a single process execution.
- `protocol_operation_id` + `response_sequence`: Identifies an individual RPC or protocol operation.
- `attempt_sequence`: Tracks retries within an operation.

### Key Events

- `engine_task_observed`: Typed `task_started`, `task_stopping` and `task_settled`. Settlement carries `outcome`, `failure_class`, `failure` and separate `cleanup_failure`.
- `share_workflow`, `receive_generation_changed`, `receive_admission_decision`, `receive_relay_content_claimed`: Source acquisition, session replacement and content-admission diagnostics. Join by task scope and `correlation` identities.
- `protocol_operation` & `protocol_response_send_returned`: Operation lifecycle, lane selection, and send attempts.
- `protocol_error_received`: Authenticated protocol errors (`scope`, `code`, `retryable`, `retry_after_ms`).
- `sender_session_terminated`: Session shutdown reason, failing component (`source`), error cause chain (`nodes`), and stack trace (`capture_stack`).
- `peer_attempt`: WebRTC / ICE attempts and disconnect causes; admitted attempts carry the actual `route` (`direct` or `turn`). Earlier stages do not claim a route.
- `relay_availability`: Current ready, configured, and terminal relay counts plus whether any relay was previously ready; separate from active download paths.
- `relay_recovering`: Per-relay attempt state and failure; sender details include connection generation, fast/slow waiting, resume/publication mode, terminal disposition, and next delay.
- `relay_lifecycle`: Connection and channel transitions, including heartbeat probe, acknowledgement, and failure with round, timeout, and elapsed wait (round trip for an acknowledgement, inbound silence for a failure).
- `filesystem_output`: Output ownership, `runtime_component`, `runtime_operation`, `runtime_decision`, `failure_stage` and `native_error_class`; `capabilities.mode` distinguishes live-only and restart-resumable output.
- `receive.output_recovery_decision`: Operation identity, retry/discard/exit choice, recovery reason, output mode, and verified bytes across attempts.

`Trace is incomplete` reports diagnostic loss, independently of transfer results.
`trace_summary` separates `contract_rejected` from `lifecycle_dropped`, write/flush failures,
and lost rejection evidence. Normal progress sampling does not make a trace incomplete.
`diagnostic_loss` identifies source loss; final loss totals also survive a full diagnostic queue. `observer_loss.rejection` describes product-event contract errors. Unknown diagnostic names or attributes are preserved, never treated as contract errors. Diagnostic export is an evolving debugging format, separate from the product/RPC contract.

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
   The top bar shows detailed recording or saved-recording status.
2. Click **Export diagnostics**. Choose **Share file**, **Save file**, or **Copy log**.
   File sharing appears when supported; some phones receive the same NDJSON content as a `.txt` attachment.
   Clipboard denial exposes selectable text for manual copying.
3. **Stop recording** ends capture without deleting evidence. **Hide notification** then dismisses the top
   bar; the same files remain available from **Connection details → Diagnostics** or the landing page's
   **Diagnostics** entry. Those entries also start recording for ordinary links.

Activation is scoped to the current tab and page path. Reload preserves the original 30-minute
deadline; it does not extend capture or undo a manual stop. The activation query is consumed on entry.
Failures retain incident details without ending capture; later previews, retries, and downloads
continue recording until **Stop recording** or the original deadline.

Compact failure evidence is retained even with recording off. Automatically recovered failures stay silent;
open **Diagnostics** to export them. Typed classifications keep separate immutable exception trees, operation
context, and evidence IDs. Repeated causes are counted independently of occurrence identities. Exception trees
are bounded to 8 KiB; silent history keeps at most 64 records / 256 KiB and four causes per classification.

The detailed trace ring is opt-in and bounded to 4 MiB / 4,096 events. Incident history is separate.
Exports are bounded to 8 MiB, reserve nominated trigger summaries, and report omitted evidence.
The first failure saves asynchronously; subsequent changes coalesce over five seconds within each capture.
Capture stops and page hiding flush pending evidence. Final snapshots and their retry budgets survive recording
restarts. Pending snapshots obey the archive retention limits, plus one in-flight body while its write settles.
Saves retry up to three times per snapshot. Unchanged evidence is not rewritten, retained events reuse encoded
records, and empty captures are not archived.
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
| `export()` | Returns the current run's evidence, events, and incidents as NDJSON text. |
| `status()` | Prints current capture state, event counts, and expiry deadline. |
| `activation()` | Reports whether reload can resume capture and its original deadline, independently of retained evidence. |
| `inspectLastFailure()` | Returns the most recent user-facing failure incident and its evidence. |
| `inspectFailureEvidence()` | Returns bounded failure snapshots, repeat counts, and omissions, including silent recovery failures. |
| `clear()` | Clears buffered logs and the current capture's saved snapshot without disabling capture. |

### Key Browser Events

- `content_scheduling`: Dispatched block requests, lane selection, standby probes, and throughput metrics (see [performance](performance.md#content-paths)).
- `operation_recovery`: Operation retry decisions, protocol generation, and available lanes.
- `connection_recovery`: Initial, fast, and slow waiting phases; attempt, generation, retry delay, wait reason (backoff, recovery capacity, or service cooldown), outcome, and failure context.
- `relay_heartbeat`: Per-relay probe round, acknowledgement or failure, buffered bytes, elapsed time (round trip, or inbound silence on failure), and timeout. Initial events can precede protocol-session identity.
- `protocol_operation`: Frame lifecycle states (`queued`, `sending`, `completed`, `withdrawn`, `failed`); failures link to a compact `failure_evidence` summary.
- `lease_retirement`: Block lease cleanup and timeouts.
- Block timeout `protocol_operation` events include `block_wait`: the receive phase, elapsed wait, and count of earlier-request fragments that extended queue allowance.

## Cross-Runtime Correlation

When debugging issues between a CLI sender and browser receiver, capture both sides simultaneously. Match `protocol_session_id` from the browser diagnostic export with the sender NDJSON trace to align timelines and compare sent vs. received frames. During recovery, correlate the relay identity, connection generation, and attempt before comparing protocol sessions: replacing a failed connection can create a new session while keeping the same download. `NotFound` describes current relay availability; check authentication, stop, file revision, and output failures separately.
