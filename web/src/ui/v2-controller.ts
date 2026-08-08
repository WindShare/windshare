import { V2DirectoryFailureError } from '../catalog/v2-client'
import type { V2CatalogScanProgress } from '../catalog/v2-client'
import type { V2CatalogEntry } from '../catalog/v2-records'
import { equalBytes } from '../crypto/bytes'
import type { V2ConnectivityActivation } from '../connectivity/v2-receiver-policy'
import { SMALL_TRANSFER_BYTE_LIMIT } from '../transfer/measure'
import { TransferPauseRequestedError } from '../transfer/output-session'
import {
  createTransferIntentDraft,
  createTransferJobId,
  selectionRulesFromPolicy,
  type TransferIntentDraft,
  type TransferTraceEvent,
} from '../transfer/intent'
import {
  V2BrowserReceiverGateway,
  type V2BrowseDirectory,
  type V2BrowsePage,
  type V2JoinedBrowserShare,
} from './v2-gateway'
import {
  acquireBrowserV2Output,
  browserV2OutputAuthority,
  browserV2OutputCapabilities,
  outputIntentAvailable,
  type V2OutputIntent,
} from './v2-output'
import {
  EMPTY_V2_PROGRESS,
  EMPTY_V2_PREVIEW,
  type V2ReceiverSnapshot,
} from './v2-model'
import {
  V2CapabilityInputLifecycle,
  type V2CapabilityJoinLease,
  type V2CapturedLocation,
  type V2DiagnosticFormatter,
  type V2SecurityMilestone,
} from './v2-capability-lifecycle'
import {
  breadcrumbsFor,
  descriptorIdentity,
  isAbortError,
  knownSingleFile,
  nowMilliseconds,
  projectBrowsePage,
  selectionAvailable,
  transferProgressSnapshot,
  transferTerminalSnapshot,
  type RetryableV2BrowseRequest,
} from './v2-controller-state'
import {
  BrowserV2PausedTaskControlPort,
  V2PausedTaskController,
  type V2PausedTaskControlPort,
} from './v2-paused-tasks'
import { V2PreviewController } from './v2-preview-controller'

export {
  captureV2Location,
  formatV2PublicError,
} from './v2-capability-lifecycle'
export type {
  V2CapturedLocation,
  V2DiagnosticFormatter,
  V2LocationCaptureOptions,
  V2SecurityMilestone,
} from './v2-capability-lifecycle'

export interface V2ReceiverControllerOptions {
  readonly diagnosticFormatter?: V2DiagnosticFormatter
  readonly onSecurityMilestone?: (milestone: V2SecurityMilestone) => void
  readonly onTransferTrace?: (event: TransferTraceEvent) => void
  readonly pausedTasks?: V2PausedTaskControlPort
}

export class V2ReceiverController {
  readonly #gateway: V2BrowserReceiverGateway
  readonly #capabilityLifecycle: V2CapabilityInputLifecycle
  readonly #listeners = new Set<() => void>()
  readonly #onTransferTrace: ((event: TransferTraceEvent) => void) | undefined
  readonly #pausedTasks: V2PausedTaskController
  readonly #previews: V2PreviewController
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
  #disposed = false

