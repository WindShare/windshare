import { blockedAdmission, terminalConnectionBlock } from '../application/receive/admission'
import { SelectionPolicy } from '../catalog/selection'
import { BrowserReceiverGateway, type BrowsePage, type JoinedBrowserShare } from '../application/browser-share/gateway'
import { CapabilityInputLifecycle } from '../application/browser-share/capability-lifecycle'
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
import type { SourceRevisionFailure } from '../transfer/state'
import type { CapturedLocation } from './capability/location'
import type { ReceiverControllerOptions } from './controller/contracts'
import {
  mapReceiverFailure,
  outcomeFailure,
  previewDownloadResultFromOutcome,
  type AsyncActionCallbacks,
  type ChooseArtifactIntent,
  type PreviewDownloadResult,
  type ReplacementActionCallbacks,
} from './controller/failures'
import { ShareJoinCoordinator } from './controller/join-coordinator'
import { BrowserNavigationCoordinator } from './controller/navigation'
import { JoinedShareObservation } from './controller/joined-observation'
import { ReceiverProjections } from './controller/projections'
import { ShareReportScope } from './controller/share-report'
import {
  initialReceiverSnapshot,
  receiverProgressSnapshot,
  resolveStartAdmission,
} from './controller-state'
import { projectDraft, scopeSelection } from './draft/model'
import { SelectionDraftScope } from './draft/scope'
import { ReceiverExperienceObservability } from './experience/observability'
import type { ReceiverSnapshot, ShareIdentity } from './model'
import { ReceiveOperationTransitions } from './operation-ownership/transitions'
import { presentReceiveOutput } from './output'
import { PreviewController } from './preview-controller'

export { formatPublicError } from '../application/browser-share/capability-lifecycle'
export type { DiagnosticFormatter, SecurityMilestone } from '../application/browser-share/capability-lifecycle'
export { captureLocation } from './capability/location'
export type { CapturedLocation, LocationCaptureOptions } from './capability/location'
export type { ReceiverControllerOptions } from './controller/contracts'
export type { AsyncActionCallbacks, ChooseArtifactIntent, PreviewDownloadResult, ReplacementActionCallbacks } from './controller/failures'
export type { ReceiverTraceEvent } from '../application/receive/contracts'

export class ReceiverController {
  readonly presentation: ReceiverProjections
  readonly #receive: ReceiveCoordinator
  readonly #capabilityLifecycle: CapabilityInputLifecycle
  readonly #capabilityIntake: ReceiverControllerOptions['capabilityIntake']
  readonly #preferences: ReceiverControllerOptions['preferences']
  readonly #listeners = new Set<() => void>()
  readonly #experienceTrace: ReceiverExperienceObservability
  readonly #previews: PreviewController
  readonly #browse: BrowserNavigationCoordinator
  readonly #joinedObservation: JoinedShareObservation
  readonly #shareReport: ShareReportScope
  readonly #joins: ShareJoinCoordinator
  readonly #unsubscribeReceive: () => void
  readonly #unsubscribePreferences: (() => void) | undefined
  readonly #operationTransitions: ReceiveOperationTransitions
  readonly #draftScope = new SelectionDraftScope()
  #snapshot: ReceiverSnapshot
  #pageUrl = ''
  #joined: JoinedBrowserShare | undefined
  #disposed = false

