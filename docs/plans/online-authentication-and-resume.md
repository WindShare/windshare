# Online Authentication, Resume Evidence and Share Links

## Direction

Deliver content from the intended sender, preserve useful partial downloads and keep ordinary sharing to one link. Source-change detection remains best-effort; known inconsistencies and incomplete results must never be silently presented as successful delivery.

## Analysis and decisions

- Independent authenticated sessions already prevent other recipients and relays from injecting sender messages. Repeating public-key signatures inside those sessions adds no necessary protection under the current threat model.
- Sources with weak reopen continuity can receive a new `FileRevision` without an observed byte change; checkpoint lookup then reports a revision conflict. The relevant boundaries are `core/content/revision_identity.go` and `osfs/internal/checkpointmodel/lineage.go`.
- Sender authentication, source comparison and local output authority are separate responsibilities. A new revision does not prove changed bytes; matching bytes do not authorize overwriting or deleting an output.
- Matching metadata may authorize reuse under an explicit best-effort policy, including its risk of missed source changes. Authentication establishes who supplied the evidence; it cannot turn metadata into proof that all bytes are unchanged.
- Reuse the existing 16-byte public-key fingerprint as the URL's share identity. It authenticates the share's signing key, not a person's real-world identity; a 128-bit fingerprint does not imply 128-bit generic collision resistance.
- Separate delivery trusts both the URL and secret received from the sender; the secret alone will no longer detect URL substitution by an attacker who also knows it. Accept this narrower protection without another credential or sharing mode.
- Keep frontend trust separate from relay selection: third-party relays do not require third-party frontends. Neither link format protects secrets entered into a malicious frontend.

## Implementation

### 1. Separate revision identity, comparison evidence and output authority

Follow source reopen through `core/content`, `osfs`, session content flows, receiver continuation and native/browser checkpoint admission. Preserve resident-revision continuity during reconnects.

Evolve the existing `RevisionContinuity`, `RevisionComparison` and checkpoint models. Distinguish metadata agreement, byte agreement, observed changes and unavailable evidence, with reasons. Keep comparison evidence independent of cryptographic revision identity, bound to the file and current revision through platform-neutral contracts; leave filesystem details in adapters.

Update the threat model to distinguish authenticated origin from best-effort source continuity. Permit reuse after a fresh applicability decision while retaining content/context binding, completeness rules and independent local write/delete authority. Keep protocol and output descriptions aligned as implementation proceeds.

Implemented: independent source comparison evidence, unchanged-source metadata admission on native and browser storage, interruption-safe checkpoint updates, retained candidates, and recovery diagnostics. Metadata agreement remains best-effort; changed-source byte comparison is deferred.

### 2. Centralize session authentication

Refactor `core/session/protocolsession` and `web/src/session` so business handlers receive messages only through the authenticated sender-session boundary.

Remove per-message control signatures, signature-only wrappers and deferred signing machinery. Preserve semantic decoding, handshake signatures, independent session keys and bindings to direction, session, lane/epoch, sequence and operation.

Keep independent authentication for the bootstrap descriptor, which precedes the session, and for relay registration, recovery and stop proofs. Remove object-authentication abstractions only when no remaining independently authenticated object needs them. Replace obsolete paths directly without bypass flags or compatibility branches. A new bootstrap protocol or offline storage system is outside scope.

### 3. Change content and recovery contracts together

Coordinate `core/session/catalogflow`, `core/session/contentflow`, `core/content/records`, native checkpoints and browser counterparts. Define the content contract and saved-state representation together so removing network signatures does not erase recovery evidence.

Route online catalog pages and file records through the session boundary, then remove their redundant signatures. Preserve record identity, file versions, ranges, lengths, catalog generations, completion semantics and the local integrity evidence needed for recovery. Keep reusable sender-cache payloads independent of recipient session keys; protect each delivery through its destination session.

Carry source comparison evidence consistently through Go and TypeScript. Distinguish candidate saved ranges from accepted progress, and separate persisted recovery evidence from network object signatures. Apply wire and saved-state changes coherently across both implementations.

### 4. Implement one automatic recovery lifecycle

Prioritize ordinary continuation of the same task within the same share, without new user choices. Preserve historical deliveries, task/source binding, local output authority and durability/integrity evidence:

1. **Metadata fast path.** Matching source identity where available, size and provider modification evidence can authorize reuse under the best-effort policy. Mismatches or insufficient evidence retain saved ranges as candidates.
2. **Saved-range comparison.** When network savings justify the read cost, compare every candidate range to be reused against the current source at the same offset. Read both sides and exchange bounded batches of cryptographic digests through the authenticated session, bound to file, current revision, offset and length. An old digest alone does not establish agreement with the current source. Reuse matches and download mismatching or missing ranges.

Ship unchanged-source reopen recovery first. Changed-source salvage requires targeted refresh of committed file evidence and an explicit task transition to the newly authenticated revision; never bypass stale-catalog checks. Pin the current revision during comparison; changes or lost continuity stop the affected comparison. Show confirmed source changes; salvaged progress belongs to the current revision and cannot complete the old version.

Share recovery policy and orchestration; adapters own candidate reads, permitted writes, layout constraints and durable admission. Initially support same-size, same-offset reuse on native/OPFS backends with safe range replacement. Direct ZIP initially supports only an entirely reusable saved prefix; otherwise follow its member recovery rules. Defer arbitrary ZIP range repair, archive relayout, general catalog refresh, size-changing recovery, cross-share reuse and shifted-content matching.

Admit eligible plaintext through an interruption-safe transition; never relabel old authentication conclusions, ciphertext, completion receipts or output permissions. Preserve incremental discovery, batched checkpoints and one output object. Compute fallback digests on demand; avoid routine whole-file hashing, snapshots, extra full-file copies or per-block disk synchronization.

Use simple named work budgets and deterministic defaults when rates are unknown, accounting for both sides' reads, backend copying, write amplification and temporary space. If comparison is uneconomic, retransfer affected ranges; if a safe current source is unavailable, retain candidates and block only the affected file. Defer sampling and adaptive cost prediction until measurements justify them; sampling estimates value, never authorizes unsampled bytes or deletion.

Emit structured recovery events with task/file identifiers, stage, reason, old/current revision and compared, reused or retained bytes.

### 5. Shorten links independently

Use the existing public-key fingerprint directly as the canonical route and sender-key commitment:

```text
URL path: base64url(publicKeyFingerprint[16])
Fragment: base64url(formatVersion[1] || readSecret[16])
```

A complete link contains both. Separate delivery sends the bare URL plus the same encoded fragment as the key string; no second secret is introduced. The receiver compares the sender public key's fingerprint with the URL identity automatically.

Replace the format consistently across Go, browser parsing, Rust relay routing/registration and presentation, with explicit versioning and no compatibility branch. Keep the secret in the fragment. The encoded path and fragment total 45 characters instead of 60, excluding separators, origin and relay parameters.

## Dependencies and expected effect

Deliver step 1 and unchanged-source metadata reuse first. Refine session authentication independently before removing signatures. Add bounded salvage after defining current-revision acquisition and backend reuse capabilities. Coordinate content and saved-state contracts across Go and TypeScript; resume does not depend on signature removal. Schedule link shortening last, independently, retaining its explicit split-delivery tradeoff.

The result is fewer authentication rules, shorter links and better preservation of partial downloads. Metadata reuse retains its stated uncertainty; fallback comparison adds bounded disk and sender work when worthwhile. CPU and throughput gains from signature removal remain unmeasured.
