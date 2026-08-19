# Stable Resume Revision Identity Plan

## Problem

After a clean `RevisionResumeGrace` expiry, the sender releases the stable handle and currently assigns a new `FileRevision` even when reopening reproduces the frozen catalog evidence. The receiver then misses its checkpoint, may misclassify owned output as a collision, and can report `unexpected`.

A stable handle proves continuous reads only while it is open. Reusing an identity across a handle gap is therefore best-effort evidence continuity, not a content hash or snapshot guarantee.

## Revision identity

- `FileRevision` identifies one share-scoped, verified source-evidence version. Handle and lease lifetimes control resource retention; they do not change a cleanly released identity.
- Model revision state explicitly:
  - `Active`: stable source is retained;
  - `Released`: cleanly evicted and reopenable from identical evidence; it is a logical state reconstructed from the frozen catalog and retains no handle or full per-file entry;
  - `Invalidated`: observed source drift; permanently rejected for the live share.
- After `OpenStable` and `Verify` match source identity, version candidate, size, and modified time exactly, derive from the normalized frozen catalog values:

  ```text
  RevisionEvidence = canonical(
      ShareInstance, FileID, SourceIdentity, VersionCandidate,
      ExpectedSize, ModifiedTime, ChunkSize
  )

  FileRevision = Trunc128(HMAC-SHA256(
      revisionIdentityKey,
      domain || RevisionEvidence
  ))
  ```

  Use a length-framed canonical encoding and an injected `RevisionIdentityDeriver`. Keep lease IDs random and independent.
- Generate the identity key once during live-share preparation. The live share owns it and destroys it only after RevisionStore open/read workers have drained. Do not reuse protocol encryption keys.
- Classify reopen evidence internally as `Match`, `Mismatch`, or `Unavailable` while preserving existing external typed errors:
  - `Match` transitions `Released` to `Active` with the same revision and a new lease;
  - only a completed comparison proving different evidence, or drift observed during an active read, is `Mismatch` and transitions permanently to `Invalidated`;
  - missing paths/catalog evidence, permission or transient I/O failures, cancellation, and other incomplete comparisons are `Unavailable`; they grant no lease and leave the revision `Released` for a later exact retry.
- Replace the current all-used-revision rejection with a lossless share-lifetime invalidation registry keyed by `(FileID, FileRevision)`. Charge it to an explicit share metadata budget and never ring-evict entries; inability to record invalidation stops further revision admission rather than allowing an invalidated revision to reopen.
- Changed, missing, or unverifiable evidence never creates a replacement revision for the same frozen `FileID`.
- The derivation reads no file content. Provider evidence equality retains the documented filesystem-token residual risk and is the product's best-effort resume boundary.
- Clean release does not revoke bounded cached blocks; reuse requires a new valid lease for the same derived revision. Invalidation revokes the revision cache.
- Keep `RevisionResumeGrace` and wire encodings unchanged.

## Checkpoint lineage

- `RecordID` continues to bind the immutable checkpoint identity through `OwnedObjectID`. `FileCheckpointV2` canonical bytes and checksum additionally bind lifecycle, generations, and verified ranges; range progress never changes `RecordID`.
- Add a separate local `CheckpointLineageID`:

  ```text
  CheckpointLineageID = SHA-256("windshare/checkpoint-lineage/v1" || framed(
      OperationID, ReceiveIntentDigest, MaterializationBindingDigest,
      FileID, CanonicalPath, MaterializerKind, AuthorityRef
  ))
  ```

- Freeze field framing and path/enum encoding with Go↔TypeScript vectors. Lineage is a derived logical lookup key, not a wire field or physical record identity. Native record names, IndexedDB primary keys, create/replace CAS, and crash recovery remain `RecordID`-addressed.
- Reconciliation first resolves candidate/committed crash evidence for each physical `RecordID` and preserves existing cross-record `OwnedObjectID` conflict checks, then indexes valid records by lineage.
- The repository owns lineage-aware create/CAS. The operation lease plus a short native critical section, or one IndexedDB read-write transaction spanning candidate and committed stores, atomically classifies the slot and installs the initial record. The critical section ends before owned-object creation or content transfer and enforces at most one selected authority per slot.
- Lookup returns an explicit decision: `Absent`, `Exact`, `RevisionConflict`, `OwnershipConflict`, or `Invalid`.
  - `Exact` resumes normally.
  - Multiple revisions are `RevisionConflict`; preserve their final and owned state.
  - The same revision with different sizes is `Invalid`.
  - The same revision and size with different owned objects is `OwnershipConflict`.
  - Corrupt or foreign records grant no lineage authority and remain bounded operation-attention evidence; only authenticated lineage-local invalidity becomes `checkpoint-invalid`.
- Verified ranges never move between records or revisions. Only an occupied destination with no authenticated checkpoint lineage for the requested slot is a collision.
- Derive lineage while reading existing state; do not migrate it. Conflicting old records remain explicit and safely discardable.

## Result projection

- Authenticated sender evidence change remains source drift.
- Receiver-local checkpoint revision conflict becomes `item-blocked: revision-conflict`; independent siblings continue. It is not source drift or operation-level `needs-attention`.
- Ownership conflict and authenticated lineage-local invalid binding remain item-local `owned-object-unknown` and `checkpoint-invalid` blocks.
- Reserve operation-level `needs-attention` for uncertain root, registry, lease, or operation ownership.
- Collision-only partial results report that existing destinations prevented completion; they never fall back to `unexpected`.
- Apply the same distinctions in native CLI and browser output.
- Emit safe structured traces for clean reopen, stable revision reuse, invalidation rejection, and checkpoint decisions. Include only existing safe IDs and closed enums.

## Implementation sequence

1. Add a fake-clock, network-free regression for a partial operation reopened after `RevisionResumeGrace`.
2. Refactor revision derivation and `Active`/`Released`/`Invalidated` state in `core/content`; wire key ownership through `core/liveshare` and align content cache invalidation.
3. Add `CheckpointLineageID`, explicit lookup decisions, and repository create/CAS in checkpoint model/file execution, then refactor native logical indexing and reconciliation without changing physical `RecordID` addressing.
4. Add the revision-conflict item outcome in transfer/resume authority and project precise results through native runtime and `cmd/wind`.
5. Enforce the same lineage decision and initial create inside one browser repository transaction spanning candidate and committed stores, then align browser presentation.
6. Update the normative lifecycle and residual risk in `docs/协议规范.md`, `docs/威胁模型.md`, and concise user documentation.

## Test strategy

- Prove clean grace expiry closes the old handle and releases quota while identical evidence returns the same revision with a new lease and valid cache authority.
- Prove positive mismatch remains invalid after metadata restoration; unavailable evidence remains released and may later reopen exactly. Neither path publishes a replacement revision.
- Prove invalidation revokes cached blocks while clean release does not.
- Cover exact resume, revision conflict, ownership conflict, invalid binding, real destination collision, concurrent reopen, and receiver restart.
- Prove concurrent native/browser creation for one lineage installs one authority, and store shutdown drains derivation before destroying its key.
- Cover partially verified and published siblings without moving ranges between revisions.
- Verify native and browser projections distinguish source drift, checkpoint conflict, and collision.
- Run focused content, liveshare, session/cache, osfs, transfer, CLI, and Web gates during iteration, then `make ci` before handoff.
