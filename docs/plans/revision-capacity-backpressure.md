# Stable revision capacity plan

## Incident

The reproduction data is in `cmd/wind/1`.

- Relay connection, direct WebRTC admission, and both content lanes succeeded.
- The receiver discovered 582 files and 6,762,858 bytes, matching the source tree.
- Progress stalled at 256 completed files, then settled at 284 completed and 298 failed.
- Failures were reported as `source/file_local/unavailable`; sender protocol operations had no transport failure.

This was a local capacity failure, not a network failure, missing source, or changed file.

## Root cause

`RevisionStore` keeps a stable file handle for `RevisionResumeGrace` after a lease ends. The same `ReleaseLease` path currently represents both an explicit receiver relinquishment and a session that disappeared unexpectedly. Rapidly settled files therefore retain handles for another 120 seconds until the share reaches `DefaultShareStableHandles` at 256.

```text
file attempt settles
  -> RELEASE lease
  -> stable handle retained for 120 seconds
  -> 256-handle share limit reached
  -> ErrQuotaExceeded
  -> quota response marked non-retryable
  -> browser records a permanent file failure
```

The design loses three important meanings:

- Explicit relinquishment: the receiver no longer owns this lease; this does not prove that output committed.
- Unexpected detachment: the receiver may reconnect and resume.
- Capacity pressure: the source is healthy but capacity is temporarily busy.

Reporting all three as an unavailable file violates the principle of least surprise.

## Product decision

Explicit relinquishment releases capacity immediately. A result proven not to have reached the receiver is rolled back immediately; ambiguous delivery and unexpected detachment record a recovery deadline even when another lease remains active. Idle recovery state may be reclaimed under real pressure, but active leases and active readers are never evicted. Durable receiver progress survives explicit relinquishment, but a later resume reopens and reverifies the live source instead of pinning its handle; it fails precisely if that source changed or disappeared. If no safe capacity can be reclaimed, every receiver waits on an authenticated retryable response instead of failing the file.

Capacity priority is:

1. Active readers and leases.
2. New transfer work.
3. Idle handles retained for best-effort detached recovery.

| Scenario | Planned experience | Cost |
|---|---|---|
| Ordinary small share | Same requests and transfer speed; lower sender resource use | No intended downgrade |
| More than 256 revisions opened within the grace window | All readable files continue transferring | Correct completion takes longer than false early failure |
| Genuine contention | Brief “Waiting for sender capacity” state | Adds bounded wait and retry traffic |
| Unexpected disconnect | Existing recovery grace remains | No change without capacity pressure |
| Recovery competes with active work | Oldest idle recovery handle is closed; reconnect reopens and reverifies | Rare resume does extra I/O and can fail if the source changed or disappeared |
| A released revision is requested again, including resume | Reopen and reverify the source | Small local I/O cost; changed or missing live sources cannot resume |

This improves the high-frequency path rather than sacrificing it for a rare case. Normal completion becomes cheaper. Under pressure, active user work wins over idle best-effort recovery state. A reclaimed revision is served again only after identity and revision verification; a changed or missing source fails precisely instead of serving different bytes.

## Architecture and execution

### 1. Give lease endings precise meanings

Refactor `core/content` around explicit lease-ending outcomes instead of an overloaded release call:

- `relinquished`: authenticated receiver `RELEASE`; it is not proof of output completion;
- `undelivered`: rollback only when the receiver is proven not to have learned the lease;
- `detached`: session loss, expiry, or ambiguous delivery that may resume;

Revision invalidation remains a separate tuple-global authority transition that revokes every lease for that revision. It is not a lease-ending outcome.

Every `detached` ending updates a revision-level recovery deadline to the latest `endedAt + RevisionResumeGrace`, even while other leases remain active. A final `relinquished` or `undelivered` lease retires the revision only when no reader and no unexpired detached recovery deadline remain. Pressure reclamation is the only path that may revoke an idle recovery deadline early.

Update every caller deliberately: authenticated `RELEASE`; initial-range, sealing, service, encoding, and proven open-result delivery rollbacks; ambiguous open-result delivery; renewal validation and delivery failures; sender-session shutdown; lease expiry; and revision invalidation. Once the receiver may know a lease, renewal or session failure is `detached` unless explicit relinquishment or invalidation proves otherwise. Do not encode these differences as Boolean flags or infer receiver output success.

### 2. Make recovery retention pressure-aware

Keep retention and reclamation inside a deep `core/content` capacity module. An injected process-scoped coordinator shared by all `RevisionStore` instances owns process/share/session accounting and the reclaimable registry; each store registers and unregisters explicitly and retains lifecycle authority. Application assembly creates one process resource owner, injects its coordinator into every prepared sender, and closes it only after all stores. Production wiring must not silently create a per-share fallback or package-global singleton.

