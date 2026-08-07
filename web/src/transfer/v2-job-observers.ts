import type { V2ShareDescriptor } from '../catalog/v2-records'
import type { V2ContentLaneStatus } from '../content/v2-broker'
import {
  snapshotProtocolSessionId,
  type TransferTraceEvent,
  type TransferTraceListener,
} from './intent'
import type { SelectionMeasure } from './measure'
import type { TransferProgress } from './v2-job-contract'
import { descriptorShareInstanceId, transferTimestampMilliseconds } from './v2-job-identity'

export interface V2TransferObserversOptions {
  readonly descriptor: V2ShareDescriptor
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

  trace(
    name: TransferTraceEvent['name'],
    extra: Omit<
      TransferTraceEvent['context'],
      'shareInstance' | 'transferJobId' | 'protocolSessionId'
    > = {},
  ): void {
    try {
      const rawProtocolSessionId = this.#options.protocolSessionId?.()
      const protocolSessionId = rawProtocolSessionId === undefined
        ? undefined
        : snapshotProtocolSessionId(rawProtocolSessionId)
      this.#options.onTrace?.(Object.freeze({
        name,
        atMilliseconds: transferTimestampMilliseconds(),
        context: Object.freeze({
          shareInstance: descriptorShareInstanceId(this.#options.descriptor),
          transferJobId: this.#options.transferJobId,
          ...(protocolSessionId === undefined ? {} : { protocolSessionId }),
          ...extra,
        }),
      }))
    } catch {
      // Trace observers are diagnostic only and cannot alter transfer authority.
    }
  }
}
