import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import type { V2ConnectivityActivation } from '../../connectivity/v2-receiver-policy'
import type {
  LateOutputFailureConsequenceCapability,
  OutputFailureBindingLease,
} from '../../output/diagnostics'
import { isTerminalLifecycleState } from '../../output/workspace'
import { TransferPauseRequestedError } from '../../transfer/output-session'
import { V2TransferFailureSettlementError } from '../../transfer/settlement/v2-output'
import {
  materializeClassifiedTransferFailure,
  type ClassifiedTransferFailure,
} from '../../transfer/job/failures'
import type { V2TransferAdmissionFailureAuthority } from '../../transfer/job/admission-error'
import {
  V2TransferAdmissionFailureError,
  type TransferJobResult,
  type TransferProgress,
} from '../../transfer/v2-job'
import type { LifecycleUserAction } from '../v2-lifecycle-presentation'
import { isAbortError } from '../v2-controller-state'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type { V2OutputPresentationController } from '../v2-output'
import type { V2BoundReceiveOperation } from '../v2-receive-runtime'
import {
  StaleReceiveBoundaryError,
  type V2ActiveCompatibleNameRepairProjection,
  type V2ReceiverControllerOptions,
} from './contracts'
import {
  ActiveReceiveObservability,
  type V2ReceivePresentationAttempt,
} from './active-receive-observability'
import {
  ActiveReceiveLifecycle,
  type ActiveReceiveLifecycleOperation,
} from './active-receive-lifecycle'
import { ActiveReceiveSettlementPresentation } from './active-receive-settlement-presentation'
import type { V2PresentationAttempt } from './presentation-attempt'

export interface ActiveReceiveJoinedShare {
  beginDownloadConnectivity(): V2ConnectivityActivation
  transferJob(
    ...args: Parameters<V2JoinedBrowserShare['transferJob']>
  ): Pick<ReturnType<V2JoinedBrowserShare['transferJob']>, 'run'>
}

interface ActiveReceiveOperation extends ActiveReceiveLifecycleOperation {
  readonly boundary: number
  readonly joined: ActiveReceiveJoinedShare
  readonly selection: V2FrozenSelectionPolicy
  readonly runtime: V2BoundReceiveOperation
  repairProjection?: V2ActiveCompatibleNameRepairProjection
  transfer?: AbortController
  connectivity?: V2ConnectivityActivation
  running?: Promise<void>
  receiveAttempt?: V2ReceivePresentationAttempt
  latestReceiveAttempt?: V2ReceivePresentationAttempt
  receiveOutputLease?: OutputFailureBindingLease
  expiryTimer?: ReturnType<typeof setTimeout>
  detachOutputCapability?: LateOutputFailureConsequenceCapability
  detachment?: Promise<void>
  unsubscribeRepairProjection?: () => void
  unsubscribeRepairProjectionActivation?: () => void
  unsubscribeOutputProgress?: () => void
}

export interface ActiveReceiveCoordinatorOptions {
  readonly outputs: V2OutputPresentationController
  readonly ownsJoinedShare: (joined: ActiveReceiveJoinedShare) => boolean
  readonly onProgress: (progress: TransferProgress) => void
  readonly trace?: V2ReceiverControllerOptions['trace']
  readonly incidents?: V2ReceiverControllerOptions['incidents']
  readonly onActionError: (error: unknown) => void
  readonly onFailure: (error: unknown) => void
}

export interface ActiveReceiveAdoption {
  readonly joined: ActiveReceiveJoinedShare
  readonly selection: V2FrozenSelectionPolicy
  readonly runtime: V2BoundReceiveOperation
  readonly repairProjection?: V2ActiveCompatibleNameRepairProjection
}

export interface PreparedActiveReceiveAdoption {
  /** Ownership installation is deliberately infallible after preparation succeeds. */
  readonly commit: () => void
  readonly start: () => void
}

export class ActiveReceiveCoordinator {
  readonly #outputs: V2OutputPresentationController
  readonly #ownsJoinedShare: (joined: ActiveReceiveJoinedShare) => boolean
  readonly #onProgress: (progress: TransferProgress) => void
  readonly #traceSource: V2ReceiverControllerOptions['trace']
  readonly #observability: ActiveReceiveObservability
  readonly #lifecycle: ActiveReceiveLifecycle
  readonly #onFailure: (error: unknown) => void
  #boundary = 0
  #operation: ActiveReceiveOperation | undefined

