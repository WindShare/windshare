# FSA Checkpoint Admission Refactor

## Context

Browser FSA DirectTree currently attempts an automatic checkpoint after 64 MiB of pending data or 30 seconds. Reopening the next writer with `keepExistingData` may copy the durable file prefix because FSA exposes no authoritative capacity reservation.

The current policy prompts from the active transfer path and applies the same limits independently to every file. A directory can therefore multiply temporary-space and write-amplification costs, while the prompt describes them as task-wide. Best-effort recovery must not interrupt or impose unbounded cost on ordinary receives.

## Product decision

- FSA DirectTree has one built-in recovery policy. It adds no setting, mode, or pre-transfer choice.
- Ordinary receives never prompt or wait for checkpoint capacity from the active write path.
- Automatic checkpoints use one conservative transfer-job budget. Exhaustion stops further automatic checkpoints without affecting transfer completion.
- Explicit pause retains the existing choices: continue while preserving partial files, or restart only operation-owned incomplete files. The pause view shows both choices' byte and temporary-space consequences; choosing preserve is the complete consent and does not trigger per-file confirmations.
- Completed files, ownership checks, durable checkpoint CAS transitions, and restart isolation remain unchanged.
- Browser hard termination remains best-effort: bounded write and space overhead take priority over guaranteeing a small restart interval.

## Target architecture

Separate checkpoint scheduling, automatic admission, shared preserving-writer capacity, and user-authorized paused recovery.

`CheckpointSchedule` is a pure per-file policy over durable, newly pending, and remaining bytes. It returns `wait-for-progress(nextPendingBytes)`, `checkpoint-now`, or `finish-without-further-checkpoint`. The required pending advance is `max(64 MiB, durablePrefixBytes)`, producing useful automatic cuts at 64 and 128 MiB; the next 256 MiB preserving open exceeds the per-open ceiling. The 30-second trigger only requests evaluation and cannot bypass the byte floor or the returned next threshold. A cut is not useful when the remaining bytes are no greater than the resulting preserving-open prefix-copy cost; the writer finishes without further automatic evaluation.

`AutomaticCheckpointAdmissionAuthority` is created once per transfer job and owns the fixed automatic budget and aggregate accounting. It survives lane reconnects and writer reopens; only an explicit new start or continue attempt creates another authority. The limits are:

- 128 MiB maximum prefix copy for one preserving open;
- 2 GiB maximum cumulative write amplification for the transfer job, enough for all eight active native writers to reach the 64 and 128 MiB cuts;
- 1 GiB maximum aggregate temporary-space reservation across concurrent writers, covering eight simultaneous 128 MiB preserving opens.

When budget pressure requires a choice, a file's first useful checkpoint is admitted before later checkpoints for other files, then the second before the third. FIFO order breaks ties. This priority changes admission only and never makes the active write path wait.

`PreservingWriterCapacityAuthority` owns reservations shared by automatic and paused recovery opens. Automatic replacement uses a non-blocking `tryHandoff`: unavailable capacity defers the cut while the current writer remains open and the transfer continues. User-authorized paused recovery uses abortable FIFO reservations, is not charged to the automatic prefix-copy or cumulative budget, and runs a prefix above 128 MiB exclusively rather than rejecting it by an automatic limit.

Automatic capacity contention is transient and never becomes a sticky decline. Immutable cost and exhausted cumulative budget are sticky. Cancellation or terminal drain removes paused-recovery queue entries.

The accepted small-file performance boundary remains unchanged: Windows Chromium keeps 15 file pipelines, 8 native writers, and 3 initial-claim inspections. Initial `truncate` writers bypass both checkpoint authorities, and files that finish below the 64 MiB floor perform only their existing final close and commit. Schedule and admission observations are emitted only on threshold or decision transitions, not per write.

An admitted preserving open returns an owned reservation token. An automatic cut first obtains a tentative budget hold and an immediately available capacity handoff; otherwise it keeps the current writer open and returns deferred. Closing the current writer releases its capacity and native-writer lease before the replacement opens, so old and next capacity reservations never coexist. Prefix-copy and cumulative-write costs commit only after the replacement opens successfully. A replacement-open failure rolls back tentative accounting, releases reservations, preserves the latest durable checkpoint, and requests an operation-level resumable pause carrying the exact path and modeled cost. Failed, unused, retired, and terminal transitions release queue entries, budget holds, and capacity reservations deterministically.

