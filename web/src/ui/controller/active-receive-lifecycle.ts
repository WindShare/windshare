import type { PresentationExclusionReason } from '../../diagnostics/incident'
import type { OutputFailureBindingLease } from '../../output/diagnostics'
import { lifecycleDeadline } from '../../output/workspace'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
} from '../v2-lifecycle-presentation'
import { isAbortError } from '../v2-controller-state'
import type { V2OutputPresentationController } from '../v2-output'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
  V2StartedArtifactAuthority,
} from '../v2-receive-runtime'
import { presentationSourceOutcome } from '../v2-receive-runtime'
import { StaleReceiveBoundaryError } from './contracts'
import {
  type ActiveReceiveObservability,
  type V2ReceivePresentationAttempt,
} from './active-receive-observability'
import type { V2PresentationAttempt } from './presentation-attempt'

const MAXIMUM_TIMER_DELAY_MILLISECONDS = 2_147_483_647

export interface ActiveReceiveLifecycleOperation {
  readonly runtime: V2BoundReceiveOperation
  transfer?: AbortController
  running?: Promise<void>
  receiveAttempt?: V2ReceivePresentationAttempt
  expiryTimer?: ReturnType<typeof setTimeout>
}

interface PendingLifecycleAction {
  readonly operation: ActiveReceiveLifecycleOperation
  readonly generation: bigint
  readonly action: LifecycleUserAction
  readonly attempt: V2PresentationAttempt
  readonly outputLease?: OutputFailureBindingLease
}

export interface ActiveReceiveLifecycleOptions {
  readonly outputs: V2OutputPresentationController<V2StartedArtifactAuthority>
  readonly observability: ActiveReceiveObservability
  readonly currentOperation: () => ActiveReceiveLifecycleOperation | undefined
  readonly operationIsCurrent: (operation: ActiveReceiveLifecycleOperation) => boolean
  readonly startTransfer: (operation: ActiveReceiveLifecycleOperation) => void
  readonly replaceDetachConsequence: (
    operation: ActiveReceiveLifecycleOperation,
    attempt: V2PresentationAttempt,
  ) => void
  readonly onActionError: (error: unknown) => void
  readonly onFailure: (error: unknown) => void
}

export class ActiveReceiveLifecycle {
  readonly #outputs: V2OutputPresentationController<V2StartedArtifactAuthority>
  readonly #observability: ActiveReceiveObservability
  readonly #currentOperation: () => ActiveReceiveLifecycleOperation | undefined
  readonly #operationIsCurrent: (operation: ActiveReceiveLifecycleOperation) => boolean
  readonly #startTransfer: (operation: ActiveReceiveLifecycleOperation) => void
  readonly #replaceDetachConsequence: ActiveReceiveLifecycleOptions['replaceDetachConsequence']
  readonly #onActionError: (error: unknown) => void
  readonly #onFailure: (error: unknown) => void
  #pending: PendingLifecycleAction | undefined

  constructor(options: ActiveReceiveLifecycleOptions) {
    this.#outputs = options.outputs
    this.#observability = options.observability
    this.#currentOperation = options.currentOperation
    this.#operationIsCurrent = options.operationIsCurrent
    this.#startTransfer = options.startTransfer
    this.#replaceDetachConsequence = options.replaceDetachConsequence
    this.#onActionError = options.onActionError
    this.#onFailure = options.onFailure
  }

