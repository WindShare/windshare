import type { V2ContentLaneStatus } from '../../content/v2-broker'
import type { IncidentScopeHandle } from '../../diagnostics/incident'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import {
  observePerformance,
  type PerformanceSummaryObservations,
} from '../../output/diagnostics/performance-summary'
import type { ReceiveIntent } from '../intent'
import type { SelectionMeasure } from '../measure'
import type { MaterializationSummary } from '../output-session'
import type {
  ArtifactLayoutClass,
  MaterializationFailureReason,
  TransferProgress,
  TransferTraceEvent,
} from './contract'
import { normalizeV2FileTransferFailure } from './failures'
import type { WorkerFamilyConsequenceFailure } from '../worker-family/supervisor'
import type { V2RevisionCapacityTrace } from '../revision-capacity/public'

export interface V2TransferObserversOptions {
  readonly intent: ReceiveIntent
  readonly transferJobId: string
  readonly protocol?: Readonly<{
    readonly sessionId: string
    readonly generation: number
  }>
  readonly lanes: V2ContentLaneStatus
  readonly incidentScope?: IncidentScopeHandle
  readonly onProgress?: (progress: TransferProgress) => void
  readonly onMeasure?: (measure: SelectionMeasure) => void
  readonly trace?: DomainTraceSource<TransferTraceEvent>
}

export interface V2TransferProgressState {
  readonly measure: SelectionMeasure
  readonly writtenBytes: bigint
  readonly recoverableBytes: bigint
  readonly completedFiles: number
  readonly completedBytes: bigint
  readonly fileErrors: number
  readonly selectionErrors: number
  readonly failedDirectories: number
  readonly capacityWaitingFiles: number
  readonly capacityAccumulatedWaitMilliseconds: number
  readonly capacityWaitAttempts: number
  readonly capacityWaitVisible: boolean
  readonly outputSessionId?: string
}

/**
 * Product observers and trace have separate ports. A trace payload is built only
 * after the revocable source yields an active observer.
 */
export class V2TransferObservers {
  readonly #options: V2TransferObserversOptions
  #performance: PerformanceSummaryObservations | undefined

  constructor(options: V2TransferObserversOptions) {
    this.#options = options
  }

  bindPerformance(performance: PerformanceSummaryObservations | undefined): void {
    if (performance === undefined || this.#performance !== undefined) return
    this.#performance = performance
  }

  completePerformance(): void {
    observePerformance(this.#performance, summary => summary.complete())
  }

  measure(value: SelectionMeasure): void {
    try {
      this.#options.onMeasure?.(value)
    } catch {
      // Product observers cannot alter catalog, output, or settlement authority.
    }
  }

  progress(state: V2TransferProgressState): void {
    const partial = state.measure.discovery === 'failed' ||
      state.failedDirectories > 0 ||
      state.fileErrors > 0 ||
      state.selectionErrors > 0
    try {
      this.#options.onProgress?.(Object.freeze({
        discoveredFiles: state.measure.discoveredFiles,
        discoveredBytes: state.measure.discoveredBytes,
        writtenBytes: state.writtenBytes,
        recoverableBytes: state.recoverableBytes,
        completedFiles: state.completedFiles,
        completedBytes: state.completedBytes,
        fileErrors: state.fileErrors,
        selectionErrors: state.selectionErrors,
        contentLanes: this.#options.lanes.size,
        discovery: state.measure.discovery,
        failedDirectories: state.failedDirectories,
        capacityWaitingFiles: state.capacityWaitingFiles,
        capacityAccumulatedWaitMilliseconds: state.capacityAccumulatedWaitMilliseconds,
        capacityWaitAttempts: state.capacityWaitAttempts,
        capacityWaitVisible: state.capacityWaitVisible,
        partial,
        transferJobId: this.#options.transferJobId,
        ...(state.outputSessionId === undefined ? {} : { outputSessionId: state.outputSessionId }),
      }))
    } catch {
      // Product observers cannot alter catalog, output, or settlement authority.
    }

