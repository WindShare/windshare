import { availableReceiveActions } from '../policy'
import type { PresentationExclusionReason } from '../../../diagnostics/incident'
import type { OutputFailureBindingLease } from '../../../output/diagnostics'
import type { LifecycleUserAction, V2ActiveReceiveControl } from '../actions'
import type { ReceiveRuntimeState } from '../state'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
} from '../contracts'
import { presentationSourceOutcome } from '../contracts'
import { StaleReceiveBoundaryError } from '../observation/contracts'
import {
  type ActiveReceiveObservability,
  type V2ReceivePresentationAttempt,
} from './observability'
import type { V2PresentationAttempt } from '../observation/attempt'
import type { ReceiveSettlementDeadline } from './settlement-wait'

export interface ActiveReceiveLifecycleOperation {
  readonly runtime: V2BoundReceiveOperation
  readonly settlementWait: ReceiveSettlementDeadline
  transfer?: AbortController
  running?: Promise<void>
  receiveAttempt?: V2ReceivePresentationAttempt
}

interface PendingLifecycleAction {
  readonly operation: ActiveReceiveLifecycleOperation
  readonly generation: bigint
  readonly action: LifecycleUserAction
  readonly attempt: V2PresentationAttempt
  readonly outputLease?: OutputFailureBindingLease
}

export interface ActiveReceiveLifecycleOptions {
  readonly state: ReceiveRuntimeState
  readonly observability: ActiveReceiveObservability
  readonly currentOperation: () => ActiveReceiveLifecycleOperation | undefined
  readonly operationIsCurrent: (operation: ActiveReceiveLifecycleOperation) => boolean
  readonly startTransfer: (operation: ActiveReceiveLifecycleOperation) => void
  readonly replaceDetachConsequence: (
    operation: ActiveReceiveLifecycleOperation,
    attempt: V2PresentationAttempt,
  ) => void
  readonly onActionError: (error: unknown) => void
  readonly onIdle: (operation: ActiveReceiveLifecycleOperation) => void
}

export class ActiveReceiveLifecycle {
  readonly #state: ReceiveRuntimeState
  readonly #observability: ActiveReceiveObservability
  readonly #currentOperation: () => ActiveReceiveLifecycleOperation | undefined
  readonly #operationIsCurrent: (operation: ActiveReceiveLifecycleOperation) => boolean
  readonly #startTransfer: (operation: ActiveReceiveLifecycleOperation) => void
  readonly #replaceDetachConsequence: ActiveReceiveLifecycleOptions['replaceDetachConsequence']
  readonly #onActionError: (error: unknown) => void
  readonly #onIdle: ActiveReceiveLifecycleOptions['onIdle']
  #pending: PendingLifecycleAction | undefined

  constructor(options: ActiveReceiveLifecycleOptions) {
    this.#state = options.state
    this.#observability = options.observability
    this.#currentOperation = options.currentOperation
    this.#operationIsCurrent = options.operationIsCurrent
    this.#startTransfer = options.startTransfer
    this.#replaceDetachConsequence = options.replaceDetachConsequence
    this.#onActionError = options.onActionError
    this.#onIdle = options.onIdle
  }