- Keep ordinary admission and settlement O(1). Search reclaimable state only after the blocking quota boundary is identified.
- Before denying a new stable handle, atomically claim the oldest eligible revision, including idle recovery state owned by another share when the process limit is the blocking boundary.
- Reclamation uses an internal handoff claim: the victim charge remains unavailable to competitors while the requester's other quota dimensions are reserved. Coordinator and store locks are never held together.
- The victim store confirms and detaches an idle handle under its lock, then calls `Close` outside all locks. After `Close` returns, stable-file ownership must be terminal even if it reports a diagnostic error; only then is the charge transferred directly to the waiting admission, or released if that request was cancelled. A panic or ownership-contract violation cannot grant capacity. If eligibility changed before detachment, abandon the claim and retry admission.
- Never reclaim active/read revisions.
- Preserve process/share/session quota hierarchy, deterministic candidate selection, and exact accounting under concurrent admission.
- Return a typed capacity-busy outcome for stable-handle or active-lease saturation only when no safe candidate exists.

The coordinator participates only in admission and settlement, never in block reads. It exposes decisions and snapshots, not its maps, locks, or store internals. This prevents protocol and UI layers from duplicating capacity policy.

### 3. Carry transient capacity semantics across the protocol

In `core/session/contentflow`, map only typed capacity-busy outcomes to `RevisionCodeQuota` with `Retryable=true` and a bounded `RetryAfter` using existing protocol limits. Keep metadata-budget exhaustion, missing/unreadable sources, revision drift, and cryptographic seal exhaustion distinct so they cannot enter a capacity retry loop.

Carry the same product semantics through both receiver stacks:

- Go: `core/session/sessionruntime` preserves authenticated capacity type and retry hints; `core/transfer` owns accumulated receive wait, cancellation, and resumable pause.
- Browser: `web/src/content` preserves authenticated capacity type and retry hints; `web/src/transfer/job` owns accumulated receive wait, cancellation, and resumable pause.

Both stacks must:

- handle an exact authenticated capacity-busy result before generic fault normalization; never infer retry authority from a generic quota error;
- retain the current bounded file-worker slot while waiting, avoiding an unbounded retry queue;
- use a named, testable wait policy and respect sender hints, cancellation, protocol-session generation changes, and additive bounded jitter;
- pause as resumable work if the receive-level wait budget ends, without incrementing `file_errors`;
- project active capacity waiting through receive progress, and show it only after a short threshold to avoid browser UI or CLI output flicker.

Strict FIFO is not promised by stateless retry with jitter. Existing per-session quotas, bounded workers, and observable wait budgets limit monopolization; add server-issued admission tickets only if evidence later shows starvation. Small transfers never enter this path, so they gain no extra delay or round trip.

### 4. Make capacity decisions observable

Add structured traces for:

- stable-handle admission, recovery retention, pressure reclamation, and denial;
- lease settlement outcome and resulting revision lifecycle;
- used/limited/reclaimable/active capacity counts;
- receiver retry scheduling, accumulated wait, retry success, pause, and cancellation.

Reuse protocol session/operation IDs and add a stable capacity decision ID. Diagnostics must distinguish `capacity_wait` from `source_unavailable`.

### 5. Add focused regression coverage

- Core tests: settlement outcomes including ambiguous delivery, mixed active/detached leases, immediate relinquished cleanup, detach grace, cross-share process reclamation, claim/reactivation and registration/close races, cancellation-safe handoff, `Close` return before capacity grant, close contract failures, active-reader safety, and quota accounting.
- Liveshare wiring test: multiple prepared senders share one application-owned process coordinator and close before their owner.
- Content-flow tests: capacity is retryable; permanent source and seal failures are not.
- Native receiver tests: hinted backoff, cancellation, successful retry, resumable pause, and unchanged file-error counts.
- Browser tests: the same retry, pause, and accounting semantics as the native receiver.
- One synthetic cross-layer case that rapidly settles more than 256 tiny files, using in-memory stable sources to avoid slowing normal local tests.

Run focused Go and web gates during each layer, then run `make ci-parallel` after all code and documentation changes.

## Constraints

- Immediate cleanup applies only to explicit relinquishment or proven undelivered rollback, and never overrides an unexpired detached recovery deadline.
- Explicit relinquishment preserves durable receiver checkpoints, not stable-source retention; later resume must reopen and reverify the live source.
- Detachment records recovery state even when another lease remains active. Pressure reclamation may revoke only idle recovery state and can reduce best-effort resume success if the source later fails verification.
- Reclamation cannot close a handle during a read; lifecycle transition and cleanup ownership remain centralized.
- New stable-handle admission cannot receive reclaimed capacity until victim `Close` has returned and stable-file ownership is terminal.
- Only typed capacity pressure is retryable. Retrying generic quota errors risks infinite loops.
- Retry timers are cancellation-aware and cannot outlive their protocol generation.
- Browser and native receivers project the same authenticated capacity response into wait, retry, and resumable pause behavior.
- Process-wide coordination must prevent idle recovery state in one share from blocking active work in another; strict FIFO is not a product guarantee.
- Reclaimed capacity is transferred through one handoff claim and is never exposed as free between victim cleanup and claimant admission; coordinator and store locks are never nested.

Do not fix the incident by only increasing the 256-handle limit, shortening the grace period, or exposing a concurrency knob. Those changes move the threshold but preserve the incorrect lifecycle model. The refactor makes lifecycle, capacity, protocol projection, and receiver scheduling separately owned and testable.
