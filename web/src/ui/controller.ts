import { SelectionPolicy } from '../catalog/selection'
import { BrowserReceiverGateway, type BrowsePage, type JoinedBrowserShare } from '../application/browser-share/gateway'
import {
  CapabilityInputLifecycle,
  type CapabilityJoinLease,
} from '../application/browser-share/capability-lifecycle'
import { capabilityInputFingerprint } from '../application/browser-share/capability-input'
import { CapabilityNavigation } from '../application/browser-share/capability-navigation'
import type {
  ReceiveAction,
  RetainedTaskSnapshot,
  SingleFileDownloadOutcome,
} from '../application/receive/contracts'
import { ReceiveCoordinator, type ReceiveCoordinatorOptions } from '../application/receive/coordinator'
import type {
  RestorePlatform,
  SafeNameRestoreExportResult,
  SafeNameRestorePlan,
  SafeNameRestorePrepared,
} from '../output/model'
import { InitialJoinControl } from '../receiver/initial-join'
import type { SourceRevisionFailure } from '../transfer/state'
import type { CapturedLocation } from './capability/location'
import type { ReceiverControllerOptions } from './controller/contracts'
import {
  joinFailure,
  mapReceiverFailure,
  outcomeFailure,
  previewDownloadResultFromOutcome,
  type ChooseArtifactIntent,
  type PreviewDownloadCallbacks,
  type PreviewDownloadResult,
  type TaskActionCallbacks,
} from './controller/failures'
import { BrowserNavigationCoordinator } from './controller/navigation'
import { JoinedShareObservation } from './controller/joined-observation'
import { ReceiverProjections } from './controller/projections'
import { initialReceiverSnapshot, joiningReceiverSnapshot, receiverProgressSnapshot } from './controller-state'
import { projectDraft, scopeSelection } from './draft/model'
import { SelectionDraftScope } from './draft/scope'
import { ReceiverExperienceObservability } from './experience/observability'
import type { ReceiverSnapshot } from './model'
import { ReceiveOperationTransitions } from './operation-ownership/transitions'
import { presentReceiveOutput } from './output'
import { PreviewController } from './preview-controller'

export { formatPublicError } from '../application/browser-share/capability-lifecycle'
export type { DiagnosticFormatter, SecurityMilestone } from '../application/browser-share/capability-lifecycle'
export { captureLocation } from './capability/location'
export type { CapturedLocation, LocationCaptureOptions } from './capability/location'
export type { ReceiverControllerOptions } from './controller/contracts'
export type { ChooseArtifactIntent, PreviewDownloadCallbacks, PreviewDownloadResult, TaskActionCallbacks } from './controller/failures'
export type { ReceiverTraceEvent } from '../application/receive/contracts'

export class ReceiverController {
  readonly presentation: ReceiverProjections
  readonly #gateway: BrowserReceiverGateway
  readonly #receive: ReceiveCoordinator
  readonly #capabilityLifecycle: CapabilityInputLifecycle
  readonly #capabilityIntake: ReceiverControllerOptions['capabilityIntake']
  readonly #preferences: ReceiverControllerOptions['preferences']
  readonly #trace: ReceiverControllerOptions['trace']
  readonly #recordFailure: ReceiverControllerOptions['recordFailure']
  readonly #capabilityNavigation = new CapabilityNavigation(() =>
    this.#joinNavigation !== undefined || this.#snapshot.connection.kind === 'connected' ||
    this.#snapshot.connection.kind === 'reconnecting')
  readonly #listeners = new Set<() => void>()
  readonly #experienceTrace: ReceiverExperienceObservability
  readonly #previews: PreviewController
  readonly #browse: BrowserNavigationCoordinator
  readonly #joinedObservation: JoinedShareObservation
  readonly #unsubscribeReceive: () => void
  readonly #unsubscribePreferences: (() => void) | undefined
  readonly #operationTransitions: ReceiveOperationTransitions
  readonly #draftScope = new SelectionDraftScope()
  #snapshot: ReceiverSnapshot
  #pageUrl = ''
  #joined: JoinedBrowserShare | undefined
  #joinNavigation: AbortController | undefined
  #joinRecovery: InitialJoinControl | undefined
  #disposed = false

