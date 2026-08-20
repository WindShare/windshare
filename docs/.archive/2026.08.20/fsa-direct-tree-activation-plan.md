# FSA DirectTree Activation Plan

## Context

The browser receiver can fail during initial directory activation with:

```text
FSA DirectTree action lacks a directory artifact
```

The incident in `cmd/wind/3/2.txt` records an unclassified authority-activation failure while discovery is open. Source inspection identifies the failing path: `offerArtifacts` exposes `save-directory-tree` once tree shape is proven, while the result-root layout and `ArtifactSpec` may still be unresolved. That nullable action can reach `StartedFSAReceive`, which requires a complete directory artifact.

Same-epoch projection refinements also clear the chosen action and pending activation, so ordinary discovery progress can invalidate an otherwise compatible picker.

## Target Flow

```text
user choice -> install one activation -> start one required picker synchronously
projection or session refinements -> reconcile the choice and preserve local authority
presentation authority + resolved action -> prepare a route-specific candidate
final activation fence -> commit one bound operation -> publish and adopt it
```

Candidate work has no durable side effects. A route commit returning `V2BoundReceiveOperation` is the activation linearization point. Before that return, the route owns and cleans partial work; if it has created durable state that cannot be removed, it must surface an owned recoverable or settlement authority rather than report harmless invalidation.

## Activation Invariants

- `V2AuthorityActivationCoordinator` solely owns the activation, semantic choice, authority task, abort signal, acquired authority, terminal transition, and cleanup.
- `V2OutputPresentationController` publishes derived UI state only. Selection locks, repeated-click rejection, and retained-operation admission read coordinator state instead of presentation snapshots.
- One activation starts at most one picker. No asynchronous refinement or retry may reopen it.
- `ArtifactChoice` captures user-visible semantics; `ResolvedArtifactAction` always contains a non-null `ArtifactSpec`.
- Compatible refinements, joined-object replacement, and ProtocolSession replacement preserve the choice, picker, and UI state while the authenticated share-instance identity and selection digest remain unchanged. Projection epoch and protocol session identify observations, not local output authority.
- Environment offer IDs are stable route identities. Re-probing may refresh observational facts but must not mint a new ID for the same installed route.
- An unresolved choice may resolve once per projection. A same-epoch selection-digest or resolved-artifact-digest change is a contract failure; a replacement projection may recompute resolution while preserving the semantic choice and authority.
- Selection-digest change, authenticated share-instance change, explicit cancellation, or semantic incompatibility invalidates the activation. Joined-object, epoch, or ProtocolSession replacement alone does not.
- Invalidation before route commit leaves no durable state. Failure after a bound operation is returned follows owned settlement and cleanup instead of being reported as harmless invalidation.
- A current picker or discovery task may remain pending. Invalidated activations are terminal immediately, and all late results are drained without side effects or incidents.

## Implementation

1. Replace nullable action authority with explicit types:
   - `ArtifactChoice` contains operation, artifact kind, recovery, preparation, and plan semantics without claiming that an artifact exists;
   - `ResolvedArtifactAction` contains the reconciled choice and a non-null `ArtifactSpec`;
   - acquisition returns an opaque presentation authority independent of projection and ProtocolSession lifetime, not a `DestinationReservation`;
   - a route-specific committer accepts only a resolved action and validated authority and returns either a `V2BoundReceiveOperation` or an owned recoverable/settlement authority;
   - reservations, persistence, and partial-failure cleanup remain encapsulated by the route implementation.

2. Split planning into pure operations:
   - reconcile a choice with the latest projection and environment as `waiting`, `retry-required`, `resolved`, or `invalidated`;
   - treat same-epoch selection or resolved-digest changes as typed contract failures;
   - bind a resolved action and candidate materialization binding into a `ReceiveIntent`.

3. Separate semantic compatibility from authority binding:
   - choice compatibility compares operation, artifact kind, recovery, preparation, plan kind, target kind, hard limits, and guarantee semantics;
   - stable route IDs bind authority but observational quota estimates are not choice semantics;
   - file and byte-count refinement remains compatible only while the selected plan is still offered;
   - the concrete authority and newest route facts must be revalidated immediately before commit.

4. Give each activation a stable identity containing activation ID, authenticated share-instance identity, selection digest, and semantic choice. Track joined object, projection epoch, and ProtocolSession as replaceable observation revisions. Create and install the activation before invoking the picker so reentrant or repeated clicks cannot start a second picker.

