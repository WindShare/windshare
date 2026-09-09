import { initialReceiverSnapshot, receiverDiagnosticSnapshot, receiverProgressSnapshot } from './v2-controller-state'
import { ReceiverExperienceObservability } from './experience/observability'
import { V2SelectionPolicy } from '../catalog/v2-selection'
import { EMPTY_SELECTION_DRAFT, projectDraft, scopeSelection, shareIdentityFromRoot } from './draft/model'
import type { V2BrowsePage } from './v2-gateway'
import {
  createSelectionSpec,
  selectionRulesSpecFromPolicy,
  validateReceiveIntent,
} from '../transfer/intent'
import type { ArtifactChoiceID } from '../transfer/intent'
import type { TransferProgress } from '../transfer/v2-job'
import {
  V2BrowserReceiverGateway,
  v2SelectionPolicyFromIntent,
  type V2JoinedBrowserShare,
} from './v2-gateway'
import {
  EMPTY_V2_PROGRESS,
  EMPTY_V2_PREVIEW,
  type V2ReceiverDiagnosticSnapshot,
  type V2ReceiverSnapshot,
} from './v2-model'
import {
  V2CapabilityInputLifecycle,
  type V2CapabilityJoinLease,
  type V2CapturedLocation,
} from './v2-capability-lifecycle'
import {
  type LifecycleUserAction,
} from './v2-lifecycle-presentation'
import {
  V2OutputPresentationController,
} from './v2-output'
import { V2PreviewController } from './v2-preview-controller'
import {
  type V2ReceiveCompositionPort,
  type V2RetainedReceiveAction,
  type V2RetainedReceiveOperation,
} from './v2-receive-runtime'
import { ActiveReceiveCoordinator } from './controller/active-receive'
import { V2ControllerObservability } from './controller/controller-observability'
import type { V2PresentationAttempt } from './controller/presentation-attempt'
import {
  StaleReceiveBoundaryError,
  type V2ReceiverControllerOptions,
} from './controller/contracts'
import {
  V2AuthorityActivationCoordinator,
} from './controller/authority-activation'
import { SelectionProjectionRuntime } from './controller/projection-observation'
import {
  RetainedInventoryCoordinator,
  type RetainedContinuationAdoption,
} from './controller/retained-inventory'
import { BrowserNavigationCoordinator } from './controller/navigation'

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

export type {
  V2ReceiverControllerOptions,
  V2ReceiverTraceEvent,
  V2RetainedInventoryTraceEvent,
} from './controller/contracts'

import { ReceiveOperationTransitions } from './operation-ownership/transitions'
import type { SourceRevisionFailure } from '../output/resume/source-revision-failures'

export class V2ReceiverController {
  readonly #gateway: V2BrowserReceiverGateway
  readonly #receive: V2ReceiveCompositionPort
  readonly #capabilityLifecycle: V2CapabilityInputLifecycle
  readonly #listeners = new Set<() => void>()
  readonly #observability: V2ControllerObservability
  readonly #experienceTrace: ReceiverExperienceObservability
  readonly #outputs: V2OutputPresentationController
  readonly #projectionObservation: SelectionProjectionRuntime
  readonly #previews: V2PreviewController
  readonly #activeReceive: ActiveReceiveCoordinator
  readonly #authority: V2AuthorityActivationCoordinator
  readonly #retained: RetainedInventoryCoordinator
  readonly #browse: BrowserNavigationCoordinator
  readonly #unsubscribeOutput: () => void
  readonly #unsubscribeAuthority: () => void
  #snapshot: V2ReceiverSnapshot
  #diagnosticGeneration = 0n
  #pageUrl = ''
  #joined: V2JoinedBrowserShare | undefined
  #joinNavigation: AbortController | undefined
  #unsubscribeScanProgress: (() => void) | undefined
  #unsubscribeProtocolGeneration: (() => void) | undefined
  #unsubscribePathActivity: (() => void) | undefined
  #unsubscribeConnection: (() => void) | undefined
  #disposed = false
  readonly #operationTransitions: ReceiveOperationTransitions

