import { V2DirectoryFailureError, type V2CatalogScanProgress } from '../catalog/v2-client'
import type { V2CatalogEntry } from '../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../catalog/v2-selection'
import type {
  V2ConnectivityActivation,
  V2ContentSizeClass,
} from '../connectivity/v2-receiver-policy'
import { equalBytes } from '../crypto/bytes'
import {
  bindMaterialization,
  type ArtifactOperation,
  type EnvironmentOffers,
} from '../output/planning'
import {
  lifecycleDeadline,
} from '../output/workspace'
import {
  createSelectionSpec,
  selectionRulesSpecFromPolicy,
  validateReceiveIntent,
  type ReceiveIntent,
  type SelectionSpec,
} from '../transfer/intent'
import { SMALL_TRANSFER_BYTE_LIMIT } from '../transfer/measure'
import {
  discoverAuthenticatedSelection,
  retryAuthenticatedSelectionDiscovery,
  SelectionProjectionController,
  type ProjectionEpoch,
  type ProjectionTraceEvent,
  type SelectionProjectionState,
} from '../transfer/projection'
import type {
  TransferJobResult,
  TransferProgress,
  TransferTraceEvent,
} from '../transfer/v2-job'
import {
  V2BrowserReceiverGateway,
  v2SelectionPolicyFromIntent,
  type V2BrowseDirectory,
  type V2BrowsePage,
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
  type V2DiagnosticFormatter,
  type V2SecurityMilestone,
} from './v2-capability-lifecycle'
import {
  breadcrumbsFor,
  isAbortError,
  projectBrowsePage,
  type RetryableV2BrowseRequest,
} from './v2-controller-state'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
} from './v2-lifecycle-presentation'
import {
  EMPTY_V2_OUTPUT_PRESENTATION,
  V2OutputPresentationController,
  type ArtifactActivationResult,
  type V2OutputTraceEvent,
} from './v2-output'
import { V2PreviewController } from './v2-preview-controller'
import {
  type V2BoundReceiveOperation,
  type V2LifecycleMutation,
  type V2ReceiveCompositionPort,
  type V2RetainedReceiveAction,
  type V2RetainedReceiveActionResult,
  type V2RetainedReceiveInventory,
  type V2RetainedReceiveOperation,
  type V2StartedArtifactAuthority,
} from './v2-receive-runtime'

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

const MAXIMUM_TIMER_DELAY_MILLISECONDS = 2_147_483_647

export type V2RetainedInventoryTraceEvent =
  | Readonly<{ name: 'receive.inventory.load.started' }>
  | Readonly<{ name: 'receive.inventory.load.completed'; operation_count: number }>
  | Readonly<{ name: 'receive.inventory.load.failed'; error_name: string }>
  | Readonly<{
      name: 'receive.inventory.action.started' | 'receive.inventory.action.completed'
      retained_action: V2RetainedReceiveAction
      continuation: V2RetainedReceiveOperation['continuation']
    }>
  | Readonly<{
      name: 'receive.inventory.action.failed'
      retained_action: V2RetainedReceiveAction
      continuation: V2RetainedReceiveOperation['continuation']
      error_name: string
    }>

export type V2ReceiverTraceEvent =
  | ProjectionTraceEvent
  | TransferTraceEvent
  | V2OutputTraceEvent
  | V2RetainedInventoryTraceEvent

export interface V2ReceiverControllerOptions {
  readonly diagnosticFormatter?: V2DiagnosticFormatter
  readonly onSecurityMilestone?: (milestone: V2SecurityMilestone) => void
  readonly onOutputTrace?: (event: V2ReceiverTraceEvent) => void
  readonly receive: V2ReceiveCompositionPort
}

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

interface ActiveReceiveOperation {
  readonly boundary: number
  readonly joined: V2JoinedBrowserShare
  readonly selection: V2FrozenSelectionPolicy
  readonly sizeClass: V2ContentSizeClass
  readonly runtime: V2BoundReceiveOperation
  transfer?: AbortController
  connectivity?: V2ConnectivityActivation
  running?: Promise<void>
  expiryTimer?: ReturnType<typeof setTimeout>
  detachment?: Promise<void>
}

