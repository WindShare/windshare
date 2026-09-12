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

- `protocol_session_id`: Global ID linking sender and receiver protocol sessions.
- `runtime_run_id`: Unique ID for a single process execution.
- `protocol_operation_id` + `response_sequence`: Identifies an individual RPC or protocol operation.
- `attempt_sequence`: Tracks retries within an operation.

### Key Events

- `protocol_operation` & `protocol_response_send_returned`: Operation lifecycle, lane selection, and send attempts.
- `protocol_error_received`: Authenticated protocol errors (`scope`, `code`, `retryable`, `retry_after_ms`).
- `sender_session_terminated`: Session shutdown reason, failing component (`source`), error cause chain (`nodes`), and stack trace (`capture_stack`).
- `peer_attempt`: P2P / ICE connection attempts, progression stages, and disconnect causes.
- `capabilities.mode`: Filesystem capabilities (`live_only` vs restart-resumable).

## Browser Diagnostics

The browser receiver retains an in-memory ring buffer of diagnostic events and incidents.

### Capturing an Issue

1. Open DevTools console and enable capture:
   ```js
   window.windshareDiagnostics.enable()
   ```
   *(Capture stays active for 30 minutes across page reloads within the same origin).*
2. Reproduce the transfer or connection issue.
3. Export the logs:
   - Click **Connection details → Developer diagnostics → Export diagnostics** in the UI, or
   - Run `copy(window.windshareDiagnostics.export())` in DevTools to copy the JSON bundle to clipboard.
4. Disable capture when finished:
   ```js
   window.windshareDiagnostics.disable()
   ```

### Console API

| Method | Description |
|---|---|
| `enable()` | Activates diagnostic recording (30-minute window). |
| `disable()` | Stops recording and prevents capture on subsequent page loads. |
| `export()` | Returns all captured events and incidents as a JSON string. |
| `status()` | Prints current capture state, event counts, and expiry deadline. |
| `inspectLastFailure()` | Returns the most recent failure incident and stack trace. |
| `clear()` | Clears buffered logs without disabling capture. |

### Key Browser Events

- `content_scheduling`: Dispatched block requests, lane selection, standby probes, and throughput metrics (see [performance](performance.md#content-paths)).
- `operation_recovery`: Automatic retry attempts and backoff across changing network lanes.
- `protocol_operation`: Frame lifecycle states (`queued`, `sending`, `completed`, `withdrawn`, `failed`).
- `lease_retirement`: Block lease cleanup and timeouts.

## Cross-Runtime Correlation

When debugging issues between a CLI sender and browser receiver, capture both sides simultaneously. Match `protocol_session_id` from the browser diagnostic export with the sender NDJSON trace to align timelines and compare sent vs. received frames.
