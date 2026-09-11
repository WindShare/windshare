import type { TransferProgress } from '../transfer/v2-job'
import { EMPTY_SELECTION_DRAFT } from './draft/model'
import { EMPTY_V2_OUTPUT_PRESENTATION } from './v2-output'
import { EMPTY_V2_PROGRESS, EMPTY_V2_PREVIEW, EMPTY_V2_RETAINED_INVENTORY, type V2ReceiverProgress } from './v2-model'
import type { V2ReceiverDiagnosticSnapshot } from './v2-model'
import type { V2CatalogEntry } from '../catalog/v2-records'
import type { V2SelectionPolicy } from '../catalog/v2-selection'
import type { V2ConnectivityActivation } from '../connectivity/v2-receiver-policy'
import type { V2FilePreview, V2PreviewPresentation } from '../preview/v2-preview'
import type { V2BrowseDirectory, V2BrowsePage } from './v2-gateway'
import type { V2BrowseRow, V2ReceiverSnapshot } from './v2-model'

export interface ActiveV2Preview {
  readonly id: number
  readonly entry: Extract<V2CatalogEntry, { kind: 'file' }>
  readonly controller: AbortController
  readonly connectivity: V2ConnectivityActivation
  session?: V2FilePreview
  seekId: number
}

export interface RetryableV2BrowseRequest {
  readonly directory: V2BrowseDirectory
  readonly pageIndex: number
  readonly route: readonly V2BrowseDirectory[]
}

export interface BrowsePageProjection {
  readonly entries: Map<string, V2CatalogEntry>
  readonly snapshot: Pick<
    V2ReceiverSnapshot,
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
  page: V2BrowsePage,
  selection: V2SelectionPolicy,
  route: readonly V2BrowseDirectory[],
): BrowsePageProjection {
  const entries = new Map(page.entries.map((entry) => [entry.idText, entry]))
  const rows: V2BrowseRow[] = page.entries.map((entry) => Object.freeze({
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
        error: page.omittedCount === 0n ? null : `${page.omittedCount} entries were omitted by the sender.`,
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

export function breadcrumbsFor(route: readonly V2BrowseDirectory[]) {
  return Object.freeze(route.map((directory) => Object.freeze({
    id: directory.idText,
    name: directory.name,
  })))
}

export function previewSnapshot(
  entry: Extract<V2CatalogEntry, { kind: 'file' }>,
  presentation: V2PreviewPresentation,
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

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

export function receiverDiagnosticSnapshot(snapshot: V2ReceiverSnapshot, generation: bigint): V2ReceiverDiagnosticSnapshot {
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

export function initialReceiverSnapshot(): V2ReceiverSnapshot {
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
    progress: EMPTY_V2_PROGRESS,
    preview: EMPTY_V2_PREVIEW,
    output: EMPTY_V2_OUTPUT_PRESENTATION,
    retained: EMPTY_V2_RETAINED_INVENTORY,
  })
}

export function receiverProgressSnapshot(progress: TransferProgress): V2ReceiverProgress {
  return Object.freeze({
    phase: progress.phase,
    materializedBytes: progress.materializedBytes,
    discoveredFiles: progress.discoveredFiles,
    discoveredBytes: progress.discoveredBytes,
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
