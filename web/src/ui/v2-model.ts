import type { V2OutputCapabilities, V2OutputIntent } from './v2-output'

export type V2ReceiverPhase =
  | 'awaiting-key'
  | 'joining'
  | 'browsing'
  | 'acquiring-output'
  | 'discovering'
  | 'transferring'
  | 'completed'
  | 'completed-errors'
  | 'paused'
  | 'pausing'
  | 'resuming'
  | 'discarding'
  | 'discarded'
  | 'retry-ready'
  | 'needs-attention'
  | 'failed'

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

export type V2PausedTaskState =
  | 'ready'
  | 'resuming'
  | 'discarding'
  | 'busy'
  | 'needs-attention'

export interface V2PausedTaskSnapshot {
  readonly id: string
  readonly backend: string
  readonly format: 'directory' | 'zip'
  readonly completedFileCount: number
  readonly authorizedForCurrentShare: boolean
  readonly state: V2PausedTaskState
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
  readonly partial: boolean
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
      readonly mimeType: string
      readonly width: number
      readonly height: number
    }
  | {
      readonly state: 'video'
      readonly fileId: string
      readonly name: string
      readonly url: string
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
  readonly selectionTotalKnown: boolean
  readonly outputCapabilities: V2OutputCapabilities
  readonly outputIntent: V2OutputIntent
  readonly canStart: boolean
  readonly directoryRetryable: boolean
  readonly progress: V2ReceiverProgress
  readonly downloadT0Milliseconds?: number
  readonly preview: V2PreviewSnapshot
  readonly pausedTasks: readonly V2PausedTaskSnapshot[]
}

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
  partial: false,
  transferJobId: '',
})

export const EMPTY_V2_PREVIEW: V2PreviewSnapshot = Object.freeze({ state: 'idle' })
