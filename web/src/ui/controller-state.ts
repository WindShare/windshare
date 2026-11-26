import type { TransferProgress } from '../transfer/progress'
import { EMPTY_SELECTION_DRAFT } from './draft/model'
import { EMPTY_OUTPUT_PRESENTATION } from './output'
import { EMPTY_PROGRESS, EMPTY_PREVIEW, type ReceiverProgress } from './model'
import { EMPTY_RETAINED_INVENTORY, type ReceiveBlockReason } from '../application/receive/contracts'
import type { CatalogEntry } from '../catalog/records'
import type { SelectionPolicy } from '../catalog/selection'
import type { ConnectivityActivation } from '../connectivity/receiver-policy'
import type { FilePreview, PreviewPresentation } from '../preview/preview'
import type { BrowseDirectory, BrowsePage, JoinedBrowserShare } from '../application/browser-share/gateway'
import type { BrowseRow, ReceiverSnapshot, SelectionDraft, ShareIdentity, StartAdmission } from './model'

export interface SuspendedShareBrowse {
  readonly joined: JoinedBrowserShare
  readonly share: ShareIdentity
  readonly draftMode: SelectionDraft['mode']
}

const EMPTY_BROWSE_SNAPSHOT = Object.freeze({
  share: null,
  browse: Object.freeze({ kind: 'idle' as const, error: null }),
  draft: EMPTY_SELECTION_DRAFT,
  rows: Object.freeze([]),
  breadcrumbs: Object.freeze([]),
  pageIndex: 0,
  pageCount: 0,
  entryCount: 0,
  omittedCount: 0n,
  selectedVisibleFiles: 0,
  selectedVisibleBytes: 0n,
  directoryRetryable: false,
  preview: EMPTY_PREVIEW,
})

export interface ActivePreview {
  readonly id: number
  readonly entry: Extract<CatalogEntry, { kind: 'file' }>
  readonly controller: AbortController
  readonly connectivity: ConnectivityActivation
  session?: FilePreview
  seekId: number
}

export interface RetryableBrowseRequest {
  readonly directory: BrowseDirectory
  readonly pageIndex: number
  readonly route: readonly BrowseDirectory[]
}

export interface BrowsePageProjection {
  readonly entries: Map<string, CatalogEntry>
  readonly snapshot: Pick<
    ReceiverSnapshot,
    | 'phase'
    | 'browse'
    | 'rows'
    | 'breadcrumbs'
    | 'pageIndex'
    | 'pageCount'
    | 'entryCount'
    | 'omittedCount'
    | 'selectedVisibleFiles'
    | 'selectedVisibleBytes'
    | 'directoryRetryable'
  >
}

export function projectBrowsePage(
  page: BrowsePage,
  selection: SelectionPolicy,
  route: readonly BrowseDirectory[],
): BrowsePageProjection {
  const entries = new Map(page.entries.map((entry) => [entry.idText, entry]))
  const rows: BrowseRow[] = page.entries.map((entry) => Object.freeze({
    id: entry.idText,
    kind: entry.kind,
    name: entry.name,
    ...(entry.kind === 'file' ? { expectedSize: entry.expectedSize } : {}),
    selection: selection.state(entry, page.directory.ancestry),
  }))
  let selectedFiles = 0
  let selectedBytes = 0n
  for (const entry of page.entries) {
    if (entry.kind === 'file' && selection.selected(entry, page.directory.ancestry)) {
      selectedFiles += 1
      selectedBytes += entry.expectedSize
    }
  }
  return Object.freeze({
    entries,
    snapshot: Object.freeze({
      phase: 'browsing',
      browse: Object.freeze({
        kind: 'ready' as const,
        error: null,
      }),
      rows: Object.freeze(rows),
      breadcrumbs: breadcrumbsFor(route),
      pageIndex: page.pageIndex,
      pageCount: page.pageCount,
      entryCount: page.entryCount,
      omittedCount: page.omittedCount,
      selectedVisibleFiles: selectedFiles,
      selectedVisibleBytes: selectedBytes,
      directoryRetryable: false,
    }),
  })
}

export function breadcrumbsFor(route: readonly BrowseDirectory[]) {
  return Object.freeze(route.map((directory) => Object.freeze({
    id: directory.idText,
    name: directory.path.at(-1) ?? null,
  })))
}

