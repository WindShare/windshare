import type { V2CatalogEntry } from '../catalog/v2-records'
import type { V2SelectionPolicy } from '../catalog/v2-selection'
import { encodeBase64Url } from '../crypto/bytes'
import type { V2ConnectivityActivation } from '../connectivity/v2-receiver-policy'
import type { V2FilePreview, V2PreviewPresentation } from '../preview/v2-preview'
import type { TransferJobResult, TransferProgress } from '../transfer/v2-job'
import type { V2BrowseDirectory, V2BrowsePage, V2JoinedBrowserShare } from './v2-gateway'
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
  readonly root?: {
    readonly entryCount: number
    readonly singleFile?: Extract<V2CatalogEntry, { kind: 'file' }>
  }
  readonly snapshot: Pick<
    V2ReceiverSnapshot,
    | 'phase'
    | 'status'
    | 'error'
    | 'rows'
    | 'breadcrumbs'
    | 'pageIndex'
    | 'pageCount'
    | 'entryCount'
    | 'omittedCount'
    | 'selectedVisibleFiles'
    | 'selectedVisibleBytes'
    | 'selectionTotalKnown'
    | 'directoryRetryable'
  >
}

export function projectBrowsePage(
  page: V2BrowsePage,
  selection: V2SelectionPolicy,
  syntheticRootId: string,
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
  const rootPage = page.directory.idText === syntheticRootId
  const onlyRootEntry = rootPage && page.entryCount === 1 && page.omittedCount === 0n
    ? page.entries[0]
    : undefined
  return Object.freeze({
    entries,
    ...(rootPage
      ? { root: Object.freeze({
          entryCount: page.entryCount,
          ...(onlyRootEntry?.kind === 'file' ? { singleFile: onlyRootEntry } : {}),
        }) }
      : {}),
    snapshot: Object.freeze({
      phase: 'browsing',
      status: page.entryCount === 0 ? 'This directory is empty.' : 'Choose what to receive.',
      error: page.omittedCount === 0n ? null : `${page.omittedCount} entries were omitted by the sender.`,
      rows: Object.freeze(rows),
      breadcrumbs: breadcrumbsFor(route),
      pageIndex: page.pageIndex,
      pageCount: page.pageCount,
      entryCount: page.entryCount,
      omittedCount: page.omittedCount,
      selectedVisibleFiles: selectedFiles,
      selectedVisibleBytes: selectedBytes,
      selectionTotalKnown: rootPage && page.pageCount === 1 && page.omittedCount === 0n &&
        page.entries.every((entry) => entry.kind === 'file'),
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

export function knownSingleFile(
  joined: V2JoinedBrowserShare | undefined,
  selectionTotalKnown: boolean,
  candidate: Extract<V2CatalogEntry, { kind: 'file' }> | undefined,
): Extract<V2CatalogEntry, { kind: 'file' }> | undefined {
  if (joined === undefined || !selectionTotalKnown || candidate === undefined) return undefined
  return joined.selection.selected(candidate, [joined.descriptor.syntheticRootId])
    ? candidate
    : undefined
}

export function selectionAvailable(
  joined: V2JoinedBrowserShare | undefined,
  rootEntryCount: number,
  selectionTotalKnown: boolean,
  page: V2BrowsePage | undefined,
): boolean {
  if (joined === undefined || rootEntryCount === 0) return false
  if (!selectionTotalKnown) {
    return joined.selection.shouldDiscover(joined.descriptor.syntheticRootId, [])
  }
  const rootAncestry = [joined.descriptor.syntheticRootId]
  return page?.entries.some(
    (entry) => joined.selection.state(entry, rootAncestry) !== 'unselected',
  ) === true
}

export function transferTerminalSnapshot(
  snapshot: V2ReceiverSnapshot,
  result: TransferJobResult,
  locallyAborted: boolean,
): V2ReceiverSnapshot {
  if (result.outcome.status === 'Paused') {
    return { ...snapshot, phase: 'paused', status: 'Transfer paused; completed output and checkpoints were retained.' }
  }
  if (result.outcome.status === 'NeedsAttention') {
    return {
      ...snapshot,
      phase: 'failed',
      status: 'Transfer stopped, but output cleanup could not be confirmed. Manual review may be required.',
    }
  }
  if (result.outcome.status === 'Aborted') {
    if (!locallyAborted) {
      throw result.abortReason ?? new Error('Transfer aborted without a terminal reason')
    }
    return { ...snapshot, phase: 'aborted', status: 'Transfer stopped.' }
  }
  if (result.outcome.status === 'CompletedWithErrors') {
    return {
      ...snapshot,
      phase: 'completed-errors',
      status: `Saved with ${result.outcome.failureCount} item error(s).`,
    }
  }
  return { ...snapshot, phase: 'completed', status: 'Transfer complete.' }
}

export function previewSnapshot(
  entry: Extract<V2CatalogEntry, { kind: 'file' }>,
  presentation: V2PreviewPresentation,
  seeking: boolean,
) {
  return Object.freeze(presentation.kind === 'image'
    ? {
        state: 'image' as const,
        fileId: entry.idText,
        name: presentation.name,
        url: presentation.url,
        mimeType: presentation.mimeType,
        width: presentation.width,
        height: presentation.height,
      }
    : {
        state: 'video' as const,
        fileId: entry.idText,
        name: presentation.name,
        url: presentation.url,
        mimeType: presentation.mimeType,
        width: presentation.width,
        height: presentation.height,
        durationSeconds: presentation.durationSeconds,
        positionSeconds: presentation.positionSeconds,
        seeking,
      })
}

export function transferProgressSnapshot(
  progress: TransferProgress,
): Pick<V2ReceiverSnapshot, 'phase' | 'status' | 'progress'> {
  const phase = progress.discovery === 'failed' || progress.writtenBytes === 0n
    ? 'discovering'
    : 'transferring'
  let status: string
  if (progress.discovery === 'complete') {
    status = 'Receiving authenticated blocks…'
  } else if (progress.discovery === 'failed') {
    status = 'Discovery stopped; saving discovered files…'
  } else {
    status = 'Discovering selected files…'
  }
  return Object.freeze({ phase, status, progress: Object.freeze({ ...progress }) })
}

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

export function nowMilliseconds(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now()
}

export function descriptorIdentity(
  text: string | undefined,
  raw: Uint8Array | undefined,
  fallback: string,
): string {
  if (text !== undefined && text.length > 0) return text
  if (raw instanceof Uint8Array && raw.byteLength > 0) return encodeBase64Url(raw)
  return fallback
}
