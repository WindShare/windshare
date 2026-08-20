import type {
  FailureFactRef,
  PresentationExclusionReason,
} from '../../diagnostics/incident'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import { validateReceiveIntent } from '../../transfer/intent'
import { EMPTY_V2_RETAINED_INVENTORY, type V2ReceiverSnapshot } from '../v2-model'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type {
  V2BoundReceiveOperation,
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'
import {
  StaleReceiveBoundaryError,
  type V2ReceiverIncidentPort,
  type V2RetainedInventoryTraceEvent,
} from './contracts'
import { V2PresentationAttempt } from './presentation-attempt'

type RetainedAttempt = V2PresentationAttempt

interface PreparedRetainedFailure {
  readonly trigger: FailureFactRef | undefined
}

interface PendingInventoryLoad {
  readonly controller: AbortController
  readonly attempt: RetainedAttempt
}

interface PendingRetainedAction {
  readonly boundary: number
  readonly inventory: V2RetainedReceiveInventory
  readonly operation: V2RetainedReceiveOperation
  readonly action: V2RetainedReceiveAction
  readonly joined?: V2JoinedBrowserShare
  readonly protocolSessionId?: string
  readonly controller: AbortController
  readonly attempt: RetainedAttempt
}

export interface RetainedContinuationAdoption {
  readonly retained: V2RetainedReceiveOperation
  readonly joined: V2JoinedBrowserShare
  readonly runtime: V2BoundReceiveOperation
}

export interface RetainedInventoryCoordinatorOptions {
  readonly receive: V2ReceiveCompositionPort
  readonly isDisposed: () => boolean
  readonly currentJoinedShare: () => V2JoinedBrowserShare | undefined
  readonly continuationBlocked: () => boolean
  readonly adoptContinuation: (input: RetainedContinuationAdoption) => Promise<void>
  readonly ownsRuntime: (runtime: V2BoundReceiveOperation) => boolean
  readonly publish: (retained: V2ReceiverSnapshot['retained']) => void
  readonly trace?: DomainTraceSource<V2RetainedInventoryTraceEvent>
  readonly onActionError: (error: unknown) => void
  readonly incidents?: V2ReceiverIncidentPort
}

export class RetainedInventoryCoordinator {
  readonly #options: RetainedInventoryCoordinatorOptions
  #load: PendingInventoryLoad | undefined
  #inventory: V2RetainedReceiveInventory | undefined
  #boundary = 0
  #pending: PendingRetainedAction | undefined

  constructor(options: RetainedInventoryCoordinatorOptions) {
    this.#options = options
  }

  get pending(): boolean {
    return this.#pending !== undefined
  }

  load(): Promise<void> {
    return this.#loadInventory()
  }

  perform(
    operation: V2RetainedReceiveOperation,
    action: V2RetainedReceiveAction,
  ): void {
    const inventory = this.#inventory
    if (
      this.#options.isDisposed() ||
      inventory === undefined ||
      this.#pending !== undefined ||
      !inventory.operations.includes(operation) ||
      !operation.actions.includes(action)
    ) {
      return
    }

    const joined = action === 'continue'
      ? this.#options.currentJoinedShare()
      : undefined
    const continuationUnavailable = action === 'continue' && (
      joined === undefined || this.#options.continuationBlocked()
    )
    const attempt = this.#newAttempt('retained_action')
    if (continuationUnavailable) {
      const error = new DOMException(
        'Open the matching share before continuing this receive task',
        'InvalidStateError',
      )
      try {
        this.#recordFailure(attempt, 'retained_action')
        this.#options.onActionError(error)
      } finally {
        this.#closeAttempt(attempt)
      }
      return
    }

    const controller = new AbortController()
    const pending: PendingRetainedAction = Object.freeze({
      boundary: ++this.#boundary,
      inventory,
      operation,
      action,
      ...(joined === undefined
        ? {}
        : { joined, protocolSessionId: joined.protocolSessionId }),
      controller,
      attempt,
    })
    this.#pending = pending
    this.#publishReadyInventory(inventory, pending)
    this.#trace(() => Object.freeze({
      name: 'receive.inventory.action.started',
      retained_action: action,
      continuation: operation.continuation,
    }))
    let started: ReturnType<V2RetainedReceiveInventory['act']>
    try {
      // The live ResumeRef is consumed in the explicit user-action stack.
      started = inventory.act(
        operation,
        action,
        controller.signal,
        attempt.outputFailures,
      )
    } catch (error) {
      this.#settleFailure(pending, error)
      return
    }
    Promise.resolve(started).then(
      (result) => this.#settleSuccess(pending, result),
      (error: unknown) => this.#settleFailure(pending, error),
    ).catch(() => undefined)
  }

  cancelPending(reason: unknown): void {
    this.#cancelPending(reason, true)
  }

  #cancelPending(reason: unknown, publishClearedSnapshot: boolean): void {
    const pending = this.#pending
    if (pending === undefined) return
    this.#boundary += 1
    pending.controller.abort(reason)
    this.#clearPending(pending, publishClearedSnapshot)
    this.#exclude(pending.attempt, 'cancelled')
    this.#closeAttempt(pending.attempt)
  }

  close(reason: unknown): void {
    const load = this.#load
    if (load !== undefined) {
      load.controller.abort(reason)
      this.#exclude(load.attempt, 'cancelled')
      this.#closeAttempt(load.attempt)
    }
    this.#load = undefined
    this.cancelPending(reason)
    this.#inventory?.close()
    this.#inventory = undefined
  }

  async #loadInventory(): Promise<void> {
    this.#cancelPending(new DOMException('Resume inventory reloaded', 'AbortError'), false)
    this.#replaceInventoryLoad()
    const attempt = this.#newAttempt('retained_inventory')
    this.#closeInventoryBeforeLoad(attempt)
    const pending = this.#beginInventoryLoad(attempt)
    let loaded: V2RetainedReceiveInventory | undefined
    try {
      loaded = await this.#options.receive.retained.list(
        pending.controller.signal,
        attempt.outputFailures,
      )
      pending.controller.signal.throwIfAborted()
      if (!this.#isCurrentLoad(pending)) {
        this.#closeLoadedInventory(loaded, attempt)
        this.#excludeInactiveLoad(pending)
        return
      }
      this.#settleLoadedInventoryPresentation(attempt, loaded)
      this.#publishLoadedInventory(loaded)
    } catch {
      this.#settleInventoryLoadFailure(pending, loaded)
    } finally {
      this.#finishInventoryLoad(pending)
    }
  }

  #replaceInventoryLoad(): void {
    const replaced = this.#load
    if (replaced === undefined) return
    replaced.controller.abort(
      new DOMException('Resume inventory reloaded', 'AbortError'),
    )
    this.#exclude(replaced.attempt, 'stale_replacement')
    this.#closeAttempt(replaced.attempt)
  }

  #closeInventoryBeforeLoad(attempt: RetainedAttempt): void {
    try {
      this.#inventory?.close()
    } catch (error) {
      this.#exclude(attempt, 'not_user_visible')
      this.#closeAttempt(attempt)
      throw error
    }
    this.#inventory = undefined
  }

  #beginInventoryLoad(attempt: RetainedAttempt): PendingInventoryLoad {
    const pending: PendingInventoryLoad = {
      controller: new AbortController(),
      attempt,
    }
    this.#load = pending
    try {
      this.#options.publish(EMPTY_V2_RETAINED_INVENTORY)
      this.#trace(() => Object.freeze({
        name: 'receive.inventory.load.started',
      }))
    } catch (error) {
      this.#exclude(attempt, 'not_user_visible')
      this.#closeAttempt(attempt)
      throw error
    }
    return pending
  }

  #isCurrentLoad(pending: PendingInventoryLoad): boolean {
    return !this.#options.isDisposed() && this.#load === pending
  }

  #closeLoadedInventory(
    loaded: V2RetainedReceiveInventory | undefined,
    attempt: RetainedAttempt,
  ): void {
    if (loaded === undefined) return
    try {
      loaded.close()
    } catch {
      this.#recordConsequence(attempt, 'cleanup')
    }
  }

  #excludeInactiveLoad(pending: PendingInventoryLoad): void {
    this.#exclude(
      pending.attempt,
      pending.controller.signal.aborted ? 'cancelled' : 'stale_replacement',
    )
  }

  #settleLoadedInventoryPresentation(
    attempt: RetainedAttempt,
    loaded: V2RetainedReceiveInventory,
  ): void {
    let trigger: FailureFactRef | undefined
    for (const fact of loaded.presentationFailures) {
      const ref = attempt.record(fact, 'contributor')
      trigger ??= ref
    }
    if (trigger === undefined) {
      this.#exclude(attempt, 'success')
      return
    }
    attempt.incident('retained_inventory', 'failed', trigger)
  }

  #publishLoadedInventory(loaded: V2RetainedReceiveInventory): void {
    this.#inventory = loaded
    this.#trace(() => Object.freeze({
      name: 'receive.inventory.load.completed',
      operation_count: loaded.operations.length,
    }))
    this.#publishReadyInventory(loaded)
  }

  #publishReadyInventory(
    inventory: V2RetainedReceiveInventory,
    pending?: PendingRetainedAction,
  ): void {
    this.#options.publish(Object.freeze({
      kind: 'ready',
      operations: Object.freeze([...inventory.operations]),
      error: null,
      pending: pending === undefined
        ? null
        : Object.freeze({
            operationId: pending.operation.operationId,
            action: pending.action,
          }),
    }))
  }

  #clearPending(
    pending: PendingRetainedAction,
    publishClearedSnapshot: boolean,
  ): void {
    if (this.#pending !== pending) return
    this.#pending = undefined
    if (publishClearedSnapshot && this.#inventory === pending.inventory) {
      this.#publishReadyInventory(pending.inventory)
    }
  }

  #settleInventoryLoadFailure(
    pending: PendingInventoryLoad,
    loaded: V2RetainedReceiveInventory | undefined,
  ): void {
    this.#closeLoadedInventory(loaded, pending.attempt)
    if (!this.#isCurrentLoad(pending) || pending.controller.signal.aborted) {
      this.#excludeInactiveLoad(pending)
      return
    }
    this.#recordFailure(pending.attempt, 'retained_inventory')
    this.#options.publish(Object.freeze({
      kind: 'failed',
      operations: Object.freeze([]),
      error: 'Stored receive tasks could not be loaded.',
      pending: null,
    }))
    this.#trace(() => Object.freeze({
      name: 'receive.inventory.load.failed',
    }))
  }

  #finishInventoryLoad(pending: PendingInventoryLoad): void {
    if (!pending.attempt.decisionSettled) {
      this.#exclude(
        pending.attempt,
        pending.controller.signal.aborted ? 'cancelled' : 'not_user_visible',
      )
    }
    this.#closeAttempt(pending.attempt)
    if (this.#load === pending) this.#load = undefined
  }

  async #settleSuccess(
    pending: PendingRetainedAction,
    result: V2RetainedReceiveActionResult,
  ): Promise<void> {
    if (!this.#isCurrent(pending)) {
      await this.#detachContinuationResult(pending, result)
      this.#retireStale(pending)
      return
    }
    if (
      result.kind === 'receive-continuation' &&
      !await this.#adoptContinuation(pending, result.runtime)
    ) {
      return
    }
    this.#completeAction(pending)
  }

  async #detachContinuationResult(
    pending: PendingRetainedAction,
    result: V2RetainedReceiveActionResult,
  ): Promise<void> {
    if (result.kind !== 'receive-continuation') return
    await this.#detachRuntime(pending, result.runtime)
  }

  async #detachRuntime(
    pending: PendingRetainedAction,
    runtime: V2BoundReceiveOperation,
  ): Promise<void> {
    try {
      await Promise.resolve(runtime.detach())
    } catch {
      this.#recordConsequence(pending.attempt, 'detach')
    }
  }

  async #adoptContinuation(
    pending: PendingRetainedAction,
    runtime: V2BoundReceiveOperation,
  ): Promise<boolean> {
    const joined = pending.joined
    if (joined === undefined) {
      await this.#detachRuntime(pending, runtime)
      this.#retireStale(pending)
      return false
    }
    try {
      await this.#validateContinuation(pending, runtime)
      await this.#options.adoptContinuation({
        retained: pending.operation,
        joined,
        runtime,
      })
    } catch (error) {
      await this.#settleContinuationFailure(pending, runtime, error)
      return false
    }
    if (
      !this.#options.ownsRuntime(runtime) ||
      this.#options.isDisposed()
    ) {
      this.#exclude(
        pending.attempt,
        this.#options.isDisposed() ? 'cancelled' : 'authority_invalidated',
      )
      this.#closeAttempt(pending.attempt)
      return false
    }
    return true
  }

  async #settleContinuationFailure(
    pending: PendingRetainedAction,
    runtime: V2BoundReceiveOperation,
    error: unknown,
  ): Promise<void> {
    if (!this.#isCurrent(pending)) {
      await this.#detachRuntime(pending, runtime)
      this.#retireStale(pending)
      return
    }
    const prepared = {
      trigger: pending.attempt.outputFailureTrigger ??
        this.#recordFailureFact(pending.attempt, 'retained_action'),
    }
    await this.#detachRuntime(pending, runtime)
    this.#settleFailure(pending, error, prepared)
  }

  #completeAction(pending: PendingRetainedAction): void {
    const exclusion = pending.action === 'discard' || pending.action === 'delete'
      ? 'user_discarded'
      : 'success'
    this.#exclude(pending.attempt, exclusion)
    try {
      this.#trace(() => Object.freeze({
        name: 'receive.inventory.action.completed',
        retained_action: pending.action,
        continuation: pending.operation.continuation,
      }))
      this.#clearPending(pending, false)
      this.#loadInventory().catch(() => undefined)
    } finally {
      this.#closeAttempt(pending.attempt)
    }
  }

  async #validateContinuation(
    pending: PendingRetainedAction,
    runtime: V2BoundReceiveOperation,
  ): Promise<void> {
    if (!this.#isCurrent(pending)) throw new StaleReceiveBoundaryError()
    const joined = pending.joined
    if (joined === undefined) throw new StaleReceiveBoundaryError()
    const intent = await validateReceiveIntent(runtime.intent)
    if (!this.#isCurrent(pending)) throw new StaleReceiveBoundaryError()
    if (
      intent.shareInstance !== joined.descriptor.shareInstanceId ||
      intent.syntheticRoot !== joined.descriptor.syntheticRootId
    ) {
      throw new DOMException(
        'Stored receive task belongs to a different share authority',
        'InvalidStateError',
      )
    }
    if (
      pending.operation.continuation !== 'resume-receive' ||
      pending.operation.operationId !== intent.operationId ||
      pending.operation.receiveIntentDigest !== intent.digest ||
      runtime.lifecycle.kind !== 'receiving' ||
      runtime.lifecycle.generation !== pending.operation.lifecycleGeneration + 1n ||
      runtime.lifecycle.operationId !== intent.operationId ||
      runtime.lifecycle.receiveIntentDigest !== intent.digest ||
      (
        intent.plan.kind !== 'direct-tree' &&
        intent.plan.kind !== 'workspace-then-publish'
      )
    ) {
      throw new TypeError(
        'retained continuation runtime does not match its persisted authority',
      )
    }
  }

  #isCurrent(pending: PendingRetainedAction): boolean {
    return (
      this.#pending === pending &&
      pending.boundary === this.#boundary &&
      !this.#options.isDisposed() &&
      (
        pending.joined === undefined ||
        (
          this.#options.currentJoinedShare() === pending.joined &&
          pending.protocolSessionId === pending.joined.protocolSessionId
        )
      )
    )
  }

  #retireStale(pending: PendingRetainedAction): void {
    this.#exclude(
      pending.attempt,
      pending.controller.signal.aborted || this.#options.isDisposed()
        ? 'cancelled'
        : 'stale_replacement',
    )
    if (
      this.#pending !== pending ||
      pending.boundary !== this.#boundary ||
      this.#options.isDisposed()
    ) {
      this.#closeAttempt(pending.attempt)
      return
    }
    this.#boundary += 1
    pending.controller.abort(new StaleReceiveBoundaryError())
    try {
      this.#trace(() => Object.freeze({
        name: 'receive.inventory.action.failed',
        retained_action: pending.action,
        continuation: pending.operation.continuation,
      }))
      this.#clearPending(pending, false)
      this.#loadInventory().catch(() => undefined)
    } finally {
      this.#closeAttempt(pending.attempt)
    }
  }

  #settleFailure(
    pending: PendingRetainedAction,
    error: unknown,
    prepared?: PreparedRetainedFailure,
  ): void {
    if (!this.#isCurrent(pending)) {
      this.#retireStale(pending)
      return
    }
    try {
      this.#trace(() => Object.freeze({
        name: 'receive.inventory.action.failed',
        retained_action: pending.action,
        continuation: pending.operation.continuation,
      }))
      if (pending.controller.signal.aborted) {
        this.#exclude(pending.attempt, 'cancelled')
      } else if (error instanceof StaleReceiveBoundaryError) {
        this.#exclude(pending.attempt, 'stale_replacement')
      } else {
        const trigger = pending.attempt.outputFailureTrigger ??
          prepared?.trigger ??
          this.#recordFailureFact(pending.attempt, 'retained_action')
        if (trigger !== undefined) {
          this.#submitFailure(
            pending.attempt,
            'retained_action',
            trigger,
          )
        } else {
          this.#exclude(pending.attempt, 'not_user_visible')
        }
        this.#options.onActionError(error)
      }
    } catch (settlementError) {
      this.#exclude(pending.attempt, 'not_user_visible')
      throw settlementError
    } finally {
      this.#clearPending(pending, false)
      this.#loadInventory().catch(() => undefined)
      this.#closeAttempt(pending.attempt)
    }
  }

  #newAttempt(
    kind: 'retained_inventory' | 'retained_action',
  ): RetainedAttempt {
    return new V2PresentationAttempt(this.#options.incidents, kind)
  }

  #recordFailure(
    attempt: RetainedAttempt,
    stage: 'retained_inventory' | 'retained_action',
  ): void {
    const trigger = attempt.outputFailureTrigger ??
      this.#recordFailureFact(attempt, stage)
    if (trigger === undefined) {
      this.#exclude(attempt, 'not_user_visible')
      return
    }
    this.#submitFailure(attempt, stage, trigger)
  }

  #recordFailureFact(
    attempt: RetainedAttempt,
    stage: 'retained_inventory' | 'retained_action',
  ): FailureFactRef | undefined {
    return attempt.recordUnclassified(stage, 'contributor', 'none')
  }

  #submitFailure(
    attempt: RetainedAttempt,
    stage: 'retained_inventory' | 'retained_action',
    trigger: FailureFactRef,
  ): void {
    attempt.incident(
      stage === 'retained_inventory'
        ? 'retained_inventory'
        : 'retained_action',
      'failed',
      trigger,
    )
  }

  #recordConsequence(
    attempt: RetainedAttempt,
    stage: 'detach' | 'cleanup',
  ): void {
    attempt.recordUnclassified(stage, 'consequence', 'none')
  }

  #exclude(
    attempt: RetainedAttempt,
    reason: PresentationExclusionReason,
  ): void {
    const boundary = attempt.handle?.identity.scopeKind === 'retained_inventory'
      ? 'retained_inventory'
      : 'retained_action'
    attempt.excluded(boundary, reason)
  }

  #closeAttempt(attempt: RetainedAttempt): void {
    attempt.close()
  }

  #trace(createEvent: () => V2RetainedInventoryTraceEvent): void {
    const observer = this.#options.trace?.current
    if (observer === undefined) return
    try {
      observer(createEvent())
    } catch {
      // Detailed tracing cannot alter retained authority or presentation.
    }
  }
}
