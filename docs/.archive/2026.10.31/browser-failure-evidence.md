# Browser Failure Evidence

Preserve the concrete reasons behind transfer failures while keeping recovery decisions explicit and normal sharing and downloading lightweight.

## Analysis

- Some typed failures lose their original message and cause in `web/src/transfer/job/failures.ts`. `ClassifiedTransferFailureError` then carries a generic message. Increasing trace capacity cannot recover evidence already discarded here.
- `FailureFact` stores exception snapshots only under `unclassified`. Whether a failure is understood and whether diagnostic evidence is available are independent concerns.
- Existing incident scopes, fact references, trigger/contributor/consequence records, priority retention, lane failure details, and local output evidence provide a useful foundation.
- Aggregation and bounded retention protect browser resources. Their limitation is preserving too few distinct explanations within one product classification. The first observed failure is not necessarily the root cause.

The diagnostics module owns evidence identifiers, storage lifetime, aggregation, and eviction. Business code supplies the cause and relevant context at the boundary that observes a failure. Keep this bookkeeping behind diagnostic ports; if ordinary business callers must manage evidence identifiers, storage lifetime, or eviction, revise the abstraction.

## Implementation sequence

1. **Make evidence independent of classification.** Refactor `web/src/diagnostics/incident/fact.ts` and the exception snapshot model so typed facts can retain diagnostic evidence without becoming `unclassified`. Capture the originating operation, message/stack, `cause` and `AggregateError.errors` branches, relevant identities, and necessary state. Use immutable snapshots bounded by text, depth, branch count, and total bytes; detect cycles and mark unavailable or truncated information.

2. **Preserve evidence across failure boundaries.** Update `web/src/transfer/job/failures.ts` and `web/src/transfer/failure/classification.ts` to preserve evidence before normalization discards the original error. Reuse captured evidence through wrapping, fault promotion, and combination. Reuse fact references only within their owning scope; diagnostic ports create a new scoped reference when attaching shared evidence elsewhere. Recovery decisions continue to depend on closed fault values; diagnostic evidence carries no retry, cancellation, or termination authority. Go's `failurestate.Failure` provides the corresponding separation.

3. **Connect failure events to their explanations.** Follow catalog traversal and session send/read failures through their owning boundaries. Retain operation, session, directory, and lane identities where available. Catalog validators should capture the violated invariant and bounded expected/actual values at the source. Connect send failures to their originating evidence, reusing existing lane and local output diagnostics. Let owning boundaries distinguish propagated cancellation and cleanup failures from the failure that stopped the operation. Capture only information the underlying component actually provides.

4. **Keep useful causes within bounded storage.** Within each product classification, count repeats by stable diagnostic cause signatures and retain a bounded selection; occurrence identities remain context, outside the grouping key. Preserve the nominated trigger and relevant contributors/consequences without treating chronology or exception order as causality. Retain bounded trigger summaries with surviving consequences after trace or root-incident eviction. Reserve export capacity for a minimal trigger summary before trimming detailed branches or secondary evidence. Expose omissions and truncation. Bound evidence bytes and record counts across capture, active aggregation, history, and export, including failure bursts.

5. **Carry the model through export and inspection.** Update incident projection, trace references, serialization, and developer inspection together. Share the exception model with output diagnostics, keeping classifications, context, and relationships readable. Retain compact failure snapshots independently of trace activation, with evidence-change notifications driving asynchronous saves through the existing bounded, page-scoped archive. Retry failed saves within a bounded budget; reload preserves the last successfully saved failure within archive retention limits. Detailed trace remains opt-in.

## Experience and cost

The expected gain is faster diagnosis and fewer requests for users to reproduce failures. This work does not itself repair the underlying transfer faults.

Keep the existing user flow and recovery behavior. Separate evidence retention from user-facing incident decisions and detailed recording status. Automatically recovered background failures may be retained silently; evidence availability alone must not trigger an error notification. Capture detailed evidence at failure boundaries, reuse it across layers, and retain lightweight events on normal transfer paths. Diagnostic failures must not interrupt transfer control.

Reuse snapshots and coalesce evidence changes before serialization. Bound capture/encoding work and save frequency as well as storage; measure costs during normal transfers and failure bursts. Avoid per-block stack collection and synchronous diagnostic persistence.
