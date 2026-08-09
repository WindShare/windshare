import { validateReceiveIntent } from '../../transfer/intent'
import { isAbortError } from '../v2-controller-state'
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
import { StaleReceiveBoundaryError, type V2RetainedInventoryTraceEvent } from './contracts'

interface PendingRetainedAction {
  readonly boundary: number
  readonly inventory: V2RetainedReceiveInventory
  readonly operation: V2RetainedReceiveOperation
  readonly action: V2RetainedReceiveAction
  readonly joined?: V2JoinedBrowserShare
  readonly protocolSessionId?: string
  readonly controller: AbortController
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
  readonly onTrace: (event: V2RetainedInventoryTraceEvent) => void
  readonly onActionError: (error: unknown) => void
}

export class RetainedInventoryCoordinator {
  readonly #options: RetainedInventoryCoordinatorOptions
  #load: AbortController | undefined
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

  perform(operation: V2RetainedReceiveOperation, action: V2RetainedReceiveAction): void {
    const inventory = this.#inventory
    if (this.#options.isDisposed() || inventory === undefined || this.#pending !== undefined ||
        !inventory.operations.includes(operation) || !operation.actions.includes(action)) return
    const joined = action === 'continue' ? this.#options.currentJoinedShare() : undefined
    if (action === 'continue' && (joined === undefined || this.#options.continuationBlocked())) {
      this.#options.onActionError(new DOMException(
        'Open the matching share before continuing this receive task',
        'InvalidStateError',
      ))
      return
    }