5. Move the complete lifetime into `V2AuthorityActivationCoordinator`:
   - start presentation authority synchronously from the click stack;
   - observe picker completion and artifact resolution concurrently;
   - on ProtocolSession replacement, start a replacement projection, discard stale results by observation revision, and retain the semantic choice and local authority;
   - reconcile only the newest applied planning result, including out-of-order planner completion across replacement projections;
   - represent `waiting-authority`, `waiting-resolution`, `retry-required`, `committing`, and terminal states explicitly;
   - abort, drain, release, settle, and detach through one owned terminal path.

6. Keep presentation stable during compatible refinement. The projection owner reports newest applied planning results to the coordinator; the presentation controller renders the coordinator snapshot without clearing the semantic choice or reopening the picker.

7. Split FSA acquisition from finalization:
   - `showDirectoryPicker` starts before the click handler returns and yields only the parent authority;
   - do not acquire the FSA root mutation lease while waiting for the picker, artifact resolution, or reconnection;
   - after both prerequisites are ready, authorize the parent and acquire the root mutation lease before choosing a candidate name;
   - hold that lease through route commit and materialization open, and release it on every exit;
   - immediately before commit, recheck authenticated share-instance identity, selection digest, newest observation revision, current semantic offer, exact authority binding, and abort state;
   - internal persistence failure must either clean all partial state or return an owned recoverable/settlement authority; successful commit returns the bound operation.

8. Handle retry and terminal outcomes explicitly:
   - unresolved discovery failure exposes retry instead of another actionable picker choice;
   - retry, joined-object replacement, and ProtocolSession replacement keep the activation and in-memory picker authority while authenticated share-instance identity and selection digest remain unchanged;
   - selection or authenticated share-instance change invalidates it and requires a new user click;
   - picker refusal returns to the offered action without an incident;
   - stale picker success or refusal is drained without reservation or unhandled rejection;
   - typed invariant failures use the existing output `Contract` fault classification instead of `unclassified`.

9. Emit structured traces for activation start, waiting prerequisites, retry, artifact resolution, semantic invalidation, commit start/result, and cleanup. Include activation ID, authenticated share instance, observed protocol session, selection digest, projection epoch, artifact kind, plan kind, invalidation reason, and operation ID once created.

## Likely Code Areas

- `web/src/output/planning/contracts.ts`
- `web/src/output/planning/offers.ts`
- `web/src/output/planning/binding.ts`
- `web/src/output/capability/acquisition.ts`
- `web/src/output/file-system-access/session.ts`
- `web/src/ui/v2-output.ts`
- `web/src/ui/v2-artifact-presentation.ts`
- `web/src/ui/v2-controller.ts`
- `web/src/ui/controller/authority-activation.ts`
- `web/src/ui/v2-receive-runtime.ts`
- `web/src/ui/v2-browser-receive-composition.ts`
- `web/src/ui/browser-receive/fsa.ts`
- controller observability and production trace projection

## Test Work

- Preserve synchronous single-picker behavior, including repeated clicks and synchronous picker failure.
- Resolve one choice through compatible refinements and out-of-order planner completion.
- Cover picker-first and artifact-first completion.
- Replace ProtocolSession before and after picker completion and artifact resolution; preserve the choice and authority and never open a second picker.
- Cover same-selection replacement projection and retry without reopening the picker.
- Preserve a choice across stable-route re-probes while requiring newest route facts and exact final authority binding.
- Verify the FSA root mutation lease is absent while waiting and held from name selection through commit.
- Inject invalidation before commit, during route persistence, and after commit; verify durable state and cleanup follow the defined boundary.
- Cover selection, authenticated share-instance, semantic target, guarantee, plan, and authority invalidation; joined-object, epoch, and ProtocolSession replacement alone remain compatible.
- Treat same-epoch selection-digest and resolved-digest changes as contract failures.
- Cover late picker success or refusal without reservation, intent publication, incidents, or unhandled rejection.
- Keep workspace and portable activation behavior covered while allowing clean route implementation rewrites behind the common committer contract.
- Keep pure planning tests separate from controller/authority integration tests to avoid increasing routine test time materially.

Run focused web planning and UI tests during implementation, then `make check` and `make ci` before handoff.

## Scope

This is an activation-state refactor. Rewrite route assemblies where that produces clearer ownership; do not add compatibility shims. Do not fabricate an artifact, weaken the non-null invariant, delay every picker until discovery completes, hold an FSA mutation lease while prerequisites are pending, add arbitrary picker or discovery timeouts, reopen a picker automatically, or preserve authority across selection or authenticated share-instance change. Update protocol and product clarification text where it describes final artifact choice, reconnection, or retry behavior.
