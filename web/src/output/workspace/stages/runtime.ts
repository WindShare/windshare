import {
  emitOutputTrace,
  outputTraceEvent,
  type OutputDiagnosticsPorts,
} from '../../diagnostics'
import { snapshotIdentity } from '../canonical'
import { reduceReceiveLifecycle, type LifecycleEvent } from '../lifecycle'
import type { ReceiveOperationRepository } from '../repository'
import { decodeStoredReceiveLifecycleState } from '../state-codec'
import { lifecycleDeadline, type ReceiveLifecycleState } from '../state'
import type {
  WorkspaceContentRequestCounter,
  WorkspaceReceiveIntent,
  WorkspaceStageTraceEvent,
  WorkspaceStageTraceListener,
} from './contracts'

/**
 * Owns the generation/lease fence shared by every workspace workflow. Specialized
 * stage modules can compose behavior without duplicating durable transition rules.
 */
export class WorkspaceStageRuntime {
  readonly repository: ReceiveOperationRepository
  readonly intent: WorkspaceReceiveIntent
  readonly leaseId: string
  readonly contentRequests: WorkspaceContentRequestCounter
  readonly #clock: () => number
  readonly #trace: WorkspaceStageTraceListener | undefined
  readonly #diagnostics: OutputDiagnosticsPorts | undefined

  constructor(input: {
    readonly repository: ReceiveOperationRepository
    readonly intent: WorkspaceReceiveIntent
    readonly leaseId: string
    readonly clock: () => number
    readonly contentRequests: WorkspaceContentRequestCounter
    readonly trace?: WorkspaceStageTraceListener
    readonly diagnostics?: OutputDiagnosticsPorts
  }) {
    this.repository = input.repository
    this.intent = input.intent
    this.leaseId = snapshotIdentity(input.leaseId, 16, 'lease ID')
    this.#clock = input.clock
    this.contentRequests = input.contentRequests
    this.#trace = input.trace
    this.#diagnostics = input.diagnostics
  }

  async lifecycle(): Promise<ReceiveLifecycleState> {
    const record = await this.repository.readLifecycle(this.intent.operationId)
    if (record === undefined) throw new TypeError('workspace lifecycle record is missing')
    const state = decodeStoredReceiveLifecycleState(record)
    if (state.receiveIntentDigest !== this.intent.digest) {
      throw new TypeError('workspace lifecycle escaped its receive intent')
    }
    return state
  }

  event<T extends Omit<LifecycleEvent, 'expectedGeneration' | 'leaseId'>>(
    event: T,
    state: ReceiveLifecycleState,
  ): T & { readonly expectedGeneration: bigint; readonly leaseId: string } {
    return Object.freeze({
      ...event,
      expectedGeneration: state.generation,
      leaseId: this.leaseId,
    })
  }

  reduce(state: ReceiveLifecycleState, event: LifecycleEvent): ReceiveLifecycleState {
    return this.reduceAt(state, event, this.now())
  }

  reduceAt(
    state: ReceiveLifecycleState,
    event: LifecycleEvent,
    nowMilliseconds: number,
  ): ReceiveLifecycleState {
    const reduction = reduceReceiveLifecycle(state, event, {
      planKind: 'workspace-then-publish',
      preparationRequired: this.intent.plan.preparation === 'exact-zip',
      activeLeaseId: this.leaseId,
      nowMilliseconds,
    })
    if (reduction.status !== 'applied' || reduction.state === state) {
      throw new TypeError('workspace lifecycle transition was stale or side-effect free')
    }
    return reduction.state
  }

  now(): number {
    const value = this.#clock()
    if (!Number.isSafeInteger(value) || value < 0) throw new TypeError('workspace clock is invalid')
    return value
  }

  commitLifecycle(current: ReceiveLifecycleState, next: ReceiveLifecycleState): Promise<void> {
    return this.repository.commitTransition({
      operationId: this.intent.operationId,
      expectedLifecycleGeneration: current.generation,
      expectedLeaseId: this.leaseId,
      lifecycle: next,
    })
  }

  requireZeroContentRequests(): void {
    if (this.contentRequests.count() !== 0n) {
      throw new TypeError('workspace content was requested before durable budget admission')
    }
  }

  requireContinuationUnexpired(state: ReceiveLifecycleState): void {
    const deadline = lifecycleDeadline(state)
    if (deadline !== undefined && this.now() >= deadline) {
      throw new DOMException(
        'Workspace deadline elapsed; expiry must be reduced before continuation',
        'InvalidStateError',
      )
    }
  }

  emit(event: WorkspaceStageTraceEvent): void {
    this.#emitReviewedTrace(event)
    this.#recordReviewedFailure(event)
    try {
      this.#trace?.(Object.freeze(event))
    } catch {
      // Diagnostics cannot roll back or counterfeit a durable ownership decision.
    }
  }

  #emitReviewedTrace(event: WorkspaceStageTraceEvent): void {
    const trace = this.#diagnostics?.trace
    switch (event.name) {
      case 'receive.publication.started':
        emitOutputTrace(trace, () => outputTraceEvent('publication', {
          backend: 'origin_private',
          transition: 'started',
        }))
        return
      case 'receive.publication.committed':
        emitOutputTrace(trace, () => outputTraceEvent('publication', {
          backend: 'origin_private',
          transition: 'committed',
        }))
        return
      case 'receive.publication.not_committed':
        emitOutputTrace(trace, () => outputTraceEvent('publication', {
          backend: 'origin_private',
          transition: 'not_committed',
        }))
        return
      case 'receive.publication.unknown':
      case 'receive.handoff.unknown':
        emitOutputTrace(trace, () => outputTraceEvent('publication', {
          backend: 'origin_private',
          transition: 'unknown',
        }))
        return
      case 'receive.materialization.paused':
        emitOutputTrace(trace, () => outputTraceEvent('continuation', {
          backend: 'origin_private',
          transition: 'paused',
        }))
        return
      case 'receive.continuation.admission_failed':
        emitOutputTrace(trace, () => outputTraceEvent('continuation', {
          backend: 'origin_private',
          transition: 'admission_failed',
        }))
        return
      case 'receive.operation.discarded':
      case 'receive.operation.cleanup_completed':
        emitOutputTrace(trace, () => outputTraceEvent('cleanup', {
          backend: 'origin_private',
          transition: 'completed',
        }))
        return
      case 'receive.operation.needs_attention':
        if (event.needs_attention_reason === 'cleanup-unknown') {
          emitOutputTrace(trace, () => outputTraceEvent('cleanup', {
            backend: 'origin_private',
            transition: 'ownership_unknown',
          }))
        }
        return
      default:
        return
    }
  }

  #recordReviewedFailure(event: WorkspaceStageTraceEvent): void {
    try {
      if (event.name === 'receive.publication.unknown' ||
          event.name === 'receive.handoff.unknown') {
        this.#diagnostics?.failures?.publication?.record({
          nativeClass: 'unknown',
          recoveryDisposition: 'needs_attention',
        })
        return
      }
      if (event.name === 'receive.continuation.admission_failed') {
        this.#diagnostics?.failures?.continuation?.record({
          nativeClass: 'invalid_state',
          recoveryDisposition: 'resumable_receive',
        })
        return
      }
      if (event.name === 'receive.operation.needs_attention' &&
          event.needs_attention_reason === 'cleanup-unknown') {
        this.#diagnostics?.failures?.cleanup?.record({
          nativeClass: 'unknown',
          recoveryDisposition: 'needs_attention',
        })
      }
    } catch {
      // Diagnostics are observation-only and cannot change durable lifecycle ownership.
    }
  }
}
