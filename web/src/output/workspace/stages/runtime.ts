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

  constructor(input: {
    readonly repository: ReceiveOperationRepository
    readonly intent: WorkspaceReceiveIntent
    readonly leaseId: string
    readonly clock: () => number
    readonly contentRequests: WorkspaceContentRequestCounter
    readonly trace?: WorkspaceStageTraceListener
  }) {
    this.repository = input.repository
    this.intent = input.intent
    this.leaseId = snapshotIdentity(input.leaseId, 16, 'lease ID')
    this.#clock = input.clock
    this.contentRequests = input.contentRequests
    this.#trace = input.trace
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
    try {
      this.#trace?.(Object.freeze(event))
    } catch {
      // Diagnostics cannot roll back or counterfeit a durable ownership decision.
    }
  }
}
