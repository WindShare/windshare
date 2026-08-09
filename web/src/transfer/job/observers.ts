import type { V2ContentLaneStatus } from '../../content/v2-broker'
import type { ReceiveIntent } from '../intent'
import type { SelectionMeasure } from '../measure'
import type { MaterializationSummary } from '../output-session'
import type {
  ArtifactLayoutClass,
  MaterializationFailureReason,
  TransferProgress,
  TransferTraceEvent,
  TransferTraceListener,
} from './contract'

export interface V2TransferObserversOptions {
  readonly intent: ReceiveIntent
  readonly transferJobId: string
  readonly lanes: V2ContentLaneStatus
  readonly protocolSessionId?: () => string
  readonly onProgress?: (progress: TransferProgress) => void
  readonly onMeasure?: (measure: SelectionMeasure) => void
  readonly onTrace?: TransferTraceListener
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

/** Keeps fallible diagnostic consumers outside transfer and output authority. */
export class V2TransferObservers {
  readonly #options: V2TransferObserversOptions

  constructor(options: V2TransferObserversOptions) {
    this.#options = options
  }

  measure(value: SelectionMeasure): void {
    try {
      this.#options.onMeasure?.(value)
    } catch {
      // Diagnostic observers own their own failures.
    }
  }

  progress(state: V2TransferProgressState): void {
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
        partial: state.measure.discovery === 'failed' || state.failedDirectories > 0 ||
          state.fileErrors > 0 || state.selectionErrors > 0,
        transferJobId: this.#options.transferJobId,
        ...(state.outputSessionId === undefined ? {} : { outputSessionId: state.outputSessionId }),
      }))
    } catch {
      // UI observers cannot alter catalog, output, or settlement authority.
    }
  }

  intentFrozen(layoutClass: ArtifactLayoutClass): void {
    this.#emit({
      ...this.#identity(),
      name: 'receive.intent.frozen',
      artifact_kind: this.#options.intent.artifact.kind,
      layout_class: layoutClass,
      plan_kind: this.#options.intent.plan.kind,
    })
  }

  directoryAdmitted(input: {
    readonly outputSessionId: string
    readonly admittedDirectoryCount: bigint
    readonly layoutClass: Exclude<ArtifactLayoutClass, 'original-file'>
  }): void {
    this.#emit({
      ...this.#identity(),
      name: 'receive.directory_admission.accepted',
      output_session_id: input.outputSessionId,
      admitted_directory_count: input.admittedDirectoryCount,
      layout_class: input.layoutClass,
    })
  }

  materializationStarted(outputSessionId: string): void {
    this.#emit({
      ...this.#identity(),
      name: 'receive.materialization.started',
      transfer_job_id: this.#options.transferJobId,
      output_session_id: outputSessionId,
      plan_kind: this.#options.intent.plan.kind,
    })
  }

  materializationFailed(
    reason: MaterializationFailureReason,
    completedFileCount: bigint,
    completedBytes: bigint,
  ): void {
    this.#emit({
      ...this.#identity(),
      name: 'receive.materialization.failed',
      transfer_job_id: this.#options.transferJobId,
      plan_kind: this.#options.intent.plan.kind,
      directory_failure_reason: reason,
      completed_file_count: completedFileCount,
      completed_bytes: completedBytes,
    })
  }

  materializationCompleted(summary: MaterializationSummary): void {
    this.#emit({
      ...this.#identity(),
      name: 'receive.materialization.completed',
      transfer_job_id: this.#options.transferJobId,
      entry_count: summary.entryCount,
      file_count: summary.fileCount,
      directory_count: summary.directoryCount,
      raw_bytes: summary.rawBytes,
    })
  }

  treeFinalized(input: {
    readonly outcome: 'published' | 'partial-directory' | 'discarded'
    readonly successCount: bigint
    readonly failureCount: bigint
  }): void {
    this.#emit({
      ...this.#identity(),
      name: 'receive.tree.finalized',
      tree_outcome: input.outcome,
      success_count: input.successCount,
      failure_count: input.failureCount,
      visibility: 'prefix-visible',
    })
  }

  #identity(): Readonly<{
    operation_id: string
    receive_intent_digest: string
    protocol_session_id?: string
  }> {
    const protocolSessionId = safeProtocolSessionId(this.#options.protocolSessionId?.())
    return Object.freeze({
      operation_id: this.#options.intent.operationId,
      receive_intent_digest: this.#options.intent.digest,
      ...(protocolSessionId === undefined ? {} : { protocol_session_id: protocolSessionId }),
    })
  }

  #emit(event: TransferTraceEvent): void {
    try {
      this.#options.onTrace?.(Object.freeze(event))
    } catch {
      // Trace observers are diagnostic only and cannot alter transfer authority.
    }
  }
}

function safeProtocolSessionId(value: string | undefined): string | undefined {
  if (value === undefined) return undefined
  if (typeof value !== 'string' || value.length === 0 ||
      new TextEncoder().encode(value).byteLength > 128) return undefined
  return value
}
