import {
  lifecycleFailureFact,
  type FailureFact,
  type FailureFactRef,
  type FailureFactRelation,
  type FailureStage,
  type PresentationExclusionReason,
  type PresentationOutcome,
} from '../../diagnostics/incident'
import type { ReceiveLifecycleState } from '../../output/workspace'
import { TransferPauseRequestedError } from '../../transfer/output-session'
import { V2TransferFailureSettlementError } from '../../transfer/settlement/v2-output'
import {
  classificationForTransferFailure,
  materializeClassifiedTransferFailure,
  type ClassifiedTransferFailure,
} from '../../transfer/job/failures'
import type { LifecycleUserAction } from '../v2-lifecycle-presentation'
import type { V2ReceiverControllerOptions, V2ReceiverTraceEvent } from './contracts'
import { V2PresentationAttempt } from './presentation-attempt'

export type ReceiveFailureTrigger = ClassifiedTransferFailure | FailureFactRef

export class V2ReceivePresentationAttempt extends V2PresentationAttempt {
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
  readonly #incidents: V2ReceiverControllerOptions['incidents']
  readonly #traceSource: V2ReceiverControllerOptions['trace']

  constructor(options: Pick<V2ReceiverControllerOptions, 'incidents' | 'trace'>) {
    this.#incidents = options.incidents
    this.#traceSource = options.trace
  }

  openReceive(): V2ReceivePresentationAttempt {
    return new V2ReceivePresentationAttempt(this.#incidents, 'receive')
  }

  openLifecycleAction(): V2PresentationAttempt {
    return new V2PresentationAttempt(this.#incidents, 'lifecycle_action')
  }

  transferTrigger(
    attempt: V2ReceivePresentationAttempt,
    error: unknown,
    provided: ClassifiedTransferFailure | undefined,
  ): ReceiveFailureTrigger | undefined {
    if (attempt.outputFailureTrigger !== undefined) return attempt.outputFailureTrigger
    if (provided !== undefined) {
      return materializeClassifiedTransferFailure(provided, attempt.handle)
    }
    if (error instanceof V2TransferFailureSettlementError) {
      return materializeClassifiedTransferFailure(error.trigger, attempt.handle)
    }
    if (error === undefined || error instanceof TransferPauseRequestedError) {
      return undefined
    }
    return this.classifyTransferFailure(attempt, error, 'contributor', 'content_read')
  }

  classifyTransferFailure(
    attempt: V2PresentationAttempt,
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
    attempt: V2PresentationAttempt | undefined,
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
    attempt: V2PresentationAttempt | undefined,
    reason: PresentationExclusionReason,
  ): void {
    attempt?.excluded('receive', reason)
  }

  lifecycleIncident(
    attempt: V2PresentationAttempt,
    trigger: FailureFactRef,
    outcome: Extract<PresentationOutcome, 'failed' | 'restart_required' | 'needs_attention'>,
  ): void {
    attempt.incident('lifecycle_action', outcome, trigger)
  }

  lifecycleExclusion(
    attempt: V2PresentationAttempt,
    reason: PresentationExclusionReason,
  ): void {
    attempt.excluded('lifecycle_action', reason)
  }

  decideLifecycleMutation(
    attempt: V2PresentationAttempt,
    lifecycle: ReceiveLifecycleState,
  ): void {
    const outcome = lifecycleActionOutcome(lifecycle)
    const fact = lifecycleFailureFactFor(lifecycle)
    if (outcome === undefined || fact === undefined) return
    const trigger = attempt.record(fact, 'contributor')
    if (trigger !== undefined) this.lifecycleIncident(attempt, trigger, outcome)
  }

  recordUnclassified(
    attempt: V2PresentationAttempt | undefined,
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
    transition: Extract<V2ReceiverTraceEvent, {
      name: 'lifecycle_action_transition'
    }>['transition'],
    action: LifecycleUserAction | 'expiry',
    lifecycleKind?: ReceiveLifecycleState['kind'],
  ): void {
    const observer = this.#traceSource?.current
    if (observer === undefined) return
    try {
      observer(Object.freeze({
        name: 'lifecycle_action_transition',
        transition,
        action,
        ...(lifecycleKind === undefined ? {} : { lifecycleKind }),
      }))
    } catch {
      // Detailed tracing is passive and cannot acquire lifecycle authority.
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
    attempt: V2PresentationAttempt,
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
