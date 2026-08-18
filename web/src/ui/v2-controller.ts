import type { V2FrozenSelectionPolicy } from '../catalog/v2-selection'
import {
  bindMaterialization,
  type ArtifactOperation,
  type EnvironmentOffers,
} from '../output/planning'
import {
  createSelectionSpec,
  selectionRulesSpecFromPolicy,
  validateReceiveIntent,
  type ReceiveIntent,
  type SelectionSpec,
} from '../transfer/intent'
import {
  discoverAuthenticatedSelection,
  retryAuthenticatedSelectionDiscovery,
  SelectionProjectionController,
  type ProjectionEpoch,
  type SelectionProjectionState,
} from '../transfer/projection'
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
  type V2ReceiverProgress,
  type V2ReceiverSnapshot,
} from './v2-model'
import {
  V2CapabilityInputLifecycle,
  type V2CapabilityJoinLease,
  type V2CapturedLocation,
} from './v2-capability-lifecycle'
import { isAbortError } from './v2-controller-state'
import type { LifecycleUserAction } from './v2-lifecycle-presentation'
import {
  EMPTY_V2_OUTPUT_PRESENTATION,
  V2OutputPresentationController,
  type ArtifactActivationResult,
} from './v2-output'
import { V2PreviewController } from './v2-preview-controller'
import {
  type V2BoundReceiveOperation,
  type V2ReceiveCompositionPort,
  type V2RetainedReceiveAction,
  type V2RetainedReceiveOperation,
  type V2StartedArtifactAuthority,
} from './v2-receive-runtime'
import { ActiveReceiveCoordinator } from './controller/active-receive'
import {
  ArtifactChoiceInvalidatedError,
  StaleReceiveBoundaryError,
  type V2ReceiverControllerOptions,
  type V2ReceiverTraceEvent,
} from './controller/contracts'
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

interface ActiveProjection {
  readonly revision: number
  readonly joined: V2JoinedBrowserShare
  readonly selection: SelectionSpec
  readonly frozenSelection: V2FrozenSelectionPolicy
  readonly epoch: ProjectionEpoch
  controller: AbortController
  protocolSessionId: string
  state: SelectionProjectionState
  environment: EnvironmentOffers
}


export class V2ReceiverController {
  readonly #gateway: V2BrowserReceiverGateway
  readonly #receive: V2ReceiveCompositionPort
  readonly #capabilityLifecycle: V2CapabilityInputLifecycle
  readonly #listeners = new Set<() => void>()
  readonly #onTrace: ((event: V2ReceiverTraceEvent) => void) | undefined
  readonly #outputs: V2OutputPresentationController<V2StartedArtifactAuthority>
  readonly #projection: SelectionProjectionController
  readonly #previews: V2PreviewController
  readonly #activeReceive: ActiveReceiveCoordinator
  readonly #retained: RetainedInventoryCoordinator
  readonly #browse: BrowserNavigationCoordinator
  readonly #unsubscribeOutput: () => void
  #snapshot: V2ReceiverSnapshot
  #pageUrl = ''
  #joined: V2JoinedBrowserShare | undefined
  #joinNavigation: AbortController | undefined
  #unsubscribeScanProgress: (() => void) | undefined
  #projectionRevision = 0
  #projectionPending: AbortController | undefined
  #activeProjection: ActiveProjection | undefined
  #acquisition: AbortController | undefined
  #disposed = false