export function previewSnapshot(
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  presentation: PreviewPresentation,
  presentationId: number,
) {
  return Object.freeze(presentation.kind === 'image'
    ? {
        state: 'image' as const,
        fileId: entry.idText,
        name: presentation.name,
        url: presentation.url,
        presentationId,
        mimeType: presentation.mimeType,
        width: presentation.width,
        height: presentation.height,
      }
    : {
        state: 'video' as const,
        fileId: entry.idText,
        name: presentation.name,
        url: presentation.url,
        presentationId,
        mimeType: presentation.mimeType,
        width: presentation.width,
        height: presentation.height,
        durationSeconds: presentation.durationSeconds,
        requestedSeconds: presentation.keyframeSeconds,
        decodeSeconds: presentation.decodeSeconds,
        seeking: false,
        failure: null,
      })
}

export function joiningReceiverSnapshot(snapshot: ReceiverSnapshot): ReceiverSnapshot {
  return Object.freeze({
    ...snapshot,
    ...EMPTY_BROWSE_SNAPSHOT,
    phase: 'joining',
    status: 'connecting',
    pathActivity: { lanes: [] },
    error: null,
    connection: { kind: 'idle' as const },
    taskDisplay: null,
    progress: EMPTY_PROGRESS,
  })
}

export function cancelledReceiverSnapshot(snapshot: ReceiverSnapshot): ReceiverSnapshot {
  return Object.freeze({
    ...snapshot,
    ...EMPTY_BROWSE_SNAPSHOT,
    phase: 'awaiting-key',
    connection: { kind: 'idle' as const },
    pathActivity: { lanes: [] },
    status: 'cancelled',
    error: null,
    taskDisplay: null,
    progress: EMPTY_PROGRESS,
  })
}

export function resolveStartAdmission(
  previous: StartAdmission,
  reason: ReceiveBlockReason | null,
  canReleaseCurrent: boolean,
  localChoiceId: string | null,
  localReason: ReceiveBlockReason | null,
): StartAdmission {
  const sameLocalChoice = localChoiceId === null
    ? previous.localChoice === null
    : previous.localChoice?.choiceId === localChoiceId &&
      previous.localChoice.reason === localReason
  if (
    previous.reason === reason &&
    previous.canReleaseCurrent === canReleaseCurrent &&
    sameLocalChoice
  ) {
    return previous
  }
  return Object.freeze({
    allowed: reason === null,
    reason,
    canReleaseCurrent,
    localChoice: localChoiceId === null
      ? null
      : Object.freeze({ choiceId: localChoiceId, allowed: localReason === null, reason: localReason }),
  })
}

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

export function initialReceiverSnapshot(): ReceiverSnapshot {
  return Object.freeze({
    ...EMPTY_BROWSE_SNAPSHOT,
    connection: { kind: 'idle' as const },
    reportUrl: null,
    startAdmission: { allowed: false, reason: 'connectFirst' as const, canReleaseCurrent: false, localChoice: null },
    activeReceiveOperationId: null,
    taskDisplay: null,
    phase: 'awaiting-key',
    status: 'enter-key',
    pathActivity: { lanes: [] },
    error: null,
    progress: EMPTY_PROGRESS,
    output: EMPTY_OUTPUT_PRESENTATION,
    retained: EMPTY_RETAINED_INVENTORY,
  })
}

export function receiverProgressSnapshot(progress: TransferProgress): ReceiverProgress {
  return Object.freeze({
    phase: progress.phase,
    materializedBytes: progress.materializedBytes,
    discoveredFiles: progress.discoveredFiles,
    discoveredBytes: progress.discoveredBytes,
    receivedObjectBytes: progress.receivedObjectBytes,
    writtenBytes: progress.writtenBytes,
    completedFiles: progress.completedFiles,
    completedBytes: progress.completedBytes,
    fileErrors: progress.fileErrors,
    selectionErrors: progress.selectionErrors,
    contentLanes: progress.contentLanes,
    discovery: progress.discovery,
    failedDirectories: progress.failedDirectories,
    sourceOmittedEntries: progress.sourceOmittedEntries,
    capacityWaitingFiles: progress.capacityWaitingFiles,
    capacityAccumulatedWaitMilliseconds: progress.capacityAccumulatedWaitMilliseconds,
    capacityWaitAttempts: progress.capacityWaitAttempts,
    capacityWaitVisible: progress.capacityWaitVisible,
    bandwidthWaitVisible: progress.bandwidthWaitVisible,
    transferJobId: progress.transferJobId,
    ...(progress.outputSessionId === undefined
      ? {}
      : { outputSessionId: progress.outputSessionId }),
  })
}
