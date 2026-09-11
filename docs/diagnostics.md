# Diagnostics

## Sender trace

Add `--trace-dir sender_trace --verbose` to `wind share` and keep the resulting NDJSON file.
On `sender_session_terminated`, `trigger` and `provenance` remain stable classifications.
Its optional `failure` captures the winning terminal decision's error evidence:

- `source` identifies the failing runtime component or lane pump.
- `nodes` preserves error types, messages, and cause branches through `parent_index`.
- `capture_stack` records functions, files, and lines at capture time. Ordinary Go errors do
  not retain their creation stack, so this is the termination capture location.
- `truncated` marks bounded evidence; `inspection_failed` marks an error method that panicked.
  `error_available: false` means no underlying error was available at termination.

Snapshots retain at most 16 error nodes, 8 cause levels, 12 frames, and 8 KiB of text; they do
not retain original error objects. Later cleanup cannot replace the winning failure snapshot.
Use `runtime_run_id` and `protocol_session_id` to correlate it with surrounding trace events.

## Filesystem capabilities

Native output admission records `capabilities.mode` and separate support/reason facts for safe
publication, operation recovery, range recovery, and crash cleanup. `live_only` means the current
process can finish safely but cannot promise restart recovery; it is not an unsafe filesystem error.
Sender revision stages `open_handle_bound` and `open_rejected` identify sources whose revision proof
lasts only while the same file handle remains open. Reopening creates a new revision.

## Native socket handoff

Native connectivity records include `stun_refresh_finished`, `socket_handoff_started`,
and `socket_handoff_finished`. Correlate them by session, peer path, and network generation;
idle work has no ICE attempt ID. The `socket` facts report result and elapsed milliseconds,
plus local and STUN endpoints for refreshes. A canceled refresh followed by a completed
handoff means the existing socket was transferred to the next ICE owner.

## Browser diagnostics

To capture a problem that happens when reopening a share:

1. Open WindShare and run `window.windshareDiagnostics.enable()` in the browser console.
2. Open the original, complete share link and reproduce the problem.
3. Use **Connection details → Developer diagnostics → Export diagnostics**, or run
   `copy(window.windshareDiagnostics.export())` in Chromium DevTools.
4. Run `window.windshareDiagnostics.disable()` when finished.

Enabling trace records a 30-minute activation deadline for this browser profile and site origin.
Reloads and new tabs restore capture before the receiver starts, using the original deadline.
Changing the host, port, browser profile, or private-browsing context does not share activation.
If browser storage is blocked, capture works only in the current page.

Each page has its own bounded, in-memory evidence and runtime identity. Export before leaving a
page whose evidence you need. A failure can seal that page's capture to preserve the surrounding
events; reopening within the activation window starts a fresh capture. `status()` reports the
current capture, `inspectLastFailure()` returns the last retained incident, and `clear()` removes
retained evidence without disabling activation. Manual `disable()` also prevents later pages
from restoring capture; already-open tabs retain their own capture state.

Browser `content_scheduling` events distinguish ordinary content, standby probes, and straggler
rescues. They include session/lane identity, dispatch sequence, file/block identity, estimated
completion time, outstanding bytes, and measured throughput. Native receiver debug logs report the
same decisions as `content lane dispatched`. See [content path scheduling](performance.md#content-paths).

Browser `operation_recovery` events distinguish retrying changed lanes, waiting for availability,
waiting for a replacement session, and exhausting retries. They include the local operation sequence,
protocol session, availability revision, lane count, and any backoff. Lane changes reset the operation's
retry budget; an unchanged connection set gets two delayed retries before the original error is returned.

Browser `protocol_operation` send transitions distinguish queued, sealing, sending, completed,
withdrawn, abandoned, and failed frames. Withdrawal consumes no envelope sequence; abandonment
ends the caller's wait while the lane retains delivery ownership. An active send has a 30-second
deadline; expiry retires the lane instead of skipping a sequence. Correlate by operation and lane.

Failed lane transitions include `failure_detail`: bounded exception text with nested causes and
stack excerpts. Keep the sender trace from the same reproduction; `protocol_session_id` pairs
browser and sender events. Do not call `enable()` again before exporting, since it starts a new capture.
