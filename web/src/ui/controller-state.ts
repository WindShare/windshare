import type { TransferProgress } from '../transfer/job'
import { EMPTY_SELECTION_DRAFT } from './draft/model'
import { EMPTY_OUTPUT_PRESENTATION } from './output'
import { EMPTY_PROGRESS, EMPTY_PREVIEW, type ReceiverProgress } from './model'
import { EMPTY_RETAINED_INVENTORY } from '../application/receive/retained/model'
import type { ReceiverDiagnosticSnapshot } from '../diagnostics/export/context'
import type { CatalogEntry } from '../catalog/records'
import type { SelectionPolicy } from '../catalog/selection'
import type { ConnectivityActivation } from '../connectivity/receiver-policy'
import type { FilePreview, PreviewPresentation } from '../preview/preview'
import type { BrowseDirectory, BrowsePage } from '../application/browser-share/gateway'
import type { BrowseRow, ReceiverSnapshot } from './model'

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
        status: page.entryCount === 0 ? 'This directory is empty.' : '',
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
    name: directory.name,
  })))
}

export function previewSnapshot(
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  presentation: PreviewPresentation,
  seeking: boolean,
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
        positionSeconds: presentation.positionSeconds,
        seeking,
      })
}

export function joiningReceiverSnapshot(snapshot: ReceiverSnapshot): ReceiverSnapshot {
  return Object.freeze({
    ...snapshot,
    phase: 'joining',
    status: 'Authenticating the share descriptor?',
    pathActivity: { lanes: [] },
    error: null,
    rows: Object.freeze([]),
    connection: { kind: 'idle' as const },
    share: null,
    draft: EMPTY_SELECTION_DRAFT,
    browse: { kind: 'idle' as const, status: '', error: null },
    taskDisplay: null,
    preview: EMPTY_PREVIEW,
    progress: EMPTY_PROGRESS,
  })
}

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

export function receiverDiagnosticSnapshot(snapshot: ReceiverSnapshot, generation: bigint): ReceiverDiagnosticSnapshot {
  const lifecycle = snapshot.output.lifecycle
  const plan = snapshot.output.plan
  const progress = snapshot.progress
  return Object.freeze({
    controller: Object.freeze({
      generation,
      phase: snapshot.phase,
    }),
    ...(lifecycle === null
      ? {}
      : {
          lifecycle: Object.freeze({
            generation: lifecycle.generation,
            state: lifecycle.kind,
          }),
        }),
    progress: Object.freeze({
      generation,
      discovery: progress.discovery,
      discoveredFiles: BigInt(progress.discoveredFiles),
      discoveredBytes: progress.discoveredBytes,
      writtenBytes: progress.writtenBytes,
      completedFiles: BigInt(progress.completedFiles),
      completedBytes: progress.completedBytes,
      fileErrors: BigInt(progress.fileErrors),
      selectionErrors: BigInt(progress.selectionErrors),
      failedDirectories: BigInt(progress.failedDirectories),
      contentLanes: progress.contentLanes,
      capacityWaitingFiles: BigInt(progress.capacityWaitingFiles),
      capacityAccumulatedWaitMilliseconds: BigInt(
        progress.capacityAccumulatedWaitMilliseconds,
      ),
      capacityWaitAttempts: BigInt(progress.capacityWaitAttempts),
      capacityWaitVisible: progress.capacityWaitVisible,
    }),
    ...(plan === null
      ? {}
      : {
          output: Object.freeze({
            generation,
            planKind: plan.kind,
          }),
        }),
  })
}

export function initialReceiverSnapshot(): ReceiverSnapshot {
  return Object.freeze({
    connection: { kind: 'idle' as const },
    share: null,
    browse: { kind: 'idle' as const, status: '', error: null },
    draft: EMPTY_SELECTION_DRAFT,
    startAdmission: { allowed: false, reason: 'Connect to the share first.', canReleaseCurrent: false },
    activeReceiveOperationId: null,
    taskDisplay: null,
    phase: 'awaiting-key',
    status: 'Waiting for the capability key.',
    pathActivity: { lanes: [] },
    error: null,
    rows: Object.freeze([]),
    breadcrumbs: Object.freeze([]),
    pageIndex: 0,
    pageCount: 0,
    entryCount: 0,
    omittedCount: 0n,
    selectedVisibleFiles: 0,
    selectedVisibleBytes: 0n,
    directoryRetryable: false,
    progress: EMPTY_PROGRESS,
    preview: EMPTY_PREVIEW,
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
    capacityWaitingFiles: progress.capacityWaitingFiles,
    capacityAccumulatedWaitMilliseconds: progress.capacityAccumulatedWaitMilliseconds,
    capacityWaitAttempts: progress.capacityWaitAttempts,
    capacityWaitVisible: progress.capacityWaitVisible,
    transferJobId: progress.transferJobId,
    ...(progress.outputSessionId === undefined
      ? {}
      : { outputSessionId: progress.outputSessionId }),
  })
}
