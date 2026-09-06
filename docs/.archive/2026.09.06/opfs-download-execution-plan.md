# OPFS download execution plan

## Product direction

Follow the [original requirements](../../clarifications/原始需求.md): begin transferring while discovering content, preserve substantial progress after interruptions, and avoid duplicate storage, duplicate writes, and a full-file copy after receiving completes. Browser-specific trade-offs are acceptable where the platform requires them.

Keep FSA for saving directly into user-selected locations. Make OPFS a first-class browser download path with its own storage semantics. Prioritize ordinary downloads; optional features must not impose duplicate payload storage or extra user steps on every task.

## Platform context

- OPFS supports in-place reads, writes, truncation, and independent `flush()` through `FileSystemSyncAccessHandle` in a Dedicated Worker. Its default exclusive handle can serve multiple network connections through one bounded write queue. [API reference](https://developer.mozilla.org/en-US/docs/Web/API/FileSystemSyncAccessHandle)
- `createWritable()` commits on close; reopening with `keepExistingData` involves copying existing content into temporary storage. OPFS does not need to inherit this approach. [API reference](https://developer.mozilla.org/en-US/docs/Web/API/FileSystemFileHandle/createWritable)
- OPFS remains private to the site, subject to browser quota and eviction. Exporting to the user's filesystem can still require another copy, so a retained artifact and its exported file may coexist. Removing internal copies does not guarantee single-copy disk usage through final saving. [Storage behavior](https://developer.mozilla.org/en-US/docs/Web/API/File_System_API/Origin_private_file_system)

## Execution order

Before step 1, define task-owned objects, checkpoint commits, and step 5's ZIP layout/recovery records together with step 4's capacity reservations. Keep authenticated reception, storage durability/capacity, ZIP layout/CRC, and task recovery/export as separate responsibilities. ZIP implementation remains in step 5; partial export follows the main download path.

### 1. Introduce native OPFS writing and durable checkpoints

Replace the [close-to-flush writer](../../../web/src/output/origin-private/workspace-tree.ts) with a Worker-owned synchronous access handle. Refactor the [shared file transaction](../../../web/src/output/persistent-tree/file-transaction.ts) to separate data flushing from writer closure and reopening. Share authenticated range and recovery semantics while keeping OPFS in-place flushing distinct from FSA close-and-reopen behavior.

Replace the [disabled automatic checkpoint profile](../../../web/src/transfer/settlement/persistent-execution.ts) with batched checkpoints driven by elapsed time and newly written bytes. Keep queues bounded and avoid an expensive operation for every received block. Handle short writes; accept an authenticated block as written only after its full write completes.

Keep committed payload ranges immutable for their revision. The task-owned object's coordinator establishes a common write boundary, flushes covered data, then atomically commits the corresponding ranges and format state, including ZIP CRC/layout. Entry transactions must not independently flush or close a shared handle. In-place cancellation cannot roll back writes: stop further reception, settle accepted writes, and retain the last committed checkpoint. Recovery trusts only committed ranges and may re-download unrecorded progress.

### 2. Reuse completed original files

Make the received object itself the downloadable artifact. Replace the full copy in [original-file promotion](../../../web/src/output/origin-private/package-store.ts) with an immutable artifact record referencing the same task-owned object. Seal the object against further writes and align [space accounting](../../../web/src/output/workspace/budget.ts) with this lifecycle.

Keep stable internal object identities and apply the user-facing filename during export. Task ownership remains unchanged through completion and saving; cleanup must respect active export readers. This avoids browser-specific rename support and cross-task sharing machinery.

### 3. Make local completion independent of the sender

Separate network continuation from local finalization in the task model and [retained-task controller](../../../web/src/ui/controller/retained-inventory.ts). Require a connected share only when more remote content or metadata is needed.

Persist selection discovery completion, selected paths, authenticated file revisions, durable progress, and artifact state. Receiving all currently discovered files does not prove the selection is complete. Once discovery and reception are complete, local finalization, saving, repeated downloads, and cleanup must work after reload with the sender offline. Track save attempts separately so a failed or cancelled save leaves the completed artifact reusable.

### 4. Establish incremental capacity accounting

Replace repeated full-directory scans in [workspace admission](../../../web/src/output/origin-private/workspace-root.ts) with operation-owned incremental accounting and reconciliation during recovery. Include received data, artifacts, temporary writes, metadata, and retained tasks. Track occupied bytes, outstanding growth reservations, and checkpoint/finalization headroom separately to avoid double counting.

Remove both the fixed 8 GiB per-task and 16 GiB aggregate workspace limits from admission, routing, and UI. Coordinate reservations across concurrent tasks and tabs using browser quota estimates and observed usage. Use known sizes for early routing advice and incremental reservations during discovery; quota estimates do not reserve disk space.

Reservations must cover file-length growth, including gaps from out-of-order ZIP writes. Use a capacity-bounded active-entry window: preserve normal parallelism when space permits; under pressure, prioritize filling allocated regions and limit further growth. Multiple connections may still serve one file. Leave headroom to settle admitted writes and commit checkpoints before pausing; preserve durable progress if writing or checkpointing fails. [Quota semantics](https://fs.spec.whatwg.org/#dom-filesystemsyncaccesshandle-write)

### 5. Receive progressively into a resumable ZIP

Refactor the [receive plan contract](../../../web/src/transfer/intent/plan.ts), [discovery scheduling](../../../web/src/transfer/v2-job-materialization.ts), [receive-then-package flow](../../../web/src/ui/browser-receive/workspace-packaging.ts), [ZIP layout](../../../web/src/output/zip-layout/layout.ts), [ZIP builder](../../../web/src/output/origin-private/zip-exporter.ts), and persisted recovery model around a ZIP stored directly in OPFS. Remove full-preparation and global path-order prerequisites; allocate fixed offsets in admission order while preserving path uniqueness and directory topology. Start receiving known selected entries while discovery continues.

Keep the existing uncompressed STORE encoding and support ZIP64. Allocate each entry only after authenticating its opened revision and exact size; persist its path, revision, size, and fixed offsets before writing payload. If an original revision becomes unavailable, preserve unrelated progress, identify the affected file, and offer an explicit new download of its replacement.

Write authenticated payload directly to its final ZIP offsets with bounded in-memory buffering. Preserve parallel reception across connections and files within capacity admission, without coupling network scheduling to a sequential ZIP writer or staging payload on disk for later copying.

Maintain CRC summaries and lengths for non-overlapping ranges, combine them in file order, and checkpoint them with durable ranges and layout state. Retries must not double-count ranges. Persist entry boundaries and ZIP directory state so large files resume internally; generate directory records incrementally with bounded memory. Completion should require metadata finalization without a full payload reread.

### 6. Add partial export after the main download path

Allow explicit export of fully received files from an interrupted task as a clearly labeled partial result. Generate it on demand without changing the original recovery records. Explain any additional space or copying; if necessary, pause only the affected task to protect export reads. Ordinary downloads must not retain duplicate payloads to support this feature.

## Download experience throughout implementation

Keep user actions focused on downloading a file, downloading a ZIP, or saving to a folder, with one-click downloads for ordinary small files. Select storage mechanisms internally based on browser capabilities, resumability, and total storage cost, including OPFS export and FSA checkpoint copies. Explain substantial extra space needs or save-time copying and offer alternatives before large transfers; do not turn internal stages into extra user steps.

Show meaningful receiving, finalizing, and saving progress, including partial discovery and actual retained storage. Hand completed artifacts to the browser automatically where supported, with an explicit save action when needed. Browser handoff must not be presented as confirmed saving. Pause preserves progress; deletion explicitly removes retained data.

Provide retained-task recovery and visible storage cleanup, attempt persistent storage when a real task needs it, and explain when browser storage protection is unavailable. Replace the blanket [24-hour expiry](../../../web/src/output/workspace/state.ts): preserve unfinished tasks and artifacts whose saving is unconfirmed, prompting users to choose what to delete when space is low. Apply automatic expiry only after verifiable saving or explicit user confirmation.
