import type { ArtifactChoice, ArtifactOffers, ReceiveOperationDisplay, SaveBackendKind } from '../../output/model'
import {
  planArtifactOffers,
  probeBrowserOutputCapabilities,
  type BrowserOutputCapabilities,
  type BrowserOutputWindow,
  type OutputSessionHandle,
  type StartBackendTaskInput,
} from '../../output/backends/contract'
import { openNewFSASession, startFSADirectoryPicker } from '../../output/backends/fsa'
import { browserOPFSEnvironment, openNewOPFSSession, type OPFSEnvironment } from '../../output/backends/opfs'
import { openPortableSession } from '../../output/backends/portable'
import { diagnosticBackendName, type OutputTraceSource } from '../../output/diagnostics'
import { RECEIVE_DATABASE_NAME, ReceiveStore } from '../../output/store/database'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { TransferJob } from '../../transfer/job'
import { EMPTY_TRANSFER_PROGRESS, type TransferProgress } from '../../transfer/progress'
import {
  createTaskId,
  policyFromSelectionSpec,
  selectionKey,
  selectionSpecFromPolicy,
  type SelectionSpec,
} from '../../transfer/selection'
import {
  initialReceiveState,
  isActiveReceiveState,
  transitionReceiveState,
  type ReceiveLifecycleState,
  type SafeRenamedSummary,
  type SourceRevisionFailures,
} from '../../transfer/state'
import { isAbortError, isPermissionDeniedError } from '../../transfer/fault'
import type { JoinedBrowserShare } from '../browser-share/gateway'
import {
  ALLOWED_ADMISSION,
  actionsForState,
  admitRetainedAction,
  blockedAdmission,
  canReleaseCurrentTask,
  canSavePartialState,
  canStartNewDownload,
} from './admission'
import {
  EMPTY_RETAINED_INVENTORY,
  type ActiveActivationSnapshot,
  type PendingRetainedAction,
  type ReceiveAction,
  type ReceiveAdmission,
  type ReceiveBlockReason,
  type ReceiveRuntimeSnapshot,
  type RetainedInventorySnapshot,
  type RetainedTaskSnapshot,
} from './contracts'
import { executeRetainedTaskAction, loadRetainedTasks } from './retained'

const PROGRESS_THROTTLE_MS = 100

export interface ReceiveRuntimePorts {
  readonly windowPort?: BrowserOutputWindow
  readonly databaseName?: string
  readonly opfsEnv?: OPFSEnvironment
  readonly outputTrace?: OutputTraceSource
}

export interface ReceiveCoordinatorOptions extends ReceiveRuntimePorts {
  readonly windowPort: BrowserOutputWindow
  readonly currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly onActionError?: (error: unknown) => void
}

/**
 * Single application orchestrator for browser receive operations.
 * Owns offer planning, picker-synchronous activation, active TransferJob lifecycle,
 * and retained task inventory over the unified `ReceiveStore`.
 */
export class ReceiveCoordinator {
  readonly #options: ReceiveCoordinatorOptions
  readonly #listeners = new Set<() => void>()
  readonly #lifetimeAbort = new AbortController()
  #storePromise: Promise<ReceiveStore> | undefined
  #store: ReceiveStore | undefined
  #capabilities: BrowserOutputCapabilities | undefined
  #capabilitiesPromise: Promise<BrowserOutputCapabilities> | undefined
  #offerEpoch = 0n
  #offerAbort: AbortController | undefined
  #offers: ArtifactOffers | null = null
  #selectionSpec: SelectionSpec | null = null
  #selectionPolicy: FrozenSelectionPolicy | null = null
  #activation: ActiveActivationSnapshot | null = null
  #activeJob: TransferJob | null = null
  #activeSession: OutputSessionHandle | null = null
  #activeTaskId: string | null = null
  #activeSelectionKey: string | null = null
  #backend: SaveBackendKind | null = null
  #isZipArchive = false
  #lifecycle: ReceiveLifecycleState | null = null
  #display: ReceiveOperationDisplay | null = null
  #safeRenamed: SafeRenamedSummary | null = null
  #sourceRevisionFailures: SourceRevisionFailures | null = null
  #progress: TransferProgress = EMPTY_TRANSFER_PROGRESS
  #interruption: 'pause' | 'stop' | null = null
  #retained: RetainedInventorySnapshot = EMPTY_RETAINED_INVENTORY
  #dismissedShareInstance: string | null = null
  #connectionUnavailable = false
  #shareEnded = false
  #progressTimer: ReturnType<typeof setTimeout> | undefined
  #snapshot: ReceiveRuntimeSnapshot
  #disposed = false

