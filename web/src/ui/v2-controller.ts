import type { ArtifactOperation } from '../output/planning'
import {
  createSelectionSpec,
  selectionRulesSpecFromPolicy,
  validateReceiveIntent,
} from '../transfer/intent'
import type { TransferProgress } from '../transfer/v2-job'
import {
  V2BrowserReceiverGateway,
  v2SelectionPolicyFromIntent,
  type V2JoinedBrowserShare,
} from './v2-gateway'
import {
  EMPTY_V2_PROGRESS,
  EMPTY_V2_PREVIEW,
  EMPTY_V2_RETAINED_INVENTORY,
  type V2ReceiverDiagnosticSnapshot,
  type V2ReceiverProgress,
  type V2ReceiverSnapshot,
} from './v2-model'
import {
  V2CapabilityInputLifecycle,
  type V2CapabilityJoinLease,
  type V2CapturedLocation,
} from './v2-capability-lifecycle'
import type { LifecycleUserAction } from './v2-lifecycle-presentation'
import {
  EMPTY_V2_OUTPUT_PRESENTATION,
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

export class V2ReceiverController {
  readonly #gateway: V2BrowserReceiverGateway
  readonly #receive: V2ReceiveCompositionPort
  readonly #capabilityLifecycle: V2CapabilityInputLifecycle
  readonly #listeners = new Set<() => void>()
  readonly #observability: V2ControllerObservability
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
  #disposed = false

  constructor(
    gateway: V2BrowserReceiverGateway,
    options: V2ReceiverControllerOptions,
  ) {
    this.#gateway = gateway
    this.#receive = options.receive
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
      onFailure: (error) => this.#fail(error),
    })
    this.#authority = new V2AuthorityActivationCoordinator({
      receive: this.#receive,
      activeReceive: this.#activeReceive,
      observability: this.#observability,
      currentProjection: () => this.#projectionObservation.current,
      currentJoinedShare: () => this.#joined,
      choiceBlocked: () => this.#retained.pending || this.#activeReceive.active,
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
          commitOwnership,
          runtime.lifecycle,
          Date.now(),
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
      onFailure: error => this.#fail(error),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    this.#retained = new RetainedInventoryCoordinator({
      receive: this.#receive,
      isDisposed: () => this.#disposed,
      currentJoinedShare: () => this.#joined,
      continuationBlocked: () => this.#activeReceive.active || this.#authority.pending,
      adoptContinuation: (input) => this.#adoptRetainedReceiveContinuation(input),
      ownsRuntime: (runtime) => this.#activeReceive.ownsRuntime(runtime),
      publish: (retained) => this.#publish({ ...this.#snapshot, retained }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      onActionError: (error) => this.#publishActionError(error),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.#browse = new BrowserNavigationCoordinator({
      currentJoinedShare: () => this.#joined,
      isDisposed: () => this.#disposed,
      snapshot: () => this.#snapshot,
      publish: (snapshot) => this.#publish(snapshot),
      publicError: (error) => this.#publicError(error),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
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
      directoryRetryable: false,
      progress: EMPTY_V2_PROGRESS,
      preview: EMPTY_V2_PREVIEW,
      output: EMPTY_V2_OUTPUT_PRESENTATION,
      retained: EMPTY_V2_RETAINED_INVENTORY,
    })
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

  readonly getDiagnosticSnapshot = (): V2ReceiverDiagnosticSnapshot => {
    const lifecycle = this.#snapshot.output.lifecycle
    const plan = this.#snapshot.output.plan
    const progress = this.#snapshot.progress
    return Object.freeze({
      controller: Object.freeze({
        generation: this.#diagnosticGeneration,
        phase: this.#snapshot.phase,
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
        generation: this.#diagnosticGeneration,
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
      }),
      ...(plan === null
        ? {}
        : {
            output: Object.freeze({
              generation: this.#diagnosticGeneration,
              planKind: plan.kind,
            }),
          }),
    })
  }

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

  toggleSelection(id: string): void {
    const joined = this.#joined
    const page = this.#browse.page
    const entry = this.#browse.entry(id)
    if (joined === undefined || page === undefined || entry === undefined || this.#selectionLocked()) return
    joined.selection.toggle(entry, page.directory.ancestry)
    this.#browse.publishPage(page)
    this.#beginSelectionProjection(joined)
  }

  openDirectory(id: string): void {
    this.#browse.openDirectory(id)
  }

  openBreadcrumb(index: number): void {
    this.#browse.openBreadcrumb(index)
  }

  showPage(index: number): void {
    this.#browse.showPage(index)
  }

  retryDirectory(): void {
    this.#browse.retryDirectory()
  }

  previewFile(id: string): void {
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

  chooseArtifact(operation: ArtifactOperation): void {
    this.#authority.choose(operation)
  }

  retryOutputConfirmation(): void {
    this.#authority.retry()
  }

  performLifecycleAction(action: LifecycleUserAction): void {
    this.#activeReceive.performLifecycleAction(action)
  }

  performRetainedAction(
    operation: V2RetainedReceiveOperation,
    action: V2RetainedReceiveAction,
  ): void {
    this.#retained.perform(operation, action)
  }

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
    this.#projectionObservation.stop(new StaleReceiveBoundaryError())
    this.#authority.invalidate(new StaleReceiveBoundaryError(), 'caller-cancelled')
    const prepared = this.#activeReceive.prepareAdoption({
      joined,
      selection,
      runtime,
    })
    this.#outputs.adoptRetainedReceiveIntentAtomically(
      intent,
      runtime.lifecycle,
      prepared.commit,
      Date.now(),
      runtime.initialWorkspaceUsage,
      runtime.activeControls,
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
        error: null,
        rows: Object.freeze([]),
        preview: EMPTY_V2_PREVIEW,
        progress: EMPTY_V2_PROGRESS,
      })
      previous = this.#joined
      this.#unsubscribeScanProgress?.()
      this.#unsubscribeScanProgress = undefined
      this.#unsubscribeProtocolGeneration?.()
      this.#unsubscribeProtocolGeneration = undefined
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
        this.#beginSelectionProjection(joined, 'observation-replacement')
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
    if (replacement === 'selection-change') this.#outputs.reset()
    this.#publish({ ...this.#snapshot, progress: EMPTY_V2_PROGRESS })
    this.#projectionObservation.start(joined, replacement)
  }

  #stopProjectionObservation(reason: unknown): void {
    this.#projectionObservation.stop(reason)
  }

  #subscribeJoinedNotifications(joined: V2JoinedBrowserShare): void {
    this.#unsubscribeScanProgress?.()
    this.#unsubscribeProtocolGeneration?.()
    this.#unsubscribeScanProgress = joined.subscribeCatalogScanProgress(
      progress => this.#browse.catalogScanProgress(joined, progress),
    )
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
    return this.#activeReceive.reset(reason)
  }

  #transferProgress(progress: TransferProgress): void {
    if (progress.transferJobId.length === 0) return
    const snapshot: V2ReceiverProgress = Object.freeze({
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
      transferJobId: progress.transferJobId,
      ...(progress.outputSessionId === undefined
        ? {}
        : { outputSessionId: progress.outputSessionId }),
    })
    this.#publish({ ...this.#snapshot, progress: snapshot })
  }

  #selectionLocked(): boolean {
    return this.#retained.pending ||
      this.#authority.pending || this.#snapshot.output.receiveIntent !== null
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

  #publish(snapshot: V2ReceiverSnapshot): void {
    this.#diagnosticGeneration += 1n
    this.#snapshot = Object.freeze(snapshot)
    for (const listener of this.#listeners) listener()
  }
}
