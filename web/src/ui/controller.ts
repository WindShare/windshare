import { BrowserReceiveApplication } from '../application/receive/application'
import { ReceiveSourceInvalidatedError } from '../application/receive/errors'
import { SOURCE_INVALIDATED_DESCRIPTION } from './lifecycle-presentation'
import { initialReceiverSnapshot, joiningReceiverSnapshot, receiverDiagnosticSnapshot, receiverProgressSnapshot } from './controller-state'
import { ReceiverExperienceObservability } from './experience/observability'
import { SelectionPolicy } from '../catalog/selection'
import { projectDraft, scopeSelection } from './draft/model'
import { SelectionDraftScope } from './draft/scope'
import type { BrowsePage } from '../application/browser-share/gateway'
import {
  createSelectionSpec,
  selectionRulesSpecFromPolicy,
} from '../transfer/intent'
import type { ArtifactChoiceID } from '../transfer/intent'
import type { TransferProgress } from '../transfer/job'
import { BrowserReceiverGateway, type JoinedBrowserShare } from '../application/browser-share/gateway'
import { EMPTY_PROGRESS, type ReceiverSnapshot } from './model'
import { type ReceiverDiagnosticSnapshot } from '../diagnostics/export/context'
import {
  CapabilityInputLifecycle,
  type CapabilityJoinLease,
} from '../application/browser-share/capability-lifecycle'
import { capabilityInputFingerprint } from '../application/browser-share/capability-input'
import { CapabilityNavigation } from '../application/browser-share/capability-navigation'
import type { CapturedLocation } from './capability/location'
import { type LifecycleUserAction } from '../application/receive/actions'
import {
  OutputPresentationController,
} from './output'
import { PreviewController } from './preview-controller'
import {
  type RetainedReceiveAction,
  type RetainedReceiveOperation,
} from '../application/receive/contracts'
import type { PresentationAttempt } from '../application/receive/observation/attempt'
import { type ReceiverControllerOptions } from './controller/contracts'
import { StaleReceiveBoundaryError } from '../application/receive/observation/contracts'
import { BrowserNavigationCoordinator } from './controller/navigation'
import { JoinedShareObservation } from './controller/joined-observation'
import { InitialJoinControl } from '../receiver/initial-join'
import { initialJoinStatus } from './experience/share-presentation'

export { formatPublicError } from '../application/browser-share/capability-lifecycle'
export type { DiagnosticFormatter, SecurityMilestone } from '../application/browser-share/capability-lifecycle'
export { captureLocation } from './capability/location'
export type { CapturedLocation, LocationCaptureOptions } from './capability/location'

export type { ReceiverControllerOptions } from './controller/contracts'
export type { ReceiverTraceEvent, RetainedInventoryTraceEvent } from '../application/receive/observation/contracts'

import { ReceiveOperationTransitions } from './operation-ownership/transitions'
import type { SourceRevisionFailure } from '../transfer/lifecycle/source-revision'

export class ReceiverController {
  readonly #gateway: BrowserReceiverGateway
  readonly #receive: BrowserReceiveApplication<JoinedBrowserShare>
  readonly #capabilityLifecycle: CapabilityInputLifecycle
  readonly #capabilityIntake: ReceiverControllerOptions['capabilityIntake']
  readonly #capabilityNavigation = new CapabilityNavigation(() =>
    this.#joinNavigation !== undefined || this.#snapshot.connection.kind === 'connected' ||
    this.#snapshot.connection.kind === 'reconnecting')
  readonly #listeners = new Set<() => void>()
  readonly #experienceTrace: ReceiverExperienceObservability
  readonly #outputs: OutputPresentationController
  readonly #previews: PreviewController
  readonly #browse: BrowserNavigationCoordinator
  readonly #joinedObservation: JoinedShareObservation
  readonly #unsubscribeOutput: () => void
  #snapshot: ReceiverSnapshot
  #diagnosticGeneration = 0n
  #pageUrl = ''
  #joined: JoinedBrowserShare | undefined
  readonly #draftScope = new SelectionDraftScope()
  #joinNavigation: AbortController | undefined
  #joinRecovery: InitialJoinControl | undefined
  #disposed = false
  readonly #operationTransitions: ReceiveOperationTransitions