  constructor(
    gateway: V2BrowserReceiverGateway,
    options: V2ReceiverControllerOptions,
  ) {
    this.#gateway = gateway
    this.#receive = options.receive
    this.#experienceTrace = new ReceiverExperienceObservability(options.trace)
    this.#observability = new V2ControllerObservability({
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#capabilityLifecycle = new V2CapabilityInputLifecycle(options)
    this.#outputs = new V2OutputPresentationController()
    this.#activeReceive = new ActiveReceiveCoordinator({
      outputs: this.#outputs,
      ownsJoinedShare: (joined) => !this.#disposed && this.#joined === joined,
      onProgress: (progress) => this.#transferProgress(progress),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
      onActionError: (error) => this.#publishActionError(error),
      onFailure: (error) => this.#publishActionError(error),
      onOwnershipReleased: () => {
        if (this.#disposed) return
        if (this.#joined !== undefined) this.#beginSelectionProjection(this.#joined)
        this.#publish(this.#snapshot)
        this.#retained.load().catch(() => undefined)
      },
      onRetainedFileFailure: () => {
        this.#resetReceiveOwnership(new DOMException('Retained file failures require a recovery choice', 'AbortError'))
          .then(() => this.#retained.load()).catch(error => this.#publishActionError(error))
      },
    })
    this.#authority = new V2AuthorityActivationCoordinator({
      receive: this.#receive,
      activeReceive: this.#activeReceive,
      observability: this.#observability,
      currentProjection: () => this.#projectionObservation.current,
      currentJoinedShare: () => this.#joined,
      choiceBlocked: () => this.#operationTransitions.startBlockedReason() !== null,
      retryProjection: (projection) => {
        this.#projectionObservation.retry(projection).catch(() => undefined)
      },
      publishProjection: ({ observationRevision, state, offers }) => {
        this.#outputs.updateProjection(observationRevision, state, offers)
      },
      adoptReceiveIntent: (choice, intent, runtime, commitOwnership) =>
        this.#outputs.adoptReceiveIntentAtomically(
          choice,
          intent,
          () => {
            commitOwnership()
            this.#snapshot = Object.freeze({ ...this.#snapshot,
              progress: EMPTY_V2_PROGRESS, taskDisplay: runtime.display ?? null })
          },
          runtime.lifecycle,
          runtime.initialWorkspaceUsage,
          runtime.activeControls,
        ),
      refreshRetainedInventory: () => {
        this.#retained.load().catch(() => undefined)
      },
      publishActionError: (error) => this.#publishActionError(error),
    })
    this.#projectionObservation = new SelectionProjectionRuntime({
      receive: this.#receive,
      authority: this.#authority,
      observability: this.#observability,
      currentJoinedShare: () => this.#joined,
      isDisposed: () => this.#disposed,
      onFailure: error => this.#publishActionError(error),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    this.#retained = new RetainedInventoryCoordinator({
      receive: this.#receive,
      isDisposed: () => this.#disposed,
      currentJoinedShare: () => this.#joined,
      continuationBlocked: () => this.#activeReceive.active || this.#authority.pending || this.#operationTransitions.pending,
      remoteContinuationUnavailable: () => this.#operationTransitions.remoteContinuationUnavailable(),
      localFinalizationBlocked: () => this.#activeReceive.active || this.#authority.pending || this.#operationTransitions.pending,
      adoptContinuation: (input) => this.#adoptRetainedReceiveContinuation(input),
      ownsRuntime: (runtime) => this.#activeReceive.ownsRuntime(runtime),
      publish: (retained) => this.#publish({ ...this.#snapshot, retained }),
      onActionCompleted: (operationId, action) => this.#retainedActionCompleted(operationId, action),
      ...(options.receive.retained.readRepairSummary === undefined
        ? {}
        : {
            repairSource: {
              readRepairSummary: (operationId: string, signal: AbortSignal) =>
                options.receive.retained.readRepairSummary!(operationId, signal),
            },
          }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      onActionError: (error) => this.#publishActionError(error),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#browse = new BrowserNavigationCoordinator({
      onPageCommitted: (page) => this.#pageCommitted(page),
      currentJoinedShare: () => this.#joined,
      isDisposed: () => this.#disposed,
      snapshot: () => this.#snapshot,
      publish: (snapshot) => this.#publish(snapshot),
      publicError: (error) => this.#publicError(error),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#snapshot = initialReceiverSnapshot()
    this.#unsubscribeOutput = this.#outputs.subscribe(() => {
      if (!this.#disposed) this.#publish({ ...this.#snapshot, output: this.#outputs.getSnapshot() })
    })
    this.#unsubscribeAuthority = this.#authority.subscribe(() => {
      this.#outputs.updateActivation(this.#authority.getSnapshot())
    })
    this.#previews = new V2PreviewController({
      snapshot: () => this.#snapshot,
      publish: (snapshot) => this.#publish(snapshot),
      publicError: (error) => this.#publicError(error),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#operationTransitions = new ReceiveOperationTransitions({
      snapshot: () => this.#snapshot, joined: () => this.#joined, disposed: () => this.#disposed,
      publish: snapshot => this.#publish(snapshot), actionError: error => this.#publishActionError(error),
      recordIntent: action => this.recordExperienceIntent(action),
      resetOwnership: reason => this.#resetReceiveOwnership(reason),
      beginProjection: (joined, reason) => this.#beginSelectionProjection(joined, reason),
      activeReceive: this.#activeReceive, authority: this.#authority, retained: this.#retained,
      outputs: this.#outputs, browse: this.#browse, previews: this.#previews, experienceTrace: this.#experienceTrace,
    })
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  readonly getSnapshot = (): V2ReceiverSnapshot => this.#snapshot

  readonly getOwnershipSnapshot = () => Object.freeze({
    currentPageEntries: this.#browse.entryCount,
    receiveOperationActive: this.#activeReceive.active,
  })

  readonly getDiagnosticSnapshot = (): V2ReceiverDiagnosticSnapshot =>
    receiverDiagnosticSnapshot(this.#snapshot, this.#diagnosticGeneration)

  initialize(captured: V2CapturedLocation): void {
    this.#pageUrl = captured.pageUrl
    this.#capabilityLifecycle.acceptCapturedLocation(captured)
    if (captured.capabilityInput !== null) {
      this.#join(captured.capabilityInput).catch(() => undefined)
    }
    this.#retained.load().catch(() => undefined)
  }

  submitKey(input: string): void {
    if (this.#disposed || input.trim().length === 0) return
    this.#join(input.trim()).catch(() => undefined)
    // The form clears its password field in this same stack before join can reject.
    this.#capabilityLifecycle.notify('key-cleared')
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
    if (this.#snapshot.draft.mode !== 'selection') joined.replaceSelection(new V2SelectionPolicy(false))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'selection' as const } })
    joined.selection.toggle(entry, page.directory.ancestry)
    this.#refreshDraft(joined, page)
  }

  enterSelectionMode(): void {
    this.recordExperienceIntent('enter-selection')
    const joined = this.#joined
    const page = this.#browse.page
    if (this.#disposed || joined === undefined || page === undefined || this.#snapshot.draft.mode === 'selection') return
    joined.replaceSelection(new V2SelectionPolicy(false))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'selection' as const } })
    this.#refreshDraft(joined, page)
  }

  exitSelectionMode(): void {
    this.recordExperienceIntent('exit-selection')
    const joined = this.#joined
    const page = this.#browse.page
    if (this.#disposed || joined === undefined || page === undefined) return
    joined.replaceSelection(scopeSelection(page))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'scope' as const } })
    this.#refreshDraft(joined, page)
  }

  selectPage(): void {
    this.recordExperienceIntent('select-page')
    const joined = this.#joined
    const page = this.#browse.page
    if (this.#disposed || joined === undefined || page === undefined) return
    if (this.#snapshot.draft.mode !== 'selection') joined.replaceSelection(new V2SelectionPolicy(false))
    this.#snapshot = Object.freeze({ ...this.#snapshot, draft: { ...this.#snapshot.draft, mode: 'selection' as const } })
    for (const entry of page.entries) joined.selection.set(entry, page.directory.ancestry, true)
    this.#refreshDraft(joined, page)
  }

  clearSelection(): void {
    this.recordExperienceIntent('clear-selection')
    const joined = this.#joined
    const page = this.#browse.page
    if (this.#disposed || joined === undefined || page === undefined) return
    joined.replaceSelection(new V2SelectionPolicy(false))
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

  chooseArtifact(choiceId: ArtifactChoiceID): void {
    this.recordExperienceIntent('choose-saving-outcome')
    if (this.#operationTransitions.startBlockedReason() !== null) return
    this.#previews.yieldToReceiving()
    this.#authority.choose(choiceId, Object.freeze({
      objectLabel: this.#snapshot.draft.label, createdAtMilliseconds: Date.now(),
    }))
  }

  cancelPreparing(): void {
    this.recordExperienceIntent('cancel-preparing')
    this.#authority.invalidate(new DOMException('Output preparation cancelled', 'AbortError'), 'caller-cancelled')
    this.#outputs.resetDraft()
    if (this.#joined !== undefined) this.#beginSelectionProjection(this.#joined, 'observation-replacement')
  }

  retryOutputConfirmation(): void {
    this.#authority.retry()
  }

  activeLifecycleActionAdmission(action: LifecycleUserAction) {
    return this.#operationTransitions.activeLifecycleActionAdmission(action)
  }

  performLifecycleAction(action: LifecycleUserAction): void {
    this.#operationTransitions.performLifecycleAction(action)
  }

  get canRetainCurrentOperation(): boolean { return this.#operationTransitions.canRetainCurrentOperation }

  retainCurrentOperation(): Promise<boolean> { return this.#operationTransitions.retainCurrentOperation() }

  retainedActionAdmission(operation: V2RetainedReceiveOperation, action: V2RetainedReceiveAction) {
    return this.#operationTransitions.retainedActionAdmission(operation, action)
  }

  performRetainedAction(operation: V2RetainedReceiveOperation, action: V2RetainedReceiveAction): void {
    this.#operationTransitions.performRetainedAction(operation, action)
  }

  prepareReplacementDownload(operation: V2RetainedReceiveOperation, failure: SourceRevisionFailure): void {
    this.#operationTransitions.prepareReplacementDownload(operation, failure)
  }

  catchUpStoppedCompatibleNames(): void { this.#operationTransitions.catchUpStoppedCompatibleNames() }

  startNewReceiveOperation(): void { this.#operationTransitions.startNewReceiveOperation() }

  async dispose(): Promise<void> {
    if (this.#disposed) return
    this.#disposed = true
    this.#capabilityLifecycle.clear()
    this.#joinNavigation?.abort(new DOMException('Receiver disposed', 'AbortError'))
    this.#browse.cancel(new DOMException('Receiver disposed', 'AbortError'))
    this.#retained.close(new DOMException('Receiver disposed', 'AbortError'))
    this.#unsubscribeScanProgress?.()
    this.#unsubscribeScanProgress = undefined
    this.#unsubscribeProtocolGeneration?.()
    this.#unsubscribeProtocolGeneration = undefined
    this.#unsubscribeConnection?.()
    this.#unsubscribeConnection = undefined
    this.#unsubscribePathActivity?.()
    this.#unsubscribePathActivity = undefined
    const detached = this.#resetReceiveOwnership(new DOMException('Receiver disposed', 'AbortError'))
    this.#unsubscribeAuthority()
    this.#unsubscribeOutput()
    this.#outputs.close()
    await Promise.allSettled([
      detached,
      this.#previews.close(),
      ...(this.#joined === undefined ? [] : [this.#joined.close()]),
    ])
    this.#listeners.clear()
  }

  async #adoptRetainedReceiveContinuation(
    input: RetainedContinuationAdoption,
  ): Promise<void> {
    const { joined, runtime } = input
    if (this.#disposed || this.#joined !== joined) throw new StaleReceiveBoundaryError()
    const intent = await validateReceiveIntent(runtime.intent)
    if (this.#disposed || this.#joined !== joined) throw new StaleReceiveBoundaryError()
    const selection = v2SelectionPolicyFromIntent(intent)
    this.#authority.invalidate(new StaleReceiveBoundaryError(), 'caller-cancelled')
    const prepared = this.#activeReceive.prepareAdoption({
      joined,
      selection,
      runtime,
      ...(runtime.repairProjection === undefined
        ? {}
        : { repairProjection: runtime.repairProjection }),
    })
    this.#outputs.adoptRetainedReceiveIntentAtomically(
      intent,
      runtime.lifecycle,
      () => {
        prepared.commit()
        this.#snapshot = Object.freeze({ ...this.#snapshot,
          progress: EMPTY_V2_PROGRESS, taskDisplay: runtime.display ?? input.retained.display ?? null })
      },
      runtime.initialWorkspaceUsage,
      runtime.activeControls,
      input.retained.repairSummary,
    )
    prepared.start()
  }

  #join(input: string): Promise<void> {
    const lease = this.#capabilityLifecycle.beginJoin(input, this.#pageUrl)
    return this.#joinOwned(lease, this.#observability.open('join'))
  }

  async #joinOwned(
    lease: V2CapabilityJoinLease,
    attempt: V2PresentationAttempt,
  ): Promise<void> {
    let navigation: AbortController | undefined
    let previous: V2JoinedBrowserShare | undefined
    let joinedReplacementInstalled = false
    this.#observability.trace(() => Object.freeze({
      name: 'join_transition',
      transition: 'started',
    }))
    try {
      this.#retained.cancelPending(new StaleReceiveBoundaryError())
      this.#activeReceive.reset(new StaleReceiveBoundaryError()).catch(() => undefined)
      if (this.#snapshot.output.receiveIntent !== null) this.#outputs.reset()
      this.#authority.suspendForJoin()
      this.#stopProjectionObservation(new StaleReceiveBoundaryError())
      await this.#previews.close()
      this.#joinNavigation?.abort(new DOMException('A newer join replaced this one', 'AbortError'))
      this.#browse.cancel(new DOMException('A newer join replaced this one', 'AbortError'))
      navigation = new AbortController()
      this.#joinNavigation = navigation
      lease.activate()
      this.#publish({
        ...this.#snapshot,
        phase: 'joining',
        status: 'Authenticating the share descriptor…',
        pathActivity: { directConnected: false, content: 'idle' as const },
        error: null,
        rows: Object.freeze([]),
        connection: { kind: 'idle' as const },
        share: null,
        draft: EMPTY_SELECTION_DRAFT,
        browse: { kind: 'idle' as const, status: '', error: null },
        taskDisplay: null,
        preview: EMPTY_V2_PREVIEW,
        progress: EMPTY_V2_PROGRESS,
      })
      previous = this.#joined
      this.#unsubscribeScanProgress?.()
      this.#unsubscribeScanProgress = undefined
      this.#unsubscribeProtocolGeneration?.()
      this.#unsubscribeProtocolGeneration = undefined
      this.#unsubscribePathActivity?.()
      this.#unsubscribePathActivity = undefined
      navigation.signal.throwIfAborted()
      const activeNavigation = navigation
      const joined = await lease.handoff((ownedInput) =>
        this.#gateway.join(ownedInput, this.#pageUrl, activeNavigation.signal))
      if (!this.#joinReplacementIsCurrent(navigation)) {
        await joined.close()
        this.#observability.exclude(attempt, 'join', 'stale_replacement')
        this.#observability.trace(() => Object.freeze({
          name: 'join_transition',
          transition: 'stale_replacement',
        }))
        return
      }
      const frozenSelection = joined.selection.snapshot()
      const selection = await createSelectionSpec({
        shareInstance: joined.descriptor.shareInstanceId,
        syntheticRoot: joined.descriptor.syntheticRootId,
        rules: selectionRulesSpecFromPolicy(frozenSelection),
      })
      navigation.signal.throwIfAborted()
      if (!this.#joinReplacementIsCurrent(navigation)) {
        await joined.close()
        return
      }
      this.#joined = joined
      joinedReplacementInstalled = true
      this.#authority.completeJoin(joined, selection)
      await previous?.close().catch(() => undefined)
      this.#observability.exclude(attempt, 'join', 'success')
      this.#observability.trace(() => Object.freeze({
        name: 'join_transition',
        transition: 'joined',
      }))
      this.#subscribeJoinedNotifications(joined)
      const root = joined.rootDirectory()
      this.#browse.clearCatalog()
      await this.#browse.loadPage(root, 0, Object.freeze([root]))
      if (this.#joined === joined && this.#browse.pageMatches(root)) {
        const share = this.#snapshot.share
        if (share?.kind === 'browser' && share.singleFolder) {
          this.#browse.openDirectory(share.homeDirectoryId)
        }
      }
    } catch (error) {
      this.#handleJoinFailure(
        error,
        lease,
        attempt,
        navigation,
        previous,
        joinedReplacementInstalled,
      )
    } finally {
      if (this.#joinNavigation === navigation) this.#joinNavigation = undefined
      lease.release()
      if (!attempt.decisionSettled) {
        this.#observability.exclude(attempt, 'join', 'stale_replacement')
      }
      attempt.close()
    }
  }

  #joinReplacementIsCurrent(navigation: AbortController): boolean {
    return this.#joinNavigation === navigation && !navigation.signal.aborted && !this.#disposed
  }

  #handleJoinFailure(
    error: unknown,
    lease: V2CapabilityJoinLease,
    attempt: V2PresentationAttempt,
    navigation: AbortController | undefined,
    previous: V2JoinedBrowserShare | undefined,
    joinedReplacementInstalled: boolean,
  ): void {
    if (navigation === undefined || this.#joinNavigation !== navigation || navigation.signal.aborted) {
      this.#observability.exclude(attempt, 'join', 'stale_replacement')
      return
    }
    if (!joinedReplacementInstalled) {
      this.#authority.cancelJoin()
      if (previous !== undefined && this.#joined === previous) {
        this.#subscribeJoinedNotifications(previous)
      }
    }
    this.#observability.fail(attempt, 'join', error, 'join')
    this.#observability.trace(() => Object.freeze({
      name: 'join_transition',
      transition: 'failed',
    }))
    this.#fail(error, lease)
  }

  #beginSelectionProjection(
    joined: V2JoinedBrowserShare,
    replacement: 'selection-change' | 'observation-replacement' = 'selection-change',
  ): void {
    if (replacement === 'selection-change') this.#outputs.resetDraft()
    this.#projectionObservation.start(joined, replacement)
  }

  #stopProjectionObservation(reason: unknown): void {
    this.#projectionObservation.stop(reason)
  }

  #subscribeJoinedNotifications(joined: V2JoinedBrowserShare): void {
    this.#unsubscribeScanProgress?.()
    this.#unsubscribeProtocolGeneration?.()
    this.#unsubscribeConnection?.()
    this.#unsubscribeConnection = joined.subscribeConnection((connection) => {
      if (!this.#disposed && this.#joined === joined) this.#publish({ ...this.#snapshot, connection })
    })
    this.#unsubscribeScanProgress = joined.subscribeCatalogScanProgress(
      progress => this.#browse.catalogScanProgress(joined, progress),
    )
    this.#unsubscribePathActivity?.()
    this.#unsubscribePathActivity = joined.subscribePathActivity((pathActivity) => {
      if (!this.#disposed && this.#joined === joined) this.#publish({ ...this.#snapshot, pathActivity })
    })
    this.#unsubscribeProtocolGeneration = joined.subscribeProtocolGeneration(() => {
      if (!this.#disposed && this.#joined === joined && this.#joinNavigation === undefined) {
        this.#beginSelectionProjection(joined, 'observation-replacement')
      }
    })
  }

  #resetReceiveOwnership(reason: unknown): Promise<void> {
    this.#stopProjectionObservation(reason)
    this.#authority.invalidate(reason, 'caller-cancelled')
    this.#outputs.reset()
    this.#publish({ ...this.#snapshot, progress: EMPTY_V2_PROGRESS, taskDisplay: null })
    return this.#activeReceive.reset(reason)
  }

  #transferProgress(progress: TransferProgress): void {
    if (progress.transferJobId.length === 0) return
    const snapshot = receiverProgressSnapshot(progress)
    this.#publish({ ...this.#snapshot, progress: snapshot })
  }

  #pageCommitted(page: V2BrowsePage): void {
    const joined = this.#joined
    if (joined === undefined) return
    let share = this.#snapshot.share
    if (page.directory.idText === joined.descriptor.syntheticRootId) {
      share = shareIdentityFromRoot(page, joined.descriptor.shareInstanceId)
    }
    const mode = this.#snapshot.draft.mode
    const scopeChanged = mode === 'scope' && this.#snapshot.breadcrumbs.at(-1)?.id !== page.directory.idText
    if (scopeChanged) joined.replaceSelection(scopeSelection(page))
    this.#snapshot = Object.freeze({ ...this.#snapshot, share, status: '',
      draft: projectDraft(mode, page, joined.selection, share) })
    if (scopeChanged || this.#projectionObservation.current === undefined) {
      this.#beginSelectionProjection(joined,
        this.#joinNavigation === undefined ? 'selection-change' : 'observation-replacement')
    }
    if (share?.kind === 'photo' && page.directory.idText === joined.descriptor.syntheticRootId &&
        !this.#activeReceive.active && !this.#authority.pending && !this.#retained.pending) {
      const entry = page.entries[0]
      if (entry?.kind === 'file') this.#previews.openAutomaticPhoto(joined, entry)
    }
  }

  #refreshDraft(joined: V2JoinedBrowserShare, page: V2BrowsePage): void {
    this.#snapshot = Object.freeze({ ...this.#snapshot,
      draft: projectDraft(this.#snapshot.draft.mode, page, joined.selection, this.#snapshot.share) })
    this.#browse.publishPage(page)
    this.#beginSelectionProjection(joined)
  }

  #publishActionError(error: unknown): void {
    this.#publish({
      ...this.#snapshot,
      error: this.#publicError(error),
    })
  }

  #fail(error: unknown, lease?: V2CapabilityJoinLease): void {
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

  #retainedActionCompleted(operationId: string, action: V2RetainedReceiveAction): void {
    if (this.#disposed || this.#activeReceive.active ||
        this.#snapshot.output.lifecycle?.operationId !== operationId ||
        (action !== 'forget' && action !== 'delete' && action !== 'discard')) return
    // Removing a durable result must also retire its remaining in-page presentation.
    this.#outputs.clearTask()
    this.#publish({ ...this.#snapshot, taskDisplay: null, progress: EMPTY_V2_PROGRESS })
  }

  #publish(snapshot: V2ReceiverSnapshot): void {
    this.#diagnosticGeneration += 1n
    const reason = this.#operationTransitions.startBlockedReason(snapshot)
    this.#snapshot = Object.freeze({ ...snapshot,
      activeReceiveOperationId: this.#activeReceive.operationId,
      startAdmission: Object.freeze({ allowed: reason === null, reason,
        canReleaseCurrent: !this.#operationTransitions.pending && !this.#retained.pending &&
          !this.#authority.pending && this.#activeReceive.canRelease }) })
    this.#experienceTrace.publish(this.#snapshot)
    for (const listener of this.#listeners) listener()
  }
}