    observePerformance(this.#performance, summary => {
      if (state.writtenBytes > 0n) summary.markMilestone('first_write')
      const discoveryComplete = state.measure.discovery === 'complete'
      const allBytesWritten = state.writtenBytes === state.measure.discoveredBytes
      if (discoveryComplete && allBytesWritten) summary.markMilestone('last_byte')
      const allFilesFinal = state.completedFiles === state.measure.discoveredFiles &&
        state.completedBytes === state.measure.discoveredBytes
      if (discoveryComplete && allFilesFinal) {
        // A fully recovered attempt can have no write callback, but its final proof
        // still proves that the last required byte preceded the last finalization.
        summary.markMilestone('last_byte')
        summary.markMilestone('last_final')
      }
    })

    this.#emit(() => Object.freeze({
      name: 'transfer_progress',
      discoveredFiles: BigInt(state.measure.discoveredFiles),
      discoveredBytes: state.measure.discoveredBytes,
      writtenBytes: state.writtenBytes,
      completedFiles: BigInt(state.completedFiles),
      completedBytes: state.completedBytes,
      fileErrors: BigInt(state.fileErrors),
      selectionErrors: BigInt(state.selectionErrors),
      failedDirectories: BigInt(state.failedDirectories),
      capacityWaitingFiles: BigInt(state.capacityWaitingFiles),
      capacityAccumulatedWaitMilliseconds: state.capacityAccumulatedWaitMilliseconds,
      capacityWaitAttempts: BigInt(state.capacityWaitAttempts),
      capacityWaitVisible: state.capacityWaitVisible,
      contentLanes: this.#options.lanes.size,
      discovery: state.measure.discovery,
      partial,
    }))
  }

  intentFrozen(layoutClass: ArtifactLayoutClass): void {
    this.#emit(() => Object.freeze({
      name: 'receive_transition',
      transition: 'intent_frozen',
      artifactKind: this.#options.intent.artifact.kind,
      layoutClass,
      planKind: this.#options.intent.plan.kind,
    }))
  }

  directoryAdmitted(input: {
    readonly admittedDirectoryCount: bigint
    readonly layoutClass: Exclude<ArtifactLayoutClass, 'original-file'>
  }): void {
    this.#emit(() => Object.freeze({
      name: 'receive_transition',
      transition: 'directory_admitted',
      admittedDirectoryCount: input.admittedDirectoryCount,
      layoutClass: input.layoutClass,
    }))
  }

  materializationStarted(): void {
    this.#emit(() => Object.freeze({
      name: 'receive_transition',
      transition: 'materialization_started',
      planKind: this.#options.intent.plan.kind,
    }))
  }

  workerConsequence(
    workerFamily: 'discovery' | 'prepared-files',
    consequence: WorkerFamilyConsequenceFailure,
    outputSessionId?: string,
  ): void {
    try {
      normalizeV2FileTransferFailure(consequence.failure, {
        relation: 'consequence',
        ...(this.#options.incidentScope === undefined
          ? {}
          : { incidentScope: this.#options.incidentScope }),
      })
    } catch {
      // Incident aggregation is passive and cannot interfere with family drainage.
    }
    this.#emit(() => Object.freeze({
      name: 'receive_transition',
      transition: 'worker_consequence_observed',
      workerFamily,
      failureSource: consequence.source,
      operationId: this.#options.intent.operationId,
      transferJobId: this.#options.transferJobId,
      ...(this.#options.protocol === undefined
        ? {}
        : {
            protocolSessionId: this.#options.protocol.sessionId,
            protocolGeneration: this.#options.protocol.generation,
          }),
      ...(outputSessionId === undefined ? {} : { outputSessionId }),
    }))
  }

  capacityWait(event: V2RevisionCapacityTrace): void {
    this.#emit(() => Object.freeze({
      name: 'receive_transition',
      transition: event.transition,
      capacityWaitId: event.waitId,
      capacitySurface: event.surface,
      receiveOperationId: this.#options.intent.operationId,
      transferJobId: this.#options.transferJobId,
      protocolSessionId: event.protocolSessionId,
      protocolOperationId: event.protocolOperationId,
      attempt: BigInt(event.attempt),
      senderHintMilliseconds: event.senderHintMilliseconds,
      jitterMilliseconds: event.jitterMilliseconds,
      delayMilliseconds: event.delayMilliseconds,
      accumulatedWaitMilliseconds: event.accumulatedWaitMilliseconds,
      activeWaiters: event.activeWaiters,
    }))
  }

  materializationFailed(
    reason: MaterializationFailureReason,
    completedFileCount: bigint,
    completedBytes: bigint,
  ): void {
    this.#emit(() => Object.freeze({
      name: 'receive_transition',
      transition: 'materialization_failed',
      planKind: this.#options.intent.plan.kind,
      materializationFailureReason: reason,
      completedFileCount,
      completedBytes,
    }))
  }

  materializationCompleted(summary: MaterializationSummary): void {
    this.#emit(() => Object.freeze({
      name: 'receive_transition',
      transition: 'materialization_completed',
      entryCount: summary.entryCount,
      fileCount: summary.fileCount,
      directoryCount: summary.directoryCount,
      rawBytes: summary.rawBytes,
    }))
  }

  treeFinalized(input: {
    readonly outcome: 'published' | 'partial-directory' | 'discarded'
    readonly successCount: bigint
    readonly failureCount: bigint
  }): void {
    this.#emit(() => Object.freeze({
      name: 'receive_transition',
      transition: 'tree_finalized',
      outcome: input.outcome === 'partial-directory' ? 'partial_directory' : input.outcome,
      successCount: input.successCount,
      failureCount: input.failureCount,
    }))
  }

  #emit(build: () => TransferTraceEvent): void {
    try {
      const observer = this.#options.trace?.current
      if (observer === undefined) return
      observer(build())
    } catch {
      // Trace diagnostics are passive and cannot alter transfer authority.
    }
  }
}