  constructor(
    gateway: BrowserReceiverGateway,
    options: ReceiverControllerOptions,
  ) {
    this.#gateway = gateway
    this.#experienceTrace = new ReceiverExperienceObservability(options.trace)
    this.#capabilityLifecycle = new CapabilityInputLifecycle(options)
    this.#capabilityIntake = options.capabilityIntake
    this.#receive = new BrowserReceiveApplication({
      receive: options.receive,
      currentJoinedShare: () => this.#joined,
      isDisposed: () => this.#disposed,
      onProgress: progress => this.#transferProgress(progress),
      onRetainedChanged: retained => this.#publish({ ...this.#snapshot, retained }),
      onChanged: () => { if (!this.#disposed) this.#publish(this.#snapshot) },
      onTaskAdopted: display => {
        this.#snapshot = Object.freeze({ ...this.#snapshot, progress: EMPTY_PROGRESS, taskDisplay: display ?? null })
      },
      onTaskCleared: () => this.#publish({ ...this.#snapshot, progress: EMPTY_PROGRESS, taskDisplay: null }),
      onBrowsingReady: joined => {
        this.#beginSelectionProjection(joined, 'observation-replacement')
        this.#publish(this.#snapshot)
      },
      onActionError: error => this.#publishActionError(error),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#outputs = new OutputPresentationController(this.#receive.observations)
    this.#browse = new BrowserNavigationCoordinator({
      onPageCommitted: (page) => this.#pageCommitted(page),
      currentJoinedShare: () => this.#joined,
      isDisposed: () => this.#disposed,
      snapshot: () => this.#snapshot,
      publish: (snapshot) => this.#publish(snapshot),
      publicError: (error) => this.#publicError(error),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#joinedObservation = new JoinedShareObservation({
      ownsJoinedShare: joined => !this.#disposed && this.#joined === joined,
      isJoining: () => this.#joinNavigation !== undefined,
      onConnection: connection => {
        this.#receive.tasks.observeConnection(connection)
        this.#publish({ ...this.#snapshot, connection })
      },
      onPathActivity: pathActivity => this.#publish({ ...this.#snapshot, pathActivity }),
      onCatalogScanProgress: (joined, progress) => this.#browse.catalogScanProgress(joined, progress),
      onProtocolGeneration: joined => this.#beginSelectionProjection(joined, 'observation-replacement'),
    })
    this.#snapshot = initialReceiverSnapshot()
    this.#unsubscribeOutput = this.#outputs.subscribe(() => {
      if (!this.#disposed) this.#publish({ ...this.#snapshot, output: this.#outputs.getSnapshot() })
    })
    this.#previews = new PreviewController({
      snapshot: () => this.#snapshot,
      publish: (snapshot) => this.#publish(snapshot),
      publicError: (error) => this.#publicError(error),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#operationTransitions = new ReceiveOperationTransitions({
      runtime: this.#receive.tasks, snapshot: () => this.#snapshot,
      publish: snapshot => this.#publish(snapshot),
      recordIntent: action => this.recordExperienceIntent(action),
      beginProjection: joined => this.#beginSelectionProjection(joined),
      browse: this.#browse, previews: this.#previews, experienceTrace: this.#experienceTrace,
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

  readonly getDiagnosticSnapshot = (): ReceiverDiagnosticSnapshot =>
    receiverDiagnosticSnapshot(this.#snapshot, this.#diagnosticGeneration)

  initialize(captured: CapturedLocation): void {
    if (this.#disposed) return
    this.#pageUrl = captured.pageUrl
    this.openLocation(captured)
    this.#receive.loadRetained().catch(() => undefined)
  }

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
    this.#receive.cancelJoin()
    this.#publish({ ...this.#snapshot, phase: 'awaiting-key', connection: { kind: 'idle' },
      status: 'Connection waiting cancelled.', error: null })
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
    if (this.#disposed || joined === undefined || page === undefined) return
    const directory = this.#draftScope.directory
    if (directory === undefined) return
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
    const entry = this.#browse.entry(id)
    if (joined === undefined || entry?.kind !== 'file') return
    this.#previews.open(joined, entry)
  }

