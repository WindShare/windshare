import type { V2ContentLaneStatus } from '../../content/v2-broker'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type { ReceiveIntent } from '../intent'
import type { SelectionMeasure } from '../measure'
import type { MaterializationSummary } from '../output-session'
import type {
  ArtifactLayoutClass,
  MaterializationFailureReason,
  TransferProgress,
  TransferTraceEvent,
} from './contract'

export interface V2TransferObserversOptions {
  readonly intent: ReceiveIntent
  readonly transferJobId: string
  readonly lanes: V2ContentLaneStatus
  readonly onProgress?: (progress: TransferProgress) => void
  readonly onMeasure?: (measure: SelectionMeasure) => void
  readonly trace?: DomainTraceSource<TransferTraceEvent>
}

export interface V2TransferProgressState {
  readonly measure: SelectionMeasure
  readonly writtenBytes: bigint
  readonly completedFiles: number
  readonly completedBytes: bigint
  readonly fileErrors: number
  readonly selectionErrors: number
  readonly failedDirectories: number
  readonly outputSessionId?: string
}

/**
 * Product observers and trace have separate ports. A trace payload is built only
 * after the revocable source yields an active observer.
 */
export class V2TransferObservers {
  readonly #options: V2TransferObserversOptions

  constructor(options: V2TransferObserversOptions) {
    this.#options = options
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
        completedFiles: state.completedFiles,
        completedBytes: state.completedBytes,
        fileErrors: state.fileErrors,
        selectionErrors: state.selectionErrors,
        contentLanes: this.#options.lanes.size,
        discovery: state.measure.discovery,
        failedDirectories: state.failedDirectories,
        partial,
        transferJobId: this.#options.transferJobId,
        ...(state.outputSessionId === undefined ? {} : { outputSessionId: state.outputSessionId }),
      }))
    } catch {
      // Product observers cannot alter catalog, output, or settlement authority.
    }

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