  constructor(
    gateway: BrowserReceiverGateway,
    options: ReceiverControllerOptions = {},
  ) {
    this.#gateway = gateway
    this.#preferences = options.preferences
    this.#trace = options.trace
    this.#recordFailure = options.recordFailure
    this.#experienceTrace = new ReceiverExperienceObservability(options.trace)
    this.#capabilityLifecycle = new CapabilityInputLifecycle(options)
    this.#capabilityIntake = options.capabilityIntake
    this.#receive = new ReceiveCoordinator({
      windowPort: (options.windowPort ?? globalThis) as ReceiveCoordinatorOptions['windowPort'],
      ...(options.databaseName === undefined ? {} : { databaseName: options.databaseName }),
      ...(options.opfsEnv === undefined ? {} : { opfsEnv: options.opfsEnv }),
      ...(options.outputTrace === undefined ? {} : { outputTrace: options.outputTrace }),
      currentJoinedShare: () => this.#joined,
      onActionError: error => this.#publishActionError(error),
    })
    this.#browse = new BrowserNavigationCoordinator({
      onPageCommitted: page => this.#pageCommitted(page),
      currentJoinedShare: () => this.#joined,
      isDisposed: () => this.#disposed,
      snapshot: () => this.#snapshot,
      publish: snapshot => this.#publish(snapshot),
      publicError: error => this.#publicError(error),
      ...(options.recordFailure === undefined ? {} : { recordFailure: options.recordFailure }),
    })
    this.#joinedObservation = new JoinedShareObservation({
      ownsJoinedShare: joined => !this.#disposed && this.#joined === joined,
      isJoining: () => this.#joinNavigation !== undefined,
      onConnection: connection => {
        this.#receive.observeConnection(connection.kind)
        this.#publish({ ...this.#snapshot, connection })
      },
      onPathActivity: pathActivity => this.#publish({ ...this.#snapshot, pathActivity }),
      onCatalogScanProgress: (joined, progress) => this.#browse.catalogScanProgress(joined, progress),
      onProtocolGeneration: joined => this.#beginSelectionProjection(joined),
    })
    this.#snapshot = this.#applyReceiveSnapshot(initialReceiverSnapshot())
    this.presentation = new ReceiverProjections(
      this.#snapshot,
      (task, action) => this.retainedActionAdmission(task, action),
      action => this.activeLifecycleActionAdmission(action),
    )
    this.#unsubscribeReceive = this.#receive.subscribe(() => {
      if (!this.#disposed) this.#publish(this.#applyReceiveSnapshot(this.#snapshot))
    })
    this.#unsubscribePreferences = this.#preferences?.subscribe(() => {
      if (!this.#disposed) this.#publish(this.#applyReceiveSnapshot(this.#snapshot))
    })
    this.#previews = new PreviewController({
      snapshot: () => this.#snapshot,
      publish: snapshot => this.#publish(snapshot),
      publicError: error => this.#publicError(error),
      ...(options.recordFailure === undefined ? {} : { recordFailure: options.recordFailure }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    this.#operationTransitions = new ReceiveOperationTransitions({
      coordinator: this.#receive,
      currentJoinedShare: () => this.#joined,
      isDisposed: () => this.#disposed,
      snapshot: () => this.#snapshot,
      publish: snapshot => this.#publish(snapshot),
      mapFailure: error => mapReceiverFailure(error, err => this.#publicError(err)),
      onActionError: error => this.#publishActionError(error),
      recordIntent: action => this.recordExperienceIntent(action),
      beginProjection: joined => this.#beginSelectionProjection(joined),
      browse: this.#browse,
      previews: this.#previews,
      experienceTrace: this.#experienceTrace,
    })
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  readonly getSnapshot = (): ReceiverSnapshot => this.#snapshot

  readonly getOwnershipSnapshot = () => Object.freeze({
    currentPageEntries: this.#browse.entryCount,
    receiveOperationActive: this.#receive.active,
  })

  initialize(captured: CapturedLocation): void {
    if (this.#disposed) return
    this.#pageUrl = captured.pageUrl
    this.openLocation(captured)
  }

  ensureDownloads(): Promise<void> { return this.#receive.ensureRetained() }

  openLocation(captured: CapturedLocation): void {
    if (this.#disposed) return
    try { this.#capabilityIntake?.accept(captured) } catch {
      // Optional presentation intent must not prevent a capability from joining.
    }
    if (captured.capabilityInput !== null) this.#join(captured.capabilityInput, captured.pageUrl).catch(() => undefined)
  }

  submitKey(input: string): void {
    if (this.#disposed || input.trim().length === 0) return
    this.openLocation({ capabilityInput: input.trim(), pageUrl: this.#pageUrl })
    // The form clears its password field in this same stack before join can reject.
    this.#capabilityLifecycle.notify('key-cleared')
  }

  requestReconnect(): void {
    if (this.#disposed) return
    this.recordExperienceIntent('reconnect-now')
    if (this.#joinRecovery !== undefined) this.#joinRecovery.request('retry')
    else this.#joined?.requestReconnect()
  }

  continueJoinWaiting(): void {
    if (this.#disposed) return
    this.recordExperienceIntent('continue-connection-wait')
    this.#joinRecovery?.request('continue')
  }

  cancelJoin(): void {
    if (this.#disposed || this.#joinNavigation === undefined) return
    const reason = new DOMException('Connection waiting cancelled', 'AbortError')
    this.#capabilityNavigation.clear()
    this.#joinNavigation.abort(reason)
    this.#browse.cancel(reason)
    if (this.#snapshot.share === null && this.#joined !== undefined) {
      const joined = this.#joined
      this.#joined = undefined
      joined.close().catch(() => undefined)
    }
    this.#receive.observeConnection('idle')
    this.#publish({
      ...this.#snapshot,
      phase: 'awaiting-key',
      connection: { kind: 'idle' },
      status: 'cancelled',
      error: null,
    })
  }

  recordExperienceIntent(action: string): void {
    this.#experienceTrace.intent(action, this.#snapshot)
  }

  toggleSelection(id: string): void {
    this.recordExperienceIntent('toggle-selection')
    const joined = this.#joined
    const page = this.#browse.page
    const entry = this.#browse.entry(id)
    if (this.#disposed || joined === undefined || page === undefined || entry === undefined) return
    if (this.#snapshot.draft.mode !== 'selection') joined.replaceSelection(new SelectionPolicy(false))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'selection' as const } })
    joined.selection.toggle(entry, page.directory.ancestry)
    this.#refreshDraft(joined, page)
  }

  enterSelectionMode(): void {
    this.recordExperienceIntent('enter-selection')
    const joined = this.#joined
    const page = this.#browse.page
    if (this.#disposed || joined === undefined || page === undefined || this.#snapshot.draft.mode === 'selection') return
    joined.replaceSelection(new SelectionPolicy(false))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'selection' as const } })
    this.#refreshDraft(joined, page)
  }

  exitSelectionMode(): void {
    this.recordExperienceIntent('exit-selection')
    const joined = this.#joined
    const page = this.#browse.page
    const directory = this.#draftScope.directory
    if (this.#disposed || joined === undefined || page === undefined || directory === undefined) return
    joined.replaceSelection(scopeSelection(directory))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'scope' as const } })
    this.#refreshDraft(joined, page)
  }

  selectPage(): void {
    this.recordExperienceIntent('select-page')
    const joined = this.#joined
    const page = this.#browse.page
    if (this.#disposed || joined === undefined || page === undefined) return
    if (this.#snapshot.draft.mode !== 'selection') joined.replaceSelection(new SelectionPolicy(false))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'selection' as const } })
    for (const entry of page.entries) joined.selection.set(entry, page.directory.ancestry, true)
    this.#refreshDraft(joined, page)
  }

  clearSelection(): void {
    this.recordExperienceIntent('clear-selection')
    const joined = this.#joined
    const page = this.#browse.page
    if (this.#disposed || joined === undefined || page === undefined) return
    joined.replaceSelection(new SelectionPolicy(false))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'selection' as const } })
    this.#refreshDraft(joined, page)
  }

  openDirectory(id: string): void {
    this.recordExperienceIntent('open-directory')
    this.#browse.openDirectory(id)
  }

  openBreadcrumb(index: number): void {
    this.recordExperienceIntent('open-breadcrumb')
    this.#browse.openBreadcrumb(index)
  }

  showPage(index: number): void {
    this.recordExperienceIntent('show-page')
    this.#browse.showPage(index)
  }

  retryDirectory(): void {
    this.recordExperienceIntent('retry-directory')
    this.#browse.retryDirectory()
  }

  previewFile(id: string): void {
    this.recordExperienceIntent('preview-file')
    const joined = this.#joined
    const page = this.#browse.page
    const entry = this.#browse.entry(id)
    if (joined === undefined || page === undefined || entry?.kind !== 'file') return
    this.#previews.open(joined, entry, page.directory)
  }

  cancelPreview(): void { this.#previews.cancel() }
  seekPreview(seconds: number): void { this.#previews.seek(seconds) }
  previewMediaPresented(presentationId: number): void { this.#previews.mediaPresented(presentationId) }
  previewMediaFailed(presentationId: number): void { this.#previews.mediaFailed(presentationId) }

  chooseArtifact(choiceId: string, intent: ChooseArtifactIntent = 'default'): void {
    this.recordExperienceIntent('choose-saving-outcome')
    if (intent === 'default' && this.#tryCompleteSinglePhotoBlob(choiceId)) return
    if (this.#receive.startBlockedReason() !== null) return
    if (this.#snapshot.error !== null) this.#publish({ ...this.#snapshot, error: null })
    const display = Object.freeze({
      objectLabel: this.#snapshot.draft.label,
      createdAtMilliseconds: Date.now(),
    })
    this.#previews.yieldToReceiving()
    this.#receive.startDownload(choiceId, display)
  }

  downloadFile(id: string): SingleFileDownloadOutcome {
    this.recordExperienceIntent('download-single-file')
    if (this.#snapshot.error !== null) this.#publish({ ...this.#snapshot, error: null })
    const page = this.#browse.page
    const entry = this.#browse.entry(id)
    if (this.#disposed || page === undefined || entry?.kind !== 'file') {
      return Object.freeze({ kind: 'blocked', reason: 'actionUnavailable' })
    }
    if (this.#receive.startBlockedReason() === null) this.#previews.yieldToReceiving()
    const outcome = this.#receive.startSingleFileDownload(entry, page.directory, {
      display: Object.freeze({ objectLabel: entry.name, createdAtMilliseconds: Date.now() }),
    })
    const failure = outcomeFailure(outcome, error => this.#publicError(error))
    if (failure !== null) this.#publish({ ...this.#snapshot, error: failure })
    return outcome
  }

  downloadPreviewedFile(callbacks?: PreviewDownloadCallbacks): PreviewDownloadResult {
    const target = this.#previews.activeTarget()
    const previewState = this.#snapshot.preview.state
    if (this.#disposed || target === undefined || previewState === 'idle') {
      return Object.freeze({ kind: 'failed', failure: Object.freeze({ kind: 'blocked', reason: 'actionUnavailable' }) })
    }
    if (target.imageBlob !== undefined) {
      this.recordExperienceIntent('save-preview-image')
      if (this.#receive.saveLocalBlob(target.imageBlob, target.entry.name)) {
        return Object.freeze({ kind: 'saved-image' })
      }
      return Object.freeze({
        kind: 'failed',
        failure: Object.freeze({ kind: 'save-unavailable', reason: 'no-safe-destination' }),
      })
    }
    this.recordExperienceIntent('download-preview-file')
    const fileId = target.entry.idText
    const closeOnActivate = previewState === 'loading' || previewState === 'error'
    const outcome = this.#receive.startSingleFileDownload(target.entry, target.directory, {
      display: Object.freeze({ objectLabel: target.entry.name, createdAtMilliseconds: Date.now() }),
      onActivated: () => {
        if (!closeOnActivate || this.#disposed) return
        const current = this.#snapshot.preview
        if ((current.state === 'loading' || current.state === 'error') && current.fileId === fileId) {
          this.#previews.cancel()
        }
      },
      onAsyncError: error => {
        if (this.#disposed) return
        const current = this.#snapshot.preview
        if (current.state !== 'idle' && current.fileId === fileId && callbacks?.onAsyncFailure !== undefined) {
          callbacks.onAsyncFailure(mapReceiverFailure(error, err => this.#publicError(err)))
        } else {
          this.#publishActionError(error)
        }
      },
    })
    return previewDownloadResultFromOutcome(outcome, error => this.#publicError(error))
  }

  cancelPreparing(): void {
    this.recordExperienceIntent('cancel-preparing')
    this.#receive.cancelActivation()
    if (this.#joined !== undefined) this.#beginSelectionProjection(this.#joined)
  }

  retryOutputConfirmation(): void { this.#receive.retryOffers() }

  activeLifecycleActionAdmission(action: ReceiveAction) {
    return this.#receive.activeActionAdmission(action)
  }

  performLifecycleAction(action: ReceiveAction, callbacks?: TaskActionCallbacks): void {
    this.#operationTransitions.performLifecycleAction(action, callbacks)
  }

  get canRetainCurrentOperation(): boolean { return this.#receive.canRetainCurrent }

  retainCurrentOperation(): Promise<boolean> { return this.#operationTransitions.retainCurrentOperation() }

  retainedActionAdmission(task: RetainedTaskSnapshot, action: ReceiveAction) {
    return this.#receive.retainedActionAdmission(task, action)
  }

  performRetainedAction(task: RetainedTaskSnapshot, action: ReceiveAction, callbacks?: TaskActionCallbacks): void {
    this.#operationTransitions.performRetainedAction(task, action, callbacks)
  }

  prepareReplacementDownload(task: RetainedTaskSnapshot, failure: SourceRevisionFailure): void {
    this.#operationTransitions.prepareReplacementDownload(task, failure)
  }

  startNewReceiveOperation(): void { this.#operationTransitions.startNewReceiveOperation() }

  detectRestorePlatform(): RestorePlatform { return this.#receive.detectRestorePlatform() }

  prepareSafeNameRestore(taskId: string, platform: RestorePlatform): Promise<SafeNameRestorePrepared> {
    return this.#receive.prepareSafeNameRestore(taskId, platform)
  }

  exportSafeNameRestoreArtifact(taskId: string, platform: RestorePlatform): Promise<SafeNameRestoreExportResult> {
    return this.#receive.exportSafeNameRestoreArtifact(taskId, platform)
  }

  downloadSafeNameRestoreManifest(plan: SafeNameRestorePlan): boolean {
    return this.#receive.downloadSafeNameRestoreManifest(plan)
  }

  mapTaskFailure(error: unknown) {
    return mapReceiverFailure(error, err => this.#publicError(err))
  }

  async dispose(): Promise<void> {
    if (this.#disposed) return
    this.#disposed = true
    this.#capabilityNavigation.clear()
    this.#capabilityLifecycle.clear()
    this.#operationTransitions.cancelPendingReplacement()
    this.#joinNavigation?.abort(new DOMException('Receiver disposed', 'AbortError'))
    this.#browse.cancel(new DOMException('Receiver disposed', 'AbortError'))
    this.#joinedObservation.close()
    this.#unsubscribeReceive()
    this.#unsubscribePreferences?.()
    await Promise.allSettled([
      this.#receive.dispose(),
      this.#previews.close(),
      ...(this.#joined === undefined ? [] : [this.#joined.close()]),
    ])
    this.#listeners.clear()
    this.presentation.close()
  }

  #join(input: string, pageUrl = this.#pageUrl): Promise<void> {
    const lease = this.#capabilityLifecycle.beginJoin(input, pageUrl)
    return this.#capabilityNavigation.open(capabilityInputFingerprint(input, pageUrl), async () => {
      if (this.#disposed) return 'blocked'
      const reason = this.#receive.shareNavigationBlockedReason()
      if (reason !== null) {
        this.#publish({ ...this.#snapshot, error: { kind: 'blocked', reason } })
        return 'blocked'
      }
      this.#pageUrl = pageUrl
      return await this.#joinOwned(lease, pageUrl) ? 'joined' : 'failed'
    }).then(decision => {
      if (this.#disposed) return
      this.recordExperienceIntent(`share-navigation-${decision}`)
      if (decision === 'reused' && this.#snapshot.connection.kind === 'reconnecting') this.requestReconnect()
    }).finally(() => lease.release())
  }

  async #joinOwned(
    lease: CapabilityJoinLease,
    pageUrl: string,
  ): Promise<boolean> {
    let navigation: AbortController | undefined
    let previous: JoinedBrowserShare | undefined
    let candidate: JoinedBrowserShare | undefined
    let joinedReplacementInstalled = false
    this.#traceJoin('started')
    try {
      this.#joinNavigation?.abort(new DOMException('A newer join replaced this one', 'AbortError'))
      this.#browse.cancel(new DOMException('A newer join replaced this one', 'AbortError'))
      this.#operationTransitions.cancelPendingReplacement()
      navigation = new AbortController()
      this.#joinNavigation = navigation
      previous = this.#joined
      lease.activate()
      this.#receive.suspendForNewShare()
      await this.#previews.close()
      navigation.signal.throwIfAborted()
      this.#publish(joiningReceiverSnapshot(this.#snapshot))
      this.#joinedObservation.suspendForJoin()
      const activeNavigation = navigation
      const recovery = new InitialJoinControl()
      this.#joinRecovery = recovery
      const joined = await lease.handoff(ownedInput =>
        this.#gateway.join(ownedInput, pageUrl, activeNavigation.signal, {
          control: recovery,
          onState: join => {
            if (!this.#joinReplacementIsCurrent(activeNavigation)) return
            this.#publish({ ...this.#snapshot, connection: { kind: 'idle', join } })
          },
        }))
      candidate = joined
      if (!this.#joinReplacementIsCurrent(navigation)) {
        this.#traceJoin('stale_replacement')
        return false
      }
      this.#joined = joined
      this.#draftScope.reset()
      this.#joinRecovery = undefined
      joinedReplacementInstalled = true
      await previous?.close().catch(() => undefined)
      if (!this.#joinReplacementIsCurrent(navigation)) return false
      this.#traceJoin('joined')
      this.#joinedObservation.observe(joined)
      this.#receive.warmCapabilities()
      await this.#openInitialBrowsePage(joined)
      return this.#joinReplacementIsCurrent(navigation)
    } catch (error) {
      this.#handleJoinFailure(error, lease, navigation, previous, joinedReplacementInstalled)
      return false
    } finally {
      this.#finishJoinAttempt(navigation, lease, candidate, joinedReplacementInstalled)
    }
  }

  async #openInitialBrowsePage(joined: JoinedBrowserShare): Promise<void> {
    const root = joined.rootDirectory()
    this.#browse.clearCatalog()
    await this.#browse.loadPage(root, 0, Object.freeze([root]))
    if (this.#joined === joined && this.#browse.pageMatches(root)) {
      const share = this.#snapshot.share
      if (share?.kind === 'browser' && share.singleFolder) {
        this.#browse.openDirectory(share.homeDirectoryId)
      }
    }
  }

  #finishJoinAttempt(
    navigation: AbortController | undefined,
    lease: CapabilityJoinLease,
    candidate: JoinedBrowserShare | undefined,
    installed: boolean,
  ): void {
    if (this.#joinNavigation === navigation) {
      this.#joinNavigation = undefined
      this.#joinRecovery = undefined
    }
    lease.release()
    if (candidate !== undefined && !installed) void candidate.close().catch(() => undefined)
  }

  #joinReplacementIsCurrent(navigation: AbortController): boolean {
    return this.#joinNavigation === navigation && !navigation.signal.aborted && !this.#disposed
  }

  #handleJoinFailure(
    error: unknown,
    lease: CapabilityJoinLease,
    navigation: AbortController | undefined,
    previous: JoinedBrowserShare | undefined,
    joinedReplacementInstalled: boolean,
  ): void {
    if (navigation === undefined || this.#joinNavigation !== navigation || navigation.signal.aborted) {
      return
    }
    if (!joinedReplacementInstalled && previous !== undefined && this.#joined === previous) {
      this.#joinedObservation.observe(previous)
    }
    this.#reportJoinFailure(error)
    this.#traceJoin('failed')
    this.#fail(error, lease)
  }

  #beginSelectionProjection(joined: JoinedBrowserShare): void {
    if (this.#draftScope.directory === undefined) return
    this.#receive.refreshOffers(joined)
  }

  #pageCommitted(page: BrowsePage): void {
    const joined = this.#joined
    if (joined === undefined) return
    const { share, draft, transition } = this.#draftScope.commitPage(
      joined,
      page,
      this.#snapshot.draft.mode,
      this.#snapshot.share,
    )
    this.#snapshot = Object.freeze({ ...this.#snapshot, share, draft, status: null })
    if (transition !== 'page-updated') this.#beginSelectionProjection(joined)
    if (share?.kind === 'photo' && page.directory.idText === joined.descriptor.syntheticRootId && !this.#receive.active) {
      const entry = page.entries[0]
      if (entry?.kind === 'file') this.#previews.openAutomaticPhoto(joined, entry, page.directory)
    }
    if (!(share?.kind === 'browser' && share.singleFolder && page.directory.idText === joined.descriptor.syntheticRootId)) {
      // The synthetic wrapper is not the directory the recipient came to browse.
      queueMicrotask(() => { if (this.#joined === joined && !this.#disposed) void this.ensureDownloads() })
    }
  }

  #refreshDraft(joined: JoinedBrowserShare, page: BrowsePage): void {
    const directory = this.#draftScope.directory
    if (directory === undefined) return
    this.#snapshot = Object.freeze({
      ...this.#snapshot,
      draft: projectDraft(this.#snapshot.draft.mode, directory, joined.selection, this.#snapshot.share),
    })
    this.#browse.publishPage(page)
    this.#beginSelectionProjection(joined)
  }

  #singlePhotoLocalChoiceId(snapshot: ReceiverSnapshot): string | null {
    const share = snapshot.share
    if (share?.kind !== 'photo') return null
    const target = this.#previews.activeTarget()
    if (target?.imageBlob === undefined || target.entry.idText !== share.file.id) return null
    const offers = snapshot.output.offers
    if (offers?.kind !== 'ready') return null
    const choices = [offers.primary, ...offers.alternatives, ...(offers.zip === null ? [] : [offers.zip])]
    return choices.find(choice => choice.backend === 'portable' && choice.artifactKind === 'original-file')?.choiceId ?? null
  }

  #tryCompleteSinglePhotoBlob(choiceId: string): boolean {
    if (this.#singlePhotoLocalChoiceId(this.#snapshot) !== choiceId) return false
    if (this.#receive.localStartBlockedReason() !== null) return true
    const target = this.#previews.activeTarget()
    if (target?.imageBlob === undefined) return false
    if (this.#snapshot.error !== null) this.#publish({ ...this.#snapshot, error: null })
    const display = Object.freeze({
      objectLabel: this.#snapshot.draft.label,
      createdAtMilliseconds: Date.now(),
    })
    this.#receive.completeLocalBlobDownload(choiceId, target.imageBlob, display)
    return true
  }

  #publishActionError(error: unknown): void {
    this.#publish({
      ...this.#snapshot,
      error: mapReceiverFailure(error, err => this.#publicError(err)),
    })
  }

  #fail(error: unknown, lease?: CapabilityJoinLease): void {
    const detail = lease?.publicError(error) ?? this.#publicError(error)
    this.#publish({
      ...this.#snapshot,
      phase: 'failed',
      status: 'stopped',
      error: joinFailure(error, detail),
    })
  }

  #publicError(error: unknown): string { return this.#capabilityLifecycle.publicError(error) }

  #language() { return this.#preferences?.getSnapshot().language ?? 'en' }

  #applyReceiveSnapshot(snapshot: ReceiverSnapshot): ReceiverSnapshot {
    const runtime = this.#receive.getSnapshot()
    return Object.freeze({
      ...snapshot,
      output: presentReceiveOutput(runtime, this.#language()),
      progress: receiverProgressSnapshot(runtime.progress),
      retained: runtime.retained,
      taskDisplay: runtime.display,
      activeReceiveOperationId: runtime.activeTaskId,
    })
  }

  #publish(snapshot: ReceiverSnapshot): void {
    const reason = this.#receive.startBlockedReason()
    const canReleaseCurrent = this.#receive.canReleaseCurrent
    const localChoiceId = this.#singlePhotoLocalChoiceId(snapshot)
    const localReason = localChoiceId === null ? null : this.#receive.localStartBlockedReason()
    const previousAdmission = this.#snapshot.startAdmission
    const sameLocalChoice = localChoiceId === null
      ? previousAdmission.localChoice === null
      : previousAdmission.localChoice?.choiceId === localChoiceId &&
        previousAdmission.localChoice.reason === localReason
    const startAdmission =
      previousAdmission.reason === reason &&
      previousAdmission.canReleaseCurrent === canReleaseCurrent &&
      sameLocalChoice
        ? previousAdmission
        : Object.freeze({
            allowed: reason === null,
            reason,
            canReleaseCurrent,
            localChoice: localChoiceId === null
              ? null
              : Object.freeze({ choiceId: localChoiceId, allowed: localReason === null, reason: localReason }),
          })
    this.#snapshot = Object.freeze({
      ...snapshot,
      activeReceiveOperationId: this.#receive.getSnapshot().activeTaskId,
      startAdmission,
    })
    if (this.#disposed) return
    this.#experienceTrace.publish(this.#snapshot)
    this.presentation.commit(this.#snapshot)
    for (const listener of this.#listeners) listener()
  }

  #reportJoinFailure(error: unknown): void {
    try {
      this.#recordFailure?.({ stage: 'join', error })
    } catch {
      // Diagnostic sinks cannot affect join control flow.
    }
  }

  #traceJoin(transition: 'started' | 'joined' | 'failed' | 'stale_replacement'): void {
    try {
      this.#trace?.current?.(Object.freeze({ name: 'join_transition', transition }))
    } catch {
      // Diagnostic sinks cannot affect join control flow.
    }
  }
}