  constructor(options: ReceiveCoordinatorOptions) {
    this.#options = options
    this.#snapshot = this.#buildSnapshot()
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  readonly getSnapshot = (): ReceiveRuntimeSnapshot => this.#snapshot

  get active(): boolean {
    return this.#activeJob !== null || this.#activation !== null
  }

  get activeTaskId(): string | null {
    return this.#activeTaskId
  }

  get canReleaseCurrent(): boolean {
    return canReleaseCurrentTask(this.#lifecycle)
  }

  get canRetainCurrent(): boolean {
    return this.#activeJob === null && this.#lifecycle?.kind === 'paused'
  }

  observeConnection(kind: 'idle' | 'connected' | 'reconnecting' | 'unavailable' | 'ended'): void {
    const unavailable = kind === 'unavailable' || kind === 'ended'
    const ended = kind === 'ended'
    if (this.#connectionUnavailable === unavailable && this.#shareEnded === ended) return
    this.#connectionUnavailable = unavailable
    this.#shareEnded = ended
    this.#publish()
  }

  startBlockedReason(): ReceiveBlockReason | null {
    if (this.#options.currentJoinedShare() === undefined) return 'connectFirst'
    if (this.#activation !== null || this.#retained.pending !== null) return 'busy'
    if (!canStartNewDownload(this.#lifecycle)) return 'downloading'
    return null
  }

  shareNavigationBlockedReason(): ReceiveBlockReason | null {
    if (this.#activeJob !== null || this.#activation !== null) return 'downloading'
    if (this.#retained.pending !== null) return 'busy'
    return null
  }

  activeActionAdmission(action: ReceiveAction): ReceiveAdmission {
    if (this.#lifecycle === null || this.#backend === null) return blockedAdmission('actionUnavailable')
    if (this.#interruption !== null) return blockedAdmission('busy')
    const canSavePartial = canSavePartialState(this.#lifecycle, this.#backend, this.#isZipArchive)
    const available = actionsForState(this.#lifecycle, this.#backend, canSavePartial)
    if (!available.includes(action)) return blockedAdmission('actionUnavailable')
    if (action === 'continue') {
      if (this.#shareEnded) return blockedAdmission('shareEnded')
      if (this.#connectionUnavailable) return blockedAdmission('connectionUnavailable')
    }
    return ALLOWED_ADMISSION
  }

  retainedActionAdmission(task: RetainedTaskSnapshot, action: ReceiveAction): ReceiveAdmission {
    const shareInstance = this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null
    return admitRetainedAction(task, action, {
      currentShareInstance: shareInstance,
      hasActiveDownload: this.active,
      hasPendingAction: this.#retained.pending !== null,
      connectionUnavailable: this.#connectionUnavailable,
      shareEnded: this.#shareEnded,
    })
  }

  refreshOffers(joined: JoinedBrowserShare): void {
    if (this.#disposed) return
    this.#offerAbort?.abort()
    const abort = new AbortController()
    this.#offerAbort = abort
    const epoch = ++this.#offerEpoch
    const frozen = joined.selection.snapshot()
    const spec = selectionSpecFromPolicy(
      joined.descriptor.shareInstanceId,
      joined.descriptor.syntheticRootId,
      frozen,
    )
    const key = selectionKey(spec)
    this.#selectionSpec = spec
    this.#selectionPolicy = frozen
    this.#offers = Object.freeze({ kind: 'confirming', epoch, selectionKey: key })
    this.#adoptMatchingRetained()
    this.#publish()

    void this.#computeOffers(joined, frozen, key, epoch, abort.signal)
  }

  retryOffers(): void {
    const joined = this.#options.currentJoinedShare()
    if (joined !== undefined) this.refreshOffers(joined)
  }

  async loadRetained(): Promise<void> {
    if (this.#disposed) return
    const store = await this.#openStoreOptional()
    if (store === undefined) {
      this.#retained = Object.freeze({ kind: 'empty', tasks: Object.freeze([]), pending: null })
      this.#publish()
      return
    }
    this.#retained = Object.freeze({ ...this.#retained, kind: 'loading' })
    this.#publish()
    try {
      const tasks = await loadRetainedTasks({
        store,
        windowPort: this.#options.windowPort,
        ...(this.#options.opfsEnv === undefined ? {} : { opfsEnv: this.#options.opfsEnv }),
        ...(this.#options.windowPort.navigator?.locks === undefined
          ? {}
          : { locks: this.#options.windowPort.navigator.locks }),
      }, this.#activeTaskId)
      if (this.#disposed) return
      this.#retained = Object.freeze({
        kind: tasks.length === 0 ? 'empty' : 'available',
        tasks,
        pending: this.#retained.pending,
      })
      this.#adoptMatchingRetained()
      this.#publish()
    } catch {
      if (this.#disposed) return
      this.#retained = Object.freeze({ kind: 'failed', tasks: Object.freeze([]), pending: null })
      this.#publish()
    }
  }

  async ensureRetained(): Promise<void> {
    if (this.#retained.kind === 'unloaded' || this.#retained.kind === 'failed') {
      await this.loadRetained()
    }
  }

  /**
   * Starts a download for `choiceId`. For FSA routes, `showDirectoryPicker` is invoked
   * synchronously before this method returns so browsers preserve trusted user activation.
   */
  startDownload(choiceId: string, display?: ReceiveOperationDisplay): boolean {
    if (this.#disposed || this.startBlockedReason() !== null) return false
    const joined = this.#options.currentJoinedShare()
    const offers = this.#offers
    const spec = this.#selectionSpec
    if (joined === undefined || offers === null || offers.kind !== 'ready' || spec === null) return false
    // Preserve draft ancestry hints from the live snapshot so discovery verifies the
    // selected path first instead of scanning unrelated sibling directories.
    const selection = this.#selectionPolicy ?? policyFromSelectionSpec(spec)

    const allChoices = [offers.primary, ...offers.alternatives, ...(offers.zip === null ? [] : [offers.zip])]
    const choice = allChoices.find(candidate => candidate.choiceId === choiceId)
    if (choice === undefined) return false

    let pickedFolder: Promise<FileSystemDirectoryHandle> | undefined
    if (choice.backend === 'fsa') {
      // Claim activation ownership BEFORE calling showDirectoryPicker so synchronous
      // reentrant calls inside the picker cannot start a second competing flow.
      this.#activation = Object.freeze({
        kind: 'waiting-picker',
        choiceId: choice.choiceId,
        suggestedName: choice.suggestedName,
      })
      try {
        pickedFolder = startFSADirectoryPicker(this.#options.windowPort)
      } catch (error) {
        this.#activation = null
        this.#reportError(error)
        return false
      }
      pickedFolder.catch(() => undefined)
    } else {
      this.#activation = Object.freeze({
        kind: 'committing',
        choiceId: choice.choiceId,
        suggestedName: choice.suggestedName,
      })
    }
    this.#publish()
    void this.#executeNewDownload(joined, choice, spec, selection, offers.selectionKey, display, pickedFolder)
    return true
  }

  cancelActivation(): void {
    if (this.#activation === null) return
    this.#activation = null
    this.#publish()
  }

  startNewDownload(): void {
    if (this.#activeJob !== null || this.#activation !== null) return
    this.#dismissedShareInstance = this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null
    this.#activeTaskId = null
    this.#activeSelectionKey = null
    this.#backend = null
    this.#isZipArchive = false
    this.#lifecycle = null
    this.#display = null
    this.#safeRenamed = null
    this.#sourceRevisionFailures = null
    this.#progress = EMPTY_TRANSFER_PROGRESS
    this.#interruption = null
    this.#publish()
  }

  performActiveAction(action: ReceiveAction): void {
    if (!this.activeActionAdmission(action).allowed) return
    if (action === 'pause' && this.#activeJob !== null) {
      this.#interruption = 'pause'
      this.#activeJob.pause('user')
      this.#publish()
      return
    }
    if (action === 'stop' && this.#activeJob !== null) {
      this.#interruption = 'stop'
      this.#activeJob.stop()
      this.#publish()
      return
    }
    if (this.#activeTaskId !== null) {
      const retained = this.#retained.tasks.find(task => task.taskId === this.#activeTaskId)
      if (retained !== undefined) {
        this.performRetainedAction(retained, action)
        return
      }
      // Fallback when active task settled before retained refresh finished.
      void this.#runRetainedTaskIdAction(this.#activeTaskId, action)
    }
  }

  performRetainedAction(task: RetainedTaskSnapshot, action: ReceiveAction): void {
    if (!this.retainedActionAdmission(task, action).allowed) return
    void this.#runRetainedTaskIdAction(task.taskId, action)
  }

  suspendForNewShare(): void {
    this.#offerAbort?.abort()
    this.#offers = null
    this.#selectionSpec = null
    this.#selectionPolicy = null
    this.#activation = null
    this.#dismissedShareInstance = null
    if (this.#activeJob === null) {
      this.#activeTaskId = null
      this.#activeSelectionKey = null
      this.#backend = null
      this.#isZipArchive = false
      this.#lifecycle = null
      this.#display = null
      this.#safeRenamed = null
      this.#sourceRevisionFailures = null
      this.#progress = EMPTY_TRANSFER_PROGRESS
    }
    this.#publish()
  }

  async dispose(): Promise<void> {
    if (this.#disposed) return
    this.#disposed = true
    this.#lifetimeAbort.abort()
    this.#offerAbort?.abort()
    this.#activeJob?.stop()
    if (this.#progressTimer !== undefined) {
      clearTimeout(this.#progressTimer)
      this.#progressTimer = undefined
    }
    await this.#activeSession?.close().catch(() => undefined)
    this.#store?.close()
    this.#listeners.clear()
  }

  async #computeOffers(
    joined: JoinedBrowserShare,
    frozen: FrozenSelectionPolicy,
    key: string,
    epoch: bigint,
    signal: AbortSignal,
  ): Promise<void> {
    try {
      const [capabilities, shape] = await Promise.all([
        this.#ensureCapabilities(),
        joined.inspectSelection(frozen, signal),
      ])
      if (signal.aborted || this.#disposed || epoch !== this.#offerEpoch) return
      this.#offers = planArtifactOffers({ epoch, selectionKey: key, shape, capabilities })
      this.#publish()
    } catch (error) {
      if (signal.aborted || this.#disposed || epoch !== this.#offerEpoch) return
      this.#offers = Object.freeze({ kind: 'retry-required', epoch, selectionKey: key })
      this.#publish()
      if (!isAbortError(error)) this.#reportError(error)
    }
  }

  async #executeNewDownload(
    joined: JoinedBrowserShare,
    choice: ArtifactChoice,
    spec: SelectionSpec,
    selection: FrozenSelectionPolicy,
    key: string,
    display: ReceiveOperationDisplay | undefined,
    pickedFolder: Promise<FileSystemDirectoryHandle> | undefined,
  ): Promise<void> {
    let parentHandle: FileSystemDirectoryHandle | undefined
    if (pickedFolder !== undefined) {
      try {
        parentHandle = await pickedFolder
      } catch (error) {
        this.#activation = null
        this.#publish()
        if (!isAbortError(error)) this.#reportError(error)
        return
      }
      if (this.#disposed || this.#activation === null) return
      this.#activation = Object.freeze({
        kind: 'committing',
        choiceId: choice.choiceId,
        suggestedName: choice.suggestedName,
      })
      this.#publish()
    }

    const taskId = createTaskId()
    const initial = initialReceiveState(taskId)
    const taskInput: StartBackendTaskInput = Object.freeze({
      taskId,
      selection: spec,
      selectionKey: key,
      artifact: choice.artifact,
      lifecycle: initial,
      ...(display === undefined ? {} : { display }),
    })

    let session: OutputSessionHandle | undefined
    try {
      session = await this.#openNewSession(choice.backend, taskInput, parentHandle)
    } catch (error) {
      this.#activation = null
      this.#publish()
      this.#reportError(error)
      return
    }

    this.#activation = null
    await this.#runSession(
      joined,
      session,
      selection,
      key,
      choice.artifact.singleFileId !== undefined,
      display ?? null,
      initial,
    )
  }

  async #openNewSession(
    backend: SaveBackendKind,
    task: StartBackendTaskInput,
    parentHandle: FileSystemDirectoryHandle | undefined,
  ): Promise<OutputSessionHandle> {
    const diagnostics = this.#options.outputTrace === undefined
      ? undefined
      : Object.freeze({ backend: diagnosticBackendName(backend), trace: this.#options.outputTrace })
    const locks = this.#options.windowPort.navigator?.locks

    if (backend === 'portable') {
      const store = await this.#openStoreOptional()
      return openPortableSession({
        task,
        windowPort: this.#options.windowPort,
        ...(store === undefined ? {} : { store }),
        ...(diagnostics === undefined ? {} : { diagnostics }),
      })
    }

    const store = await this.#requireStore()
    if (backend === 'fsa') {
      if (parentHandle === undefined) {
        throw new DOMException('No folder was selected', 'AbortError')
      }
      return openNewFSASession({
        task,
        parentHandle,
        store,
        ...(locks === undefined ? {} : { locks }),
        ...(diagnostics === undefined ? {} : { diagnostics }),
      })
    }

    const opfsEnv = this.#options.opfsEnv ?? browserOPFSEnvironment(this.#options.windowPort.navigator?.storage, locks)
    return openNewOPFSSession({
      task,
      store,
      env: opfsEnv,
      windowPort: this.#options.windowPort,
      ...(diagnostics === undefined ? {} : { diagnostics }),
    })
  }

  async #runSession(
    joined: JoinedBrowserShare,
    session: OutputSessionHandle,
    selection: FrozenSelectionPolicy,
    activeSelectionKey: string,
    stripSingleFilePrefix: boolean,
    display: ReceiveOperationDisplay | null,
    initialLifecycle: ReceiveLifecycleState,
  ): Promise<void> {
    const connectivity = joined.beginDownloadConnectivity()
    const downloading = initialLifecycle.kind === 'downloading'
      ? initialLifecycle
      : transitionReceiveState(initialLifecycle, { kind: 'downloading', phase: 'receiving' })
    this.#activeSession = session
    this.#activeTaskId = session.taskId
    this.#activeSelectionKey = activeSelectionKey
    this.#backend = session.backend
    this.#isZipArchive = false
    this.#lifecycle = downloading
    this.#display = display
    this.#safeRenamed = null
    this.#sourceRevisionFailures = null
    this.#interruption = null
    this.#progress = EMPTY_TRANSFER_PROGRESS

    const job = joined.createTransferJob(session.transferOutput, connectivity, {
      selection,
      stripSingleFilePrefix,
      onProgress: progress => this.#observeProgress(progress),
    })
    this.#activeJob = job
    this.#publish()

    try {
      const result = await job.run()
      this.#flushProgressNow()
      if (result.status === 'completed' && this.#lifecycle?.kind === 'downloading') {
        this.#lifecycle = transitionReceiveState(this.#lifecycle, { kind: 'downloading', phase: 'finalizing' })
        this.#publish()
      }
      const settled = await session.settle(result)
      this.#isZipArchive = settled.artifact.kind === 'zip-archive'
      this.#lifecycle = settled.lifecycle
      this.#display = settled.display ?? this.#display
      this.#safeRenamed = settled.safeRenamed ?? null
      this.#sourceRevisionFailures = settled.sourceRevisionFailures ?? null
      this.#interruption = null
    } catch (error) {
      this.#interruption = null
      if (this.#lifecycle !== null) {
        this.#lifecycle = transitionReceiveState(this.#lifecycle, {
          kind: 'failed',
          reason: isAbortError(error) ? 'cancelled' : 'error',
          ...(error instanceof Error ? { detail: error.message } : {}),
        })
      }
      this.#reportError(error)
    } finally {
      this.#activeJob = null
      this.#activeSession = null
      await session.close().catch(() => undefined)
      try { connectivity.close() } catch { /* Best-effort connectivity release. */ }
      this.#publish()
      void this.loadRetained()
    }
  }

  async #runRetainedTaskIdAction(taskId: string, action: ReceiveAction): Promise<void> {
    const store = await this.#openStoreOptional()
    if (store === undefined) return

    const pending: PendingRetainedAction = Object.freeze({ taskId, action })
    this.#retained = Object.freeze({ ...this.#retained, pending })
    this.#publish()

    try {
      const outcome = await executeRetainedTaskAction({
        store,
        windowPort: this.#options.windowPort,
        ...(this.#options.opfsEnv === undefined ? {} : { opfsEnv: this.#options.opfsEnv }),
        ...(this.#options.windowPort.navigator?.locks === undefined
          ? {}
          : { locks: this.#options.windowPort.navigator.locks }),
      }, taskId, action)

      this.#retained = Object.freeze({ ...this.#retained, pending: null })
      if (outcome.kind === 'completed') {
        if (this.#activeTaskId === taskId) {
          if (action === 'forget' || action === 'discard') {
            this.startNewDownload()
          } else if (outcome.updated !== undefined) {
            this.#lifecycle = outcome.updated.lifecycle
            this.#publish()
          }
        }
        await this.loadRetained()
        return
      }

      const joined = this.#options.currentJoinedShare()
      if (joined === undefined || joined.descriptor.shareInstanceId !== outcome.task.shareInstance) {
        await outcome.session.close()
        throw new DOMException('Open the matching share link to continue this download', 'InvalidStateError')
      }
      const selection = policyFromSelectionSpec(outcome.task.selection)
      const stripSingle = outcome.task.artifact.singleFileId !== undefined
      await this.#runSession(
        joined,
        outcome.session,
        selection,
        outcome.task.selectionKey,
        stripSingle,
        outcome.task.display ?? null,
        outcome.task.lifecycle,
      )
    } catch (error) {
      this.#retained = Object.freeze({ ...this.#retained, pending: null })
      if (isPermissionDeniedError(error)) {
        // Keep paused with clear permission-needed state rather than losing progress.
        await this.loadRetained()
      }
      this.#publish()
      this.#reportError(error)
    }
  }

  #observeProgress(progress: TransferProgress): void {
    this.#progress = progress
    if (this.#progressTimer !== undefined) return
    this.#progressTimer = setTimeout(() => {
      this.#progressTimer = undefined
      if (!this.#disposed) this.#publish()
    }, PROGRESS_THROTTLE_MS)
  }

  #flushProgressNow(): void {
    if (this.#progressTimer !== undefined) {
      clearTimeout(this.#progressTimer)
      this.#progressTimer = undefined
    }
  }

  async #ensureCapabilities(): Promise<BrowserOutputCapabilities> {
    if (this.#capabilities !== undefined) return this.#capabilities
    // Capability probing is coordinator-scoped rather than selection-scoped so
    // switching selections cannot cancel and poison the shared probe promise.
    const pending = this.#capabilitiesPromise ??= probeBrowserOutputCapabilities(
      this.#options.windowPort,
      this.#lifetimeAbort.signal,
    )
    try {
      const caps = await pending
      this.#capabilities = caps
      return caps
    } catch (error) {
      if (this.#capabilitiesPromise === pending) this.#capabilitiesPromise = undefined
      throw error
    }
  }

  async #requireStore(): Promise<ReceiveStore> {
    if (this.#store !== undefined) return this.#store
    const pending = this.#storePromise ??= ReceiveStore.open(
      this.#options.databaseName ?? RECEIVE_DATABASE_NAME,
      this.#options.windowPort.indexedDB,
    )
    try {
      const store = await pending
      if (this.#disposed) {
        store.close()
        throw new DOMException('Receive coordinator disposed', 'AbortError')
      }
      this.#store = store
      return store
    } catch (error) {
      if (this.#storePromise === pending) this.#storePromise = undefined
      throw error
    }
  }

  async #openStoreOptional(): Promise<ReceiveStore | undefined> {
    if (typeof this.#options.windowPort.indexedDB?.open !== 'function') return undefined
    try {
      return await this.#requireStore()
    } catch {
      return undefined
    }
  }

  #reportError(error: unknown): void {
    if (!this.#disposed) this.#options.onActionError?.(error)
  }

  #buildSnapshot(): ReceiveRuntimeSnapshot {
    const canSavePartial = canSavePartialState(this.#lifecycle, this.#backend, this.#isZipArchive)
    const actions = this.#lifecycle === null || this.#backend === null || this.#interruption !== null
      ? Object.freeze([])
      : actionsForState(this.#lifecycle, this.#backend, canSavePartial)
    return Object.freeze({
      offers: this.#offers,
      activation: this.#activation,
      activeTaskId: isActiveReceiveState(this.#lifecycle ?? { kind: 'failed', taskId: '', generation: 0n, reason: 'error' })
        ? this.#activeTaskId
        : null,
      activeSelectionKey: this.#activeSelectionKey,
      backend: this.#backend,
      lifecycle: this.#lifecycle,
      display: this.#display,
      safeRenamed: this.#safeRenamed,
      sourceRevisionFailures: this.#sourceRevisionFailures,
      progress: this.#progress,
      actions,
      interruption: this.#interruption,
      retained: this.#retained,
    })
  }

  #adoptMatchingRetained(): void {
    if (this.#lifecycle !== null || this.#activeJob !== null || this.#activation !== null) return
    const joined = this.#options.currentJoinedShare()
    const offers = this.#offers
    if (joined === undefined || offers === null) return
    const shareInstance = joined.descriptor.shareInstanceId
    if (this.#dismissedShareInstance === shareInstance) return
    const candidates = this.#retained.tasks.filter(task =>
      task.shareInstance === shareInstance &&
      task.selectionKey === offers.selectionKey &&
      isAdoptableRetainedTask(task))
    if (candidates.length === 0) return
    candidates.sort((left, right) =>
      (right.display?.createdAtMilliseconds ?? 0) - (left.display?.createdAtMilliseconds ?? 0))
    const match = candidates[0]!
    this.#activeTaskId = match.taskId
    this.#activeSelectionKey = match.selectionKey
    this.#backend = match.backend
    this.#isZipArchive = match.canSavePartial
    this.#lifecycle = match.lifecycle
    this.#display = match.display ?? null
    this.#safeRenamed = match.safeRenamed ?? null
    this.#sourceRevisionFailures = match.sourceRevisionFailures ?? null
  }

  #publish(): void {
    this.#snapshot = this.#buildSnapshot()
    for (const listener of this.#listeners) {
      try { listener() } catch { /* Subscriber failure cannot break coordinator state. */ }
    }
  }
}

function isAdoptableRetainedTask(task: Pick<RetainedTaskSnapshot, 'backend' | 'lifecycle'>): boolean {
  const { lifecycle } = task
  return lifecycle.kind === 'paused' ||
    lifecycle.kind === 'ready-to-save' ||
    (lifecycle.kind === 'download-started' && lifecycle.retained) ||
    (task.backend === 'opfs' && lifecycle.kind === 'partial')
}
