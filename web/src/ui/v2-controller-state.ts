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

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}
