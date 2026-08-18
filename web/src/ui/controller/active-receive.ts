import type {
  V2ConnectivityActivation,
} from '../../connectivity/v2-receiver-policy'
import { lifecycleDeadline } from '../../output/workspace'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import type { TransferJobResult, TransferProgress, TransferTraceEvent } from '../../transfer/v2-job'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
} from '../v2-lifecycle-presentation'
import { isAbortError } from '../v2-controller-state'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type { V2OutputPresentationController } from '../v2-output'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
  V2StartedArtifactAuthority,
} from '../v2-receive-runtime'

const MAXIMUM_TIMER_DELAY_MILLISECONDS = 2_147_483_647

interface ActiveReceiveOperation {
  readonly boundary: number
  readonly joined: V2JoinedBrowserShare
  readonly selection: V2FrozenSelectionPolicy
  readonly runtime: V2BoundReceiveOperation
  transfer?: AbortController
  connectivity?: V2ConnectivityActivation
  running?: Promise<void>
  expiryTimer?: ReturnType<typeof setTimeout>
  detachment?: Promise<void>
}

interface PendingLifecycleAction {
  readonly operation: ActiveReceiveOperation
  readonly generation: bigint
  readonly action: LifecycleUserAction
}

export interface ActiveReceiveCoordinatorOptions {
  readonly outputs: V2OutputPresentationController<V2StartedArtifactAuthority>
  readonly ownsJoinedShare: (joined: V2JoinedBrowserShare) => boolean
  readonly onProgress: (progress: TransferProgress) => void
  readonly onTrace: (event: TransferTraceEvent) => void
  readonly onActionError: (error: unknown) => void
  readonly onFailure: (error: unknown) => void
}

export interface ActiveReceiveAdoption {
  readonly joined: V2JoinedBrowserShare
  readonly selection: V2FrozenSelectionPolicy
  readonly runtime: V2BoundReceiveOperation
}

export class ActiveReceiveCoordinator {
  readonly #outputs: V2OutputPresentationController<V2StartedArtifactAuthority>
  readonly #ownsJoinedShare: (joined: V2JoinedBrowserShare) => boolean
  readonly #onProgress: (progress: TransferProgress) => void
  readonly #onTrace: (event: TransferTraceEvent) => void
  readonly #onActionError: (error: unknown) => void
  readonly #onFailure: (error: unknown) => void
  #boundary = 0
  #operation: ActiveReceiveOperation | undefined
  #pendingLifecycleAction: PendingLifecycleAction | undefined

  constructor(options: ActiveReceiveCoordinatorOptions) {
    this.#outputs = options.outputs
    this.#ownsJoinedShare = options.ownsJoinedShare
    this.#onProgress = options.onProgress
    this.#onTrace = options.onTrace
    this.#onActionError = options.onActionError
    this.#onFailure = options.onFailure
  }

  get active(): boolean {
    return this.#operation !== undefined
  }

  ownsRuntime(runtime: V2BoundReceiveOperation): boolean {
    return this.#operation?.runtime === runtime
  }

  adopt(input: ActiveReceiveAdoption): void {
    const operation: ActiveReceiveOperation = {
      boundary: ++this.#boundary,
      ...input,
    }
    this.#pendingLifecycleAction = undefined
    this.#operation = operation
    this.#startTransfer(operation)
  }

