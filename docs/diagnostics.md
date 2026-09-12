# Diagnostics

## Sender trace

Add `--trace-dir sender_trace --verbose` to `wind share` and keep the resulting NDJSON file.
Native NDJSON uses schema 4. Protocol observations retain `observed_at`, the source timestamp;
the envelope `time` records recorder admission. The facts have separate scopes:

- `protocol_response_send_not_started`: setup ended before any attempt. `request_kind` is absent
  when no route supplied that context; no lane or receipt is invented.
- `protocol_response_send_returned`: immutable `response_result` at the call boundary, including
  bounded `attempts`, aggregate `evidence`, call `end`, and independent `cleanup`.
- `protocol_send_attempt_settled`: final receipt evidence for an earlier pending attempt.
- `protocol_error_received`: authenticated error content and its actual receiving lane.
- `protocol_operation` and `sender_content_decision`: operation lifecycle and content decisions.

`transport_confirmed` proves transport acceptance, not peer receipt or processing. Earlier uncertain
attempts remain in history when retry succeeds. Each attempt retains its original lane, policy admission,
receipt progress, outcome, boundary `end`, and bounded `cause` (kind, detail, truncation).
`pending_attempt_sequence` links a stopped wait to a possible later settlement; it never changes the
returned result. Records can arrive in either order. Missing settlement means missing evidence.

Join responses by `runtime_run_id`, `protocol_session_id`, `protocol_operation_id`, and local
`response_sequence`; add `attempt_sequence` for an attempt. Sequences are decimal strings in the
payload. Cross-runtime `CorrelationV1` is unchanged. `protocol_error` appears once and contains
only `scope`, `code`, `retryable`, and optional `retry_after_ms`, including on sender responses.

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

`observer_loss.rejection` records source event/location/stage, rejected field/rule, and a first
sample of raw values. Invalid enum numbers and identities survive failed conversion. Association
strings are diagnostic samples, not validated business correlation. `evidence` is bounded to 16 fields,
256 bytes per value, and 4 KiB total; `truncated`, `omitted_fields`, and `omitted_bytes` report limits.

Counts are deltas: first report immediately, repeats at most every five seconds during activity,
then final flush. Signatures exclude values and IDs; 31 signatures plus an overflow counter bound
aggregation. `omitted_samples` reports overflow. `trace_summary.rejection_evidence_dropped` counts
anomaly records lost during construction, admission, or writing, separately from original observation
loss. Writer/flush failures and incomplete status remain authoritative even without a final summary.

Failed `peer_attempt` records include the last completed stage, time waiting there, observed
deadline expiry, and the primary termination cause/close initiator. Unknown ownership stays
`unknown`; later cleanup cannot relabel a remote close as local cancellation.

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

Browser incident/trace records and bundles use schema 2, with unchanged `CorrelationV1`.
Authenticated errors carry the same pure `protocol_error` content as native traces; request and
receive correlation stay in their enclosing record. Standalone incidents retain a receiver context
wrapper. Browser exports do not model the native sender response lifecycle.

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

During pre-failure recording, milestones and exceptional outcomes each receive a bounded
reservation of up to 256 events / 256 KiB, capped at one quarter of the existing window.
Routine send/write events cannot crowd out these records; newer outcomes eventually replace
older ones. Peer-attempt terminal records, cancellation, discarded late responses, and publication
outcomes receive this protection. Export before reload; clearing capture clears all reservations.

Each page has its own bounded, in-memory evidence and runtime identity. Export before leaving a
page whose evidence you need. A failure can seal that page's capture to preserve the surrounding
events; reopening within the activation window starts a fresh capture. `status()` reports the
current capture, `inspectLastFailure()` returns the last retained incident, and `clear()` removes
retained evidence without disabling activation. Manual `disable()` also prevents later pages
from restoring capture; already-open tabs retain their own capture state.

Unclassified join, projection, and save-activation incidents include `payload.unclassified.exception`
with the exception name, message, stack, and cause. Each text field is limited to 2 KiB; `null`
means no exception evidence was supplied. The snapshot does not retain the original error object.

Browser `content_scheduling` events distinguish ordinary content, standby probes, and straggler
rescues. They include session/lane identity, dispatch sequence, file/block identity, estimated
completion time, outstanding bytes, and measured throughput. Native receiver debug logs report the
same decisions as `content lane dispatched`. See [content path scheduling](performance.md#content-paths).

Browser `lease_retirement` events record shared-read deferral, idempotent release retries, confirmed
reclamation, and abandonment with a reason. Session identity, hexadecimal lease ID, and attempt count
correlate cleanup across lanes. Remote retirement has a 30-second budget; an unconfirmed release falls
back to sender TTL/session teardown and does not invalidate downloaded files. Shared reads retain
renewal until they drain, even after the departing consumer's bounded wait ends.

Browser `operation_recovery` events distinguish retrying changed lanes, waiting for availability,
waiting for a replacement session, and exhausting retries. They include the local operation sequence,
protocol session, availability revision, lane count, and any backoff. Lane changes reset the operation's
retry budget; an unchanged connection set gets two delayed retries before the original error is returned.

Browser `protocol_operation` send transitions distinguish queued, sealing, sending, completed,
withdrawn, abandoned, and failed frames. Withdrawal consumes no envelope sequence; abandonment
ends the caller's wait while the lane retains delivery ownership. An active send has a 30-second
deadline; expiry retires the lane instead of skipping a sequence. Correlate by operation and lane.

`cancelled` includes the cancellation reason and, when captured at admission, the lease ID and
block-request summary. `late_response_discarded` records the first validated late completion/error,
its original settlement, cancellation reason, and wire error without creating a failure incident.
Lease IDs use the same hexadecimal representation as sender trace. Correlate them with sender
`sender_content_decision.content_decision.kind`: `block_lease_released` proves an explicit release was remembered when the
request was rejected; `block_lease_not_owned`, `block_lease_expired`, and `block_lease_invalid`
distinguish other authority failures. Once bounded release history expires, absence alone cannot
prove why a lease is no longer owned. Local cancellation never waits for remote notification.

Failed lane transitions include `failure_detail`: bounded exception text with nested causes and
stack excerpts. Keep the sender trace from the same reproduction; `protocol_session_id` pairs
browser and sender events. Do not call `enable()` again before exporting, since it starts a new capture.
