import { receiveLifecycleStorageEvidence } from '../../../transfer/lifecycle/interruption'
import {
  lifecycleFailureFact,
  type FailureFact,
  type FailureFactRef,
  type FailureFactRelation,
  type FailureStage,
  type PresentationExclusionReason,
  type PresentationOutcome,
} from '../../../diagnostics/incident'
import type { ReceiveLifecycleState } from '../../../output/workspace'
import { TransferPauseRequestedError } from '../../../transfer/output/session'
import { TransferFailureSettlementError } from '../../../transfer/settlement/output'
import {
  classificationForTransferFailure,
  materializeClassifiedTransferFailure,
} from '../../../transfer/job/failures'
import {
  type ClassifiedTransferFailure,
} from '../../../transfer/failure/classification'
import type { LifecycleUserAction } from '../actions'
import type { ReceiveObservationOptions } from '../observation/contracts'
import type { ReceiverTraceEvent } from '../observation/contracts'
import { PresentationAttempt } from '../observation/attempt'

export type ReceiveFailureTrigger = ClassifiedTransferFailure | FailureFactRef

export class ReceivePresentationAttempt extends PresentationAttempt {
  interruptionExclusion?: Extract<
    PresentationExclusionReason,
    'user_paused' | 'user_stopped'
  >
}

/**
 * Keeps receive/lifecycle evidence policy separate from product orchestration.
 * The coordinator asks this module to classify and present an already-observed
 * result; this module never starts, cancels, or mutates receive authority.
 */
export class ActiveReceiveObservability {
  readonly #incidents: ReceiveObservationOptions['incidents']
  readonly #traceSource: ReceiveObservationOptions['trace']

  constructor(options: Pick<ReceiveObservationOptions, 'incidents' | 'trace'>) {
    this.#incidents = options.incidents
    this.#traceSource = options.trace
  }

