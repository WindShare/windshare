# Online Authentication, Resume Evidence and Share Links

## Direction

Deliver content from the intended sender, preserve useful partial downloads and keep ordinary sharing to one link. Source-change detection remains best-effort; known inconsistencies and incomplete results must never be silently presented as successful delivery.

## Analysis and decisions

- The signed handshake and independent session keys establish sender origin for in-session messages against relays and other recipients.
- Sources with weak reopen continuity can receive a new `FileRevision` for identical bytes. The relevant boundaries are `core/content/revision_identity.go` and `osfs/internal/checkpointmodel/lineage.go`.
- Sender authentication, source comparison and local output authority are separate responsibilities. Matching metadata can justify best-effort reuse and can miss same-size edits that preserve modification evidence. Authentication establishes who supplied the evidence.

## Implementation

### 1. Separate revision identity, comparison evidence and output authority

`RevisionContinuity` defines the scope in which a `FileRevision` stays stable, which may be a single open handle. `RevisionComparison` describes source evidence for reusing saved ranges in the current file revision. Distinguish metadata agreement, observed mismatch and unavailable evidence. Source adapters interpret provider metadata. Output adapters verify local ownership and authorize writes and cleanup.

Native and browser storage already support metadata-based admission, interruption-safe checkpoint updates and recovery diagnostics. Keep the threat model, protocol and output docs aligned with these guarantees.

### 2. Centralize session authentication

Refactor `core/session/protocolsession` and `web/src/session` so business handlers receive messages only through the authenticated sender-session boundary.

Once the boundary handles all online control traffic, remove per-message control signatures, signature-only wrappers and deferred signing machinery. Preserve semantic decoding, session and lane handshake signatures, independent session keys and bindings to direction, session, lane/epoch, sequence and operation.

Keep independent authentication for the bootstrap descriptor, which precedes the session, and for relay registration, recovery and stop proofs. Remove object-authentication abstractions only when no remaining independently authenticated object needs them. Replace obsolete paths directly without bypass flags or compatibility branches. A new bootstrap protocol or offline storage system is outside scope.

### 3. Change content and recovery contracts together

Coordinate `core/session/catalogflow`, `core/session/contentflow`, `core/content/records`, native checkpoints and browser counterparts. Define the content contract and saved-state representation together so removing network signatures does not erase recovery evidence.

Before removing online catalog and file-record signatures, route those payloads through the session boundary. Define how sender-side cache reloads verify payloads against committed records and which local integrity evidence receiver checkpoints retain. Preserve record identity, file revisions, ranges, lengths, catalog generations and completion semantics. Reuse sender-cache payloads across recipients and authenticate each delivery in its destination session.

Keep candidate ranges, accepted progress and persisted recovery evidence distinct. Coordinate wire and saved-state changes across Go and TypeScript. Bind the online session version to the signed handshake and reject mismatches before operations begin.

### 4. Continue reopened files in the same task

For same-revision reconnects, keep downloading missing ranges. When a source reopens with a new revision and matching metadata, outputs with independent file checkpoints drain the old attempt and persist reusable progress. Retry the file within the same task and share using the existing checkpoint admission and a live lease for the authenticated current revision. Bound retries per file and honor user pause and cancellation. Keep discovery incremental and checkpoints batched. Use metadata comparison and existing local integrity checks for the automatic path.

Reconcile progress from admitted ranges and record the file outcome once. Preserve historical deliveries and local output authority. Report observed metadata changes separately from unavailable evidence and retain ordinary file candidates when admission fails. Direct ZIP pauses when a source reopens with a different revision. User-initiated resume applies its whole-prefix admission and member rollback rules. Trace task and file IDs, old and current revisions, the decision reason and reused or retained bytes.

## Deferred saved-range comparison

Revisit when measurements show that retransmitting rejected candidate ranges is a material cost. Changed-source recovery first needs a targeted refresh of committed file evidence and a task transition to a pinned current revision. Compare only where the read and backend costs justify the network savings. Bind matches to the file, revision and exact ranges, and check the local object and checkpoint generation at admission. Limit initial reuse to same-size, same-offset ranges and keep Direct ZIP's whole-prefix constraint.

## Deferred link shortening

Keep the current link format and its separate-key check against the bare URL. Moving the public-key fingerprint into the route would save 15 characters across the path and fragment, but would remove the key's independent binding to the share path and require a coordinated Go, browser and Rust relay change. Keep frontend trust separate from relay selection so users can use a third-party relay with a trusted frontend.

## Delivery order

Prioritize automatic metadata-based continuation within the running task. Centralize control authentication independently and address cache and recovery contracts before removing object signatures. Measure time to resume, reused and retransmitted bytes, and ordinary transfer throughput.
