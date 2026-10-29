# Native output

Windows, Linux, and macOS share one best-effort resume lifecycle. Ordinary Go files retain partial data and checksummed checkpoints; narrow native adapters provide task exclusion and atomic no-replace publication. Recovery does not require hard links, filesystem-specific object IDs, or extended attributes.

Continuation skips files already delivered by that task, even if their public copies were moved, deleted, replaced, or the source changed. A durable delivery receipt records history, not current file presence. Receipts outlive private-object cleanup and remain until the task completes or is explicitly discarded. Interrupted publication without a receipt is reconciled for that file.

Partials are reused only for the same task and source revision, after checking type, size, recorded metadata, and range bounds. Mtime alone does not invalidate progress. This avoids full-file rereads, but cannot detect every external modification of staged bytes. Guarantees cover process restart, not power loss.

Scheduling uses accepted ranges. Restart-reusable progress includes only persisted unfinished ranges; it excludes historical deliveries. Data is synced before checkpoint metadata. Checkpoints flush after 8 MiB of unsaved data at a range boundary, after two seconds while idle, and at pause, normal exit, and completion. Ordinary continuation loads file records on demand with a bounded cache. Publication moves the same data object without a final copy.

`get` automatically continues a matching task. `get --redownload` reserves a separate, collision-safe destination and leaves the original continuation target intact; both tasks may occupy disk space. `resume retry --operation <id>` selects either task. Changed revisions and uncertain partials block only affected files. Task-owned checkpoint write remnants are reclaimed automatically; discard also removes owned unfinished data, preserving published or unrecognized files.

Live-only files release their stages on delivery. On supported roots, crash cleanup drains a bounded batch at each destination bind; a backlog does not disable cleanup. Unrecognized objects are preserved.

Shared owners: `destinationauthority` (reservations), `outputruntime` (task lifetime), `outputsession` (attempt settlement), `fileexecution` (file lifecycle), `checkpointstore` (records), and `resumeauthority` (inventory and discard). The production backend is `outputportable`; platform-specific files supply native primitives within that shared implementation.

See [CLI recovery](cli.md#resume) and [validation](validation.md).
