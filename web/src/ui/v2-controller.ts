import { V2DirectoryFailureError } from '../catalog/v2-client'
import type { V2CatalogScanProgress } from '../catalog/v2-client'
import type { V2CatalogEntry } from '../catalog/v2-records'
import { equalBytes } from '../crypto/bytes'
import type { V2ConnectivityActivation } from '../connectivity/v2-receiver-policy'
import { V2FilePreview, type V2PreviewPresentation } from '../preview/v2-preview'
import { SMALL_TRANSFER_BYTE_LIMIT } from '../transfer/measure'
import { OutputSessionSuspendedError } from '../transfer/output-session'
import type { V2TransferProgress } from '../transfer/v2-job'
import { V2BrowserReceiverGateway, type V2BrowseDirectory, type V2BrowsePage, type V2JoinedBrowserShare } from './v2-gateway'
import {
  acquireBrowserV2Output,
  browserV2OutputCapabilities,
  openBrowserV2OutputSession,
  outputIntentAvailable,
  type V2OutputIntent,
} from './v2-output'
import {
  EMPTY_V2_PROGRESS,
  EMPTY_V2_PREVIEW,
  type V2BrowseRow,
  type V2ReceiverProgress,
  type V2ReceiverSnapshot,
} from './v2-model'

interface ActiveV2Preview {
  readonly id: number
  readonly entry: Extract<V2CatalogEntry, { kind: 'file' }>
  readonly controller: AbortController
  readonly connectivity: V2ConnectivityActivation
  session?: V2FilePreview
  seekId: number
}

interface RetryableV2BrowseRequest {
  readonly directory: V2BrowseDirectory
  readonly pageIndex: number
  readonly route: readonly V2BrowseDirectory[]
}

export interface V2CapturedLocation {
  readonly capabilityInput: string | null
  readonly pageUrl: string
}

export function captureV2Location(windowPort: Window = window): V2CapturedLocation {
  const input = windowPort.location.href
  const sanitized = new URL(input)
  const capabilityInput = sanitized.hash.length > 1 ? input : null
  sanitized.hash = ''
  // Secret erasure precedes crypto, browser feature detection, and relay dialing.
  windowPort.history.replaceState(windowPort.history.state, '', sanitized)
  return Object.freeze({ capabilityInput, pageUrl: sanitized.href })
}

export class V2ReceiverController {
  readonly #gateway: V2BrowserReceiverGateway
  readonly #listeners = new Set<() => void>()
  #snapshot: V2ReceiverSnapshot
  #pageUrl = ''
  #joined: V2JoinedBrowserShare | undefined
  #page: V2BrowsePage | undefined
  #directories: V2BrowseDirectory[] = []
  #entries = new Map<string, V2CatalogEntry>()
  #rootSingleFile: Extract<V2CatalogEntry, { kind: 'file' }> | undefined
  #rootEntryCount = 0
  #navigation: AbortController | undefined
  #pendingNavigationKey: string | undefined
  #loadingDirectory: V2BrowseDirectory | undefined
  #retryableBrowse: RetryableV2BrowseRequest | undefined
  #unsubscribeScanProgress: (() => void) | undefined
  #transfer: AbortController | undefined
  #preview: ActiveV2Preview | undefined
  #nextPreviewId = 1
  #disposed = false

