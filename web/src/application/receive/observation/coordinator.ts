import { RemoteOperationError } from '../../../content/session-operations'
import {
  faultFailureFact,
  unclassifiedFailureFact,
  type FailureFactRef,
  type FailureFactRelation,
  type FailureStage,
  type PresentationBoundary,
  type PresentationExclusionReason,
} from '../../../diagnostics/incident'
import { ArtifactPlanningContractError } from '../../../output/planning'
import {
  FaultScope,
  OutputFaultCode,
  outputFault,
  type Fault,
} from '../../../transfer/failure/fault'
import { ActivationStateContractError } from '../activation/model'
import type { ReceiveObservationOptions } from '../observation/contracts'
import type { ReceiverTraceEvent } from './contracts'
import { PresentationAttempt } from './attempt'

type ControllerBoundary = Extract<PresentationBoundary, 'join' | 'projection_authority'>
type ControllerScopeKind = 'join' | 'projection' | 'authority_activation'
type ControllerFailureStage = Extract<
  FailureStage,
  'join' | 'projection' | 'authority_activation'
>
type ControllerTraceEvent = Extract<
  ReceiverTraceEvent,
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
export class ControllerObservability {
  readonly #incidents: ReceiveObservationOptions['incidents']
  readonly #traceSource: ReceiveObservationOptions['trace']

  constructor(options: Pick<ReceiveObservationOptions, 'incidents' | 'trace'>) {
    this.#incidents = options.incidents
    this.#traceSource = options.trace
  }

  open(kind: ControllerScopeKind): PresentationAttempt {
    return new PresentationAttempt(this.#incidents, kind)
  }

  fail(
    attempt: PresentationAttempt,
    boundary: ControllerBoundary,
    error: unknown,
    stage: ControllerFailureStage,
  ): void {
    const trigger = attempt.outputFailureTrigger ?? recordFailureTrigger(attempt, error, stage)
    if (trigger !== undefined) attempt.incident(boundary, 'failed', trigger)
  }

  exclude(
    attempt: PresentationAttempt,
    boundary: ControllerBoundary,
    reason: PresentationExclusionReason,
  ): void {
    attempt.excluded(boundary, reason)
  }

  recordConsequence(
    attempt: PresentationAttempt,
    stage: Extract<FailureStage, 'projection' | 'settlement' | 'detach' | 'cleanup'>,
  ): FailureFactRef | undefined {
    return attempt.recordUnclassified(stage, 'consequence', 'none')
  }

  record(
    attempt: PresentationAttempt,
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
  attempt: PresentationAttempt,
  error: unknown,
  stage: ControllerFailureStage,
): FailureFactRef | undefined {
  if (error instanceof RemoteOperationError) {
    return attempt.record(error.failureFact, 'contributor')
  }
  if (stage === 'authority_activation' && isActivationContractError(error)) {
    return attempt.record(faultFailureFact({
      stage,
      recoveryDisposition: 'terminal',
      fault: ACTIVATION_CONTRACT_FAULT,
    }), 'contributor')
  }
  return attempt.record(unclassifiedFailureFact({
    stage, recoveryDisposition: 'terminal', error,
  }), 'contributor')
}

function isActivationContractError(
  error: unknown,
): error is ArtifactPlanningContractError | ActivationStateContractError {
  return error instanceof ArtifactPlanningContractError ||
    error instanceof ActivationStateContractError
}