  cancelPreview(): void {
    this.#previews.cancel()
  }

  seekPreview(seconds: number): void {
    this.#previews.seek(seconds)
  }

  previewMediaPresented(presentationId: number): void {
    this.#previews.mediaPresented(presentationId)
  }

  previewMediaFailed(presentationId: number): void {
    this.#previews.mediaFailed(presentationId)
  }

  chooseArtifact(choiceId: ArtifactChoiceID, recoveryPreference: import('../output/browser-delivery/model').BrowserRecoveryPreference = 'automatic'): void {
    this.recordExperienceIntent('choose-saving-outcome')
    if (this.#receive.tasks.startBlockedReason() !== null) return
    this.#previews.yieldToReceiving()
    this.#receive.chooseArtifact(choiceId, Object.freeze({
      objectLabel: this.#snapshot.draft.label, createdAtMilliseconds: Date.now(),
    }), recoveryPreference)
  }

  cancelPreparing(): void {
    this.recordExperienceIntent('cancel-preparing')
    this.#receive.cancelPreparing(new DOMException('Output preparation cancelled', 'AbortError'))
    if (this.#joined !== undefined) this.#beginSelectionProjection(this.#joined, 'observation-replacement')
  }

  retryOutputConfirmation(): void {
    this.#receive.retryOutputConfirmation()
  }

  activeLifecycleActionAdmission(action: LifecycleUserAction) {
    return this.#receive.tasks.activeLifecycleActionAdmission(action)
  }

  performLifecycleAction(action: LifecycleUserAction): void {
    this.#operationTransitions.performLifecycleAction(action)
  }

  get canRetainCurrentOperation(): boolean { return this.#receive.tasks.canRetainCurrentOperation }

  retainCurrentOperation(): Promise<boolean> { return this.#operationTransitions.retainCurrentOperation() }

  retainedActionAdmission(operation: RetainedReceiveOperation, action: RetainedReceiveAction) {
    return this.#receive.tasks.retainedActionAdmission(operation, action)
  }

  performRetainedAction(operation: RetainedReceiveOperation, action: RetainedReceiveAction): void {
    this.#operationTransitions.performRetainedAction(operation, action)
  }

  prepareReplacementDownload(operation: RetainedReceiveOperation, failure: SourceRevisionFailure): void {
    this.#operationTransitions.prepareReplacementDownload(operation, failure)
  }

  catchUpStoppedCompatibleNames(): void { this.#receive.tasks.catchUpStoppedCompatibleNames() }

  startNewReceiveOperation(): void { this.#operationTransitions.startNewReceiveOperation() }

  async dispose(): Promise<void> {
    if (this.#disposed) return
    this.#disposed = true
    this.#capabilityNavigation.clear()
    this.#capabilityLifecycle.clear()
    this.#joinNavigation?.abort(new DOMException('Receiver disposed', 'AbortError'))
    this.#browse.cancel(new DOMException('Receiver disposed', 'AbortError'))
    this.#joinedObservation.close()
    const detached = this.#receive.close(new DOMException('Receiver disposed', 'AbortError'))
    this.#unsubscribeOutput()
    this.#outputs.close()
    await Promise.allSettled([
      detached,
      this.#previews.close(),
      ...(this.#joined === undefined ? [] : [this.#joined.close()]),
    ])
    this.#listeners.clear()
  }

  #join(input: string, pageUrl = this.#pageUrl): Promise<void> {
    const lease = this.#capabilityLifecycle.beginJoin(input, pageUrl)
    return this.#capabilityNavigation.open(capabilityInputFingerprint(input, pageUrl), async () => {
      if (this.#disposed) return 'blocked'
      const reason = this.#receive.tasks.shareNavigationBlockedReason()
      if (reason !== null) {
        this.#publishActionError(new Error(reason))
        return 'blocked'
      }
      this.#pageUrl = pageUrl
      return await this.#joinOwned(lease, this.#receive.diagnostics.open('join'), pageUrl) ? 'joined' : 'failed'
    }).then(decision => {
      if (this.#disposed) return
      this.recordExperienceIntent(`share-navigation-${decision}`)
      if (decision === 'reused' && this.#snapshot.connection.kind === 'reconnecting') this.requestReconnect()
    }).finally(() => lease.release())
  }

  async #joinOwned(
    lease: CapabilityJoinLease,
    attempt: PresentationAttempt,
    pageUrl: string,
  ): Promise<boolean> {
    let navigation: AbortController | undefined
    let previous: JoinedBrowserShare | undefined
    let candidate: JoinedBrowserShare | undefined
    let joinedReplacementInstalled = false
    this.#receive.diagnostics.trace(() => Object.freeze({
      name: 'join_transition',
      transition: 'started',
    }))
    try {
      // Claim navigation before awaiting cleanup; a slow old preview must never
      // abort a newer link or start dialing after the document has been disposed.
      this.#joinNavigation?.abort(new DOMException('A newer join replaced this one', 'AbortError'))
      this.#browse.cancel(new DOMException('A newer join replaced this one', 'AbortError'))
      navigation = new AbortController()
      this.#joinNavigation = navigation
      previous = this.#joined
      lease.activate()
      const detached = this.#receive.suspendForJoin(new StaleReceiveBoundaryError())
      await Promise.all([detached, this.#previews.close()])
      navigation.signal.throwIfAborted()
      this.#publish(joiningReceiverSnapshot(this.#snapshot))
      this.#joinedObservation.suspendForJoin()
      navigation.signal.throwIfAborted()
      const activeNavigation = navigation
      const recovery = new InitialJoinControl()
      this.#joinRecovery = recovery
      const joined = await lease.handoff((ownedInput) =>
        this.#gateway.join(ownedInput, pageUrl, activeNavigation.signal, {
          control: recovery,
          onState: join => {
            if (!this.#joinReplacementIsCurrent(activeNavigation)) return
            this.#publish({ ...this.#snapshot, connection: { kind: 'idle', join },
              status: initialJoinStatus(join) })
          },
        }))
      candidate = joined
      if (!this.#joinReplacementIsCurrent(navigation)) {
        this.#receive.diagnostics.exclude(attempt, 'join', 'stale_replacement')
        this.#receive.diagnostics.trace(() => Object.freeze({
          name: 'join_transition',
          transition: 'stale_replacement',
        }))
        return false
      }
      const frozenSelection = joined.selection.snapshot()
      const selection = await createSelectionSpec({
        shareInstance: joined.descriptor.shareInstanceId,
        syntheticRoot: joined.descriptor.syntheticRootId,
        rules: selectionRulesSpecFromPolicy(frozenSelection),
      })
      if (!this.#joinReplacementIsCurrent(navigation)) {
        return false
      }
      this.#joined = joined
      this.#draftScope.reset()
      this.#joinRecovery = undefined
      joinedReplacementInstalled = true
      this.#receive.completeJoin(joined, selection)
      await previous?.close().catch(() => undefined)
      if (!this.#joinReplacementIsCurrent(navigation)) return false
      this.#receive.diagnostics.exclude(attempt, 'join', 'success')
      this.#receive.diagnostics.trace(() => Object.freeze({
        name: 'join_transition',
        transition: 'joined',
      }))
      this.#joinedObservation.observe(joined)
      const root = joined.rootDirectory()
      this.#browse.clearCatalog()
      await this.#browse.loadPage(root, 0, Object.freeze([root]))
      if (this.#joined === joined && this.#browse.pageMatches(root)) {
        const share = this.#snapshot.share
        if (share?.kind === 'browser' && share.singleFolder) {
          this.#browse.openDirectory(share.homeDirectoryId)
        }
      }
      return this.#joinReplacementIsCurrent(navigation)
    } catch (error) {
      this.#handleJoinFailure(
        error,
        lease,
        attempt,
        navigation,
        previous,
        joinedReplacementInstalled,
      )
      return false
    } finally {
      if (this.#joinNavigation === navigation) {
        this.#joinNavigation = undefined
        this.#joinRecovery = undefined
      }
      lease.release()
      if (!attempt.decisionSettled) {
        this.#receive.diagnostics.exclude(attempt, 'join', 'stale_replacement')
      }
      attempt.close()
      if (candidate !== undefined && !joinedReplacementInstalled) await candidate.close().catch(() => undefined)
    }
  }

  #joinReplacementIsCurrent(navigation: AbortController): boolean {
    return this.#joinNavigation === navigation && !navigation.signal.aborted && !this.#disposed
  }

  #handleJoinFailure(
    error: unknown,
    lease: CapabilityJoinLease,
    attempt: PresentationAttempt,
    navigation: AbortController | undefined,
    previous: JoinedBrowserShare | undefined,
    joinedReplacementInstalled: boolean,
  ): void {
    if (navigation === undefined || this.#joinNavigation !== navigation || navigation.signal.aborted) {
      this.#receive.diagnostics.exclude(attempt, 'join', 'stale_replacement')
      return
    }
    if (!joinedReplacementInstalled) {
      this.#receive.cancelJoin()
      if (previous !== undefined && this.#joined === previous) {
        this.#joinedObservation.observe(previous)
      }
    }
    this.#receive.diagnostics.fail(attempt, 'join', error, 'join')
    this.#receive.diagnostics.trace(() => Object.freeze({
      name: 'join_transition',
      transition: 'failed',
    }))
    this.#fail(error, lease)
  }

  #beginSelectionProjection(
    joined: JoinedBrowserShare,
    replacement: 'selection-change' | 'observation-replacement' = 'selection-change',
  ): void {
    if (this.#draftScope.directory === undefined) return
    this.#receive.beginProjection(joined, replacement)
  }

  #transferProgress(progress: TransferProgress): void {
    if (progress.transferJobId.length === 0) return
    const snapshot = receiverProgressSnapshot(progress)
    this.#publish({ ...this.#snapshot, progress: snapshot })
  }

  #pageCommitted(page: BrowsePage): void {
    const joined = this.#joined
    if (joined === undefined) return
    const { share, draft, transition } = this.#draftScope.commitPage(
      joined, page, this.#snapshot.draft.mode, this.#snapshot.share)
    this.#snapshot = Object.freeze({ ...this.#snapshot, share, draft, status: '' })
    if (transition !== 'page-updated') {
      this.#beginSelectionProjection(joined, transition === 'scope-initialized' ? 'observation-replacement' : 'selection-change')
    }
    if (share?.kind === 'photo' && page.directory.idText === joined.descriptor.syntheticRootId &&
        this.#receive.canPreviewAutomatically) {
      const entry = page.entries[0]
      if (entry?.kind === 'file') this.#previews.openAutomaticPhoto(joined, entry)
    }
  }

  #refreshDraft(joined: JoinedBrowserShare, page: BrowsePage): void {
    const directory = this.#draftScope.directory
    if (directory === undefined) return
    this.#snapshot = Object.freeze({ ...this.#snapshot,
      draft: projectDraft(this.#snapshot.draft.mode, directory, joined.selection, this.#snapshot.share) })
    this.#browse.publishPage(page)
    this.#beginSelectionProjection(joined)
  }

  #publishActionError(error: unknown): void {
    this.#publish({
      ...this.#snapshot,
      error: error instanceof ReceiveSourceInvalidatedError ? SOURCE_INVALIDATED_DESCRIPTION : this.#publicError(error),
    })
  }

  #fail(error: unknown, lease?: CapabilityJoinLease): void {
    this.#publish({
      ...this.#snapshot,
      phase: 'failed',
      status: 'The receiver stopped safely.',
      error: lease?.publicError(error) ?? this.#publicError(error),
    })
  }

  #publicError(error: unknown): string {
    return this.#capabilityLifecycle.publicError(error)
  }

  #publish(snapshot: ReceiverSnapshot): void {
    this.#diagnosticGeneration += 1n
    const reason = this.#receive.tasks.startBlockedReason()
    this.#snapshot = Object.freeze({ ...snapshot,
      activeReceiveOperationId: this.#receive.operationId,
      startAdmission: Object.freeze({ allowed: reason === null, reason,
        canReleaseCurrent: this.#receive.tasks.canReleaseCurrent }) })
    this.#experienceTrace.publish(this.#snapshot)
    for (const listener of this.#listeners) listener()
  }
}
