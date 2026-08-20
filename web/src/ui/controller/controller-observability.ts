import { V2RemoteOperationError } from '../../content/v2-session-operations'
import {
  faultFailureFact,
  type FailureFactRef,
  type FailureFactRelation,
  type FailureStage,
  type PresentationBoundary,
  type PresentationExclusionReason,
} from '../../diagnostics/incident'
import { ArtifactPlanningContractError } from '../../output/planning'
import {
  FaultScope,
  OutputFaultCode,
  outputFault,
  type Fault,
} from '../../transfer/fault'
import { V2ActivationStateContractError } from './activation-model'
import type { V2ReceiverControllerOptions, V2ReceiverTraceEvent } from './contracts'
import { V2PresentationAttempt } from './presentation-attempt'

type ControllerBoundary = Extract<PresentationBoundary, 'join' | 'projection_authority'>
type ControllerScopeKind = 'join' | 'projection' | 'authority_activation'
type ControllerFailureStage = Extract<
  FailureStage,
  'join' | 'projection' | 'authority_activation'
>
type ControllerTraceEvent = Extract<
  V2ReceiverTraceEvent,
  { readonly name: 'join_transition' | 'authority_transition' }
>

const ACTIVATION_CONTRACT_FAULT: Fault = outputFault(
  FaultScope.OutputPause,
  OutputFaultCode.Contract,
)

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
    stage: ControllerFailureStage,
  ): void {
    const trigger = attempt.outputFailureTrigger ?? recordFailureTrigger(attempt, error, stage)
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

function recordFailureTrigger(
  attempt: V2PresentationAttempt,
  error: unknown,
  stage: ControllerFailureStage,
): FailureFactRef | undefined {
  if (error instanceof V2RemoteOperationError) {
    return attempt.record(error.failureFact, 'contributor')
  }
  if (stage === 'authority_activation' && isActivationContractError(error)) {
    return attempt.record(faultFailureFact({
      stage,
      recoveryDisposition: 'terminal',
      fault: ACTIVATION_CONTRACT_FAULT,
    }), 'contributor')
  }
  return attempt.recordUnclassified(stage, 'contributor')
}

function isActivationContractError(
  error: unknown,
): error is ArtifactPlanningContractError | V2ActivationStateContractError {
  return error instanceof ArtifactPlanningContractError ||
    error instanceof V2ActivationStateContractError
}
