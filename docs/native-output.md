# Native output

Windows, Linux, and macOS share one best-effort resume lifecycle. Ordinary Go files retain partial data and checksummed checkpoints; narrow native adapters provide task exclusion and atomic no-replace publication. Recovery does not require hard links, filesystem-specific object IDs, or extended attributes.

Continuation skips files already delivered by that task, even if their public copies were moved, deleted, replaced, or the source changed. A durable delivery receipt records history, not current file presence. Receipts outlive private-object cleanup and remain until the task completes or is explicitly discarded. Interrupted publication without a receipt is reconciled for that file.

Partials carry a random ownership trailer outside their logical payload. Recovery checks that token, task/source binding, size, and range bounds; native file numbers and mtime do not invalidate saved progress. Destination namespaces and result roots retain random, checksummed identity markers; directory times and native IDs are not recovery authority. Markers are removed after completion or discard without changing source mtime. These are best-effort checks, not protection against cloned metadata or every external byte edit. Guarantees cover process restart, not power loss.

Scheduling uses accepted ranges. Restart-reusable progress includes only persisted unfinished ranges; it excludes historical deliveries. Data is synced before checkpoint metadata. Checkpoints flush after 8 MiB of unsaved data at a range boundary, after two seconds while idle, and at pause, normal exit, and completion. Ordinary continuation loads file records on demand with a bounded cache. Publication records bounded content samples, removes the ownership trailer, then moves the same object without a final copy. The samples reconcile an interrupted publication; they never authorize deletion of trailerless data. A crash in that narrow gap may leave a complete staged file that can be delivered but requires inspection before discard.

`get` automatically continues a matching task. `get --redownload` reserves a separate, collision-safe destination and leaves the original continuation target intact; both tasks may occupy disk space. `resume retry --operation <id>` selects either task. Changed revisions and uncertain partials block only affected files. Task-owned checkpoint write remnants are reclaimed automatically; discard also removes owned unfinished data, preserving published or unrecognized files.

Live-only files release their stages on delivery. On supported roots, crash cleanup drains a bounded batch at each destination bind; a backlog does not disable cleanup. Unrecognized objects are preserved.

Shared owners: `destinationauthority` (reservations), `outputruntime` (task lifetime), `outputsession` (attempt settlement), `fileexecution` (file lifecycle), `checkpointstore` (records), and `resumeauthority` (inventory and discard). The production backend is `outputportable`; platform-specific files supply native primitives within that shared implementation.

See [CLI recovery](cli.md#resume) and [validation](validation.md).
