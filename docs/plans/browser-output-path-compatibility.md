# Browser Output Path Compatibility

## Context

The receives captured in `cmd/wind/2` and `cmd/wind/4` established relay and direct lanes, then stopped at the same 20 files and 35,124 bytes. The shared tree contains `.git/hooks`, which Chromium on Windows rejects for File System Access writes.

WindShare currently lets that namespace `DOMException` escape as generic checkpoint/state I/O. Continue therefore retries the same immutable DirectTree plan and makes no progress. Receive intents and file checkpoints must remain immutable content/output authority; later user decisions belong to a separate operation-local boundary.

## Product Direction

- Preserve progressive discovery and the existing low-priority, cancellable, one-level selected-root catalog prefetch; do not add a complete-tree compatibility scan.
- Keep the ordinary successful path free of compatibility probes, extra network requests, and decision-state writes.
- On the first proven-safe path incompatibility, quiesce the run and ask the user. “Skip this item” may prompt again later; “automatically skip later incompatible paths” suppresses only later prompts in this operation. Each later rejection still requires the same safe proof and atomic omission transition; permission changes, uncertain writes, and unrelated errors still pause.
- Never silently skip, rename, or repeatedly retry an incompatible path.
- Keep DirectTree folder receive as the primary experience. For a proven-safe incompatibility, make skipping the rejected item and continuing the folder receive the primary recovery action; keep ZIP and native receive as fallbacks.
- Report exact saved-file, individually skipped-file, skipped-subtree-root, unresolved-selection, and genuine-failure counts. Do not claim descendant counts for an unvisited skipped subtree.

## Execution Plan

### 1. Prove and classify a destination-path rejection

Before finalizing the proof model, capture the exact Windows Chromium call sequence for `.git/hooks`: the File System Access method that rejects, whether an entry or handle exists afterward, writer state, committed bytes, checkpoint state, permissions, and parent/root authority.

Add a typed local destination-path rejection carrying available receiver destination-path context, relative artifact path, entry kind, namespace operation and exact browser API stage, an enumerated JavaScript exception kind covering `TypeError`, `DOMException`, and unknown values, an enumerated native error class, observed side effects, and affected scope (`file`, `subtree`, or `artifact-root`).

Classify it as safe only from post-rejection evidence, never from an error name, message, or path rule. Initially support the pre-object cut: the target entry was not created or claimed, the selected root and parent authority are unchanged, and read/write permission remains granted. If the Chromium capture shows a post-create rejection, add a second proof cut that closes any writer, proves the entry was created by this operation with zero committed bytes and unchanged handle ownership, persists a recoverable cleanup intent, removes it through the serialized mutation authority, and then proves absence. The enter-decision transaction clears that intent; recovery must reverify and finish cleanup or pause. Implement this rollback only when captured behavior requires it. Permission loss, uncertain side effects, failed cleanup, or failed authority verification remains an output-wide pause.

Keep physical path scope separate from transfer disposition. Before user approval, the rejection is `decision-required` and bypasses generic file/directory failure isolation. After approval it becomes an omission, not a transfer failure. Cleanup or authority failure still controls the safer lifecycle outcome.

### 2. Add a first-class durable decision state

Add two lease-free durable lifecycle states rather than reusing `resumable-receive`:

- `awaiting-destination-decision` restores the same prompt after reload, crash, or reconnect;
- `destination-decision-applied` records the chosen omission policy and is ready for normal resume admission.

Both follow the existing resumable-receive retention policy and bind the decision ID, rejection evidence, checkpoint-set digest and exact progress, omission-ledger generation and digest, and retention deadline.

Define one deterministic quiescence sequence: assign each mutation a monotonic admission sequence and, at the deepest serialized namespace-mutation gate, latch the earliest admitted rejection, close discovery and output admission, and advance the epoch before releasing that mutation's queue successor. This cancels accepted-but-not-entered mutations before any browser API call. Then stop this receiver operation's catalog/revision/block requests, let already-entered native mutations return, settle open output transactions, checkpoint only committed bytes, and release source revisions. Publish the prompt only after this sequence completes and visible progress is stable.

Refactor browser persistence behind a destination-decision authority. Keep all participating records in one IndexedDB database. File System Access proof and cleanup run outside IndexedDB transactions under the serialized mutation authority; the transaction awaits no browser API and consumes finalized proof evidence while checking expected record versions. Its enter-decision transaction spans receive record, manifest, receive handle and lease, checkpoint candidate and committed checkpoint, physical-file handle, and the new decision and omission stores; it records verified progress, retires a pristine candidate only after the selected proof cut completes, and changes lifecycle and lease atomically.