`PersistentExecutionRecoveryPolicy` describes only the paused-file choice. `PersistentFileTransaction` executes writer and checkpoint transitions but does not own attempt policy or accounting. Pause settlement persists the authoritative selection facts needed after reload: discovered file count, discovered bytes, and discovery state, bound to the lifecycle generation and checkpoint-set digest. `RecoverySummary` is derived from those facts and the matching validated checkpoint snapshot rather than persisted as another source of truth. It reports completed bytes, verified partial bytes, bytes remaining when preserving, bytes redownloaded when restarting, maximum preserving-open temporary bytes, and whether discovery is complete. Incomplete discovery is presented as known-so-far data.

Structured observations include the receive operation, transfer job, output session, file path, trigger, cost, remaining budget, queue decision, and release reason.

## Execution sequence

1. Remove `PersistentTemporarySpacePurpose`, `bindPersistentTemporarySpaceConfirmation`, `confirmTemporarySpace`, and `globalThis.confirm` from FSA recovery. Reduce `PersistentExecutionRecoveryPolicy` to the paused-file decision.

2. Remove the per-call checkpoint budget from `OutputFileTransaction.automaticCheckpoint`. Keep trigger configuration separate from the transfer-job admission authority so there is one budget source.

3. Introduce the pure three-outcome checkpoint schedule with the exact pending-byte floor and remaining-work rule above. `wait-for-progress` supplies the next evaluation threshold; `finish-without-further-checkpoint` is terminal for that file.

4. Introduce the transfer-job-scoped automatic admission authority and the shared preserving-writer capacity authority with first-before-later checkpoint priority, non-blocking automatic handoff, abortable FIFO paused recovery, commit, release, cancellation, terminal drain, and sticky exhaustion decisions.

5. Route preserving opens through owned reservations. Replace `#nextPreservingOpenApproved` and per-file cumulative accounting, settling the reservation on every writer-open, close, abort, commit, pause, retire, and error path.

6. Bind one automatic authority and one capacity authority when creating the FSA DirectTree execution. Coordinate reservation order with the native-writer concurrency limit and retain them across lane reconnects in the same transfer job.

7. Persist the pause-time selection facts with their lifecycle generation and checkpoint-set digest. Derive `RecoverySummary` from the matching validated checkpoint snapshot and show one pause-level comparison before either recovery action. Do not add modal or per-file confirmations.

8. Treat “Continue and preserve partial files” as user authorization outside the automatic budget. Serialize an oversized preserving open. A native capacity failure during either automatic replacement or authorized recovery leaves the task paused with the exact file and modeled cost. “Restart incomplete files” keeps the existing exact-handle and IndexedDB CAS reset without touching completed files.

9. Rewrite fixtures around zero small-file admission/reservation work, retained 15/8/3 concurrency, transfer-job totals, exact sparse scheduling, first-before-later checkpoint priority, eight-writer large-file amplification bounds, lane reconnects, non-blocking automatic contention, full-capacity replacement handoff, paused-recovery FIFO and oversized exclusivity, reservation ownership, resumable writer-open failure, cancellation, terminal cleanup, recovery summaries after reload including discovered-but-unstarted files, and the absence of modal prompts. Remove tests for cached consent and per-file budgets.

## Expected user experience

Normal receives have no recovery settings or checkpoint dialogs. Automatic recovery remains bounded, never waits for preserving-writer capacity, and checkpoint I/O becomes progressively sparser. The budget covers all eight active native writers through the 128 MiB cut, while first-checkpoint priority prevents later cuts from consuming protection needed by another eligible large file. Capacity contention or budget exhaustion never blocks completion or resets because a lane reconnects.

On pause, users keep the existing preserve-or-restart actions without follow-up confirmations. The UI reports the known completed and verified partial bytes, each choice's remaining receive bytes, and the maximum modeled temporary space for preserving. Native space failure retains every checkpoint and identifies the blocked file.

## Follow-up TODO

- Revisit a separate opt-in FSA chunk-staging plan for very large files only if user demand justifies the extra disk space, approximately doubled writes, and final assembly wait. It must not become a flag inside DirectTree or change its default space-first behavior.

## Non-goals

- User-selectable recovery modes or budgets.
- Guaranteeing a small restart interval after browser or operating-system termination.
- Implementing large-file chunk staging or final assembly in this refactor.
- Changing checkpoint ownership, lineage, or IndexedDB transaction semantics.
- Redownloading completed files.
- Adding background uploads or server-side persistence.
- Protecting the current pre-v1 policy types for compatibility.
