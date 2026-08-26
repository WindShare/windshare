import {
  snapshotRecoverySelectionFacts,
  type ReceiveLifecycleState,
  type RecoverySelectionFacts,
} from '../output/workspace/state'
import type { TransferJobOptions, TransferJobResult } from './job/contract'
import {
  V2ClassifiedTransferFailureError,
  normalizeV2FileTransferFailure,
  type ClassifiedTransferFailure,
} from './job/failures'
import {
  failedTreeOutcome,
  requireSuccessfulWorker,
  validateCompletionLifecycle,
  validatePauseLifecycle,
  validatePreparationRejectionLifecycle,
  validateStopLifecycle,
} from './job/lifecycle'
import type { V2TransferObservers } from './job/observers'
import type { SelectionMeasure, SelectionMeasureTracker } from './measure'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
  type CompletedTransferWorkerSettlement,
  type TransferWorkerSettlement,
} from './outcome'
import {
  TransferStopRequestedError,
  type ExactPreparationEvidence,
  type MaterializationSummary,
  type PlanExecution,
} from './output-session'
import type { V2TransferProgressLedger } from './progress/v2-ledger'
import {
  pauseFailedV2Execution,
  withOutputSettlementTimeout,
  withQuiescentOutputSettlementTimeout,
} from './settlement/v2-output'
import type { V2JobFailureAuthority } from './v2-job-failure-authority'

export interface TransferJobSettlementContext {
  readonly options: Pick<
    TransferJobOptions,
    'plans' | 'incidentScope' | 'outputSettlementDeadline'
  >
  readonly lifetime: AbortController
  readonly measure: SelectionMeasureTracker
  readonly failures: V2JobFailureAuthority
  readonly progress: V2TransferProgressLedger
  readonly transferJobId: string
  readonly outputSettlementTimeoutMilliseconds: number
  readonly intent: () => TransferJobOptions['intent']
  readonly execution: () => PlanExecution | undefined
  readonly preparation: () => ExactPreparationEvidence | undefined
  readonly observers: () => V2TransferObservers | undefined
  readonly externalCancellationRequested: () => boolean
  readonly materializationSummary: () => MaterializationSummary
  readonly emitProgress: () => void
}

/**
 * Owns worker-to-lifecycle settlement so every terminal route shares one error,
 * cancellation, and timeout ordering independent of materialization strategy.
 */
export class TransferJobSettlement {
  readonly #context: TransferJobSettlementContext
  #lastWorker: TransferWorkerSettlement | undefined

  constructor(context: TransferJobSettlementContext) {
    this.#context = context
  }

  async completeWorkers(measure: SelectionMeasure): Promise<TransferJobResult> {
    const worker: CompletedTransferWorkerSettlement = this.#context.failures.failureCount === 0
      ? transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
      : transferWorkerSettlement('CompletedWithErrors', this.#context.failures.snapshot())
    this.#lastWorker = worker
    const summary = this.#context.materializationSummary()
    const execution = this.#requireExecution()
    if (execution.planKind !== 'direct-tree' && worker.status !== 'Succeeded') {
      this.#context.observers()?.materializationFailed(
        worker.trigger?.materializationFailureReason ?? 'content-read-failed',
        BigInt(this.#context.progress.completedFiles),
        this.#context.progress.completedBytes,
      )
      const failureTrigger = requireWorkerFailureTrigger(worker)
      const reason = new V2ClassifiedTransferFailureError(failureTrigger)
      const lifecycle = await this.#pause(worker, reason, measure, failureTrigger)
      return this.#result(worker, lifecycle, measure, reason, failureTrigger)
    }
    this.#context.observers()?.materializationCompleted(summary)
    const lifecycle = await this.#settleCompleted(execution, worker, summary)
    if (execution.planKind === 'direct-tree') {
      this.#context.observers()?.treeFinalized({
        outcome: lifecycle.kind === 'partial-directory' ? 'partial-directory' : 'published',
        successCount: BigInt(this.#context.progress.completedFiles),
        failureCount: BigInt(worker.failureCount),
      })
    }
    return this.#result(worker, lifecycle, measure)
  }