  constructor(
    gateway: V2BrowserReceiverGateway,
    options: V2ReceiverControllerOptions,
  ) {
    this.#gateway = gateway
    this.#receive = options.receive
    this.#onTrace = options.onOutputTrace
    this.#capabilityLifecycle = new V2CapabilityInputLifecycle(options)
    this.#projection = new SelectionProjectionController((event) => this.#trace(event))
    this.#outputs = new V2OutputPresentationController({
      releaseStaleAuthority: (authority) => authority.release(new StaleReceiveBoundaryError()),
      onTrace: (event) => this.#trace(event),
    })
    this.#activeReceive = new ActiveReceiveCoordinator({
      outputs: this.#outputs,
      ownsJoinedShare: (joined) => !this.#disposed && this.#joined === joined,
      onProgress: (progress) => this.#transferProgress(progress),
      onTrace: (event) => this.#trace(event),
      onActionError: (error) => this.#publishActionError(error),
      onFailure: (error) => this.#fail(error),
    })
    this.#retained = new RetainedInventoryCoordinator({
      receive: this.#receive,
      isDisposed: () => this.#disposed,
      currentJoinedShare: () => this.#joined,
      continuationBlocked: () => this.#activeReceive.active || this.#acquisition !== undefined ||
        this.#snapshot.output.chosenAction !== null,
      adoptContinuation: (input) => this.#adoptRetainedReceiveContinuation(input),
      ownsRuntime: (runtime) => this.#activeReceive.ownsRuntime(runtime),
      publish: (retained) => this.#publish({ ...this.#snapshot, retained }),
      onTrace: (event) => this.#trace(event),
      onActionError: (error) => this.#publishActionError(error),
    })
    this.#browse = new BrowserNavigationCoordinator({
      currentJoinedShare: () => this.#joined,
      isDisposed: () => this.#disposed,
      snapshot: () => this.#snapshot,
      publish: (snapshot) => this.#publish(snapshot),
      publicError: (error) => this.#publicError(error),
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
    this.#previews = new V2PreviewController({
      snapshot: () => this.#snapshot,
      publish: (snapshot) => this.#publish(snapshot),
      publicError: (error) => this.#publicError(error),
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

  previewMediaFailed(url: string): void {
    this.#previews.mediaFailed(url)
  }

  chooseArtifact(operation: ArtifactOperation): void {
    const joined = this.#joined
    const projection = this.#activeProjection
    if (joined === undefined || projection === undefined ||
        this.#retained.pending || this.#activeReceive.active) return
    if (projection.protocolSessionId !== joined.protocolSessionId) {
      this.#beginSelectionProjection(joined)
      return
    }
    const activation = this.#outputs.activateArtifact(
      operation,
      (action) => this.#receive.startArtifactAuthority(action),
    )
    activation.then((result) => {
      if (result.kind !== 'acquired') return
      return this.#finishArtifactAuthority(result)
    }).catch((error: unknown) => {
      if (!isAbortError(error) && !(error instanceof ArtifactChoiceInvalidatedError)) {
        this.#publishActionError(error)
      }
    })
  }

  retryOutputConfirmation(): void {
    const projection = this.#activeProjection
    if (projection === undefined) return
    this.#outputs.retryConfirmation((epoch) => {
      if (this.#activeProjection !== projection || projection.epoch !== epoch) return
      this.#retryProjection(projection).catch((error: unknown) => {
        if (!isAbortError(error)) this.#fail(error)
      })
    }).catch(() => undefined)
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
    const detached = this.#resetReceiveOwnership(new DOMException('Receiver disposed', 'AbortError'))
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
    this.#projectionRevision += 1
    this.#projectionPending?.abort(new StaleReceiveBoundaryError())
    this.#projectionPending = undefined
    this.#activeProjection?.controller.abort(new StaleReceiveBoundaryError())
    this.#activeProjection = undefined
    this.#acquisition?.abort(new StaleReceiveBoundaryError())
    this.#acquisition = undefined
    this.#outputs.adoptRetainedReceiveIntent(
      intent,
      runtime.lifecycle,
      Date.now(),
      runtime.initialWorkspaceUsage,
      runtime.activeControls,
    )
    this.#activeReceive.adopt({
      joined,
      selection,
      runtime,
    })
  }

  #join(input: string): Promise<void> {
    const lease = this.#capabilityLifecycle.beginJoin(input, this.#pageUrl)
    return this.#joinOwned(lease)
  }

  async #joinOwned(lease: V2CapabilityJoinLease): Promise<void> {
    let navigation: AbortController | undefined
    try {
      this.#retained.cancelPending(new StaleReceiveBoundaryError())
      this.#resetReceiveOwnership(new StaleReceiveBoundaryError()).catch(() => undefined)
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
      if (this.#joinNavigation !== navigation || navigation.signal.aborted || this.#disposed) {
        await joined.close()
        return
      }
      this.#joined = joined
      this.#unsubscribeScanProgress = joined.subscribeCatalogScanProgress(
        (progress) => this.#browse.catalogScanProgress(joined, progress),
      )
      const root = joined.rootDirectory()
      this.#browse.clearCatalog()
      await this.#browse.loadPage(root, 0, Object.freeze([root]))
      if (this.#joined === joined && this.#browse.pageMatches(root)) {
        this.#beginSelectionProjection(joined)
      }
    } catch (error) {
      if (navigation !== undefined && this.#joinNavigation === navigation && !navigation.signal.aborted) {
        this.#fail(error, lease)
      }
    } finally {
      if (this.#joinNavigation === navigation) this.#joinNavigation = undefined
      lease.release()
    }
  }

  #beginSelectionProjection(joined: V2JoinedBrowserShare): void {
    const revision = ++this.#projectionRevision
    this.#projectionPending?.abort(new StaleReceiveBoundaryError())
    this.#projectionPending = undefined
    this.#activeProjection?.controller.abort(new StaleReceiveBoundaryError())
    this.#activeProjection = undefined
    this.#acquisition?.abort(new StaleReceiveBoundaryError())
    this.#acquisition = undefined
    this.#outputs.invalidate()
    this.#publish({ ...this.#snapshot, progress: EMPTY_V2_PROGRESS })

    const controller = new AbortController()
    this.#projectionPending = controller
    const frozenSelection = joined.selection.snapshot()
    const environment = Promise.resolve(this.#receive.environment(controller.signal))
    this.#startProjectionOwned(
      revision,
      joined,
      frozenSelection,
      controller,
      environment,
    ).catch((error: unknown) => {
      if (!controller.signal.aborted && revision === this.#projectionRevision &&
          this.#joined === joined) this.#fail(error)
    }).finally(() => {
      if (this.#projectionPending === controller) this.#projectionPending = undefined
    })
  }

  async #startProjectionOwned(
    revision: number,
    joined: V2JoinedBrowserShare,
    frozenSelection: V2FrozenSelectionPolicy,
    controller: AbortController,
    environmentTask: Promise<EnvironmentOffers>,
  ): Promise<void> {
    const selection = await createSelectionSpec({
      shareInstance: joined.descriptor.shareInstanceId,
      syntheticRoot: joined.descriptor.syntheticRootId,
      rules: selectionRulesSpecFromPolicy(frozenSelection),
    })
    controller.signal.throwIfAborted()
    if (!this.#projectionBoundaryIsCurrent(revision, joined)) return

    const environment = await environmentTask
    controller.signal.throwIfAborted()
    if (!this.#projectionBoundaryIsCurrent(revision, joined)) return
    const protocolSessionId = joined.protocolSessionId
    const state = this.#projection.beginSelection(selection, protocolSessionId)
    const active: ActiveProjection = {
      revision,
      joined,
      selection,
      frozenSelection,
      epoch: state.projection.epoch,
      controller,
      protocolSessionId,
      state,
      environment,
    }
    this.#activeProjection = active
    if (this.#projectionPending === controller) this.#projectionPending = undefined
    await this.#outputs.updateProjection(state, environment)
    if (this.#activeProjection !== active || controller.signal.aborted) return
    if (active.protocolSessionId !== joined.protocolSessionId) {
      this.#beginSelectionProjection(joined)
      return
    }
    await this.#consumeProjection(
      active,
      controller,
      discoverAuthenticatedSelection(
        this.#projection,
        joined.projectionSource(frozenSelection),
        controller.signal,
      ),
    )
  }

  async #retryProjection(active: ActiveProjection): Promise<void> {
    active.controller.abort(new DOMException('Projection retry replaced the prior source', 'AbortError'))
    const controller = new AbortController()
    active.controller = controller
    const environment = await Promise.resolve(this.#receive.environment(controller.signal))
    controller.signal.throwIfAborted()
    if (this.#activeProjection !== active || active.controller !== controller ||
        !this.#projectionBoundaryIsCurrent(
      active.revision,
      active.joined,
    )) return
    active.environment = environment
    active.protocolSessionId = active.joined.protocolSessionId
    await this.#consumeProjection(
      active,
      controller,
      retryAuthenticatedSelectionDiscovery(
        this.#projection,
        active.joined.projectionSource(active.frozenSelection, true),
        controller.signal,
      ),
    )
  }

  async #consumeProjection(
    active: ActiveProjection,
    controller: AbortController,
    states: AsyncGenerator<SelectionProjectionState, SelectionProjectionState>,
  ): Promise<void> {
    for await (const state of states) {
      if (this.#activeProjection !== active || active.controller !== controller ||
          controller.signal.aborted) return
      active.state = state
      await this.#outputs.updateProjection(state, active.environment)
    }
  }

  async #finishArtifactAuthority(
    activation: Extract<ArtifactActivationResult<V2StartedArtifactAuthority>, { kind: 'acquired' }>,
  ): Promise<void> {
    const activeProjection = this.#activeProjection
    if (activeProjection === undefined || activeProjection.epoch !== activation.projectionEpoch ||
        !this.#outputs.acquiredAuthorityIsCurrent(activation)) {
      await activation.authority.release(new StaleReceiveBoundaryError())
      return
    }

    const controller = new AbortController()
    this.#acquisition?.abort(new StaleReceiveBoundaryError())
    this.#acquisition = controller
    let frozenIntent: ReceiveIntent | undefined
    let finalizedRuntime: V2BoundReceiveOperation | undefined
    try {
      const runtime = await activation.authority.finalize(async (acquired) => {
        controller.signal.throwIfAborted()
        if (!this.#authorityBoundaryIsCurrent(activeProjection, activation)) {
          throw new StaleReceiveBoundaryError()
        }
        const environment = await Promise.resolve(this.#receive.environment(controller.signal))
        controller.signal.throwIfAborted()
        if (!this.#authorityBoundaryIsCurrent(activeProjection, activation)) {
          throw new StaleReceiveBoundaryError()
        }
        const bound = await bindMaterialization({
          selection: activeProjection.selection,
          chosenAction: activation.action,
          currentProjection: activeProjection.state.projection,
          currentDiscovery: activeProjection.state.discovery,
          currentEnvironment: environment,
          acquired,
        })
        if (bound.kind !== 'bound') throw new ArtifactChoiceInvalidatedError()
        frozenIntent = bound.intent
        return bound.intent
      }, controller.signal)
      finalizedRuntime = runtime
      controller.signal.throwIfAborted()
      if (!this.#authorityBoundaryIsCurrent(activeProjection, activation) ||
          frozenIntent === undefined) {
        await runtime.settleTransferAdmissionFailure(new StaleReceiveBoundaryError())
        await runtime.detach()
        return
      }

      const intent = await validateReceiveIntent(runtime.intent)
      if (intent.digest !== frozenIntent.digest || runtime.lifecycle.operationId !== intent.operationId ||
          runtime.lifecycle.receiveIntentDigest !== intent.digest) {
        throw new TypeError('bound receive runtime does not match the frozen intent authority')
      }
      if (!this.#outputs.adoptReceiveIntent(
        activeProjection.epoch,
        intent,
        runtime.lifecycle,
        Date.now(),
        runtime.initialWorkspaceUsage,
        runtime.activeControls,
      )) {
        await runtime.settleTransferAdmissionFailure(new StaleReceiveBoundaryError())
        await runtime.detach()
        return
      }
      activeProjection.controller.abort(new DOMException('Receive intent is frozen', 'AbortError'))
      this.#activeReceive.adopt({
        joined: activeProjection.joined,
        selection: activeProjection.frozenSelection,
        runtime,
      })
    } catch (error) {
      if (finalizedRuntime === undefined) {
        await Promise.resolve(activation.authority.release(error)).catch(() => undefined)
      } else {
        await Promise.resolve(finalizedRuntime.settleTransferAdmissionFailure(error))
          .catch(() => undefined)
        await Promise.resolve(finalizedRuntime.detach()).catch(() => undefined)
      }
      if (this.#activeProjection === activeProjection && !controller.signal.aborted) {
        await this.#refreshCurrentOffers(activeProjection).catch(() => undefined)
      }
      throw error
    } finally {
      if (this.#acquisition === controller) this.#acquisition = undefined
    }
  }

  async #refreshCurrentOffers(active: ActiveProjection): Promise<void> {
    if (this.#activeProjection !== active) return
    if (active.protocolSessionId !== active.joined.protocolSessionId) {
      this.#beginSelectionProjection(active.joined)
      return
    }
    const environment = await Promise.resolve(this.#receive.environment(active.controller.signal))
    if (this.#activeProjection !== active || active.controller.signal.aborted) return
    if (active.protocolSessionId !== active.joined.protocolSessionId) {
      this.#beginSelectionProjection(active.joined)
      return
    }
    active.environment = environment
    await this.#outputs.updateProjection(active.state, environment)
  }

  #authorityBoundaryIsCurrent(
    projection: ActiveProjection,
    activation: Extract<ArtifactActivationResult<V2StartedArtifactAuthority>, { kind: 'acquired' }>,
  ): boolean {
    return this.#activeProjection === projection &&
      projection.epoch === activation.projectionEpoch &&
      this.#joined === projection.joined &&
      projection.protocolSessionId === projection.joined.protocolSessionId &&
      this.#outputs.acquiredAuthorityIsCurrent(activation)
  }

  #projectionBoundaryIsCurrent(revision: number, joined: V2JoinedBrowserShare): boolean {
    return revision === this.#projectionRevision && this.#joined === joined && !this.#disposed
  }

  #resetReceiveOwnership(reason: unknown): Promise<void> {
    this.#projectionRevision += 1
    this.#projectionPending?.abort(reason)
    this.#projectionPending = undefined
    this.#activeProjection?.controller.abort(reason)
    this.#activeProjection = undefined
    this.#acquisition?.abort(reason)
    this.#acquisition = undefined
    this.#outputs.invalidate()
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
      this.#snapshot.output.chosenAction !== null || this.#snapshot.output.receiveIntent !== null
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

  #trace(event: V2ReceiverTraceEvent): void {
    try {
      this.#onTrace?.(event)
    } catch {
      // Diagnostic observers cannot alter epoch, authority, or operation ownership.
    }
  }

  #publicError(error: unknown): string {
    return this.#capabilityLifecycle.publicError(error)
  }

  #publish(snapshot: V2ReceiverSnapshot): void {
    this.#snapshot = Object.freeze(snapshot)
    for (const listener of this.#listeners) listener()
  }
}