  perform(action: LifecycleUserAction): void {
    const active = this.#currentOperation()
    const output = this.#outputs.getSnapshot()
    const lifecycle = output.lifecycle
    const presented = output.lifecyclePresentation
    if (
      active === undefined ||
      lifecycle === null ||
      presented === null ||
      this.#pending !== undefined ||
      !presented.actions.some(candidate => candidate.kind === action)
    ) {
      return
    }

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

  cancelExpiry(operation: ActiveReceiveLifecycleOperation): void {
    if (operation.expiryTimer !== undefined) clearTimeout(operation.expiryTimer)
    delete operation.expiryTimer
  }

  async applyMutation(
    active: ActiveReceiveLifecycleOperation,
    expectedGeneration: bigint,
    mutation: V2LifecycleMutation,
    beforePublish?: () => void,
  ): Promise<boolean> {
    const current = this.#outputs.getSnapshot().lifecycle
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
      this.#outputs.getSnapshot().lifecycle?.generation !== expectedGeneration
    ) {
      return false
    }
    isolateDiagnostic(beforePublish)
    const controls = mutation.activeControls ?? Object.freeze([])
    if (!this.#outputs.updateLifecycle(mutation.lifecycle, Date.now(), usage, controls)) return false
    this.scheduleExpiry(active)
    if (mutation.resumeTransfer === true) this.#resumeTransferWhenIdle(active)
    return true
  }

  scheduleExpiry(active: ActiveReceiveLifecycleOperation): void {
    this.cancelExpiry(active)
    const lifecycle = this.#outputs.getSnapshot().lifecycle
    if (!this.#operationIsCurrent(active) || lifecycle === null) return
    const deadline = lifecycleDeadline(lifecycle)
    if (deadline === undefined) return
    const delay = Math.min(
      MAXIMUM_TIMER_DELAY_MILLISECONDS,
      Math.max(0, deadline - Date.now()),
    )
    const expectedGeneration = lifecycle.generation
    active.expiryTimer = setTimeout(() => {
      delete active.expiryTimer
      this.#observeExpiry(active, expectedGeneration, deadline).catch(() => undefined)
    }, delay)
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

    this.#outputs.updateActiveControls(Object.freeze([]))
    try {
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
      this.#outputs.updateActiveControls(active.runtime.activeControls)
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
      const applied = await this.applyMutation(
        pending.operation,
        pending.generation,
        mutation,
        () => this.#observability.decideLifecycleMutation(pending.attempt, mutation.lifecycle),
      )
      if (!applied) {
        this.#observability.lifecycleExclusion(pending.attempt, 'stale_replacement')
        this.#observability.emitLifecycleTrace('excluded', pending.action)
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

  async #observeExpiry(
    active: ActiveReceiveLifecycleOperation,
    expectedGeneration: bigint,
    deadline: number,
  ): Promise<void> {
    if (
      !this.#operationIsCurrent(active) ||
      this.#outputs.getSnapshot().lifecycle?.generation !== expectedGeneration
    ) {
      return
    }
    if (Date.now() < deadline) {
      this.scheduleExpiry(active)
      return
    }

    const lifecycle = this.#outputs.getSnapshot().lifecycle
    if (lifecycle === null) return
    const attempt = this.#observability.openLifecycleAction()
    const outputLease = active.runtime.bindOutputFailures?.(attempt.outputFailures)
    this.#observability.emitLifecycleTrace('started', 'expiry')
    try {
      const mutation = await active.runtime.observeExpiry(lifecycle)
      const applied = await this.applyMutation(
        active,
        expectedGeneration,
        mutation,
        () => {
          this.#observability.decideLifecycleMutation(attempt, mutation.lifecycle)
          if (!attempt.decisionSettled && mutation.lifecycle.kind === 'expired') {
            this.#observability.lifecycleExclusion(attempt, 'normal_expiry')
          }
        },
      )
      if (!applied) this.#observability.lifecycleExclusion(attempt, 'stale_replacement')
      if (!attempt.decisionSettled) this.#observability.lifecycleExclusion(attempt, 'success')
      this.#observability.emitLifecycleTrace('completed', 'expiry', mutation.lifecycle.kind)
    } catch (error) {
      this.#observability.emitLifecycleTrace('failed', 'expiry')
      if (isAbortError(error)) {
        this.#observability.lifecycleExclusion(attempt, 'not_user_visible')
      } else {
        const trigger = attempt.outputFailureTrigger ??
          this.#observability.recordUnclassified(
            attempt,
            'lifecycle_action',
            'contributor',
          )
        if (trigger !== undefined) {
          this.#observability.lifecycleIncident(attempt, trigger, 'failed')
        }
        if (this.#operationIsCurrent(active)) this.#reportFailure(error)
      }
    } finally {
      outputLease?.revoke()
      this.#replaceDetachConsequence(active, attempt)
      attempt.close()
    }
  }

  #reportFailure(error: unknown): void {
    try {
      this.#onFailure(error)
    } catch {
      // A presentation observer cannot replace the failure that was already published.
    }
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