For a skip decision, the apply-decision transaction writes the omission policy and moves to `destination-decision-applied`; it never writes `receiving`. Resume admission must reacquire destination permission, the Web Lock, in-memory output authority, and a fresh receive lease before atomically entering `receiving`. The same user action may perform both steps when authority remains available; otherwise the operation stays truthfully ready to resume. Fallback transitions follow section 6.

Release in-memory authority and the Web Lock only after the enter-decision transaction commits; do not claim that release is part of IndexedDB atomicity. Publish the decision prompt only after both releases are verified. If enter-decision fails, remain output-wide paused and publish no decision prompt. If post-commit release cannot be verified, keep the decision non-actionable, persist a cleanup-required incident, and let recovery complete release before restoring the prompt or admitting work. If apply-decision fails, keep the same prompt, report the persistence failure, and admit no work.

Expose actions for:

- skipping the rejected file or directory subtree;
- automatically skipping later proven-safe path incompatibilities in this operation;
- ending DirectTree and starting a browser ZIP operation;
- opening the same share in the native application;
- stopping while keeping or removing only WindShare-owned partial output.

Durable decision records may retain available absolute and relative destination paths and raw browser error context for developer diagnosis. Path and exception text are evidence only and must not drive compatibility classification.

### 3. Resume through an operation-local omission policy

Persist omissions separately from the receive intent, checkpoint journal, and transfer failures. Key each omission by the authenticated parent-entry coordinate (`parentDirectoryId`, `parentGeneration`, `entryKind`, `entryId`) and carry its source and artifact paths for local presentation. A child directory generation is optional evidence, not part of the pruning key, so a restored operation can prune without fetching it.

Load the bounded ledger once when opening the operation and consult it in memory during discovery. An omission already in the ledger prunes a directory before loading its child generation and prunes a file before opening its revision. Before the first live rejection is latched, authenticated discovery may already have loaded the rejected directory's child generation or deeper generations while resolving opaque selections; do not impose or report a fixed pre-latch depth. After latching, admit no new catalog-generation, revision, or block request for the rejected scope. Preserve already authenticated evidence without fetching more data solely to reconcile the omission.

Classify an explicit target as covered only when authenticated ancestry already proves that it lies below the omitted entry. Never infer ancestry from an opaque target ID. Otherwise record it as `unresolved-by-omission`; do not fetch additional catalog generations solely to reconcile it.

Give the ledger named entry-count and metadata-byte limits. If it cannot retain another complete omission, return to the decision state and offer ZIP/native fallback; never drop evidence silently. Keep `omittedFailureCount` reserved for truncated failure evidence.

When the operation-local automatic-skip policy is active, each later matching rejection still quiesces and proves safety. Commit verified progress, candidate disposition, the new omission, and `destination-decision-applied` in one transaction. Then use normal resume admission to reacquire authority and enter `receiving` without another decision prompt. A failed proof or any other error pauses and is never converted into an omission.

### 4. Preserve the authenticated file-open invariant

Keep the current ordering:

```text
discover file -> admit parent directory -> open authenticated revision
-> create/open owned output file -> request missing byte ranges -> commit
```

A first live directory rejection may occur after authenticated discovery has already admitted work below it, but no new catalog, revision, or byte-range request is admitted for that scope after the rejection is latched. A restored omission prunes before its child-generation request. A file rejection may occur after revision open and creation of a pristine checkpoint candidate, but before block requests. Tie that candidate to the rejected entry and retire it only after the selected absence or owned-pristine rollback proof completes. Do not create speculative placeholders before source revision authority is established.

### 5. Represent incomplete results truthfully

Do not encode an approved omission as either a failure or plain success. Store the canonical fields and summaries in one durable operation-outcome record; lifecycle and worker status are projections, and the settlement receipt binds that record's digest. Refactor transfer and output outcomes into orthogonal fields:

- execution: `completed`, `paused`, `stopped`, or `superseded`;
- selection coverage: `complete`, `approved-omissions`, or `unresolved`;
- independent exact summaries for failures, omissions, and unresolved selections;
- publication: `published` only for completed execution with complete coverage and zero failures; otherwise `partial`.

Omissions and genuine failures may coexist without manufacturing either category. Persist a bounded, paged omission ledger and bind its digest and exact category counts into the settlement receipt. The final UI and explicit local export show relative skipped files and subtree roots; an unvisited subtree remains one subtree omission rather than invented descendants.