  get pending(): boolean { return this.#pending !== undefined }

  canPerform(action: LifecycleUserAction): boolean {
    const active = this.#currentOperation()
    const facts = this.#state.getSnapshot()
    if (active === undefined || !this.#operationIsCurrent(active) || this.#pending !== undefined ||
        facts.lifecycle === null || facts.plan === null) return false
    if ((action === 'pause' || action === 'stop') &&
        (active.transfer === undefined || active.transfer.signal.aborted || !active.runtime.activeControls.includes(action))) return false
    return availableReceiveActions({
      state: facts.lifecycle, plan: facts.plan, activeControls: facts.activeControls,
      interruption: facts.receiveInterruption, recoverySummary: facts.recoverySummary,
      browserDelivery: facts.browserFolderProgress?.summary ?? null,
    }).includes(action)
  }

  perform(action: LifecycleUserAction): void {
    if (!this.canPerform(action)) return
    const active = this.#currentOperation()
    const lifecycle = this.#state.getSnapshot().lifecycle
    if (active === undefined || lifecycle === null) return

    const attempt = this.#observability.openLifecycleAction()
    const pending: PendingLifecycleAction = Object.freeze({
      operation: active,
      generation: lifecycle.generation,
      action,
      attempt,
      ...(active.runtime.bindOutputFailures === undefined
        ? {}
        : { outputLease: active.runtime.bindOutputFailures(attempt.outputFailures) }),
    })
    this.#pending = pending
    this.#observability.emitLifecycleTrace('started', action)

    if (action === 'pause' || action === 'stop') {
      this.#interruptReceive(active, action, pending)
      return
    }

    let started: V2LifecycleMutation | PromiseLike<V2LifecycleMutation>
    try {
      // Publication pickers must start before the rendered click returns.
      started = active.runtime.startLifecycleAction(action, lifecycle)
    } catch (error) {
      this.#finishFailure(pending, error)
      return
    }
    Promise.resolve(started).then(
      mutation => this.#finishMutation(pending, mutation),
      (error: unknown) => this.#finishFailure(pending, error),
    ).catch(() => undefined)
  }

  cancel(reason: PresentationExclusionReason): void {
    const pending = this.#pending
    this.#pending = undefined
    if (pending === undefined) return
    this.#observability.lifecycleExclusion(pending.attempt, reason)
    this.#closeAttempt(pending)
  }

  async applyMutation(
    active: ActiveReceiveLifecycleOperation,
    expectedGeneration: bigint,
    mutation: V2LifecycleMutation,
    beforePublish?: () => void,
  ): Promise<boolean> {
    const current = this.#state.getSnapshot().lifecycle
    if (
      !this.#operationIsCurrent(active) ||
      current === null ||
      current.generation !== expectedGeneration ||
      mutation.lifecycle.operationId !== active.runtime.intent.operationId ||
      mutation.lifecycle.receiveIntentDigest !== active.runtime.intent.digest
    ) {
      return false
    }
    const usage = mutation.workspaceUsage === undefined
      ? await Promise.resolve(active.runtime.resolveWorkspaceUsage(mutation.lifecycle))
      : mutation.workspaceUsage
    if (
      !this.#operationIsCurrent(active) ||
      this.#state.getSnapshot().lifecycle?.generation !== expectedGeneration
    ) {
      return false
    }
    isolateDiagnostic(beforePublish)
    // Journal-only local actions can finish without changing the authenticated lifecycle cut.
    const unchangedUsage = mutation.workspaceUsage === undefined ||
      mutation.workspaceUsage === this.#state.getSnapshot().workspaceUsage
    if (mutation.lifecycle === current && mutation.resumeTransfer !== true && active.running === undefined &&
        mutation.activeControls === undefined && unchangedUsage) return true
    const controls = mutation.activeControls ?? Object.freeze([])
    if (!this.#state.settleLifecycle(mutation.lifecycle, usage, controls, undefined, mutation.recoverySummary)) return false
    if (mutation.resumeTransfer === true) this.#resumeTransferWhenIdle(active)
    return true
  }

  #interruptReceive(
    active: ActiveReceiveLifecycleOperation,
    control: V2ActiveReceiveControl,
    pending: PendingLifecycleAction,
  ): void {
    const transfer = active.transfer
    if (
      transfer === undefined ||
      transfer.signal.aborted ||
      !active.runtime.activeControls.includes(control)
    ) {
      this.#finishExcluded(pending, 'stale_replacement')
      return
    }

    this.#state.updateActiveControls(Object.freeze([]))
    try {
      active.settlementWait.begin(control)
      active.runtime.interrupt(control, transfer)
      if (!transfer.signal.aborted) {
        throw new TypeError('receive interruption did not synchronously close the transfer lifetime')
      }
      if (active.receiveAttempt !== undefined) {
        active.receiveAttempt.interruptionExclusion =
          control === 'pause' ? 'user_paused' : 'user_stopped'
      }
      this.#finishExcluded(
        pending,
        control === 'pause' ? 'user_paused' : 'user_stopped',
      )
    } catch (error) {
      active.settlementWait.cancel(control)
      this.#state.updateActiveControls(active.runtime.activeControls)
      this.#finishFailure(pending, error)
    }
  }