  constructor(gateway = new V2BrowserReceiverGateway()) {
    this.#gateway = gateway
    const outputCapabilities = browserV2OutputCapabilities()
    const outputIntent: V2OutputIntent = outputCapabilities.nativeDirectory ? 'directory' : 'download'
    this.#snapshot = Object.freeze({
      phase: 'awaiting-key',
      status: 'Waiting for the capability key.',
      error: null,
      rows: Object.freeze([]),
      breadcrumbs: Object.freeze([]),
      pageIndex: 0,
      pageCount: 0,
      entryCount: 0,
      omittedCount: 0n,
      selectedVisibleFiles: 0,
      selectedVisibleBytes: 0n,
      selectionTotalKnown: false,
      outputCapabilities,
      outputIntent,
      canStart: false,
      directoryRetryable: false,
      progress: EMPTY_V2_PROGRESS,
      preview: EMPTY_V2_PREVIEW,
    })
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  readonly getSnapshot = (): V2ReceiverSnapshot => this.#snapshot

  readonly getOwnershipSnapshot = () => Object.freeze({
    currentPageEntries: this.#entries.size,
    retainedRootCandidates: this.#rootSingleFile === undefined ? 0 : 1,
  })

  initialize(captured: V2CapturedLocation): void {
    this.#pageUrl = captured.pageUrl
    if (captured.capabilityInput !== null) this.#join(captured.capabilityInput).catch(() => undefined)
  }

  submitKey(input: string): void {
    if (this.#disposed || input.trim().length === 0) return
    this.#join(input).catch(() => undefined)
  }

  chooseOutput(intent: V2OutputIntent): void {
    const available = outputIntentAvailable(this.#snapshot.outputCapabilities, intent)
    if (!available || this.#isTransferActive()) return
    this.#publish({
      ...this.#snapshot,
      outputIntent: intent,
      canStart: this.#selectionAvailable(),
    })
  }

  toggleSelection(id: string): void {
    const joined = this.#joined
    const page = this.#page
    const entry = this.#entries.get(id)
    if (joined === undefined || page === undefined || entry === undefined || this.#isTransferActive()) return
    joined.selection.toggle(entry, page.directory.ancestry)
    this.#publishPage(page)
  }

  openDirectory(id: string): void {
    const joined = this.#joined
    const page = this.#page
    const entry = this.#entries.get(id)
    if (joined === undefined || page === undefined || entry?.kind !== 'directory') return
    let child: V2BrowseDirectory
    try {
      child = joined.childDirectory(page.directory, entry)
    } catch (error) {
      this.#publish({
        ...this.#snapshot,
        phase: 'browsing',
        status: 'This directory cannot be opened safely.',
        error: publicError(error),
        directoryRetryable: false,
      })
      return
    }
    const route = Object.freeze([...this.#directories, child])
    this.#loadPage(child, 0, route).catch(() => undefined)
  }

  openBreadcrumb(index: number): void {
    const directory = this.#directories[index]
    if (directory === undefined || index === this.#directories.length - 1) return
    const route = Object.freeze(this.#directories.slice(0, index + 1))
    this.#loadPage(directory, 0, route).catch(() => undefined)
  }

  showPage(index: number): void {
    const directory = this.#page?.directory
    if (directory === undefined || index < 0 || index >= this.#snapshot.pageCount) return
    this.#loadPage(directory, index, this.#directories).catch(() => undefined)
  }

  retryDirectory(): void {
    const retry = this.#retryableBrowse
    if (retry !== undefined && this.#snapshot.directoryRetryable) {
      this.#loadPage(retry.directory, retry.pageIndex, retry.route, true).catch(() => undefined)
    }
  }

  previewFile(id: string): void {
    const joined = this.#joined
    const entry = this.#entries.get(id)
    if (joined === undefined || entry?.kind !== 'file') return
    // Connectivity is the first post-guard action. This keeps malformed or stale
    // row identities side-effect free while preserving the user's valid click as t0.
    const connectivity = joined.beginPreviewConnectivity()
    this.#closeActivePreview()
    const active: ActiveV2Preview = {
      id: this.#nextPreviewId++,
      entry,
      controller: new AbortController(),
      connectivity,
      seekId: 0,
    }
    this.#preview = active
    this.#publish({
      ...this.#snapshot,
      preview: Object.freeze({ state: 'loading', fileId: entry.idText, name: entry.name }),
    })
    this.#runPreview(joined, active).catch(() => undefined)
  }

  cancelPreview(): void {
    this.#closeActivePreview()
    this.#publish({ ...this.#snapshot, preview: EMPTY_V2_PREVIEW })
  }

  seekPreview(seconds: number): void {
    const active = this.#preview
    if (active?.session === undefined || this.#snapshot.preview.state !== 'video') return
    const seekId = ++active.seekId
    this.#publish({
      ...this.#snapshot,
      preview: Object.freeze({ ...this.#snapshot.preview, seeking: true }),
    })
    active.session.seek(seconds, active.controller.signal).then(
      (presentation) => {
        if (this.#preview !== active || active.seekId !== seekId) return
        this.#publish({
          ...this.#snapshot,
          preview: previewSnapshot(active.entry, presentation, false),
        })
      },
      (error: unknown) => {
        if (this.#preview !== active || active.seekId !== seekId || isAbortError(error)) return
        const failure = Object.freeze({
          state: 'error' as const,
          fileId: active.entry.idText,
          name: active.entry.name,
          message: publicError(error),
        })
        this.#closeActivePreview()
        this.#publish({
          ...this.#snapshot,
          preview: failure,
        })
      },
    )
  }

  previewMediaFailed(url: string): void {
    const active = this.#preview
    const preview = this.#snapshot.preview
    if (active === undefined || (preview.state !== 'image' && preview.state !== 'video') ||
        preview.url !== url) return
    const failure = Object.freeze({
      state: 'error' as const,
      fileId: active.entry.idText,
      name: active.entry.name,
      message: 'The browser could not decode this bounded media preview.',
    })
    this.#closeActivePreview()
    this.#publish({ ...this.#snapshot, preview: failure })
  }

  startDownload(): void {
    const joined = this.#joined
    if (joined === undefined || !this.#snapshot.canStart || this.#isTransferActive()) return
    // Download t0 and P2P belong to the first post-guard statement; selection
    // classification and picker acquisition remain downstream in the click stack.
    const connectivity = joined.beginDownloadConnectivity('unknown')
    const single = this.#knownSingleFile()
    let sizeClass: 'small' | 'large' | 'unknown' = 'unknown'
    if (single !== undefined) {
      sizeClass = single.expectedSize >= SMALL_TRANSFER_BYTE_LIMIT ? 'large' : 'small'
    }
    connectivity.observeSizeClass(sizeClass)
    const selection = single === undefined
      ? { kind: 'Progressive' as const, suggestedArchiveName: 'windshare.zip' }
      : { kind: 'KnownSingleFile' as const, suggestedName: single.name, exactBytes: single.expectedSize }
    let acquired
    try {
      // This call must remain in the click stack; no catalog or storage await may precede it.
      acquired = acquireBrowserV2Output(this.#snapshot.outputIntent, selection)
    } catch (error) {
      connectivity.close()
      this.#fail(error)
      return
    }
    this.#publish({
      ...this.#snapshot,
      phase: 'acquiring-output',
      status: 'Waiting for the output destination.',
      error: null,
    })
    this.#transfer = new AbortController()
    this.#runTransfer(joined, acquired, connectivity).catch(() => undefined)
  }

  abortDownload(): void {
    if (!this.#isTransferActive()) return
    this.#publish({ ...this.#snapshot, phase: 'aborting', status: 'Stopping the transfer…' })
    this.#transfer?.abort(new DOMException('User stopped the transfer', 'AbortError'))
  }

  async dispose(options: { readonly preserveOutputRecovery?: boolean } = {}): Promise<void> {
    if (this.#disposed) return
    this.#disposed = true
    this.#navigation?.abort(new DOMException('Receiver disposed', 'AbortError'))
	this.#unsubscribeScanProgress?.()
	this.#unsubscribeScanProgress = undefined
    this.#transfer?.abort(options.preserveOutputRecovery
      ? new OutputSessionSuspendedError()
      : new DOMException('Receiver disposed', 'AbortError'))
    const previewClose = this.#closeActivePreview()
    await previewClose
    await this.#joined?.close()
    this.#listeners.clear()
  }

  async #join(input: string): Promise<void> {
    await this.#closeActivePreview()
    this.#navigation?.abort(new DOMException('A newer join replaced this one', 'AbortError'))
    this.#pendingNavigationKey = undefined
    this.#loadingDirectory = undefined
    this.#retryableBrowse = undefined
    const navigation = new AbortController()
    this.#navigation = navigation
    this.#publish({
      ...this.#snapshot,
      phase: 'joining',
      status: 'Authenticating the share descriptor…',
      error: null,
      rows: Object.freeze([]),
      preview: EMPTY_V2_PREVIEW,
    })
    try {
      const previous = this.#joined
	  this.#unsubscribeScanProgress?.()
	  this.#unsubscribeScanProgress = undefined
      await previous?.close()
      navigation.signal.throwIfAborted()
      if (this.#joined === previous) this.#joined = undefined
      const joined = await this.#gateway.join(input, this.#pageUrl, navigation.signal)
      if (this.#navigation !== navigation || navigation.signal.aborted || this.#disposed) {
        await joined.close()
        return
      }
      this.#joined = joined
	  this.#unsubscribeScanProgress = joined.subscribeCatalogScanProgress(
		(progress) => this.#catalogScanProgress(joined, progress),
	  )
      const root = joined.rootDirectory()
      this.#page = undefined
      this.#directories = []
      this.#entries.clear()
      this.#rootSingleFile = undefined
      this.#rootEntryCount = 0
      await this.#loadPage(root, 0, Object.freeze([root]))
    } catch (error) {
      if (this.#navigation === navigation && !navigation.signal.aborted) this.#fail(error)
    }
  }

  async #loadPage(
    directory: V2BrowseDirectory,
    pageIndex: number,
    route: readonly V2BrowseDirectory[],
    explicitRetry = false,
  ): Promise<void> {
    const joined = this.#joined
    if (joined === undefined) return
    const candidateRoute = Object.freeze([...route])
    if (candidateRoute.at(-1)?.idText !== directory.idText) {
      throw new TypeError('Browse route does not end at its requested directory')
    }
    const navigationKey = JSON.stringify([
      joined.recoveryIdentity,
      candidateRoute.map((candidate) => candidate.idText),
      pageIndex,
      explicitRetry,
    ])
    if (
      this.#pendingNavigationKey === navigationKey &&
      this.#navigation?.signal.aborted === false
    ) return
    this.#navigation?.abort(new DOMException('A newer browse request replaced this one', 'AbortError'))
    const navigation = new AbortController()
    this.#navigation = navigation
    this.#pendingNavigationKey = navigationKey
    this.#loadingDirectory = directory
    this.#publish({
      ...this.#snapshot,
      phase: 'joining',
      status: `Loading ${directory.name}…`,
      error: null,
      directoryRetryable: false,
    })
    try {
      const page = await joined.page(directory, pageIndex, {
        signal: navigation.signal,
        explicitRetry,
      })
      navigation.signal.throwIfAborted()
      if (this.#navigation !== navigation || this.#joined !== joined || this.#disposed) return
      // Route, page, rows, and breadcrumbs are one publication boundary. A late
      // or failed child load therefore cannot expose a breadcrumb without data.
      this.#directories = [...candidateRoute]
      this.#page = page
      this.#retryableBrowse = undefined
      this.#publishPage(page)
    } catch (error) {
      if (this.#navigation !== navigation || navigation.signal.aborted) return
      const retryable = error instanceof V2DirectoryFailureError && error.failure.retryable
      this.#retryableBrowse = retryable
        ? Object.freeze({ directory, pageIndex, route: candidateRoute })
        : undefined
      this.#publish({
        ...this.#snapshot,
        phase: 'browsing',
        status: 'This directory could not be listed.',
        error: publicError(error),
        breadcrumbs: this.#breadcrumbs(),
        directoryRetryable: retryable,
        canStart: this.#selectionAvailable() && this.#outputAvailable(),
      })
    } finally {
      if (this.#navigation === navigation) {
        this.#loadingDirectory = undefined
        this.#pendingNavigationKey = undefined
        this.#navigation = undefined
      }
    }
  }

  #catalogScanProgress(joined: V2JoinedBrowserShare, progress: V2CatalogScanProgress): void {
	const directory = this.#loadingDirectory
	if (
	  this.#joined !== joined || directory === undefined ||
	  !equalBytes(directory.id, progress.directoryId)
	) return
	this.#publish({
	  ...this.#snapshot,
	  status: `Scanning ${directory.name}… ${progress.discoveredEntries} entries discovered; total still unknown.`,
	})
  }

  #publishPage(page: V2BrowsePage): void {
    const joined = this.#joined
    if (joined === undefined) return
    this.#entries = new Map(page.entries.map((entry) => [entry.idText, entry]))
    const rootPage = page.directory.idText === joined.descriptor.syntheticRootId
    if (rootPage) {
      this.#rootEntryCount = page.entryCount
      const onlyEntry = page.entryCount === 1 && page.omittedCount === 0n
        ? page.entries[0]
        : undefined
      this.#rootSingleFile = onlyEntry?.kind === 'file' ? onlyEntry : undefined
    }
    const rows: V2BrowseRow[] = page.entries.map((entry) => Object.freeze({
      id: entry.idText,
      kind: entry.kind,
      name: entry.name,
      ...(entry.kind === 'file' ? { expectedSize: entry.expectedSize } : {}),
      selection: joined.selection.state(entry, page.directory.ancestry),
    }))
    const known = rootPage && page.pageCount === 1 && page.omittedCount === 0n &&
      page.entries.every((entry) => entry.kind === 'file')
    let selectedFiles = 0
    let selectedBytes = 0n
    for (const entry of page.entries) {
      if (entry.kind === 'file' && joined.selection.selected(entry, page.directory.ancestry)) {
        selectedFiles += 1
        selectedBytes += entry.expectedSize
      }
    }
    this.#publish({
      ...this.#snapshot,
      phase: 'browsing',
      status: page.entryCount === 0 ? 'This directory is empty.' : 'Choose what to receive.',
      error: page.omittedCount === 0n ? null : `${page.omittedCount} entries were omitted by the sender.`,
      rows: Object.freeze(rows),
      breadcrumbs: this.#breadcrumbs(),
      pageIndex: page.pageIndex,
      pageCount: page.pageCount,
      entryCount: page.entryCount,
      omittedCount: page.omittedCount,
      selectedVisibleFiles: selectedFiles,
      selectedVisibleBytes: selectedBytes,
      selectionTotalKnown: known,
      canStart: this.#selectionAvailable() && this.#outputAvailable(),
      directoryRetryable: false,
    })
  }

  async #runPreview(joined: V2JoinedBrowserShare, active: ActiveV2Preview): Promise<void> {
    try {
      const session = await joined.preview(
        active.entry,
        active.connectivity,
        active.controller.signal,
      )
      if (this.#preview !== active || active.controller.signal.aborted) {
        await session.close().catch(() => undefined)
        return
      }
      active.session = session
      this.#publish({
        ...this.#snapshot,
        preview: previewSnapshot(active.entry, session.current, false),
      })
    } catch (error) {
      if (this.#preview !== active || active.controller.signal.aborted) return
      this.#preview = undefined
      active.connectivity.close()
      this.#publish({
        ...this.#snapshot,
        preview: Object.freeze({
          state: 'error',
          fileId: active.entry.idText,
          name: active.entry.name,
          message: publicError(error),
        }),
      })
    }
  }

  #closeActivePreview(): Promise<void> {
    const active = this.#preview
    if (active === undefined) return Promise.resolve()
    this.#preview = undefined
    active.controller.abort(new DOMException('Preview closed', 'AbortError'))
    active.connectivity.close()
    const close = active.session?.close() ?? Promise.resolve()
    return close
      .catch(() => undefined)
  }

  async #runTransfer(
    joined: V2JoinedBrowserShare,
    acquired: ReturnType<typeof acquireBrowserV2Output>,
    connectivity: V2ConnectivityActivation,
  ): Promise<void> {
    try {
      const capability = await acquired
      this.#transfer?.signal.throwIfAborted()
      const output = await openBrowserV2OutputSession(
        capability,
        `${joined.recoveryIdentity}:${this.#snapshot.outputIntent}`,
      )
      this.#publish({
        ...this.#snapshot,
        phase: 'discovering',
        status: 'Discovering the selected tree and opening content lanes…',
        progress: EMPTY_V2_PROGRESS,
      })
      const job = joined.transferJob(output, connectivity, {
        onProgress: (progress) => this.#acceptProgress(progress),
        onMeasure: (measure) => connectivity.observeSizeClass(measure.sizeClass),
      })
      const result = await job.run(this.#transfer?.signal)
      if (result.outcome.status === 'Aborted') {
        this.#publish({ ...this.#snapshot, phase: 'aborted', status: 'Transfer stopped.' })
      } else if (result.outcome.status === 'CompletedWithErrors') {
        this.#publish({
          ...this.#snapshot,
          phase: 'completed-errors',
          status: `Saved with ${result.outcome.failureCount} item error(s).`,
        })
      } else {
        this.#publish({ ...this.#snapshot, phase: 'completed', status: 'Transfer complete.' })
      }
    } catch (error) {
      if (this.#transfer?.signal.aborted) {
        this.#publish({ ...this.#snapshot, phase: 'aborted', status: 'Transfer stopped.' })
      } else if (this.#snapshot.phase === 'acquiring-output') {
        this.#publish({
          ...this.#snapshot,
          phase: 'browsing',
          status: isAbortError(error) ? 'Output selection was cancelled.' : 'Choose an output destination and try again.',
          error: isAbortError(error) ? null : publicError(error),
          canStart: this.#selectionAvailable() && this.#outputAvailable(),
        })
      } else {
        this.#fail(error)
      }
    } finally {
      connectivity.close()
      this.#transfer = undefined
    }
  }

  #acceptProgress(progress: V2TransferProgress): void {
    const snapshot: V2ReceiverProgress = Object.freeze({ ...progress })
    this.#publish({
      ...this.#snapshot,
      phase: progress.writtenBytes > 0n ? 'transferring' : 'discovering',
      status: progress.discoveryComplete ? 'Receiving authenticated blocks…' : 'Discovering selected files…',
      progress: snapshot,
    })
  }

  #knownSingleFile(): Extract<V2CatalogEntry, { kind: 'file' }> | undefined {
    const joined = this.#joined
    if (joined === undefined || !this.#snapshot.selectionTotalKnown) return undefined
    const ancestry = [joined.descriptor.syntheticRootId]
    const candidate = this.#rootSingleFile
    return candidate !== undefined && joined.selection.selected(candidate, ancestry)
      ? candidate
      : undefined
  }

  #breadcrumbs() {
    return Object.freeze(this.#directories.map((directory) => Object.freeze({
      id: directory.idText,
      name: directory.name,
    })))
  }

  #isTransferActive(): boolean {
    return this.#transfer !== undefined
  }

  #outputAvailable(): boolean {
    return outputIntentAvailable(this.#snapshot.outputCapabilities, this.#snapshot.outputIntent)
  }

  #selectionAvailable(): boolean {
    const joined = this.#joined
    if (joined === undefined || this.#rootEntryCount === 0) return false
    if (!this.#snapshot.selectionTotalKnown) {
      return joined.selection.shouldDiscover(joined.descriptor.syntheticRootId, [])
    }
    const rootAncestry = [joined.descriptor.syntheticRootId]
    return this.#page?.entries.some(
      (entry) => joined.selection.state(entry, rootAncestry) !== 'unselected',
    ) === true
  }

  #fail(error: unknown): void {
    this.#publish({
      ...this.#snapshot,
      phase: 'failed',
      status: 'The receiver stopped safely.',
      error: publicError(error),
    })
  }

  #publish(snapshot: V2ReceiverSnapshot): void {
    this.#snapshot = Object.freeze(snapshot)
    for (const listener of this.#listeners) listener()
  }
}

function publicError(error: unknown): string {
  return error instanceof Error && error.message.length > 0
    ? error.message
    : 'An unexpected receiver error occurred.'
}

function previewSnapshot(
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

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}