  constructor(
    gateway = new V2BrowserReceiverGateway(),
    options: V2ReceiverControllerOptions = {},
  ) {
    this.#gateway = gateway
    this.#onTransferTrace = options.onTransferTrace
    this.#capabilityLifecycle = new V2CapabilityInputLifecycle(options)
    this.#pausedTasks = new V2PausedTaskController(
      options.pausedTasks ?? new BrowserV2PausedTaskControlPort(),
      {
        joined: () => this.#joined,
        disposed: () => this.#disposed,
        regularTransferActive: () => this.#transfer !== undefined,
        snapshot: () => this.#snapshot,
        publish: (snapshot) => this.#publish(snapshot),
        publicError: (error) => this.#publicError(error),
        transferTrace: (event) => this.#onTransferTrace?.(event),
      },
    )
    this.#previews = new V2PreviewController({
      snapshot: () => this.#snapshot,
      publish: (snapshot) => this.#publish(snapshot),
      publicError: (error) => this.#publicError(error),
    })
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
      pausedTasks: Object.freeze([]),
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
    this.#capabilityLifecycle.acceptCapturedLocation(captured)
    this.#pausedTasks.refresh().catch(() => undefined)
    if (captured.capabilityInput !== null) {
      this.#join(captured.capabilityInput).catch(() => undefined)
    }
  }

  submitKey(input: string): void {
    if (this.#disposed || input.trim().length === 0) return
    this.#join(input.trim()).catch(() => undefined)
    // The form has already emptied its password field; publish the matching
    // controller ownership milestone in the same call stack, before any join
    // rejection can be converted into UI state.
    this.#capabilityLifecycle.notify('key-cleared')
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
        error: this.#publicError(error),
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
    this.#previews.open(joined, entry)
  }

  cancelPreview(): void {
    this.#previews.cancel()
  }

  seekPreview(seconds: number): void {
    this.#previews.seek(seconds)
  }

  previewMediaFailed(url: string): void {
    this.#previews.mediaFailed(url)
  }

  startDownload(): void {
    const joined = this.#joined
    if (joined === undefined || !this.#snapshot.canStart || this.#isTransferActive()) return
    // Download t0 and P2P belong to the first post-guard statement; selection
    // classification and picker acquisition remain downstream in the click stack.
    const downloadT0Milliseconds = nowMilliseconds()
    const shareInstanceId = descriptorIdentity(
      joined.descriptor.shareInstanceId,
      joined.descriptor.shareInstance,
      joined.recoveryIdentity,
    )
    const syntheticRootId = descriptorIdentity(
      joined.descriptor.syntheticRootId,
      joined.descriptor.syntheticRoot,
      'synthetic-root',
    )
    const transferJobId = createTransferJobId()
    const draft = createTransferIntentDraft({
      shareInstance: shareInstanceId,
      syntheticRoot: syntheticRootId,
      selection: selectionRulesFromPolicy(joined.selection.snapshot()),
    })
    this.#emitTransferTrace(Object.freeze({
      name: 'download-t0',
      atMilliseconds: downloadT0Milliseconds,
      context: Object.freeze({
        shareInstance: shareInstanceId,
        transferJobId,
        decision: 'click-guard-passed',
      }),
    }))
    this.#emitTransferTrace(Object.freeze({
      name: 'intent-draft',
      atMilliseconds: nowMilliseconds(),
      context: Object.freeze({
        shareInstance: shareInstanceId,
        transferJobId,
        decision: 'picker-pending',
      }),
    }))
    const connectivity = joined.beginDownloadConnectivity('unknown')
    const single = knownSingleFile(
      joined,
      this.#snapshot.selectionTotalKnown,
      this.#rootSingleFile,
    )
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
      downloadT0Milliseconds,
    })
    this.#transfer = new AbortController()
    this.#runTransfer(joined, acquired, connectivity, draft, transferJobId).catch(() => undefined)
  }

  pauseDownload(): void {
    if (!this.#isTransferActive()) return
    this.#publish({ ...this.#snapshot, phase: 'pausing', status: 'Pausing the transfer…' })
    const reason = new TransferPauseRequestedError('User paused the transfer')
    this.#transfer?.abort(reason)
    this.#pausedTasks.abortTransfer(reason)
  }

  resumePausedTask(id: string): void {
    this.#pausedTasks.resume(id)
  }

  cancelPausedTask(id: string): void {
    this.#pausedTasks.discard(id)
  }

  async dispose(): Promise<void> {
    if (this.#disposed) return
    this.#disposed = true
    this.#capabilityLifecycle.clear()
    this.#navigation?.abort(new DOMException('Receiver disposed', 'AbortError'))
    this.#unsubscribeScanProgress?.()
    this.#unsubscribeScanProgress = undefined
    const reason = new TransferPauseRequestedError(
      'Receiver disposed with durable recovery retained',
    )
    this.#transfer?.abort(reason)
    this.#pausedTasks.abortTransfer(reason)
    const previewClose = this.#previews.close()
    await previewClose
    await this.#joined?.close()
    this.#pausedTasks.close()
    this.#listeners.clear()
  }

  #join(input: string): Promise<void> {
    const lease = this.#capabilityLifecycle.beginJoin(input, this.#pageUrl)
    return this.#joinOwned(lease)
  }

  async #joinOwned(lease: V2CapabilityJoinLease): Promise<void> {
    let navigation: AbortController | undefined
    try {
      await this.#previews.close()
      this.#navigation?.abort(new DOMException('A newer join replaced this one', 'AbortError'))
      this.#pendingNavigationKey = undefined
      this.#loadingDirectory = undefined
      this.#retryableBrowse = undefined
      navigation = new AbortController()
      this.#navigation = navigation
      lease.activate()
      this.#publish({
        ...this.#snapshot,
        phase: 'joining',
        status: 'Authenticating the share descriptor…',
        error: null,
        rows: Object.freeze([]),
        preview: EMPTY_V2_PREVIEW,
      })
      const previous = this.#joined
      this.#unsubscribeScanProgress?.()
      this.#unsubscribeScanProgress = undefined
      await previous?.close()
      navigation.signal.throwIfAborted()
      if (this.#joined === previous) this.#joined = undefined
      const activeNavigation = navigation
      const joinTask = lease.handoff((ownedInput) =>
        this.#gateway.join(ownedInput, this.#pageUrl, activeNavigation.signal))
      const joined = await joinTask
      if (this.#navigation !== navigation || navigation.signal.aborted || this.#disposed) {
        await joined.close()
        return
      }
      this.#joined = joined
      this.#pausedTasks.publishRows()
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
      if (
        navigation !== undefined &&
        this.#navigation === navigation &&
        !navigation.signal.aborted
      ) this.#fail(error, lease)
    } finally {
      lease.release()
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
        error: this.#publicError(error),
        breadcrumbs: breadcrumbsFor(this.#directories),
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
    const projection = projectBrowsePage(
      page,
      joined.selection,
      joined.descriptor.syntheticRootId,
      this.#directories,
    )
    this.#entries = projection.entries
    if (projection.root !== undefined) {
      this.#rootEntryCount = projection.root.entryCount
      this.#rootSingleFile = projection.root.singleFile
    }
    const snapshot = { ...this.#snapshot, ...projection.snapshot }
    this.#publish({
      ...snapshot,
      canStart: selectionAvailable(
        joined,
        this.#rootEntryCount,
        snapshot.selectionTotalKnown,
        page,
      ) && this.#outputAvailable(),
    })
  }

  async #runTransfer(
    joined: V2JoinedBrowserShare,
    acquired: ReturnType<typeof acquireBrowserV2Output>,
    connectivity: V2ConnectivityActivation,
    intent: TransferIntentDraft,
    transferJobId: string,
  ): Promise<void> {
    const output = browserV2OutputAuthority(acquired)
    try {
      const job = joined.transferJob(output, connectivity, {
        onProgress: (progress) => this.#publish({
          ...this.#snapshot,
          ...transferProgressSnapshot(progress),
        }),
        onMeasure: (measure) => connectivity.observeSizeClass(measure.sizeClass),
        onTrace: (event) => this.#onTransferTrace?.(event),
        intent,
        transferJobId,
      })
      const result = await job.run(this.#transfer?.signal)
      this.#publish(transferTerminalSnapshot(
        this.#snapshot,
        result,
      ))
    } catch (error) {
      await output.abort(error)
      if (this.#transfer?.signal.aborted) {
        this.#publish({
          ...this.#snapshot,
          phase: 'retry-ready',
          status: 'Transfer stopped before a resumable checkpoint opened; retry starts from byte zero.',
        })
      } else if (this.#snapshot.phase === 'acquiring-output') {
        this.#publish({
          ...this.#snapshot,
          phase: 'browsing',
          status: isAbortError(error) ? 'Output selection was cancelled.' : 'Choose an output destination and try again.',
          error: isAbortError(error) ? null : this.#publicError(error),
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

  #isTransferActive(): boolean {
    return this.#transfer !== undefined || this.#pausedTasks.active()
  }

  #outputAvailable(): boolean {
    return outputIntentAvailable(this.#snapshot.outputCapabilities, this.#snapshot.outputIntent)
  }

  #selectionAvailable(): boolean {
    return selectionAvailable(
      this.#joined,
      this.#rootEntryCount,
      this.#snapshot.selectionTotalKnown,
      this.#page,
    )
  }

  #fail(error: unknown, lease?: V2CapabilityJoinLease): void {
    this.#publish({
      ...this.#snapshot,
      phase: 'failed',
      status: 'The receiver stopped safely.',
      error: lease?.publicError(error) ?? this.#publicError(error),
    })
  }

  #publicError(error: unknown): string { return this.#capabilityLifecycle.publicError(error) }

  #emitTransferTrace(event: TransferTraceEvent): void {
    try {
      this.#onTransferTrace?.(event)
    } catch {
      // Diagnostics are observational; a rejected observer must not cancel a
      // user-activated picker or alter the connectivity/output authority path.
    }
  }

  #publish(snapshot: V2ReceiverSnapshot): void {
    this.#snapshot = Object.freeze(snapshot)
    for (const listener of this.#listeners) listener()
  }
}