    const controller = new AbortController()
    const pending: PendingRetainedAction = Object.freeze({
      boundary: ++this.#boundary,
      inventory,
      operation,
      action,
      ...(joined === undefined ? {} : { joined, protocolSessionId: joined.protocolSessionId }),
      controller,
    })
    this.#pending = pending
    this.#options.onTrace(Object.freeze({
      name: 'receive.inventory.action.started',
      retained_action: action,
      continuation: operation.continuation,
    }))
    let started: ReturnType<V2RetainedReceiveInventory['act']>
    try {
      // The live ResumeRef is consumed in the explicit user-action stack.
      started = inventory.act(operation, action, controller.signal)
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
    const pending = this.#pending
    if (pending === undefined) return
    this.#boundary += 1
    this.#pending = undefined
    pending.controller.abort(reason)
  }

  close(reason: unknown): void {
    this.#load?.abort(reason)
    this.#load = undefined
    this.cancelPending(reason)
    this.#inventory?.close()
    this.#inventory = undefined
  }

  async #loadInventory(): Promise<void> {
    this.#load?.abort(new DOMException('Resume inventory reloaded', 'AbortError'))
    this.#inventory?.close()
    this.#inventory = undefined
    const controller = new AbortController()
    this.#load = controller
    this.#options.publish(EMPTY_V2_RETAINED_INVENTORY)
    this.#options.onTrace(Object.freeze({ name: 'receive.inventory.load.started' }))
    let loaded: V2RetainedReceiveInventory | undefined
    try {
      loaded = await this.#options.receive.retained.list(controller.signal)
      controller.signal.throwIfAborted()
      if (this.#options.isDisposed() || this.#load !== controller) {
        loaded.close()
        return
      }
      this.#inventory = loaded
      this.#options.onTrace(Object.freeze({
        name: 'receive.inventory.load.completed',
        operation_count: loaded.operations.length,
      }))
      this.#options.publish(Object.freeze({
        kind: 'ready',
        operations: Object.freeze([...loaded.operations]),
        error: null,
      }))
    } catch (error) {
      loaded?.close()
      if (controller.signal.aborted || this.#options.isDisposed() || this.#load !== controller) return
      this.#options.publish(Object.freeze({
        kind: 'failed',
        operations: Object.freeze([]),
        error: 'Stored receive tasks could not be loaded.',
      }))
      this.#options.onTrace(Object.freeze({
        name: 'receive.inventory.load.failed',
        error_name: error instanceof Error ? error.name : 'UnknownError',
      }))
    } finally {
      if (this.#load === controller) this.#load = undefined
    }
  }

  async #settleSuccess(
    pending: PendingRetainedAction,
    result: V2RetainedReceiveActionResult,
  ): Promise<void> {
    if (!this.#isCurrent(pending)) {
      if (result.kind === 'receive-continuation') {
        await Promise.resolve(result.runtime.detach()).catch(() => undefined)
      }
      this.#retireStale(pending)
      return
    }
    if (result.kind === 'receive-continuation') {
      const joined = pending.joined
      if (joined === undefined) {
        await Promise.resolve(result.runtime.detach()).catch(() => undefined)
        this.#settleFailure(pending, new StaleReceiveBoundaryError())
        return
      }
      try {
        await this.#validateContinuation(pending, result.runtime)
        await this.#options.adoptContinuation({
          retained: pending.operation,
          joined,
          runtime: result.runtime,
        })
      } catch (error) {
        await Promise.resolve(result.runtime.detach()).catch(() => undefined)
        this.#settleFailure(pending, error)
        return
      }
      if (!this.#options.ownsRuntime(result.runtime) || this.#options.isDisposed()) return
    } else if (!this.#isCurrent(pending)) {
      this.#retireStale(pending)
      return
    }
    this.#pending = undefined
    this.#options.onTrace(Object.freeze({
      name: 'receive.inventory.action.completed',
      retained_action: pending.action,
      continuation: pending.operation.continuation,
    }))
    this.#loadInventory().catch(() => undefined)
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
    if (intent.shareInstance !== joined.descriptor.shareInstanceId ||
        intent.syntheticRoot !== joined.descriptor.syntheticRootId) {
      throw new DOMException(
        'Stored receive task belongs to a different share authority',
        'InvalidStateError',
      )
    }
    if (pending.operation.continuation !== 'resume-receive' ||
        pending.operation.operationId !== intent.operationId ||
        pending.operation.receiveIntentDigest !== intent.digest ||
        runtime.lifecycle.kind !== 'receiving' ||
        runtime.lifecycle.generation !== pending.operation.lifecycleGeneration + 1n ||
        runtime.lifecycle.operationId !== intent.operationId ||
        runtime.lifecycle.receiveIntentDigest !== intent.digest ||
        (intent.plan.kind !== 'direct-tree' && intent.plan.kind !== 'workspace-then-publish')) {
      throw new TypeError('retained continuation runtime does not match its persisted authority')
    }
  }

  #isCurrent(pending: PendingRetainedAction): boolean {
    return this.#pending === pending && pending.boundary === this.#boundary &&
      !this.#options.isDisposed() && (pending.joined === undefined || (
        this.#options.currentJoinedShare() === pending.joined &&
        pending.protocolSessionId === pending.joined.protocolSessionId
      ))
  }

  #retireStale(pending: PendingRetainedAction): void {
    if (this.#pending !== pending || pending.boundary !== this.#boundary ||
        this.#options.isDisposed()) return
    this.cancelPending(new StaleReceiveBoundaryError())
    this.#options.onTrace(Object.freeze({
      name: 'receive.inventory.action.failed',
      retained_action: pending.action,
      continuation: pending.operation.continuation,
      error_name: 'AbortError',
    }))
    this.#loadInventory().catch(() => undefined)
  }

  #settleFailure(pending: PendingRetainedAction, error: unknown): void {
    if (!this.#isCurrent(pending)) {
      this.#retireStale(pending)
      return
    }
    this.#pending = undefined
    this.#options.onTrace(Object.freeze({
      name: 'receive.inventory.action.failed',
      retained_action: pending.action,
      continuation: pending.operation.continuation,
      error_name: error instanceof Error ? error.name : 'UnknownError',
    }))
    if (!isAbortError(error)) this.#options.onActionError(error)
    this.#loadInventory().catch(() => undefined)
  }
}
