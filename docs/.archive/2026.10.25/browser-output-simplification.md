# Browser Output Simplification

Unify shared task semantics and the meaning of reusable progress while keeping route capabilities and storage-specific commit behavior explicit. Preserve progressive receiving, useful large-file recovery, and reasonable disk usage.

## Current assessment

- Control differences are real: folder downloads offer pause and stop; Direct ZIP and workspace receiving offer pause; portable downloads offer stop.
- Most recovery metadata already shares the output IndexedDB database. The other output databases serve capacity accounting, filesystem-handle coordination, and ZIP metadata spooling. Database count alone does not explain recovery complexity.
- File checkpoints, artifact construction, delivery receipts, and capacity reservations describe different facts. Some coordination can be shared, but a single range bitmap cannot replace them.
- The three ZIP paths already share encoding and layout primitives in `output/zip-layout/policy.ts`. Their remaining differences include sequential writing, random-access writes, and durable recovery.
- Compatible-name handling activates after a name is rejected. Its maintenance cost is substantial, but ordinary downloads do not create its ledger. Code size is not evidence of a large runtime penalty.

The architectural opportunity is to give each fact one owner and reuse common lifecycle rules, instead of allowing route implementations to redefine them.

## State ownership

| Domain | Authoritative meaning |
|---|---|
| Task | Selection, receive intent, pause, continuation, termination |
| Reusable content | Authenticated source and revision, local replica location and ownership, committed ranges and generation |
| Artifact | File or ZIP layout, directory structure, committed output position, completion state |
| Delivery | Saved destination evidence or browser-download handoff |
| Capacity | Reservations and ownership of temporary storage |

Use one content-availability contract across routes. Source identity includes the share instance, FileID, and FileRevision; availability also depends on the local replica still being accessible and valid. Keep received, written, and durably reusable progress distinct.

Evolve `OutputSourceIdentity`, `VerifiedDurableRanges`, and existing checkpoints into this contract; do not add a second have-state index. Shared lifecycle rules belong in `transfer/**`, orchestration in `application/**`, and storage evidence and commit execution in `output/**`.

Commit checkpoints and corresponding lifecycle changes in one repository transaction; shared orchestration must preserve existing atomic commits. Filesystem writes remain a separate boundary, requiring ordered commits and reconciliation after interruption.

## Implementation order

1. **Define shared interruption contracts.** Define task terminal states, retention rules, and storage settlement results before extending the existing application lifecycle. Pause stops new receiving work, settles accepted writes, and retains continuation state. Where supported, stop ends the task, preserves delivered results and complete results awaiting delivery, and releases only abandoned incomplete temporary data. Retained results remain available to save or explicitly delete; failed cleanup stays retryable. Expose only controls supported by each route. Remove route-owned task transitions as shared orchestration takes over.

2. **Separate content evidence from artifact state.** Refactor `FileCheckpointV2`, OPFS task entries, and Direct ZIP member evidence around the common content contract. Preserve backend-specific object identities, ZIP offsets, and completion records. Keep checkpoint cadence and prefix-copy/write-amplification budgets backend-specific. Derive progress and continuation decisions from those authoritative facts, retiring duplicate owners as the shared contracts take over. If canonical checkpoint encoding changes, update Go/TypeScript implementations and shared vectors together.

3. **Consolidate persistence coordination.** Reuse repository commit, ownership, retirement, and cleanup mechanisms across routes. Keep capacity reservations distinct from content possession. Put operation, artifact, and checkpoint identifiers into shared lifecycle traces so interrupted writes and cleanup can be reconstructed without route-specific interpretation.

4. **Reduce ZIP orchestration duplication.** Build on the existing encoder and layout primitives. Share member accounting and completion logic where their semantics match; keep sequential FSA commits and concurrent random-access OPFS writes with file-level failure isolation behind explicit storage contracts. Preserve streaming discovery and incremental output.

5. **Simplify exceptional naming and presentation.** Separate durable name mappings from restoration-script generation. Keep repair activation local to rejected names; script availability must not gate compatible-name saving. Retain result-oriented choices and default-route ranking; redesign presentation independently of the current UI. Describe the OPFS workspace option as better suited to long-running downloads because it preserves recoverable progress, alongside its final-export copying and space costs.

## Product trade-offs

Keep Direct ZIP's existing pause, continuation, explicit deletion, checkpoint policy, and recovery behavior. Adding stop or partial-ZIP export to Direct ZIP is outside this refactor. Preserve existing folder stop retention. Sharing a directory picker with folder output does not make the resulting artifacts interchangeable.

Preserve OPFS pause, continuation, partial export where supported, and explicit deletion. Partial ZIP export requires `showSaveFilePicker`, writes completed members to a new destination, and leaves the original archive resumable until explicitly deleted. Keep retained space visible. Do not add stop: ending continuation would not reclaim the shared archive.

Prioritize simplifying compatible-name restoration machinery over removing the ability to save rejected names. Retain clear mappings, reports, and automated name restoration through the existing script or an equivalent tool; offer an appropriate ZIP alternative when useful. Archive name preservation does not imply that every destination filesystem can later extract those names unchanged.

A common metadata model should preserve direct writing and bounded staging. Requiring every download to finish in OPFS before export would add copying, space pressure, and waiting. Likewise, shared ZIP logic should not require discovering the whole directory first.

There is no usage evidence establishing feature frequency yet. Judge simplification against the product's main experience: starting quickly, continuing large transfers, saving complete results, and avoiding unnecessary disk usage. The expected developer benefit is fewer independent lifecycle and recovery rules; user gains should come from consistent controls and reliable continuation, not merely fewer implementations.