interface PendingLifecycleAction {
  readonly operation: ActiveReceiveOperation
  readonly generation: bigint
  readonly action: LifecycleUserAction
}

interface PendingRetainedAction {
  readonly boundary: number
  readonly inventory: V2RetainedReceiveInventory
  readonly operation: V2RetainedReceiveOperation
  readonly action: V2RetainedReceiveAction
  readonly joined?: V2JoinedBrowserShare
  readonly protocolSessionId?: string
  readonly controller: AbortController
}

class ArtifactChoiceInvalidatedError extends Error {
  constructor() {
    super('The selected artifact action is no longer offered by current authority facts')
    this.name = 'ArtifactChoiceInvalidatedError'
  }
}

class StaleReceiveBoundaryError extends DOMException {
  constructor() {
    super('A newer receive boundary replaced this action', 'AbortError')
  }
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
  readonly #unsubscribeOutput: () => void
  #snapshot: V2ReceiverSnapshot
  #pageUrl = ''
  #joined: V2JoinedBrowserShare | undefined
  #page: V2BrowsePage | undefined
  #directories: V2BrowseDirectory[] = []
  #entries = new Map<string, V2CatalogEntry>()
  #navigation: AbortController | undefined
  #retainedInventoryLoad: AbortController | undefined
  #retainedInventory: V2RetainedReceiveInventory | undefined
  #retainedActionBoundary = 0
  #pendingRetainedAction: PendingRetainedAction | undefined
  #pendingNavigationKey: string | undefined
  #loadingDirectory: V2BrowseDirectory | undefined
  #retryableBrowse: RetryableV2BrowseRequest | undefined
  #unsubscribeScanProgress: (() => void) | undefined
  #projectionRevision = 0
  #projectionPending: AbortController | undefined
  #activeProjection: ActiveProjection | undefined
  #acquisition: AbortController | undefined
  #operationBoundary = 0
  #operation: ActiveReceiveOperation | undefined
  #pendingLifecycleAction: PendingLifecycleAction | undefined
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
    currentPageEntries: this.#entries.size,
    receiveOperationActive: this.#operation !== undefined,
  })

  initialize(captured: V2CapturedLocation): void {
    this.#pageUrl = captured.pageUrl
    this.#capabilityLifecycle.acceptCapturedLocation(captured)
    if (captured.capabilityInput !== null) {
      this.#join(captured.capabilityInput).catch(() => undefined)
    }
    this.#loadRetainedInventory().catch(() => undefined)
  }

  submitKey(input: string): void {
    if (this.#disposed || input.trim().length === 0) return
    this.#join(input.trim()).catch(() => undefined)
    // The form clears its password field in this same stack before join can reject.
    this.#capabilityLifecycle.notify('key-cleared')
  }

  toggleSelection(id: string): void {
    const joined = this.#joined
    const page = this.#page
    const entry = this.#entries.get(id)
    if (joined === undefined || page === undefined || entry === undefined || this.#selectionLocked()) return
    joined.selection.toggle(entry, page.directory.ancestry)
    this.#publishPage(page)
    this.#beginSelectionProjection(joined)
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

  chooseArtifact(operation: ArtifactOperation): void {
    const joined = this.#joined
    const projection = this.#activeProjection
    if (joined === undefined || projection === undefined ||
        this.#pendingRetainedAction !== undefined || this.#operation !== undefined) return
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
    const active = this.#operation
    const lifecycle = this.#snapshot.output.lifecycle
    const presented = this.#snapshot.output.lifecyclePresentation
    if (active === undefined || lifecycle === null || presented === null ||
        this.#pendingLifecycleAction !== undefined ||
        !presented.actions.some((candidate) => candidate.kind === action)) return

    if (action === 'pause' || action === 'stop') {
      this.#interruptReceive(active, action)
      return
    }

    const pending = Object.freeze({
      operation: active,
      generation: lifecycle.generation,
      action,
    })
    this.#pendingLifecycleAction = pending
    let started: V2LifecycleMutation | PromiseLike<V2LifecycleMutation>
    try {
      // Publication pickers start inside this call, before the rendered click returns.
      started = active.runtime.startLifecycleAction(action, lifecycle)
    } catch (error) {
      if (this.#pendingLifecycleAction !== pending) return
      this.#pendingLifecycleAction = undefined
      this.#publishActionError(error)
      return
    }
    Promise.resolve(started).then(
      (mutation) => {
        if (this.#pendingLifecycleAction !== pending) return
        this.#pendingLifecycleAction = undefined
        return this.#applyLifecycleMutation(active, lifecycle.generation, mutation)
      },
      (error: unknown) => {
        if (this.#pendingLifecycleAction !== pending) return
        this.#pendingLifecycleAction = undefined
        if (!isAbortError(error)) this.#publishActionError(error)
      },
    ).catch(() => undefined)
  }

  performRetainedAction(
    operation: V2RetainedReceiveOperation,
    action: V2RetainedReceiveAction,
  ): void {
    const inventory = this.#retainedInventory
    if (this.#disposed || inventory === undefined || this.#pendingRetainedAction !== undefined ||
        !inventory.operations.includes(operation) || !operation.actions.includes(action)) return
    const joined = action === 'continue' ? this.#joined : undefined
    if (action === 'continue' && (joined === undefined || this.#operation !== undefined ||
        this.#acquisition !== undefined || this.#snapshot.output.chosenAction !== null)) {
      this.#publishActionError(new DOMException(
        'Open the matching share before continuing this receive task',
        'InvalidStateError',
      ))
      return
    }

    const controller = new AbortController()
    const pending: PendingRetainedAction = Object.freeze({
      boundary: ++this.#retainedActionBoundary,
      inventory,
      operation,
      action,
      ...(joined === undefined
        ? {}
        : { joined, protocolSessionId: joined.protocolSessionId }),
      controller,
    })
    this.#pendingRetainedAction = pending
    this.#trace(Object.freeze({
      name: 'receive.inventory.action.started',
      retained_action: action,
      continuation: operation.continuation,
    }))
    let started: ReturnType<V2RetainedReceiveInventory['act']>
    try {
      // The exact live ResumeRef is consumed before this explicit action stack returns.
      started = inventory.act(operation, action, controller.signal)
    } catch (error) {
      this.#settleRetainedActionFailure(pending, error)
      return
    }
    Promise.resolve(started).then(
      (result) => this.#settleRetainedActionSuccess(pending, result),
      (error: unknown) => this.#settleRetainedActionFailure(pending, error),
    ).catch(() => undefined)
  }

  async dispose(): Promise<void> {
    if (this.#disposed) return
    this.#disposed = true
    this.#capabilityLifecycle.clear()
    this.#navigation?.abort(new DOMException('Receiver disposed', 'AbortError'))
    this.#retainedInventoryLoad?.abort(new DOMException('Receiver disposed', 'AbortError'))
    this.#retainedInventoryLoad = undefined
    this.#cancelPendingRetainedAction(new DOMException('Receiver disposed', 'AbortError'))
    this.#retainedInventory?.close()
    this.#retainedInventory = undefined
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

  async #loadRetainedInventory(): Promise<void> {
    this.#retainedInventoryLoad?.abort(new DOMException('Resume inventory reloaded', 'AbortError'))
    this.#retainedInventory?.close()
    this.#retainedInventory = undefined
    const controller = new AbortController()
    this.#retainedInventoryLoad = controller
    this.#publish({
      ...this.#snapshot,
      retained: EMPTY_V2_RETAINED_INVENTORY,
    })
    this.#trace(Object.freeze({ name: 'receive.inventory.load.started' }))
    let loaded: V2RetainedReceiveInventory | undefined
    try {
      loaded = await this.#receive.retained.list(controller.signal)
      controller.signal.throwIfAborted()
      if (this.#disposed || this.#retainedInventoryLoad !== controller) {
        loaded.close()
        return
      }
      this.#retainedInventory = loaded
      this.#trace(Object.freeze({
        name: 'receive.inventory.load.completed',
        operation_count: loaded.operations.length,
      }))
      this.#publish({
        ...this.#snapshot,
        retained: Object.freeze({
          kind: 'ready',
          operations: Object.freeze([...loaded.operations]),
          error: null,
        }),
      })
    } catch (error) {
      loaded?.close()
      if (controller.signal.aborted || this.#disposed || this.#retainedInventoryLoad !== controller) return
      this.#publish({
        ...this.#snapshot,
        retained: Object.freeze({
          kind: 'failed',
          operations: Object.freeze([]),
          error: 'Stored receive tasks could not be loaded.',
        }),
      })
      this.#trace(Object.freeze({
        name: 'receive.inventory.load.failed',
        error_name: error instanceof Error ? error.name : 'UnknownError',
      }))
    } finally {
      if (this.#retainedInventoryLoad === controller) this.#retainedInventoryLoad = undefined
    }
  }

  #cancelPendingRetainedAction(reason: unknown): void {
    const pending = this.#pendingRetainedAction
    if (pending === undefined) return
    this.#retainedActionBoundary += 1
    this.#pendingRetainedAction = undefined
    pending.controller.abort(reason)
  }

  #retireStaleRetainedAction(pending: PendingRetainedAction): void {
    if (this.#pendingRetainedAction !== pending ||
        pending.boundary !== this.#retainedActionBoundary || this.#disposed) return
    this.#cancelPendingRetainedAction(new StaleReceiveBoundaryError())
    this.#trace(Object.freeze({
      name: 'receive.inventory.action.failed',
      retained_action: pending.action,
      continuation: pending.operation.continuation,
      error_name: 'AbortError',
    }))
    this.#loadRetainedInventory().catch(() => undefined)
  }

  async #settleRetainedActionSuccess(
    pending: PendingRetainedAction,
    result: V2RetainedReceiveActionResult,
  ): Promise<void> {
    if (!this.#retainedActionIsCurrent(pending)) {
      if (result.kind === 'receive-continuation') {
        await Promise.resolve(result.runtime.detach()).catch(() => undefined)
      }
      this.#retireStaleRetainedAction(pending)
      return
    }
    if (result.kind === 'receive-continuation') {
      try {
        await this.#adoptRetainedReceiveContinuation(pending, result.runtime)
      } catch (error) {
        await Promise.resolve(result.runtime.detach()).catch(() => undefined)
        this.#settleRetainedActionFailure(pending, error)
        return
      }
      if (this.#operation?.runtime !== result.runtime || this.#disposed) return
    } else if (!this.#retainedActionIsCurrent(pending)) {
      this.#retireStaleRetainedAction(pending)
      return
    }
    this.#pendingRetainedAction = undefined
    this.#trace(Object.freeze({
      name: 'receive.inventory.action.completed',
      retained_action: pending.action,
      continuation: pending.operation.continuation,
    }))
    this.#loadRetainedInventory().catch(() => undefined)
  }

  async #adoptRetainedReceiveContinuation(
    pending: PendingRetainedAction,
    runtime: V2BoundReceiveOperation,
  ): Promise<void> {
    const joined = pending.joined
    if (joined === undefined || !this.#retainedActionIsCurrent(pending)) {
      throw new StaleReceiveBoundaryError()
    }
    const intent = await validateReceiveIntent(runtime.intent)
    if (!this.#retainedActionIsCurrent(pending)) throw new StaleReceiveBoundaryError()
    if (intent.shareInstance !== joined.descriptor.shareInstanceId ||
        intent.syntheticRoot !== joined.descriptor.syntheticRootId) {
      throw new DOMException(
        'Stored receive task belongs to a different share authority',
        'InvalidStateError',
      )
    }
    if (pending.operation.continuation !== 'resume-receive' ||
        pending.operation.operationId !== intent.operationId ||
        pending.operation.receiveIntentDigest !== intent.digest ||
        runtime.lifecycle.kind !== 'receiving' ||
        runtime.lifecycle.generation !== pending.operation.lifecycleGeneration + 1n ||
        runtime.lifecycle.operationId !== intent.operationId ||
        runtime.lifecycle.receiveIntentDigest !== intent.digest ||
        (intent.plan.kind !== 'direct-tree' && intent.plan.kind !== 'workspace-then-publish')) {
      throw new TypeError('retained continuation runtime does not match its persisted authority')
    }
    const selection = v2SelectionPolicyFromIntent(intent)
    const operation: ActiveReceiveOperation = {
      boundary: ++this.#operationBoundary,
      joined,
      selection,
      // Persisted intent has no speculative size projection; the large policy is the
      // conservative connectivity class and does not alter authenticated transfer scope.
      sizeClass: 'large',
      runtime,
    }
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
    this.#pendingLifecycleAction = undefined
    this.#operation = operation
    this.#startTransfer(operation)
  }

  #retainedActionIsCurrent(pending: PendingRetainedAction): boolean {
    return this.#pendingRetainedAction === pending &&
      pending.boundary === this.#retainedActionBoundary && !this.#disposed &&
      (pending.joined === undefined || (
        this.#joined === pending.joined &&
        pending.protocolSessionId === pending.joined.protocolSessionId
      ))
  }

  #settleRetainedActionFailure(pending: PendingRetainedAction, error: unknown): void {
    if (!this.#retainedActionIsCurrent(pending)) {
      this.#retireStaleRetainedAction(pending)
      return
    }
    this.#pendingRetainedAction = undefined
    this.#trace(Object.freeze({
      name: 'receive.inventory.action.failed',
      retained_action: pending.action,
      continuation: pending.operation.continuation,
      error_name: error instanceof Error ? error.name : 'UnknownError',
    }))
    if (!isAbortError(error)) this.#publishActionError(error)
    this.#loadRetainedInventory().catch(() => undefined)
  }

  #join(input: string): Promise<void> {
    const lease = this.#capabilityLifecycle.beginJoin(input, this.#pageUrl)
    return this.#joinOwned(lease)
  }

  async #joinOwned(lease: V2CapabilityJoinLease): Promise<void> {
    let navigation: AbortController | undefined
    try {
      this.#cancelPendingRetainedAction(new StaleReceiveBoundaryError())
      this.#resetReceiveOwnership(new StaleReceiveBoundaryError()).catch(() => undefined)
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
      await this.#loadPage(root, 0, Object.freeze([root]))
      if (this.#joined === joined && this.#pageMatches(root)) {
        this.#beginSelectionProjection(joined)
      }
    } catch (error) {
      if (navigation !== undefined && this.#navigation === navigation && !navigation.signal.aborted) {
        this.#fail(error, lease)
      }
    } finally {
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
    this.#pendingLifecycleAction = undefined
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
        await runtime.abandon(new StaleReceiveBoundaryError())
        await runtime.detach()
        return
      }

      const intent = await validateReceiveIntent(runtime.intent)
      if (intent.digest !== frozenIntent.digest || runtime.lifecycle.operationId !== intent.operationId ||
          runtime.lifecycle.receiveIntentDigest !== intent.digest) {
        throw new TypeError('bound receive runtime does not match the frozen intent authority')
      }
      const operation: ActiveReceiveOperation = {
        boundary: ++this.#operationBoundary,
        joined: activeProjection.joined,
        selection: activeProjection.frozenSelection,
        sizeClass: activeProjection.state.projection.metrics.byteCountLowerBound >=
          SMALL_TRANSFER_BYTE_LIMIT ? 'large' : 'small',
        runtime,
      }
      if (!this.#outputs.adoptReceiveIntent(
        activeProjection.epoch,
        intent,
        runtime.lifecycle,
        Date.now(),
        runtime.initialWorkspaceUsage,
        runtime.activeControls,
      )) {
        await runtime.abandon(new StaleReceiveBoundaryError())
        await runtime.detach()
        return
      }
      activeProjection.controller.abort(new DOMException('Receive intent is frozen', 'AbortError'))
      this.#operation = operation
      this.#startTransfer(operation)
    } catch (error) {
      if (finalizedRuntime === undefined) {
        await Promise.resolve(activation.authority.release(error)).catch(() => undefined)
      } else {
        await Promise.resolve(finalizedRuntime.abandon(error)).catch(() => undefined)
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

  #startTransfer(active: ActiveReceiveOperation): void {
    if (!this.#operationIsCurrent(active) || active.transfer !== undefined) return
    let connectivity: V2ConnectivityActivation | undefined
    let transfer: AbortController | undefined
    try {
      connectivity = active.joined.beginDownloadConnectivity(active.sizeClass)
      transfer = new AbortController()
      active.connectivity = connectivity
      active.transfer = transfer
      const job = active.joined.transferJob(
        active.runtime.plans,
        active.runtime.intent,
        connectivity,
        {
          selection: active.selection,
          transferJobId: active.runtime.transferJobId,
          onProgress: (progress) => this.#transferProgress(active, progress),
          onTrace: (event) => this.#trace(event),
        },
      )
      const ownedConnectivity = connectivity
      const ownedTransfer = transfer
      active.running = job.run(ownedTransfer.signal).then(
        (result) => this.#settleTransfer(active, result),
        (error: unknown) => this.#settleTransferFailure(active, error),
      ).catch((error: unknown) => this.#settleTransferFailure(active, error)).finally(async () => {
        ownedConnectivity.close()
        if (active.connectivity === ownedConnectivity) delete active.connectivity
        if (active.transfer === ownedTransfer) delete active.transfer
        if (!this.#operationIsCurrent(active)) await this.#detachOperation(active)
      })
    } catch (error) {
      transfer?.abort(error)
      connectivity?.close()
      if (active.connectivity === connectivity) delete active.connectivity
      if (active.transfer === transfer) delete active.transfer
      this.#settleTransferFailure(active, error).catch((settlementError: unknown) => {
        if (this.#operationIsCurrent(active)) this.#fail(settlementError)
      })
    }
  }

  async #settleTransfer(active: ActiveReceiveOperation, result: TransferJobResult): Promise<void> {
    if (!this.#operationIsCurrent(active)) return
    if (result.intent.digest !== active.runtime.intent.digest) {
      throw new TypeError('transfer result does not belong to the active receive intent')
    }
    const usage = await Promise.resolve(active.runtime.resolveWorkspaceUsage(result.lifecycle))
    if (!this.#operationIsCurrent(active)) return
    if (!this.#outputs.updateLifecycle(result.lifecycle, Date.now(), usage, Object.freeze([]))) {
      throw new TypeError('transfer lifecycle did not advance the active receive operation')
    }
    this.#scheduleExpiry(active)
  }

  async #settleTransferFailure(active: ActiveReceiveOperation, error: unknown): Promise<void> {
    if (!this.#operationIsCurrent(active)) return
    try {
      const mutation = await Promise.resolve(active.runtime.abandon(error))
      await this.#applyLifecycleMutation(
        active,
        this.#snapshot.output.lifecycle?.generation ?? 0n,
        mutation,
      )
    } catch (settlementError) {
      if (this.#operationIsCurrent(active)) this.#fail(settlementError)
    }
  }

  #interruptReceive(active: ActiveReceiveOperation, control: V2ActiveReceiveControl): void {
    const transfer = active.transfer
    if (transfer === undefined || transfer.signal.aborted ||
        !active.runtime.activeControls.includes(control)) return
    this.#outputs.updateActiveControls(Object.freeze([]))
    try {
      active.runtime.interrupt(control, transfer)
      if (!transfer.signal.aborted) {
        throw new TypeError('receive interruption did not synchronously close the transfer lifetime')
      }
    } catch (error) {
      this.#outputs.updateActiveControls(active.runtime.activeControls)
      this.#publishActionError(error)
    }
  }

  async #applyLifecycleMutation(
    active: ActiveReceiveOperation,
    expectedGeneration: bigint,
    mutation: V2LifecycleMutation,
  ): Promise<void> {
    const current = this.#snapshot.output.lifecycle
    if (!this.#operationIsCurrent(active) || current === null ||
        current.generation !== expectedGeneration ||
        mutation.lifecycle.operationId !== active.runtime.intent.operationId ||
        mutation.lifecycle.receiveIntentDigest !== active.runtime.intent.digest) return
    const usage = mutation.workspaceUsage === undefined
      ? await Promise.resolve(active.runtime.resolveWorkspaceUsage(mutation.lifecycle))
      : mutation.workspaceUsage
    if (!this.#operationIsCurrent(active) ||
        this.#snapshot.output.lifecycle?.generation !== expectedGeneration) return
    const controls = mutation.activeControls ?? Object.freeze([])
    if (!this.#outputs.updateLifecycle(mutation.lifecycle, Date.now(), usage, controls)) return
    this.#scheduleExpiry(active)
    if (mutation.resumeTransfer === true) this.#resumeTransferWhenIdle(active)
  }

  #resumeTransferWhenIdle(active: ActiveReceiveOperation): void {
    const running = active.running
    if (active.transfer === undefined || running === undefined) {
      this.#startTransfer(active)
      return
    }
    running.finally(() => {
      if (this.#operationIsCurrent(active)) this.#startTransfer(active)
    }).catch(() => undefined)
  }

  #scheduleExpiry(active: ActiveReceiveOperation): void {
    if (active.expiryTimer !== undefined) clearTimeout(active.expiryTimer)
    delete active.expiryTimer
    const lifecycle = this.#snapshot.output.lifecycle
    if (!this.#operationIsCurrent(active) || lifecycle === null) return
    const deadline = lifecycleDeadline(lifecycle)
    if (deadline === undefined) return
    const delay = Math.min(
      MAXIMUM_TIMER_DELAY_MILLISECONDS,
      Math.max(0, deadline - Date.now()),
    )
    const expectedGeneration = lifecycle.generation
    active.expiryTimer = setTimeout(() => {
      delete active.expiryTimer
      this.#observeExpiry(active, expectedGeneration, deadline).catch((error: unknown) => {
        if (!isAbortError(error) && this.#operationIsCurrent(active)) this.#fail(error)
      })
    }, delay)
  }

  async #observeExpiry(
    active: ActiveReceiveOperation,
    expectedGeneration: bigint,
    deadline: number,
  ): Promise<void> {
    if (!this.#operationIsCurrent(active) ||
        this.#snapshot.output.lifecycle?.generation !== expectedGeneration) return
    if (Date.now() < deadline) {
      this.#scheduleExpiry(active)
      return
    }
    const lifecycle = this.#snapshot.output.lifecycle
    if (lifecycle === null) return
    const mutation = await active.runtime.observeExpiry(lifecycle)
    await this.#applyLifecycleMutation(active, expectedGeneration, mutation)
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

  #operationIsCurrent(active: ActiveReceiveOperation): boolean {
    return this.#operation === active && active.boundary === this.#operationBoundary &&
      this.#joined === active.joined && !this.#disposed
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
    this.#pendingLifecycleAction = undefined
    const active = this.#operation
    this.#operation = undefined
    if (active === undefined) return Promise.resolve()
    if (active.expiryTimer !== undefined) clearTimeout(active.expiryTimer)
    active.transfer?.abort(reason)
    if (active.running !== undefined) {
      return active.running.then(
        () => this.#detachOperation(active),
        () => this.#detachOperation(active),
      )
    }
    return this.#detachOperation(active)
  }

  #detachOperation(active: ActiveReceiveOperation): Promise<void> {
    active.detachment ??= Promise.resolve(active.runtime.detach()).then(() => undefined)
    return active.detachment
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
    if (this.#pendingNavigationKey === navigationKey &&
        this.#navigation?.signal.aborted === false) return
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
      // Route, page, rows, and breadcrumbs publish atomically so stale pages stay invisible.
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
    if (this.#joined !== joined || directory === undefined ||
        !equalBytes(directory.id, progress.directoryId)) return
    this.#publish({
      ...this.#snapshot,
      status: `Scanning ${directory.name}… ${progress.discoveredEntries} entries discovered; total still unknown.`,
    })
  }

  #transferProgress(active: ActiveReceiveOperation, progress: TransferProgress): void {
    if (!this.#operationIsCurrent(active) || progress.transferJobId.length === 0) return
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

  #publishPage(page: V2BrowsePage): void {
    const joined = this.#joined
    if (joined === undefined) return
    const projection = projectBrowsePage(page, joined.selection, this.#directories)
    this.#entries = projection.entries
    this.#publish({ ...this.#snapshot, ...projection.snapshot })
  }

  #pageMatches(directory: V2BrowseDirectory): boolean {
    return this.#page?.directory.idText === directory.idText
  }

  #selectionLocked(): boolean {
    return this.#pendingRetainedAction !== undefined ||
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