  async #finishMutation(
    pending: PendingLifecycleAction,
    mutation: V2LifecycleMutation,
  ): Promise<void> {
    if (this.#pending !== pending) {
      this.#observability.lifecycleExclusion(pending.attempt, 'stale_replacement')
      this.#closeAttempt(pending)
      return
    }
    this.#pending = undefined
    try {
      // Local commands must return a new commit or the original unchanged authority;
      // equivalent snapshots are accepted only when settling an observed transfer.
      const hasMutationAuthority = mutation.lifecycle.generation > pending.generation ||
        mutation.lifecycle === this.#state.getSnapshot().lifecycle
      const applied = hasMutationAuthority && await this.applyMutation(
        pending.operation,
        pending.generation,
        mutation,
        mutation.actionOutcome?.kind !== 'failed'
          ? () => this.#observability.decideLifecycleMutation(pending.attempt, mutation.lifecycle)
          : undefined,
      )
      if (!applied) {
        this.#observability.lifecycleExclusion(pending.attempt, 'stale_replacement')
        this.#observability.emitLifecycleTrace('excluded', pending.action)
      } else if (mutation.actionOutcome?.kind === 'failed') {
        this.#finishFailure(pending, mutation.actionOutcome.error, false)
        this.#onIdle(pending.operation)
        return
      } else if (!pending.attempt.decisionSettled) {
        this.#observability.lifecycleExclusion(
          pending.attempt,
          pending.action === 'discard' || pending.action === 'delete'
            ? 'user_discarded'
            : 'success',
        )
        this.#observability.emitLifecycleTrace('completed', pending.action, mutation.lifecycle.kind)
      }
    } catch (error) {
      this.#finishFailure(pending, error, false)
      return
    }
    this.#closeAttempt(pending)
    this.#onIdle(pending.operation)
  }

  #finishFailure(
    pending: PendingLifecycleAction,
    error: unknown,
    clearPending = true,
  ): void {
    if (clearPending && this.#pending !== pending) {
      this.#observability.lifecycleExclusion(pending.attempt, 'stale_replacement')
      this.#closeAttempt(pending)
      return
    }
    if (this.#pending === pending) this.#pending = undefined
    const sourceOutcome = presentationSourceOutcome(error)
    if (sourceOutcome !== 'native_fault' || error instanceof StaleReceiveBoundaryError) {
      let reason: PresentationExclusionReason = 'cancelled'
      if (sourceOutcome === 'picker_refused') reason = 'picker_refused'
      if (sourceOutcome === 'stale_replacement' || error instanceof StaleReceiveBoundaryError) {
        reason = 'stale_replacement'
      }
      this.#finishExcluded(pending, reason, false)
      return
    }

    const trigger = pending.attempt.outputFailureTrigger ??
      this.#observability.recordUnclassified(
        pending.attempt,
        'lifecycle_action',
        'contributor',
      )
    if (trigger !== undefined) {
      this.#observability.lifecycleIncident(pending.attempt, trigger, 'failed')
    }
    this.#observability.emitLifecycleTrace('failed', pending.action)
    try {
      this.#onActionError(error)
    } catch {
      // Presentation diagnostics cannot replace lifecycle action authority.
    }
    this.#closeAttempt(pending)
  }

  #finishExcluded(
    pending: PendingLifecycleAction,
    reason: PresentationExclusionReason,
    clearPending = true,
  ): void {
    if (clearPending && this.#pending === pending) this.#pending = undefined
    this.#observability.lifecycleExclusion(pending.attempt, reason)
    this.#observability.emitLifecycleTrace('excluded', pending.action)
    this.#closeAttempt(pending)
  }

  #resumeTransferWhenIdle(active: ActiveReceiveLifecycleOperation): void {
    const running = active.running
    if (active.transfer === undefined || running === undefined) {
      this.#startTransfer(active)
      return
    }
    running.finally(() => {
      if (this.#operationIsCurrent(active)) this.#startTransfer(active)
    }).catch(() => undefined)
  }

  #closeAttempt(pending: PendingLifecycleAction): void {
    pending.outputLease?.revoke()
    this.#replaceDetachConsequence(pending.operation, pending.attempt)
    pending.attempt.close()
  }
}

function isolateDiagnostic(callback: (() => void) | undefined): void {
  try {
    callback?.()
  } catch {
    // A diagnostics adapter cannot prevent the already-authorized publication.
  }
}
