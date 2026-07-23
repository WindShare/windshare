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
  | 'aborting'
  | 'aborted'
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

export interface V2ReceiverProgress {
  readonly discoveredFiles: number
  readonly discoveredBytes: bigint
  readonly writtenBytes: bigint
  readonly completedFiles: number
  readonly contentLanes: number
  readonly discoveryComplete: boolean
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
  readonly preview: V2PreviewSnapshot
}

export const EMPTY_V2_PROGRESS: V2ReceiverProgress = Object.freeze({
  discoveredFiles: 0,
  discoveredBytes: 0n,
  writtenBytes: 0n,
  completedFiles: 0,
  contentLanes: 0,
  discoveryComplete: false,
})

export const EMPTY_V2_PREVIEW: V2PreviewSnapshot = Object.freeze({ state: 'idle' })
