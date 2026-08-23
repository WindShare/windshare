import type { CompatibleNameRepairSummary } from '../output/file-system-access/compatible-name/model'
import type { V2OutputPresentationSnapshot } from './v2-output'
import type {
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from './v2-receive-runtime'

export type V2RetainedReceivePresentationOperation = Readonly<
  V2RetainedReceiveOperation & { readonly repairSummary?: CompatibleNameRepairSummary }
>

/** Shell navigation is separate from the receive lifecycle owned by W2-E. */
export type V2ReceiverPhase = 'awaiting-key' | 'joining' | 'browsing' | 'failed'

export interface V2BrowseRow {
  readonly id: string
  readonly kind: 'directory' | 'file'
  readonly name: string
  readonly expectedSize?: bigint
  readonly selection: 'selected' | 'unselected' | 'mixed'
}

export interface V2Breadcrumb {
  readonly id: string
  readonly name: string
}

export interface V2ReceiverProgress {
  readonly discoveredFiles: number
  readonly discoveredBytes: bigint
  readonly writtenBytes: bigint
  readonly completedFiles: number
  readonly completedBytes: bigint
  readonly fileErrors: number
  readonly selectionErrors: number
  readonly contentLanes: number
  readonly discovery: 'open' | 'complete' | 'failed'
  readonly failedDirectories: number
  readonly capacityWaitingFiles: number
  readonly capacityAccumulatedWaitMilliseconds: number
  readonly capacityWaitAttempts: number
  readonly capacityWaitVisible: boolean
  readonly transferJobId: string
  readonly outputSessionId?: string
}

export type V2PreviewSnapshot =
  | { readonly state: 'idle' }
  | {
      readonly state: 'loading'
      readonly fileId: string
      readonly name: string
    }
  | {
      readonly state: 'image'
      readonly fileId: string
      readonly name: string
      readonly url: string
      readonly presentationId: number
      readonly mimeType: string
      readonly width: number
      readonly height: number
    }
  | {
      readonly state: 'video'
      readonly fileId: string
      readonly name: string
      readonly url: string
      readonly presentationId: number
      readonly mimeType: string
      readonly width: number
      readonly height: number
      readonly durationSeconds: number
      readonly positionSeconds: number
      readonly seeking: boolean
    }
  | {
      readonly state: 'error'
      readonly fileId: string
      readonly name: string
      readonly message: string
    }

export interface V2PendingRetainedReceiveActionSnapshot {
  readonly operationId: string
  readonly action: V2RetainedReceiveAction
}

export type V2RetainedReceiveInventorySnapshot =
  | Readonly<{
      kind: 'loading'
      operations: readonly V2RetainedReceivePresentationOperation[]
      error: null
      pending: null
    }>
  | Readonly<{
      kind: 'ready'
      operations: readonly V2RetainedReceivePresentationOperation[]
      error: null
      pending: V2PendingRetainedReceiveActionSnapshot | null
    }>
  | Readonly<{
      kind: 'failed'
      operations: readonly V2RetainedReceivePresentationOperation[]
      error: string
      pending: null
    }>

export interface V2ControllerDiagnosticSnapshot {
  readonly generation: bigint
  readonly phase: V2ReceiverPhase
}

export interface V2LifecycleDiagnosticSnapshot {
  readonly generation: bigint
  readonly state: NonNullable<V2OutputPresentationSnapshot['lifecycle']>['kind']
}

export interface V2ProgressDiagnosticSnapshot {
  readonly generation: bigint
  readonly discovery: V2ReceiverProgress['discovery']
  readonly discoveredFiles: bigint
  readonly discoveredBytes: bigint
  readonly writtenBytes: bigint
  readonly completedFiles: bigint
  readonly completedBytes: bigint
  readonly fileErrors: bigint
  readonly selectionErrors: bigint
  readonly failedDirectories: bigint
  readonly contentLanes: number
  readonly capacityWaitingFiles: bigint
  readonly capacityAccumulatedWaitMilliseconds: bigint
  readonly capacityWaitAttempts: bigint
  readonly capacityWaitVisible: boolean
}

export interface V2OutputDiagnosticSnapshot {
  readonly generation: bigint
  readonly planKind: NonNullable<V2OutputPresentationSnapshot['plan']>['kind']
}

export interface V2ReceiverDiagnosticSnapshot {
  readonly controller: V2ControllerDiagnosticSnapshot
  readonly lifecycle?: V2LifecycleDiagnosticSnapshot
  readonly progress: V2ProgressDiagnosticSnapshot
  readonly output?: V2OutputDiagnosticSnapshot
}

export interface V2ReceiverSnapshot {
  readonly phase: V2ReceiverPhase
  readonly status: string
  readonly error: string | null
  readonly rows: readonly V2BrowseRow[]
  readonly breadcrumbs: readonly V2Breadcrumb[]
  readonly pageIndex: number
  readonly pageCount: number
  readonly entryCount: number
  readonly omittedCount: bigint
  readonly selectedVisibleFiles: number
  readonly selectedVisibleBytes: bigint
  readonly directoryRetryable: boolean
  readonly progress: V2ReceiverProgress
  readonly preview: V2PreviewSnapshot
  readonly output: V2OutputPresentationSnapshot
  readonly retained: V2RetainedReceiveInventorySnapshot
}

export const EMPTY_V2_RETAINED_INVENTORY: V2RetainedReceiveInventorySnapshot = Object.freeze({
  kind: 'loading',
  operations: Object.freeze([]),
  error: null,
  pending: null,
})

export const EMPTY_V2_PROGRESS: V2ReceiverProgress = Object.freeze({
  discoveredFiles: 0,
  discoveredBytes: 0n,
  writtenBytes: 0n,
  completedFiles: 0,
  completedBytes: 0n,
  fileErrors: 0,
  selectionErrors: 0,
  contentLanes: 0,
  discovery: 'open',
  failedDirectories: 0,
  capacityWaitingFiles: 0,
  capacityAccumulatedWaitMilliseconds: 0,
  capacityWaitAttempts: 0,
  capacityWaitVisible: false,
  transferJobId: '',
})

export const EMPTY_V2_PREVIEW: V2PreviewSnapshot = Object.freeze({ state: 'idle' })
