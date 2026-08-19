import { V2RemoteOperationError } from '../../content/v2-session-operations'
import type {
  FailureFactRef,
  FailureFactRelation,
  FailureStage,
  PresentationBoundary,
  PresentationExclusionReason,
} from '../../diagnostics/incident'
import type { V2ReceiverControllerOptions, V2ReceiverTraceEvent } from './contracts'
import { V2PresentationAttempt } from './presentation-attempt'

type ControllerBoundary = Extract<PresentationBoundary, 'join' | 'projection_authority'>
type ControllerScopeKind = 'join' | 'projection' | 'authority_activation'
type ControllerTraceEvent = Extract<
  V2ReceiverTraceEvent,
  { readonly name: 'join_transition' | 'authority_transition' }
>

/**
 * Controller diagnostics observe authority transitions but never hold authority
 * themselves. Keeping the port here makes reporter failure and lazy trace lookup
 * uniform across join, projection, and activation owners.
 */
export class V2ControllerObservability {
  readonly #incidents: V2ReceiverControllerOptions['incidents']
  readonly #traceSource: V2ReceiverControllerOptions['trace']

  constructor(options: Pick<V2ReceiverControllerOptions, 'incidents' | 'trace'>) {
    this.#incidents = options.incidents
    this.#traceSource = options.trace
  }

  open(kind: ControllerScopeKind): V2PresentationAttempt {
    return new V2PresentationAttempt(this.#incidents, kind)
  }

  fail(
    attempt: V2PresentationAttempt,
    boundary: ControllerBoundary,
    error: unknown,
    stage: Extract<FailureStage, 'join' | 'projection' | 'authority_activation'>,
  ): void {
    const trigger = attempt.outputFailureTrigger ?? (
      error instanceof V2RemoteOperationError
        ? attempt.record(error.failureFact, 'contributor')
        : attempt.recordUnclassified(stage, 'contributor')
    )
    if (trigger !== undefined) attempt.incident(boundary, 'failed', trigger)
  }

  exclude(
    attempt: V2PresentationAttempt,
    boundary: ControllerBoundary,
    reason: PresentationExclusionReason,
  ): void {
    attempt.excluded(boundary, reason)
  }

  recordConsequence(
    attempt: V2PresentationAttempt,
    stage: Extract<FailureStage, 'projection' | 'settlement' | 'detach' | 'cleanup'>,
  ): FailureFactRef | undefined {
    return attempt.recordUnclassified(stage, 'consequence', 'none')
  }

  record(
    attempt: V2PresentationAttempt,
    stage: Extract<FailureStage, 'join' | 'projection' | 'authority_activation'>,
    relation: FailureFactRelation,
  ): FailureFactRef | undefined {
    return attempt.recordUnclassified(stage, relation)
  }

  trace(createEvent: () => ControllerTraceEvent): void {
    const observer = this.#traceSource?.current
    if (observer === undefined) return
    try {
      observer(createEvent())
    } catch {
      // Detailed trace cannot alter epoch, authority, or operation ownership.
    }
  }
}