  openReceive(): ReceivePresentationAttempt {
    return new ReceivePresentationAttempt(this.#incidents, 'receive')
  }

  openLifecycleAction(): PresentationAttempt {
    return new PresentationAttempt(this.#incidents, 'lifecycle_action')
  }

  transferTrigger(
    attempt: ReceivePresentationAttempt,
    error: unknown,
    provided: ClassifiedTransferFailure | undefined,
  ): ReceiveFailureTrigger | undefined {
    if (attempt.outputFailureTrigger !== undefined) return attempt.outputFailureTrigger
    if (provided !== undefined) {
      return materializeClassifiedTransferFailure(provided, attempt.handle)
    }
    if (error instanceof TransferFailureSettlementError) {
      return materializeClassifiedTransferFailure(error.trigger, attempt.handle)
    }
    if (error === undefined || error instanceof TransferPauseRequestedError) {
      return undefined
    }
    return this.classifyTransferFailure(attempt, error, 'contributor', 'content_read')
  }

  classifyTransferFailure(
    attempt: PresentationAttempt,
    error: unknown,
    relation: FailureFactRelation,
    stage: FailureStage,
  ): ClassifiedTransferFailure | undefined {
    try {
      return classificationForTransferFailure(error, {
        stage,
        relation,
        ...(attempt.handle === undefined ? {} : { incidentScope: attempt.handle }),
      })
    } catch {
      return undefined
    }
  }

  receiveIncident(
    attempt: PresentationAttempt | undefined,
    classification: ReceiveFailureTrigger,
    outcome: PresentationOutcome,
  ): void {
    if (attempt === undefined) return
    const trigger = 'factSequence' in classification
      ? classification
      : this.#triggerRef(attempt, classification)
    if (trigger !== undefined) attempt.incident('receive', outcome, trigger)
  }

  receiveExclusion(
    attempt: PresentationAttempt | undefined,
    reason: PresentationExclusionReason,
  ): void {
    attempt?.excluded('receive', reason)
  }

  lifecycleIncident(
    attempt: PresentationAttempt,
    trigger: FailureFactRef,
    outcome: Extract<PresentationOutcome, 'failed' | 'restart_required' | 'needs_attention'>,
  ): void {
    attempt.incident('lifecycle_action', outcome, trigger)
  }

  lifecycleExclusion(
    attempt: PresentationAttempt,
    reason: PresentationExclusionReason,
  ): void {
    attempt.excluded('lifecycle_action', reason)
  }

  decideLifecycleMutation(
    attempt: PresentationAttempt,
    lifecycle: ReceiveLifecycleState,
  ): void {
    const outcome = lifecycleActionOutcome(lifecycle)
    const fact = lifecycleFailureFactFor(lifecycle)
    if (outcome === undefined || fact === undefined) return
    const trigger = attempt.record(fact, 'contributor')
    if (trigger !== undefined) this.lifecycleIncident(attempt, trigger, outcome)
  }

  recordUnclassified(
    attempt: PresentationAttempt | undefined,
    stage: 'content_read' | 'settlement' | 'detach' | 'cleanup' | 'lifecycle_action',
    relation: FailureFactRelation,
  ): FailureFactRef | undefined {
    if (attempt === undefined) return undefined
    return attempt.recordUnclassified(
      stage,
      relation,
      stage === 'content_read' ? 'terminal' : 'none',
    )
  }

  emitLifecycleTrace(
    transition: Extract<ReceiverTraceEvent, {
      name: 'lifecycle_action_transition'
    }>['transition'],
    action: LifecycleUserAction,
    lifecycle?: ReceiveLifecycleState,
  ): void {
    const observer = this.#traceSource?.current
    if (observer === undefined) return
    try {
      observer(Object.freeze({
        name: 'lifecycle_action_transition',
        transition,
        action,
        ...(lifecycle === undefined ? {} : receiveLifecycleStorageEvidence(lifecycle)),
      }))
    } catch {
      // Detailed tracing is passive and cannot acquire lifecycle authority.
    }
  }

  emitOwnershipTrace(state: 'releasing' | 'released' | 'release-failed', lifecycle: ReceiveLifecycleState): void {
    try {
      this.#traceSource?.current?.(Object.freeze({ name: 'receiver_experience', transition: 'ownership',
        state, operationId: lifecycle.operationId, generation: lifecycle.generation, lifecycleKind: lifecycle.kind }))
    } catch {
      // Observers cannot delay release of a settled output authority.
    }
  }

  static receiveOutcome(lifecycle: ReceiveLifecycleState): PresentationOutcome {
    switch (lifecycle.kind) {
      case 'partial-directory':
        return lifecycle.reason === 'failures' ? 'partial_directory_failures' : 'failed'
      case 'resumable-receive':
        return 'resumable_receive'
      case 'resumable-package':
        return 'resumable_package'
      case 'restart-required':
        return 'restart_required'
      case 'needs-attention':
        return 'needs_attention'
      default:
        return 'failed'
    }
  }

  #triggerRef(
    attempt: PresentationAttempt,
    classification: ClassifiedTransferFailure,
  ): FailureFactRef | undefined {
    const scope = attempt.handle
    if (scope === undefined) return undefined
    const existing = classification.factRef
    if (
      existing !== undefined &&
      existing.scope.scopeKind === scope.identity.scopeKind &&
      existing.scope.scopeSequence === scope.identity.scopeSequence
    ) {
      return existing
    }
    return attempt.record(classification.fact, 'contributor')
  }
}

function lifecycleActionOutcome(
  lifecycle: ReceiveLifecycleState,
): Extract<PresentationOutcome, 'failed' | 'restart_required' | 'needs_attention'> | undefined {
  switch (lifecycle.kind) {
    case 'restart-required':
      return 'restart_required'
    case 'needs-attention':
      return 'needs_attention'
    case 'partial-directory':
      return lifecycle.reason === 'failures' ? 'failed' : undefined
    default:
      return undefined
  }
}

function lifecycleFailureFactFor(lifecycle: ReceiveLifecycleState): FailureFact | undefined {
  switch (lifecycle.kind) {
    case 'restart-required':
      return lifecycleFailureFact({
        stage: 'lifecycle_action',
        recoveryDisposition: 'restart_required',
        kind: lifecycle.kind,
        reason: lifecycle.reason,
      })
    case 'needs-attention':
      return lifecycleFailureFact({
        stage: 'lifecycle_action',
        recoveryDisposition: 'needs_attention',
        kind: lifecycle.kind,
        reason: lifecycle.reason,
      })
    case 'partial-directory':
      if (lifecycle.reason !== 'failures') return undefined
      return lifecycleFailureFact({
        stage: 'lifecycle_action',
        recoveryDisposition: 'none',
        kind: lifecycle.kind,
        reason: lifecycle.reason,
      })
    default:
      return undefined
  }
}
