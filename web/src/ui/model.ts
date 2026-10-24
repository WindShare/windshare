import type { RetainedReceiveInventorySnapshot } from '../application/receive/retained/model'
import type { OutputPresentationSnapshot } from './output'

/** Shell navigation is separate from the receive lifecycle owned by W2-E. */
export type ReceiverPhase = 'awaiting-key' | 'joining' | 'browsing' | 'failed'

export interface BrowseRow {
  readonly id: string
  readonly kind: 'directory' | 'file'
  readonly name: string
  readonly expectedSize?: bigint
  readonly selection: 'selected' | 'unselected' | 'mixed'
}

export interface Breadcrumb {
  readonly id: string
  readonly name: string
}

export interface ReceiverProgress {
  readonly receivedObjectBytes: bigint
  readonly phase: 'receiving' | 'finishing'
  /** Accepted output payload includes retained coverage; it does not assert restart durability. */
  readonly materializedBytes: bigint
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

export type PreviewSnapshot =
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

export type ShareIdentity =
  | Readonly<{ kind: 'photo' | 'video' | 'file'; shareInstance: string; name: string; file: BrowseRow }>
  | Readonly<{ kind: 'browser'; shareInstance: string; name: string; homeDirectoryId: string; singleFolder: boolean }>

export interface BrowseState {
  readonly kind: 'idle' | 'loading' | 'ready' | 'failed'
  readonly status: string
  readonly error: string | null
}

export interface SelectionDraft {
  readonly mode: 'scope' | 'selection'
  readonly scope: 'whole-share' | 'current-folder' | 'selected'
  readonly label: string
  readonly summary: string
  readonly empty: boolean
}

export interface ReceiverSnapshot {
  readonly connection: import('../receiver/connection-state').ReceiverConnectionSnapshot |
    Readonly<{ kind: 'idle'; join?: import('../receiver/initial-join').InitialJoinState }>
  readonly share: ShareIdentity | null
  readonly browse: BrowseState
  readonly draft: SelectionDraft
  readonly startAdmission: Readonly<{ allowed: boolean; reason: string | null; canReleaseCurrent: boolean }>
  readonly activeReceiveOperationId: string | null
  readonly taskDisplay: import('../output/workspace/operation-display').ReceiveOperationDisplay | null
  readonly pathActivity: import('../receiver/path-activity').ReceiverPathActivitySnapshot
  readonly phase: ReceiverPhase
  readonly status: string
  readonly error: string | null
  readonly rows: readonly BrowseRow[]
  readonly breadcrumbs: readonly Breadcrumb[]
  readonly pageIndex: number
  readonly pageCount: number
  readonly entryCount: number
  readonly omittedCount: bigint
  readonly selectedVisibleFiles: number
  readonly selectedVisibleBytes: bigint
  readonly directoryRetryable: boolean
  readonly progress: ReceiverProgress
  readonly preview: PreviewSnapshot
  readonly output: OutputPresentationSnapshot
  readonly retained: RetainedReceiveInventorySnapshot
}

export const EMPTY_PROGRESS: ReceiverProgress = Object.freeze({
  phase: 'receiving',
  materializedBytes: 0n,
  discoveredFiles: 0,
  discoveredBytes: 0n,
  receivedObjectBytes: 0n,
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

export const EMPTY_PREVIEW: PreviewSnapshot = Object.freeze({ state: 'idle' })
