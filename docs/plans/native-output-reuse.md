# Native file output and recovery reuse

Make ordinary downloads available across Windows, Linux, and macOS while sharing file operations and best-effort recovery. Preserve downloaded progress and avoid a second full-file copy.

## Analysis

- Recovery orchestration already lives in shared modules: `outputruntime`, `fileexecution`, `checkpointstore`, and `destinationauthority`. Extend these modules rather than create a macOS recovery stack.
- [Platform and Directory](../../osfs/internal/outputcap/capability.go) combine live file operations with restart identity, locking, publication, and durability. This makes ordinary output depend on a broad native implementation.
- Capability selection already distinguishes resumable and live-only execution. However, [Darwin platform creation](../../osfs/posix_stability_darwin.go) rejects the destination before that selection runs.
- Basic file I/O is portable. Fixed-handle namespace operations, publication without replacement, persistent identity, locking, and durability have genuine platform differences. Those differences require narrow adapters; they do not justify separate download algorithms.
- The CLI cross-compiles for Darwin arm64 and amd64. This proves buildability, not macOS runtime correctness.

## Target design

Keep three responsibilities distinct:

| Responsibility | Owner |
| --- | --- |
| File creation lifecycle, range writes, checkpoint ordering, recovery decisions, publication orchestration, cleanup policy | Shared filesystem modules |
| Live handles, identity observations, locks, namespace mutations, filesystem durability primitives | Platform adapters with narrow contracts |
| Destination capability selection and user-visible execution state | Shared output runtime and platform-neutral engine contracts |

Use Go file APIs where their guarantees match the operation. Preserve anchored directory/file handles where needed; do not replace them with unchecked path operations.

Ordinary output requires safe publication and content integrity. Restart recovery adds independently established capabilities. Assess the actual destination filesystem; platform names and certification labels alone must not decide whether a download can run. Keep `core/**` and shared engine code free of native filesystem dependencies.

## Execution

1. **Separate live output from recovery capabilities.** Refactor capability selection and consumer interfaces around `outputcap`, `destinationauthority`, and `outputruntime`. Distinguish unsupported capabilities, temporary I/O failures, and object-local uncertainty. Safe new downloads may run without restart recovery. Existing operations must remain visible with blocked reasons when necessary, never silently become fresh downloads. Cleanup uncertainty must not revoke independently proven recovery.

2. **Consolidate file operations and narrow native adapters.** Share lifecycle, error classification, and I/O wrappers where guarantees match; retain platform-specific creation/rollback, identity, locking, and atomic no-replace publication. Use the [macOS runner evidence](https://github.com/WindShare/windshare/blob/c766f2f1eda1462ef61344875f324f3bbfba3b7e/spikes/native-output-darwin/README.md) as the APFS starting point for identity, atomic no-replace publication, and process-restart recovery; keep object identity separate from content-change counters. Migrate Linux and Windows without weakening recovery, then integrate macOS through those contracts. In `checkpointmodel` and `outputprobe`, separate runtime capabilities from versioned identity and cleanup evidence. Runtime probes alone cannot prove restart identity or durability; replace certification gates with explicit evidence contracts. Never emulate atomic no-replace publication with plain rename or check-then-write.

3. **Reuse one recovery pipeline.** Keep checkpoint interpretation, range accounting, recovery decisions, and cleanup policy in shared modules; adapters supply object evidence. Let output operations outlive transfer attempts, retaining live handles and verified ranges; distinguish process-local progress from restart checkpoints in core transfer contracts. Release live resources on completion, explicit discard, or process exit. Preserve data-before-checkpoint ordering and verify file/revision binding; path or size alone is never proof. Separate retryable failures from unprovable ownership. Retain partial data and isolate file-local failures; root or registry uncertainty may block dependent operations. Provide retry where evidence permits and explicit discard of proven-owned state without resetting unrelated progress or deleting unknown objects. Prioritize macOS process-restart recovery on local APFS; keep live-only as an interim milestone or capability-based fallback. Represent process-restart and power-loss guarantees separately.

4. **Integrate and remove superseded paths.** Route CLI `get` and `resume` through shared execution and recovery inventory. Carry typed recovery reasons and available actions through engine contracts. Keep paused operations alive for interactive retry, discard, or exit, including publication collisions. Ctrl+C exits while preserving restart checkpoints; non-interactive runs return an explicit outcome instead of waiting for input. Select the strongest supported recovery automatically, explain live-only exit consequences before transfer, and show blocked items. Keep one data copy and bounded metadata, without full-copy completion or silent duplicate downloads. Extend existing traces with operation identity, capability decisions, recovery rejection reasons, and affected ranges. Remove superseded orchestration and certification gates while retaining identity validation and native semantic tests.

5. **Validate the semantic boundaries.** Reuse existing focused suites and adapter contracts for capability loss with existing progress, pause/retry, interrupted writes/checkpoints, collisions/replacement, restart recovery, and publication without a full copy. Run applicable repository gates. Verify macOS file/folder downloads and process-restart recovery on GitHub Actions macOS runners, recording the actual filesystem, plus Darwin arm64/amd64 builds. Keep native tests focused on platform semantics; do not duplicate the recovery suite.

Scope: native receive/output and recovery. Sharing, P2P policy, browser storage, installers, and release signing remain separate work.