  constructor(
    gateway: BrowserReceiverGateway,
    options: ReceiverControllerOptions = {},
  ) {
    this.#preferences = options.preferences
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
      isJoining: () => this.#joins.isJoining,
      onConnection: connection => {
        this.#receive.observeConnection(terminalConnectionBlock(connection))
        this.#publish({ ...this.#snapshot, connection })
      },
      onPathActivity: pathActivity => this.#publish({ ...this.#snapshot, pathActivity }),
      onCatalogScanProgress: (joined, progress) => this.#browse.catalogScanProgress(joined, progress),
      onProtocolGeneration: joined => this.#beginSelectionProjection(joined),
    })
    this.#shareReport = new ShareReportScope(() => {
      if (!this.#disposed) this.#publish(this.#snapshot)
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
    this.#joins = new ShareJoinCoordinator({
      gateway,
      capabilityLifecycle: this.#capabilityLifecycle,
      receive: this.#receive,
      browse: this.#browse,
      draftScope: this.#draftScope,
      joinedObservation: this.#joinedObservation,
      shareReport: this.#shareReport,
      previews: this.#previews,
      cancelPendingReplacement: () => this.#operationTransitions.cancelPendingReplacement(),
      currentJoinedShare: () => this.#joined,
      setJoinedShare: joined => { this.#joined = joined },
      isDisposed: () => this.#disposed,
      snapshot: () => this.#snapshot,
      setSnapshot: snapshot => { this.#snapshot = snapshot },
      publish: snapshot => this.#publish(snapshot),
      publicError: error => this.#publicError(error),
      onPageUrl: pageUrl => { this.#pageUrl = pageUrl },
      recordIntent: action => this.recordExperienceIntent(action),
      requestReconnect: () => this.requestReconnect(),
      beginSelectionProjection: joined => this.#beginSelectionProjection(joined),
      openAutomaticPhotoPreview: (joined, share, page) => this.#openAutomaticPhotoPreview(joined, share, page),
      ...(options.recordFailure === undefined ? {} : { recordFailure: options.recordFailure }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
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

  refreshDownloads(): Promise<void> { return this.#receive.refreshRetained() }

  openLocation(captured: CapturedLocation): void {
    if (this.#disposed) return
    try { this.#capabilityIntake?.accept(captured) } catch {
      // Optional presentation intent must not prevent a capability from joining.
    }
    if (captured.capabilityInput !== null) this.#joins.open(captured.capabilityInput, captured.pageUrl).catch(() => undefined)
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
    if (!this.#joins.requestReconnect()) this.#joined?.requestReconnect()
  }

  continueJoinWaiting(): void {
    if (this.#disposed) return
    this.recordExperienceIntent('continue-connection-wait')
    this.#joins.continueWaiting()
  }

  cancelJoin(): void {
    this.#joins.cancel()
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
    this.#operationTransitions.cancelPendingReplacement()
    this.#clearReplacementSelectedStatus()
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
    if (this.#receive.startBlockedReason() === null) {
      this.#operationTransitions.cancelPendingReplacement()
      this.#clearReplacementSelectedStatus()
      this.#previews.yieldToReceiving()
    }
    const outcome = this.#receive.startSingleFileDownload(entry, page.directory, {
      display: Object.freeze({ objectLabel: entry.name, createdAtMilliseconds: Date.now() }),
    })
    const failure = outcomeFailure(outcome, error => this.#publicError(error))
    if (failure !== null) this.#publish({ ...this.#snapshot, error: failure })
    return outcome
  }

  downloadPreviewedFile(callbacks?: AsyncActionCallbacks): PreviewDownloadResult {
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
    if (this.#receive.startBlockedReason() === null) {
      this.#operationTransitions.cancelPendingReplacement()
      this.#clearReplacementSelectedStatus()
    }
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
        const handled =
          current.state !== 'idle' &&
          current.fileId === fileId &&
          callbacks?.onAsyncFailure !== undefined &&
          callbacks.onAsyncFailure(mapReceiverFailure(error, err => this.#publicError(err)))
        if (!handled) {
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

  performLifecycleAction(action: ReceiveAction, callbacks?: AsyncActionCallbacks): void {
    this.#operationTransitions.performLifecycleAction(action, callbacks)
  }

  get canRetainCurrentOperation(): boolean { return this.#receive.canRetainCurrent }

  retainCurrentOperation(): Promise<boolean> { return this.#operationTransitions.retainCurrentOperation() }

  retainedActionAdmission(task: RetainedTaskSnapshot, action: ReceiveAction) {
    return this.#receive.retainedActionAdmission(task, action)
  }

  performRetainedAction(task: RetainedTaskSnapshot, action: ReceiveAction, callbacks?: AsyncActionCallbacks): void {
    this.#operationTransitions.performRetainedAction(task, action, callbacks)
  }

  replacementAdmission(task: RetainedTaskSnapshot) {
    const admission = this.#receive.replacementAdmission(task)
    if (admission.allowed && this.#snapshot.share === null) {
      return blockedAdmission('matchingShare')
    }
    return admission
  }

  prepareReplacementDownload(
    task: RetainedTaskSnapshot,
    failure: SourceRevisionFailure,
    callbacks?: ReplacementActionCallbacks,
  ): void {
    this.#operationTransitions.prepareReplacementDownload(task, failure, callbacks)
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
    this.#joins.dispose()
    this.#capabilityLifecycle.clear()
    this.#operationTransitions.cancelPendingReplacement()
    this.#browse.cancel(new DOMException('Receiver disposed', 'AbortError'))
    this.#joinedObservation.close()
    this.#shareReport.close()
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

  #openAutomaticPhotoPreview(joined: JoinedBrowserShare, share: ShareIdentity | null, page: BrowsePage): void {
    if (share?.kind === 'photo' && page.directory.idText === joined.descriptor.syntheticRootId && !this.#receive.active) {
      const entry = page.entries[0]
      if (entry?.kind === 'file') this.#previews.openAutomaticPhoto(joined, entry, page.directory)
    }
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
    this.#openAutomaticPhotoPreview(joined, share, page)
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
      ...(this.#snapshot.status === 'replacement-selected' ? { status: null } : {}),
      draft: projectDraft(this.#snapshot.draft.mode, directory, joined.selection, this.#snapshot.share),
    })
    this.#browse.publishPage(page)
    this.#beginSelectionProjection(joined)
  }

  #clearReplacementSelectedStatus(): void {
    if (this.#snapshot.status === 'replacement-selected') {
      this.#publish({ ...this.#snapshot, status: null })
    }
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
    const localChoiceId = this.#singlePhotoLocalChoiceId(snapshot)
    const startAdmission = resolveStartAdmission(
      this.#snapshot.startAdmission,
      this.#receive.startBlockedReason(),
      this.#receive.canReleaseCurrent,
      localChoiceId,
      localChoiceId === null ? null : this.#receive.localStartBlockedReason(),
    )
    this.#snapshot = Object.freeze({
      ...snapshot,
      reportUrl: this.#shareReport.url,
      activeReceiveOperationId: this.#receive.getSnapshot().activeTaskId,
      startAdmission,
    })
    if (this.#disposed) return
    const composition = this.presentation.stage(this.#snapshot)
    this.#experienceTrace.publish(this.#snapshot, composition)
    this.presentation.publish()
    for (const listener of this.#listeners) listener()
  }
}