Map lifecycle to these fields explicitly: `awaiting-destination-decision` and `destination-decision-applied` are non-terminal `paused`; completing the remaining traversal with approved omissions is terminal `completed` + `approved-omissions` + `partial`; an explicit stop is terminal `stopped`; replacement by a durable fallback is terminal `superseded`; proof, cleanup, or persistence uncertainty stays `paused` and never becomes an approved omission. Reserve ordinary resumable state for interruptions that can continue the same plan without a destination decision.

### 6. Keep fallback operation boundaries explicit

ZIP fallback creates a new browser receive intent with a new operation ID and the same frozen SelectionSpec. Key the successor uniquely by source operation, decision ID, and fallback kind. In one IndexedDB transaction, create the successor's initial records and link it while marking the old DirectTree operation `superseded`; if the transaction fails, the old operation remains awaiting the same decision. Starting or restoring the successor is idempotent and always resolves to that same operation. Already saved output remains unless the user explicitly removes WindShare-owned partial state. Do not promise reuse of materialized bytes across output plans.

If the incompatible path is the artifact root, do not offer a meaningless subtree skip. Offer a new DirectTree operation with an explicitly chosen compatible root name, ZIP, or native receive. Browser-to-browser root-name fallback uses the same atomic successor transition as ZIP.

Opening the native application transfers only the share capability unless a separate authenticated selection-handoff contract exists. Do not claim that browser-local selection is preserved; require reselection or defer seamless handoff to that separate feature. Launching an external application does not prove a successor receive exists, so the browser operation remains paused until the user explicitly stops it.

### 7. Update presentation and diagnostics

Explain that the browser rejected a destination path while the sender and connection remain healthy. Remove same-plan Continue from the decision state. For whole-directory receives, present “Skip this item and continue” first; label the bulk action “Automatically skip later incompatible paths in this receive,” state that permission changes, uncertain writes, and other errors still stop the task, and warn that either skip action produces an incomplete directory. After recording a skip, automatically attempt resume admission; request permission when required and otherwise show that the operation is ready to resume. Keep ZIP and native receive as fallbacks.

Prioritize complete local developer evidence. Durable rejection records and structured diagnostics include the rejection, operation, and session identifiers; browser API stage; entry kind and affected scope; available absolute and relative paths and filenames; raw JavaScript exception type, `DOMException` name when present, message, and stack; enumerated exception and native classes; permission, handle-identity, ownership, and side-effect proof facts; decisions; and cleanup consequences. Runtime classification still uses enumerated stages and verified facts rather than matching diagnostic text.

### 8. Verify the behavior

First add a Windows Chromium call-sequence regression for `.git/hooks` that records the exact rejecting API and the before/after entry, handle, writer, permission, checkpoint, and committed-byte state. Use that evidence to select the supported proof cut before finalizing the lifecycle implementation. Cover `TypeError`, `DOMException`, and unknown exception normalization independently from the side-effect proof. Separate safe absence from permission loss, created-entry ambiguity, and ownership loss; if post-create rejection occurs, also cover successful, failed, and crash-interrupted owned-pristine rollback. Cover checkpoint-candidate disposition and crash cuts on both destination-decision transactions.

Cover concurrent rejections, cleanup failure, reload restoration, decision-applied resume admission, exact-skip and automatic-skip, ledger overflow, authenticated explicit-target reconciliation, artifact-root rejection, idempotent fallback creation, and exact settlement/export encoding. Prove a restored directory omission loads no child generation, revision, or block; capture any work admitted before a live rejection and prove no new generation, revision, or block request for its scope is admitted after latching; prove a skipped file requests no block and unrelated siblings complete.

Verify that ordinary successful receives invoke no compatibility probe or destination-decision persistence, diagnostics preserve the captured failure context, and ZIP creates exactly one successor intent from the frozen selection without requiring a new share. Update lifecycle, retention, settlement, threat-model, and product-clarification docs where their existing claims change. Keep native selection handoff outside this plan. Run focused web gates during implementation and `make ci` before handoff.

## Delivery Order

1. Windows Chromium call-sequence evidence, selected proof cuts, typed error, and complete local diagnostic capture.
2. Deep namespace-mutation gate, durable lifecycle, and destination-decision persistence authority.
3. Orthogonal outcome/settlement model and omission-ledger discovery integration.
4. Decision UI, local export, and atomic idempotent ZIP/root-name fallback.
5. Focused browser scenarios and repository gates.