  async settleIncompletePreparation(measure: SelectionMeasure): Promise<TransferJobResult> {
    const worker = transferWorkerSettlement('CompletedWithErrors', this.#context.failures.snapshot())
    this.#lastWorker = worker
    const failureTrigger = requireWorkerFailureTrigger(worker)
    const reason = new V2ClassifiedTransferFailureError(failureTrigger)
    const lifecycle = await this.#pause(worker, reason, measure, failureTrigger)
    return this.#result(worker, lifecycle, measure, reason, failureTrigger)
  }

  preparationRejected(state: ReceiveLifecycleState): TransferJobResult {
    const lifecycle = validatePreparationRejectionLifecycle(this.#context.intent(), state)
    const worker = transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY)
    this.#lastWorker = worker
    return this.#result(worker, lifecycle, this.#context.measure.snapshot())
  }

  async settleRunFailure(error: unknown): Promise<TransferJobResult> {
    const normalized = normalizeV2FileTransferFailure(error, {
      ...(this.#context.externalCancellationRequested()
        ? { signal: this.#context.lifetime.signal }
        : {}),
      ...(this.#context.options.incidentScope === undefined
        ? {}
        : { incidentScope: this.#context.options.incidentScope }),
    })
    const abortReason = normalized.diagnostic
    const failureTrigger = normalized.kind === 'fault'
      ? normalized.diagnostic.classification
      : undefined
    if (!this.#context.lifetime.signal.aborted) this.#context.lifetime.abort(abortReason)
    let measure = this.#context.measure.snapshot()
    if (measure.discovery === 'open') {
      measure = this.#context.measure.fail()
      this.#context.observers()?.measure(measure)
      this.#context.emitProgress()
    }
    const worker = this.#lastWorker ?? transferWorkerSettlement(
      'Paused',
      this.#context.failures.snapshot(),
    )
    this.#context.observers()?.materializationFailed(
      failureTrigger?.materializationFailureReason ??
        worker.trigger?.materializationFailureReason ??
        'content-read-failed',
      BigInt(this.#context.progress.completedFiles),
      this.#context.progress.completedBytes,
    )
    const lifecycle = abortReason instanceof TransferStopRequestedError
      ? await this.#stop(worker, abortReason)
      : await this.#pause(worker, abortReason, measure, failureTrigger)
    if (this.#context.execution()?.planKind === 'direct-tree') {
      const outcome = failedTreeOutcome(lifecycle)
      if (outcome !== undefined) {
        this.#context.observers()?.treeFinalized({
          outcome,
          successCount: BigInt(this.#context.progress.completedFiles),
          failureCount: BigInt(worker.failureCount),
        })
      }
    }
    return this.#result(worker, lifecycle, measure, abortReason, failureTrigger)
  }

  async #settleCompleted(
    execution: PlanExecution,
    worker: CompletedTransferWorkerSettlement,
    summary: MaterializationSummary,
  ): Promise<ReceiveLifecycleState> {
    if (execution.planKind === 'direct-tree') {
      const state = await withQuiescentOutputSettlementTimeout(
        'settle completed plan execution',
        this.#context.outputSettlementTimeoutMilliseconds,
        signal => execution.settle({
          transferJobId: this.#context.transferJobId,
          worker,
          materialization: summary,
        }, signal),
        this.#context.options.outputSettlementDeadline,
      )
      return validateCompletionLifecycle(this.#context.intent(), worker, state)
    }

    const controller = new AbortController()
    try {
      const state = await withOutputSettlementTimeout(
        'settle completed plan execution',
        this.#context.outputSettlementTimeoutMilliseconds,
        () => execution.settle({
          transferJobId: this.#context.transferJobId,
          worker: requireSuccessfulWorker(worker),
          materialization: summary,
        }, controller.signal),
        undefined,
        this.#context.options.outputSettlementDeadline,
      )
      return validateCompletionLifecycle(this.#context.intent(), worker, state)
    } finally {
      controller.abort(new DOMException('Plan settlement boundary closed', 'AbortError'))
    }
  }

  async #pause(
    worker: TransferWorkerSettlement,
    reason: unknown,
    measure: SelectionMeasure,
    failureTrigger?: ClassifiedTransferFailure,
  ): Promise<ReceiveLifecycleState> {
    const execution = this.#context.execution()
    return pauseFailedV2Execution({
      intent: this.#context.intent(),
      ...(execution === undefined ? {} : { execution }),
      authority: this.#context.options.plans,
      worker,
      materialization: this.#context.materializationSummary(),
      selectionFacts: recoverySelectionFacts(measure),
      reason,
      ...(failureTrigger === undefined ? {} : { failureTrigger }),
      ...(this.#context.options.incidentScope === undefined
        ? {}
        : { incidentScope: this.#context.options.incidentScope }),
      timeoutMilliseconds: this.#context.outputSettlementTimeoutMilliseconds,
      ...(this.#context.options.outputSettlementDeadline === undefined
        ? {}
        : { deadline: this.#context.options.outputSettlementDeadline }),
      validateState: state => validatePauseLifecycle(this.#context.intent(), worker, state),
    })
  }

  async #stop(
    worker: TransferWorkerSettlement,
    reason: TransferStopRequestedError,
  ): Promise<ReceiveLifecycleState> {
    if (worker.status !== 'Paused') {
      throw new TypeError('Stop-and-keep-partial requires a drained paused worker settlement')
    }
    const execution = this.#requireExecution()
    if (execution.planKind !== 'direct-tree') {
      throw new TypeError('Stop-and-keep-partial requires DirectTree execution')
    }
    const stop = execution.stop
    if (stop === undefined) {
      throw new TypeError('DirectTree execution does not implement Stop-and-keep-partial')
    }
    return validateStopLifecycle(
      this.#context.intent(),
      await withQuiescentOutputSettlementTimeout(
        'stop direct-tree execution',
        this.#context.outputSettlementTimeoutMilliseconds,
        signal => stop({
          transferJobId: this.#context.transferJobId,
          worker,
          materialization: this.#context.materializationSummary(),
          reason,
        }, signal),
        this.#context.options.outputSettlementDeadline,
      ),
    )
  }

  #result(
    worker: TransferWorkerSettlement,
    lifecycle: ReceiveLifecycleState,
    measure: SelectionMeasure,
    abortReason?: unknown,
    failureTrigger: ClassifiedTransferFailure | undefined = worker.trigger,
  ): TransferJobResult {
    const execution = this.#context.execution()
    const preparation = this.#context.preparation()
    const repairSummary = execution?.planKind === 'direct-tree'
      ? execution.repairSummary?.()
      : undefined
    const recoverySummary = execution?.planKind === 'direct-tree'
      ? execution.recoverySummary?.()
      : undefined
    return Object.freeze({
      worker,
      lifecycle,
      measure,
      transferJobId: this.#context.transferJobId,
      intent: this.#context.intent(),
      ...(abortReason === undefined ? {} : { abortReason }),
      ...(failureTrigger === undefined ? {} : { failureTrigger }),
      ...(execution === undefined ? {} : { outputDurability: execution.output.capabilities.durability }),
      ...(preparation === undefined ? {} : { preparation }),
      ...(repairSummary === undefined ? {} : { repairSummary }),
      ...(recoverySummary === undefined ? {} : { recoverySummary }),
    })
  }

  #requireExecution(): PlanExecution {
    const execution = this.#context.execution()
    if (execution === undefined) throw new Error('plan execution is unavailable')
    return execution
  }
}

function recoverySelectionFacts(measure: SelectionMeasure): RecoverySelectionFacts {
  if (!Number.isSafeInteger(measure.discoveredFiles) || measure.discoveredFiles < 0) {
    throw new TypeError('pause selection file count must be an exact non-negative integer')
  }
  if (measure.discoveredBytes < 0n || measure.discovery === 'open') {
    throw new TypeError('pause selection facts require terminal non-negative discovery evidence')
  }
  return snapshotRecoverySelectionFacts({
    discoveredFileCount: BigInt(measure.discoveredFiles),
    discoveredBytes: measure.discoveredBytes,
    discovery: measure.discovery,
  })
}

function requireWorkerFailureTrigger(
  worker: TransferWorkerSettlement,
): ClassifiedTransferFailure {
  if (worker.trigger === undefined) {
    throw new TypeError('Failed transfer worker is missing nominated failure authority')
  }
  return worker.trigger
}
