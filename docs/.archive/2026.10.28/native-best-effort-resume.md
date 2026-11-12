# Native best-effort resume

## Direction and trade-offs

Preserve native download progress across process restarts using ordinary Go file operations. Continuing a task skips delivered files and downloads only missing ranges of unchanged source revisions. Strong native identity remains an optional enhancement.

The current workspace already has range checkpoints and file-local failure isolation. The gaps are filesystem-dependent recovery admission and requiring completed files to remain at their original paths. Live delivery receipts already express historical completion, but expire with the operation.

| Change | Benefit | Cost or boundary |
|---|---|---|
| Portable persistence | More destinations retain expensive partial downloads; fewer platform dependencies | Basic metadata checks cannot prove local bytes were never externally modified |
| Persistent delivery receipts | Moving or renaming completed files does not trigger redelivery; completion becomes independent of ownership | Metadata storage and the crash window between publication and recording completion |
| Batched checkpoints | Protects progress without duplicate data writes or full-file rereads | Small-file metadata overhead; crashes may lose the latest unsaved progress |
| Failure isolation | Unaffected files continue through the existing shared lifecycle | Shared-state, destination, and task-exclusion failures may still pause the operation |

Continuing a download finishes remaining deliveries; moved, renamed, or deleted completed files stay delivered within that task. Fresh downloads never inherit old receipts. Revision matching for partial reuse, range bounds, task exclusion, and protection against overwriting unrelated files remain necessary.

## Execution sequence

### 1. Unify completion and progress semantics

Refactor `core/transfer`, `osfs/internal/checkpointmodel`, and `fileexecution` to distinguish:

- Historical delivery, bound to a task, logical file, and source revision.
- Reusable partial data, its location, and persisted authenticated ranges.
- Authority for a specific write, publication, or deletion.

Use one shared lifecycle, not separate strict and portable recovery engines. Persisted progress records previously accepted bytes without claiming immunity to later local modification.

### 2. Persist historical delivery

Persist delivery as the file lifecycle's terminal state. Consult task/file receipts before reopening source revisions or requiring public-file ownership; later source changes or disappearance do not revoke confirmed delivery. Release completed-object anchors only after the receipt is durable; retain receipts independently of object cleanup until the operation is durably completed or explicitly discarded.

Keep interrupted publication distinct from confirmed delivery. A crash after publication but before recording completion requires file-local reconciliation; it must not silently authorize overwriting a replacement or restarting the directory.

### 3. Make partial recovery portable

Refactor `osfs/internal/checkpointstore` and file execution to retain partials and progress through ordinary Go APIs. Use generation-tagged, checksummed records that preserve the last readable checkpoint across interrupted writes; persist data before its progress record.

Reopen partials using task/source binding, file type, size, recorded metadata, and range bounds. A changed source revision blocks only that file's partial reuse. An mtime change alone must not invalidate saved ranges: normal writes after the last checkpoint also change it. Reuse saved ranges without routine full-file hashing; preserve uncertain partials for file-local handling. Normal pause and exit retain progress; explicit discard removes owned unfinished state.

Refactor platform admission and file creation alongside `outputcap`, `destinationauthority`, and `outputruntime`, keeping recovery, publication, and cleanup guarantees independent. Make hard-link anchors and strict restart identity optional evidence for portable recovery; revisit existing hard-link, ACL, and namespace gates according to each operation's needs. Missing restart identity must not select disposable output. Retain narrow adapters for locking and safe no-replace publication; Go rename alone is insufficient.

### 4. Bound ordinary-path costs

Separate accepted in-process ranges from persisted restart ranges in the transfer contract before batching checkpoint writes. Scheduling uses accepted ranges; only persisted ranges count as restart-reusable. Flush at pause, normal exit, and completion boundaries; keep metadata memory bounded and disk updates incremental. Preserve progressive discovery and a single data object without a final full copy.

Checkpoint automatically when either an unsaved-byte limit or a time limit is reached, bounding potential retransmission. Keep small-file metadata overhead low and load receipt/checkpoint indexes on demand instead of rebuilding all completed-file state before resuming. Resume must not wait for a full directory scan or whole-file reread.

### 5. Integrate and simplify

Keep `get` automatic continuation and `resume retry` on one `engine` path. Retry reuses the operation and checkpoints. Separate automatic matching from operation identity: redownload leaves the original continuation target intact, and retry by operation ID selects either task.

Expose explicit redownload through engine and CLI: create a new operation and collision-safe destination reservation for the requested scope, with no inherited receipts. The original task retains its partials and blocked state; redownload does not settle it and may require space for both downloads. Report historical deliveries and restart-reusable progress separately; prior delivery does not assert current file presence.

Reuse file-local isolation and pause for shared failures. Trace resume, skipped delivery, checkpoint persistence, and blocked files with task/file identifiers.

Remove superseded identity gates, completed-object dependencies, and duplicate lifecycle branches. Replace internal formats where needed without adding compatibility layers.