  performLifecycleAction(action: LifecycleUserAction): void {
    const active = this.#operation
    const output = this.#outputs.getSnapshot()
    const lifecycle = output.lifecycle
    const presented = output.lifecyclePresentation
    if (active === undefined || lifecycle === null || presented === null ||
        this.#pendingLifecycleAction !== undefined ||
        !presented.actions.some((candidate) => candidate.kind === action)) return

    if (action === 'pause' || action === 'stop') {
      this.#interruptReceive(active, action)
      return
    }

    const pending = Object.freeze({
      operation: active,
      generation: lifecycle.generation,
      action,
    })
    this.#pendingLifecycleAction = pending
    let started: V2LifecycleMutation | PromiseLike<V2LifecycleMutation>
    try {
      // Publication pickers must start before the rendered click returns.
      started = active.runtime.startLifecycleAction(action, lifecycle)
    } catch (error) {
      if (this.#pendingLifecycleAction !== pending) return
      this.#pendingLifecycleAction = undefined
      this.#onActionError(error)
      return
    }
    Promise.resolve(started).then(
      (mutation) => {
        if (this.#pendingLifecycleAction !== pending) return
        this.#pendingLifecycleAction = undefined
        return this.#applyLifecycleMutation(active, lifecycle.generation, mutation)
      },
      (error: unknown) => {
        if (this.#pendingLifecycleAction !== pending) return
        this.#pendingLifecycleAction = undefined
        if (!isAbortError(error)) this.#onActionError(error)
      },
    ).catch(() => undefined)
  }

  reset(reason: unknown): Promise<void> {
    this.#pendingLifecycleAction = undefined
    const active = this.#operation
    this.#operation = undefined
    if (active === undefined) return Promise.resolve()
    if (active.expiryTimer !== undefined) clearTimeout(active.expiryTimer)
    active.transfer?.abort(reason)
    if (active.running !== undefined) {
      return active.running.then(
        () => this.#detachOperation(active),
        () => this.#detachOperation(active),
      )
    }
    return this.#detachOperation(active)
  }

  #startTransfer(active: ActiveReceiveOperation): void {
    if (!this.#operationIsCurrent(active) || active.transfer !== undefined) return
    let connectivity: V2ConnectivityActivation | undefined
    let transfer: AbortController | undefined
    try {
      connectivity = active.joined.beginDownloadConnectivity()
      transfer = new AbortController()
      active.connectivity = connectivity
      active.transfer = transfer
      const job = active.joined.transferJob(
        active.runtime.plans,
        active.runtime.intent,
        connectivity,
        {
          selection: active.selection,
          transferJobId: active.runtime.transferJobId,
          onProgress: (progress) => {
            if (this.#operationIsCurrent(active)) this.#onProgress(progress)
          },
          onTrace: this.#onTrace,
        },
      )
      const ownedConnectivity = connectivity
      const ownedTransfer = transfer
      active.running = job.run(ownedTransfer.signal).then(
        (result) => this.#settleTransfer(active, result),
        (error: unknown) => this.#settleTransferFailure(active, error),
      ).catch((error: unknown) => this.#settleTransferFailure(active, error)).finally(async () => {
        ownedConnectivity.close()
        if (active.connectivity === ownedConnectivity) delete active.connectivity
        if (active.transfer === ownedTransfer) delete active.transfer
        if (!this.#operationIsCurrent(active)) await this.#detachOperation(active)
      })
    } catch (error) {
      transfer?.abort(error)
      connectivity?.close()
      if (active.connectivity === connectivity) delete active.connectivity
      if (active.transfer === transfer) delete active.transfer
      this.#settleTransferFailure(active, error).catch((settlementError: unknown) => {
        if (this.#operationIsCurrent(active)) this.#onFailure(settlementError)
      })
    }
  }

  async #settleTransfer(active: ActiveReceiveOperation, result: TransferJobResult): Promise<void> {
    if (!this.#operationIsCurrent(active)) return
    if (result.intent.digest !== active.runtime.intent.digest) {
      throw new TypeError('transfer result does not belong to the active receive intent')
    }
    const usage = await Promise.resolve(active.runtime.resolveWorkspaceUsage(result.lifecycle))
    if (!this.#operationIsCurrent(active)) return
    if (!this.#outputs.updateLifecycle(result.lifecycle, Date.now(), usage, Object.freeze([]))) {
      throw new TypeError('transfer lifecycle did not advance the active receive operation')
    }
    this.#scheduleExpiry(active)
  }

  async #settleTransferFailure(active: ActiveReceiveOperation, error: unknown): Promise<void> {
    if (!this.#operationIsCurrent(active)) return
    try {
      const mutation = await Promise.resolve(active.runtime.abandon(error))
      await this.#applyLifecycleMutation(
        active,
        this.#outputs.getSnapshot().lifecycle?.generation ?? 0n,
        mutation,
      )
    } catch (settlementError) {
      if (this.#operationIsCurrent(active)) this.#onFailure(settlementError)
    }
  }

  #interruptReceive(active: ActiveReceiveOperation, control: V2ActiveReceiveControl): void {
    const transfer = active.transfer
    if (transfer === undefined || transfer.signal.aborted ||
        !active.runtime.activeControls.includes(control)) return
    this.#outputs.updateActiveControls(Object.freeze([]))
    try {
      active.runtime.interrupt(control, transfer)
      if (!transfer.signal.aborted) {
        throw new TypeError('receive interruption did not synchronously close the transfer lifetime')
      }
    } catch (error) {
      this.#outputs.updateActiveControls(active.runtime.activeControls)
      this.#onActionError(error)
    }
  }

  async #applyLifecycleMutation(
    active: ActiveReceiveOperation,
    expectedGeneration: bigint,
    mutation: V2LifecycleMutation,
  ): Promise<void> {
    const current = this.#outputs.getSnapshot().lifecycle
    if (!this.#operationIsCurrent(active) || current === null ||
        current.generation !== expectedGeneration ||
        mutation.lifecycle.operationId !== active.runtime.intent.operationId ||
        mutation.lifecycle.receiveIntentDigest !== active.runtime.intent.digest) return
    const usage = mutation.workspaceUsage === undefined
      ? await Promise.resolve(active.runtime.resolveWorkspaceUsage(mutation.lifecycle))
      : mutation.workspaceUsage
    if (!this.#operationIsCurrent(active) ||
        this.#outputs.getSnapshot().lifecycle?.generation !== expectedGeneration) return
    const controls = mutation.activeControls ?? Object.freeze([])
    if (!this.#outputs.updateLifecycle(mutation.lifecycle, Date.now(), usage, controls)) return
    this.#scheduleExpiry(active)
    if (mutation.resumeTransfer === true) this.#resumeTransferWhenIdle(active)
  }

  #resumeTransferWhenIdle(active: ActiveReceiveOperation): void {
    const running = active.running
    if (active.transfer === undefined || running === undefined) {
      this.#startTransfer(active)
      return
    }
    running.finally(() => {
      if (this.#operationIsCurrent(active)) this.#startTransfer(active)
    }).catch(() => undefined)
  }

  #scheduleExpiry(active: ActiveReceiveOperation): void {
    if (active.expiryTimer !== undefined) clearTimeout(active.expiryTimer)
    delete active.expiryTimer
    const lifecycle = this.#outputs.getSnapshot().lifecycle
    if (!this.#operationIsCurrent(active) || lifecycle === null) return
    const deadline = lifecycleDeadline(lifecycle)
    if (deadline === undefined) return
    const delay = Math.min(MAXIMUM_TIMER_DELAY_MILLISECONDS, Math.max(0, deadline - Date.now()))
    const expectedGeneration = lifecycle.generation
    active.expiryTimer = setTimeout(() => {
      delete active.expiryTimer
      this.#observeExpiry(active, expectedGeneration, deadline).catch((error: unknown) => {
        if (!isAbortError(error) && this.#operationIsCurrent(active)) this.#onFailure(error)
      })
    }, delay)
  }

  async #observeExpiry(
    active: ActiveReceiveOperation,
    expectedGeneration: bigint,
    deadline: number,
  ): Promise<void> {
    if (!this.#operationIsCurrent(active) ||
        this.#outputs.getSnapshot().lifecycle?.generation !== expectedGeneration) return
    if (Date.now() < deadline) {
      this.#scheduleExpiry(active)
      return
    }
    const lifecycle = this.#outputs.getSnapshot().lifecycle
    if (lifecycle === null) return
    const mutation = await active.runtime.observeExpiry(lifecycle)
    await this.#applyLifecycleMutation(active, expectedGeneration, mutation)
  }

  #operationIsCurrent(active: ActiveReceiveOperation): boolean {
    return this.#operation === active && active.boundary === this.#boundary &&
      this.#ownsJoinedShare(active.joined)
  }

  #detachOperation(active: ActiveReceiveOperation): Promise<void> {
    active.detachment ??= Promise.resolve(active.runtime.detach()).then(() => undefined)
    return active.detachment
  }
}