  constructor(options: ActiveReceiveCoordinatorOptions) {
    this.#outputs = options.outputs
    this.#ownsJoinedShare = options.ownsJoinedShare
    this.#onProgress = options.onProgress
    this.#traceSource = options.trace
    this.#observability = new ActiveReceiveObservability({
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#onFailure = options.onFailure
    this.#lifecycle = new ActiveReceiveLifecycle({
      outputs: this.#outputs,
      observability: this.#observability,
      currentOperation: () => this.#operation,
      operationIsCurrent: operation =>
        this.#operationIsCurrent(operation as ActiveReceiveOperation),
      startTransfer: operation => this.#startTransfer(operation as ActiveReceiveOperation),
      replaceDetachConsequence: (operation, attempt) =>
        this.#replaceDetachConsequence(operation as ActiveReceiveOperation, attempt),
      onActionError: options.onActionError,
      onFailure: error => this.#reportTransferFailure(error),
    })
  }

  get active(): boolean {
    return this.#operation !== undefined
  }

  ownsRuntime(runtime: V2BoundReceiveOperation): boolean {
    return this.#operation?.runtime === runtime
  }

  adopt(input: ActiveReceiveAdoption): void {
    const output = this.#outputs.getSnapshot()
    if (output.receiveIntent === null ||
        output.receiveIntent.operationId !== input.runtime.intent.operationId ||
        output.receiveIntent.digest !== input.runtime.intent.digest) {
      throw new TypeError('active receive runtime does not belong to the presented receive intent')
    }
    const prepared = this.prepareAdoption(input)
    prepared.commit()
    prepared.start()
  }

  prepareAdoption(input: ActiveReceiveAdoption): PreparedActiveReceiveAdoption {
    if (!this.#ownsJoinedShare(input.joined)) throw new StaleReceiveBoundaryError()
    if (this.#operation !== undefined) {
      throw new TypeError('active receive ownership must be cleared before adoption')
    }
    const settlementPresentation = new ActiveReceiveSettlementPresentation({
      outputs: this.#outputs,
      operationIsCurrent: () => this.#operationIsCurrent(operation),
    })
    const operation: ActiveReceiveOperation = {
      boundary: this.#boundary + 1,
      ...input,
      settlementPresentation,
    }
    let committed = false
    return Object.freeze({
      commit: () => {
        if (committed) return
        committed = true
        this.#boundary = operation.boundary
        this.#lifecycle.cancel('stale_replacement')
        this.#operation = operation
      },
      start: () => {
        if (!committed) return
        this.#startOutputProgress(operation)
        this.#startTransfer(operation)
      },
    })
  }

  performLifecycleAction(action: LifecycleUserAction): void {
    this.#lifecycle.perform(action)
  }

  reset(reason: unknown): Promise<void> {
    this.#lifecycle.cancel(
      reason instanceof StaleReceiveBoundaryError ? 'stale_replacement' : 'cancelled',
    )
    const active = this.#operation
    this.#operation = undefined
    if (active === undefined) return Promise.resolve()
    this.#stopRepairProjection(active)
    this.#stopOutputProgress(active)
    this.#lifecycle.cancelExpiry(active)
    active.transfer?.abort(reason)
    this.#observability.receiveExclusion(
      active.receiveAttempt,
      reason instanceof StaleReceiveBoundaryError ? 'stale_replacement' : 'cancelled',
    )
    if (active.running !== undefined) {
      return active.running.then(
        () => this.#detachOperation(active),
        () => this.#detachOperation(active),
      )
    }
    return this.#detachOperation(active).finally(() => {
      this.#closeAttempt(active.receiveAttempt)
    })
  }

  #startTransfer(active: ActiveReceiveOperation): void {
    if (!this.#operationIsCurrent(active) || active.transfer !== undefined) return
    const attempt = this.#observability.openReceive()
    active.receiveAttempt = attempt
    active.latestReceiveAttempt = attempt
    const outputLease = active.runtime.bindOutputFailures?.(attempt.outputFailures)
    if (outputLease !== undefined) active.receiveOutputLease = outputLease

    let connectivity: V2ConnectivityActivation | undefined
    let transfer: AbortController | undefined
    let task: Promise<void>
    try {
      this.#startRepairProjectionActivation(active)
      this.#startRepairProjection(active)
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
          onProgress: progress => {
            if (this.#operationIsCurrent(active)) this.#onProgress(progress)
          },
          ...(this.#traceSource === undefined ? {} : { trace: this.#traceSource }),
          ...(attempt.handle === undefined ? {} : { incidentScope: attempt.handle }),
          outputSettlementDeadline: active.settlementPresentation,
        },
      )
      task = job.run(transfer.signal).then(
        result => this.#settleTransfer(active, attempt, result),
        (error: unknown) => error instanceof V2TransferAdmissionFailureError
          ? this.#settleTransferAdmissionFailure(active, attempt, error)
          : this.#settleUnexpectedTransferFailure(active, attempt, error),
      )
    } catch (error) {
      transfer?.abort(error)
      task = this.#settleTransferAdmissionFailure(active, attempt, error)
    }

    const ownedConnectivity = connectivity
    const ownedTransfer = transfer
    active.running = task.finally(async () => {
      try {
        ownedConnectivity?.close()
      } catch (error) {
        const classified = this.#observability.classifyTransferFailure(
          attempt,
          error,
          'consequence',
          'cleanup',
        )
        if (classified === undefined) {
          this.#observability.recordUnclassified(attempt, 'cleanup', 'consequence')
        }
      }
      if (active.connectivity === ownedConnectivity) delete active.connectivity
      if (active.transfer === ownedTransfer) delete active.transfer
      if (!this.#operationIsCurrent(active)) await this.#detachOperation(active)
      if (!attempt.decisionSettled) this.#observability.receiveExclusion(attempt, 'success')
      this.#replaceDetachConsequence(active, attempt)
      if (active.receiveOutputLease !== undefined) {
        active.receiveOutputLease.revoke()
        delete active.receiveOutputLease
      }
      this.#closeAttempt(attempt)
      if (active.receiveAttempt === attempt) delete active.receiveAttempt
    })
  }

  async #settleTransfer(
    active: ActiveReceiveOperation,
    attempt: V2ReceivePresentationAttempt,
    result: TransferJobResult,
  ): Promise<void> {
    if (!this.#operationIsCurrent(active)) {
      this.#observability.receiveExclusion(attempt, 'stale_replacement')
      return
    }
    this.#assertTransferResultIdentity(active, result)

    const trigger = this.#observability.transferTrigger(attempt, result.abortReason, result.failureTrigger)
    const usage = await Promise.resolve(active.runtime.resolveWorkspaceUsage(result.lifecycle))
    if (!this.#operationIsCurrent(active)) {
      this.#observability.receiveExclusion(attempt, 'stale_replacement')
      return
    }

    if (trigger !== undefined) {
      this.#observability.receiveIncident(
        attempt,
        trigger,
        ActiveReceiveObservability.receiveOutcome(result.lifecycle),
      )
    } else if (attempt.interruptionExclusion !== undefined) {
      this.#observability.receiveExclusion(attempt, attempt.interruptionExclusion)
    }

    try {
      if (!this.#outputs.updateLifecycle(
        result.lifecycle,
        Date.now(),
        usage,
        Object.freeze([]),
        repairSummaryAtResultBoundary(result),
        result.recoverySummary ?? null,
      )) {
        throw new TypeError('transfer lifecycle did not advance the active receive operation')
      }
      if (!this.#outputs.adoptTransferResult(result)) {
        throw new TypeError('transfer result escaped its receive-intent authority')
      }
    } catch (error) {
      const publicationFailure = this.#observability.classifyTransferFailure(
        attempt,
        error,
        trigger === undefined ? 'contributor' : 'consequence',
        'settlement',
      )
      if (trigger === undefined && publicationFailure !== undefined) {
        this.#observability.receiveIncident(attempt, publicationFailure, 'failed')
      }
      throw error
    }

    if (!attempt.decisionSettled) this.#observability.receiveExclusion(attempt, 'success')
    this.#lifecycle.scheduleExpiry(active)
    if (result.abortReason !== undefined && trigger !== undefined) {
      this.#reportTransferFailure(result.abortReason)
    }
  }

  async #settleTransferAdmissionFailure(
    active: ActiveReceiveOperation,
    attempt: V2ReceivePresentationAttempt,
    error: unknown,
  ): Promise<void> {
    if (!this.#operationIsCurrent(active)) {
      this.#observability.receiveExclusion(attempt, 'stale_replacement')
      return
    }

    const authority = this.#admissionFailureAuthority(error)
    if (authority?.kind === 'canceled') {
      this.#observability.receiveExclusion(attempt, 'cancelled')
    }
    const trigger = this.#admissionFailureTrigger(attempt, error, authority)

    try {
      const mutation = await Promise.resolve(
        active.runtime.settleTransferAdmissionFailure(
          authority?.kind === 'fault' ? authority.classification : error,
        ),
      )
      const applied = await this.#lifecycle.applyMutation(
        active,
        this.#outputs.getSnapshot().lifecycle?.generation ?? 0n,
        mutation,
        () => {
          if (trigger !== undefined) {
            this.#observability.receiveIncident(
              attempt,
              trigger,
              ActiveReceiveObservability.receiveOutcome(mutation.lifecycle),
            )
          } else if (!attempt.decisionSettled) {
            this.#observability.receiveExclusion(attempt, 'cancelled')
          }
        },
      )
      if (!applied) {
        this.#observability.receiveExclusion(attempt, 'stale_replacement')
        return
      }
    } catch (settlementError) {
      this.#reportAdmissionSettlementFailure(attempt, error, trigger, settlementError)
      return
    }

    if (trigger !== undefined) this.#reportTransferFailure(error)
  }

  #admissionFailureAuthority(
    error: unknown,
  ): V2TransferAdmissionFailureAuthority | undefined {
    return error instanceof V2TransferAdmissionFailureError ? error.authority : undefined
  }

  #admissionFailureTrigger(
    attempt: V2ReceivePresentationAttempt,
    error: unknown,
    authority: V2TransferAdmissionFailureAuthority | undefined,
  ): ClassifiedTransferFailure | undefined {
    if (authority?.kind === 'canceled') return undefined
    if (authority?.kind === 'fault') {
      return materializeClassifiedTransferFailure(authority.classification, attempt.handle)
    }
    return this.#observability.classifyTransferFailure(
      attempt,
      error,
      'contributor',
      'content_read',
    )
  }

  #reportAdmissionSettlementFailure(
    attempt: V2ReceivePresentationAttempt,
    admissionError: unknown,
    trigger: ClassifiedTransferFailure | undefined,
    settlementError: unknown,
  ): void {
    const consequence = this.#observability.classifyTransferFailure(
      attempt,
      settlementError,
      trigger === undefined ? 'contributor' : 'consequence',
      'settlement',
    )
    const selectedTrigger = trigger ?? consequence
    if (selectedTrigger !== undefined) {
      this.#observability.receiveIncident(attempt, selectedTrigger, 'failed')
    }
    const consequences = consequence === undefined ? [] : [consequence]
    const productError = selectedTrigger === undefined
      ? admissionError
      : new V2TransferFailureSettlementError(trigger, consequences)
    this.#reportTransferFailure(productError)
  }

  async #settleUnexpectedTransferFailure(
    active: ActiveReceiveOperation,
    attempt: V2ReceivePresentationAttempt,
    error: unknown,
  ): Promise<void> {
    if (!this.#operationIsCurrent(active)) {
      this.#observability.receiveExclusion(attempt, 'stale_replacement')
      return
    }
    const callerOwnedAbort = isAbortError(error) &&
      active.transfer?.signal.aborted === true && active.transfer.signal.reason === error
    if (callerOwnedAbort || error instanceof TransferPauseRequestedError) {
      this.#observability.receiveExclusion(
        attempt,
        attempt.interruptionExclusion ?? 'cancelled',
      )
      return
    }
    const trigger = this.#observability.classifyTransferFailure(
      attempt,
      error,
      'contributor',
      'content_read',
    )
    if (trigger !== undefined) this.#observability.receiveIncident(attempt, trigger, 'failed')
    this.#reportTransferFailure(error)
  }

  #reportTransferFailure(error: unknown): void {
    try {
      this.#onFailure(error)
    } catch {
      // A presentation observer cannot replace the failure that was already published.
    }
  }

  #assertTransferResultIdentity(
    active: ActiveReceiveOperation,
    result: TransferJobResult,
  ): void {
    if (result.intent.operationId !== active.runtime.intent.operationId ||
        result.intent.digest !== active.runtime.intent.digest) {
      throw new TypeError('transfer result does not belong to the active receive intent')
    }
  }

  #operationIsCurrent(active: ActiveReceiveOperation): boolean {
    return this.#operation === active &&
      active.boundary === this.#boundary &&
      this.#ownsJoinedShare(active.joined)
  }

  #startRepairProjection(active: ActiveReceiveOperation): void {
    const projection = active.repairProjection
    if (projection === undefined || active.unsubscribeRepairProjection !== undefined) return
    const unsubscribe = projection.subscribe(summary => {
      if (!this.#operationIsCurrent(active)) return
      this.#outputs.updateRepairSummary(active.runtime.intent.operationId, summary)
    })
    if (this.#operationIsCurrent(active)) {
      active.unsubscribeRepairProjection = unsubscribe
      return
    }
    unsubscribe()
  }

  #startRepairProjectionActivation(active: ActiveReceiveOperation): void {
    if (active.repairProjection !== undefined ||
        active.unsubscribeRepairProjectionActivation !== undefined) return
    const subscribe = active.runtime.subscribeRepairProjectionActivation
    if (subscribe === undefined) return
    const unsubscribe = subscribe.call(active.runtime, projection => {
      if (!this.#operationIsCurrent(active) || active.repairProjection !== undefined) return
      active.repairProjection = projection
      this.#startRepairProjection(active)
    })
    if (this.#operationIsCurrent(active)) {
      active.unsubscribeRepairProjectionActivation = unsubscribe
      return
    }
    unsubscribe()
  }

  #stopRepairProjection(active: ActiveReceiveOperation): void {
    const unsubscribeActivation = active.unsubscribeRepairProjectionActivation
    if (unsubscribeActivation !== undefined) {
      delete active.unsubscribeRepairProjectionActivation
      try {
        unsubscribeActivation()
      } catch {
        // Activation-source cleanup cannot replace receive ownership settlement.
      }
    }
    const unsubscribe = active.unsubscribeRepairProjection
    if (unsubscribe === undefined) return
    delete active.unsubscribeRepairProjection
    try {
      unsubscribe()
    } catch {
      // Presentation-source cleanup cannot replace receive ownership settlement.
    }
  }

  #startOutputProgress(active: ActiveReceiveOperation): void {
    const source = active.runtime.outputProgress
    if (source === undefined || active.unsubscribeOutputProgress !== undefined) return
    const publish = (progress: ReturnType<typeof source.getSnapshot>) => {
      if (!this.#operationIsCurrent(active)) return
      try {
        this.#outputs.updateDirectZipProgress(progress)
      } catch (error) {
        this.#reportTransferFailure(error)
      }
    }
    try {
      publish(source.getSnapshot())
      const unsubscribe = source.subscribe(publish)
      if (this.#operationIsCurrent(active)) {
        active.unsubscribeOutputProgress = unsubscribe
      } else {
        unsubscribe()
      }
    } catch (error) {
      this.#reportTransferFailure(error)
    }
  }

  #stopOutputProgress(active: ActiveReceiveOperation): void {
    const unsubscribe = active.unsubscribeOutputProgress
    if (unsubscribe === undefined) return
    delete active.unsubscribeOutputProgress
    try {
      unsubscribe()
    } catch {
      // The operation remains authoritative; a presentation-source cleanup cannot replace it.
    }
  }

  #detachOperation(active: ActiveReceiveOperation): Promise<void> {
    if (active.detachment !== undefined) return active.detachment
    this.#stopOutputProgress(active)
    const lateCapability = active.detachOutputCapability
    const outputLease = lateCapability === undefined
      ? undefined
      : active.runtime.bindOutputFailures?.(lateCapability.sinks)
    active.detachment = Promise.resolve().then(() => active.runtime.detach()).then(
      () => undefined,
      (error: unknown) => {
        this.#observability.recordUnclassified(
          active.latestReceiveAttempt,
          'detach',
          'consequence',
        )
        throw error
      },
    ).finally(() => {
      outputLease?.revoke()
      lateCapability?.revoke()
      if (active.detachOutputCapability === lateCapability) {
        delete active.detachOutputCapability
      }
    })
    return active.detachment
  }

  #replaceDetachConsequence(
    active: ActiveReceiveOperation,
    attempt: V2PresentationAttempt,
  ): void {
    if (active.detachment !== undefined) return
    active.detachOutputCapability?.revoke()
    delete active.detachOutputCapability
    if (!attempt.incidentSettled) return
    const capability = attempt.createLateCleanupCapability()
    if (capability !== undefined) active.detachOutputCapability = capability
  }

  #closeAttempt(attempt: V2PresentationAttempt | undefined): void {
    attempt?.close()
  }
}

function repairSummaryAtResultBoundary(
  result: TransferJobResult,
): TransferJobResult['repairSummary'] | null {
  if (result.repairSummary !== undefined) return result.repairSummary
  return isTerminalLifecycleState(result.lifecycle) ? null : undefined
}
