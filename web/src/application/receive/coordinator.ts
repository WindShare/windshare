import type { ArtifactChoice, ArtifactOffers, ReceiveOperationDisplay, SaveBackendKind } from '../../output/model'
import {
  planArtifactOffers,
  probeBrowserOutputCapabilities,
  type BrowserOutputCapabilities,
  type BrowserOutputWindow,
  type OutputSessionHandle,
  type StartBackendTaskInput,
} from '../../output/backends/contract'
import { startFSADirectoryPicker } from '../../output/backends/fsa'
import type { OPFSEnvironment } from '../../output/backends/opfs'
import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import { DisconnectCheckpointGuard } from './disconnect-checkpoint-guard'
import { ReceiveStorage, type ReceiveStoreAccess } from './receive-storage'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { TransferJob, TransferJobResult } from '../../transfer/job'
import { EMPTY_TRANSFER_PROGRESS, type TransferProgress } from '../../transfer/progress'
import {
  artifactRootPath,
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
import { executeRetainedTaskAction, loadRetainedTasks, matchingRetainedTask, reopenRetainedTask, retainedTaskEnvironment } from './retained'
import { closeOutputSession, openNewOutputSession } from './session-opener'
import { BeforeUnloadGuard, type BeforeUnloadEventTarget } from './unload-guard'
import { ScreenWakeLockGuard, type ScreenWakeLockWindowTarget } from './wake-lock-guard'

const PROGRESS_THROTTLE_MS = 100

export type ReceiveWindowPort =
  BrowserOutputWindow & Partial<BeforeUnloadEventTarget> & ScreenWakeLockWindowTarget

export interface ReceiveRuntimePorts {
  readonly windowPort?: ReceiveWindowPort
  readonly databaseName?: string
  readonly opfsEnv?: OPFSEnvironment
  readonly outputTrace?: OutputTraceSource
}

export interface ReceiveCoordinatorOptions extends ReceiveRuntimePorts {
  readonly windowPort: ReceiveWindowPort
  readonly currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly onActionError?: (error: unknown) => void
}

interface ActiveReceiveSession {
  readonly taskId: string
  job?: TransferJob
}

interface DownloadRequest {
  readonly joined: JoinedBrowserShare
  readonly choice: ArtifactChoice
  readonly spec: SelectionSpec
  readonly selection: FrozenSelectionPolicy
  readonly key: string
  readonly display: ReceiveOperationDisplay | undefined
}

/**
 * Single application orchestrator for browser receive operations.
 * Owns offer planning, picker-synchronous activation, active TransferJob lifecycle,
 * and retained task inventory over the unified `ReceiveStore`.
 */
export class ReceiveCoordinator {
  readonly #options: ReceiveCoordinatorOptions
  readonly #unloadGuard: BeforeUnloadGuard
  readonly #wakeLockGuard: ScreenWakeLockGuard
  readonly #listeners = new Set<() => void>()
  readonly #lifetimeAbort = new AbortController()
  readonly #storage: ReceiveStorage
  #capabilities: BrowserOutputCapabilities | undefined
  #capabilitiesPromise: Promise<BrowserOutputCapabilities> | undefined
  #offerEpoch = 0n
  #offerAbort: AbortController | undefined
  #offers: ArtifactOffers | null = null
  #selectionSpec: SelectionSpec | null = null
  #selectionPolicy: FrozenSelectionPolicy | null = null
  #activation: ActiveActivationSnapshot | null = null
  #session: ActiveReceiveSession | null = null
  #disconnectGuard: DisconnectCheckpointGuard | undefined
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
    this.#storage = new ReceiveStorage(options.windowPort.indexedDB, options.databaseName)
    this.#unloadGuard = new BeforeUnloadGuard(options.windowPort)
    this.#wakeLockGuard = new ScreenWakeLockGuard(options.windowPort)
    this.#snapshot = this.#buildSnapshot()
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  readonly getSnapshot = (): ReceiveRuntimeSnapshot => this.#snapshot

  get active(): boolean {
    return this.#session !== null || this.#activation !== null
  }

  get activeTaskId(): string | null {
    return this.#activeTaskId
  }

  get canReleaseCurrent(): boolean {
    return this.#session === null && canReleaseCurrentTask(this.#lifecycle)
  }

  get canRetainCurrent(): boolean {
    return this.#session === null && this.#lifecycle?.kind === 'paused'
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
    if (this.#session !== null || !canStartNewDownload(this.#lifecycle)) return 'downloading'
    return null
  }

  shareNavigationBlockedReason(): ReceiveBlockReason | null {
    if (this.#session !== null || this.#activation !== null) return 'downloading'
    if (this.#retained.pending !== null) return 'busy'
    return null
  }

  activeActionAdmission(action: ReceiveAction): ReceiveAdmission {
    if (this.#lifecycle === null || this.#backend === null) return blockedAdmission('actionUnavailable')
    if (this.#interruption !== null || (this.#retained.pending !== null && action !== 'pause' && action !== 'stop')) {
      return blockedAdmission('busy')
    }
    if (!this.#activeActions().includes(action)) return blockedAdmission('actionUnavailable')
    if (action === 'continue') {
      if (this.#shareEnded) return blockedAdmission('shareEnded')
      if (this.#connectionUnavailable) return blockedAdmission('connectionUnavailable')
    }
    return ALLOWED_ADMISSION
  }

  retainedActionAdmission(task: RetainedTaskSnapshot, action: ReceiveAction): ReceiveAdmission {
    if (this.#session?.taskId === task.taskId) return blockedAdmission('busy')
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

  loadRetained(): Promise<void> {
    return this.#storage.run(access => this.#loadRetained(access))
  }

  async #loadRetained(access: ReceiveStoreAccess): Promise<void> {
    if (this.#disposed) return
    const store = await access.openStoreOptional()
    if (this.#disposed) return
    if (store === undefined) {
      this.#retained = Object.freeze({ kind: 'empty', tasks: Object.freeze([]), pending: null })
      this.#publish()
      return
    }
    this.#retained = Object.freeze({ ...this.#retained, kind: 'loading' })
    this.#publish()
    try {
      const env = retainedTaskEnvironment(store, this.#options, this.#lifetimeAbort.signal)
      const tasks = await loadRetainedTasks(env, this.#session?.taskId ?? null)
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

    // The picker must retain this click's identity even if a cancelled prompt settles during a later download.
    const activation: ActiveActivationSnapshot = Object.freeze({
      kind: choice.backend === 'fsa' ? 'waiting-picker' : 'committing',
      choiceId: choice.choiceId,
      suggestedName: choice.suggestedName,
    })
    this.#activation = activation
    let pickedFolder: Promise<FileSystemDirectoryHandle> | undefined
    if (activation.kind === 'waiting-picker') {
      try {
        pickedFolder = startFSADirectoryPicker(this.#options.windowPort)
      } catch (error) {
        if (this.#ownsActivation(activation)) {
          this.#activation = null
          this.#reportError(error)
        }
        return false
      }
    }
    this.#publish()
    const request = { joined, choice, spec, selection, key: offers.selectionKey, display }
    void this.#executeNewDownload(request, activation, pickedFolder)
    return true
  }

  cancelActivation(): void {
    if (this.#activation?.kind !== 'waiting-picker') return
    this.#activation = null
    this.#publish()
  }

  startNewDownload(): void {
    if (this.#session !== null || this.#activation !== null) return
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
    const job = this.#session?.job
    if ((action === 'pause' || action === 'stop') && job !== undefined) {
      this.#interruption = action
      if (action === 'pause') job.pause('user')
      else job.stop()
      this.#closeDisconnectGuard()
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
    if (this.#session === null) {
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

  dispose(): Promise<void> {
    if (this.#disposed) return this.#storage.close()
    this.#disposed = true
    this.#listeners.clear()
    this.#lifetimeAbort.abort()
    this.#unloadGuard.dispose()
    this.#wakeLockGuard.dispose()
    this.#offerAbort?.abort()
    this.#session?.job?.detach()
    this.#closeDisconnectGuard()
    if (this.#progressTimer !== undefined) {
      clearTimeout(this.#progressTimer)
      this.#progressTimer = undefined
    }
    this.#snapshot = this.#buildSnapshot()
    return this.#storage.close()
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

  #ownsActivation(activation: ActiveActivationSnapshot): boolean {
    return !this.#disposed && this.#activation === activation
  }

  async #executeNewDownload(
    request: DownloadRequest,
    activation: ActiveActivationSnapshot,
    pickedFolder: Promise<FileSystemDirectoryHandle> | undefined,
  ): Promise<void> {
    let parentHandle: FileSystemDirectoryHandle | undefined
    let committing = activation
    if (pickedFolder !== undefined) {
      try {
        parentHandle = await pickedFolder
      } catch (error) {
        if (!this.#ownsActivation(activation)) return
        this.#activation = null
        this.#publish()
        if (!isAbortError(error)) this.#reportError(error)
        return
      }
      if (!this.#ownsActivation(activation)) return
      committing = Object.freeze({ ...activation, kind: 'committing' })
      this.#activation = committing
      this.#publish()
    }

    await this.#storage.run(access => this.#openDownload(access, request, committing, parentHandle))
  }

  async #openDownload(
    access: ReceiveStoreAccess,
    request: DownloadRequest,
    activation: ActiveActivationSnapshot,
    parentHandle: FileSystemDirectoryHandle | undefined,
  ): Promise<void> {
    if (!this.#ownsActivation(activation)) return
    const { joined, choice, spec, selection, key, display } = request
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
      session = await openNewOutputSession({
        backend: choice.backend,
        task: taskInput,
        parentHandle,
        windowPort: this.#options.windowPort,
        ...(this.#options.opfsEnv === undefined ? {} : { opfsEnv: this.#options.opfsEnv }),
        ...(this.#options.outputTrace === undefined ? {} : { outputTrace: this.#options.outputTrace }),
        ...access, signal: this.#lifetimeAbort.signal,
      })
    } catch (error) {
      if (!this.#ownsActivation(activation)) return
      this.#activation = null
      this.#publish()
      this.#reportError(error)
      return
    }

    if (!this.#ownsActivation(activation)) return this.#closeDetachedSession(session)
    this.#activation = null
    await this.#runSession(
      joined,
      session,
      selection,
      key,
      artifactRootPath(choice.artifact),
      display ?? null,
      initial,
    )
  }

  async #runSession(
    joined: JoinedBrowserShare,
    session: OutputSessionHandle,
    selection: FrozenSelectionPolicy,
    activeSelectionKey: string,
    rootPath: readonly string[],
    display: ReceiveOperationDisplay | null,
    initialLifecycle: ReceiveLifecycleState,
  ): Promise<void> {
    if (this.#disposed) return this.#closeDetachedSession(session)
    let connectivity: ReturnType<JoinedBrowserShare['beginDownloadConnectivity']> | undefined
    const downloading = initialLifecycle.kind === 'downloading'
      ? initialLifecycle
      : transitionReceiveState(initialLifecycle, { kind: 'downloading', phase: 'receiving' })
    // Local actions must wait for the output lock even when connectivity or job creation fails.
    const owner: ActiveReceiveSession = { taskId: session.taskId }
    this.#session = owner
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

    try {
      connectivity = joined.beginDownloadConnectivity()
      const job = joined.createTransferJob(session.transferOutput, connectivity, {
        selection, rootPath,
        onProgress: progress => this.#observeProgress(progress),
        onUncheckpointedWrite: () => this.#disconnectGuard?.invalidate(),
      })
      owner.job = job
      if (session.transferOutput.capabilities.resumable) {
        this.#disconnectGuard = new DisconnectCheckpointGuard(joined, {
          job, taskId: session.taskId, changed: () => this.#publish(),
          ...(this.#options.outputTrace === undefined ? {} : { trace: this.#options.outputTrace }),
        })
      }
      this.#unloadGuard.setArmed(true)
      this.#wakeLockGuard.setArmed(true)
      this.#publish()
      const result = await job.run()
      this.#closeDisconnectGuard()
      await this.#settleSession(session, result)
    } catch (error) {
      this.#failSession(error)
    } finally {
      this.#closeDisconnectGuard()
      this.#unloadGuard.setArmed(false)
      this.#wakeLockGuard.setArmed(false)
      await closeOutputSession(session, this.#options.outputTrace)
      try { connectivity?.close() } catch { /* Best-effort connectivity release. */ }
      this.#session = null
      this.#publish()
      if (!this.#disposed) void this.loadRetained()
    }
  }

  async #settleSession(session: OutputSessionHandle, result: TransferJobResult): Promise<void> {
    this.#flushProgressNow()
    if (result.status === 'detached') {
      this.#traceDetached(session)
      return
    }
    if (this.#lifecycle?.kind === 'downloading') {
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
  }

  #failSession(error: unknown): void {
    this.#interruption = null
    if (this.#lifecycle !== null) {
      this.#lifecycle = transitionReceiveState(this.#lifecycle, {
        kind: 'failed',
        reason: isAbortError(error) ? 'cancelled' : 'error',
        ...(error instanceof Error ? { detail: error.message } : {}),
      })
    }
    this.#reportError(error)
  }

  #closeDisconnectGuard(): void {
    this.#disconnectGuard?.close()
    this.#disconnectGuard = undefined
  }

  #closeDetachedSession(session: OutputSessionHandle): Promise<void> {
    this.#traceDetached(session)
    return closeOutputSession(session, this.#options.outputTrace)
  }

  #traceDetached(session: OutputSessionHandle): void {
    emitOutputTrace(this.#options.outputTrace, 'settlement', { task_id: session.taskId, transition: 'detached' })
  }

  #runRetainedTaskIdAction(taskId: string, action: ReceiveAction): Promise<void> {
    if (this.#disposed || this.#retained.pending !== null) return Promise.resolve()
    const pending: PendingRetainedAction = Object.freeze({ taskId, action })
    this.#retained = Object.freeze({ ...this.#retained, pending })
    this.#publish()
    return this.#storage.run(access => this.#executeRetainedAction(access, taskId, action))
  }

  async #executeRetainedAction(access: ReceiveStoreAccess, taskId: string, action: ReceiveAction): Promise<void> {
    try {
      const store = await access.requireStore()
      if (this.#disposed) return
      const env = retainedTaskEnvironment(store, this.#options, this.#lifetimeAbort.signal)
      if (action === 'continue') {
        const { joined, task, session } = await reopenRetainedTask(env, taskId, this.#options.currentJoinedShare)
        this.#retained = Object.freeze({ ...this.#retained, pending: null })
        await this.#runSession(joined, session, policyFromSelectionSpec(task.selection), task.selectionKey,
          artifactRootPath(task.artifact), task.display ?? null, task.lifecycle)
        return
      }
      const outcome = await executeRetainedTaskAction(env, taskId, action)
      if (this.#disposed) return
      this.#retained = Object.freeze({ ...this.#retained, pending: null })
      if (this.#activeTaskId === taskId) {
        if (action === 'forget' || action === 'discard') this.startNewDownload()
        else if (outcome.updated !== undefined) {
          this.#lifecycle = outcome.updated.lifecycle
          this.#publish()
        }
      }
      await this.loadRetained()
    } catch (error) {
      if (this.#disposed) return
      this.#retained = Object.freeze({ ...this.#retained, pending: null })
      if (action === 'continue' || isPermissionDeniedError(error)) {
        // Re-read durable state after a failed open or a lost permission.
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

  #reportError(error: unknown): void {
    if (!this.#disposed) this.#options.onActionError?.(error)
  }

  #activeActions(): readonly ReceiveAction[] {
    if (this.#session !== null && (this.#session.job === undefined || this.#lifecycle?.kind !== 'downloading')) return []
    const canSavePartial = canSavePartialState(this.#lifecycle, this.#backend, this.#isZipArchive)
    return this.#lifecycle === null || this.#backend === null || this.#interruption !== null
      ? Object.freeze([])
      : actionsForState(this.#lifecycle, this.#backend, canSavePartial)
  }

  #buildSnapshot(): ReceiveRuntimeSnapshot {
    return Object.freeze({
      offers: this.#offers,
      activation: this.#activation,
      activeTaskId: this.#lifecycle !== null && isActiveReceiveState(this.#lifecycle)
        ? this.#activeTaskId
        : null,
      activeSelectionKey: this.#activeSelectionKey,
      backend: this.#backend,
      lifecycle: this.#lifecycle,
      display: this.#display,
      safeRenamed: this.#safeRenamed,
      sourceRevisionFailures: this.#sourceRevisionFailures,
      outageCheckpointed: this.#disconnectGuard?.checkpointed ?? false,
      progress: this.#progress,
      actions: this.#activeActions(),
      interruption: this.#interruption,
      retained: this.#retained,
    })
  }

  #adoptMatchingRetained(): void {
    if (this.#lifecycle !== null || this.#session !== null || this.#activation !== null) return
    const joined = this.#options.currentJoinedShare()
    const offers = this.#offers
    if (joined === undefined || offers === null) return
    const shareInstance = joined.descriptor.shareInstanceId
    if (this.#dismissedShareInstance === shareInstance) return
    const match = matchingRetainedTask(this.#retained.tasks, shareInstance, offers.selectionKey)
    if (match === undefined) return
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
    if (this.#disposed) return
    this.#snapshot = this.#buildSnapshot()
    for (const listener of this.#listeners) {
      try { listener() } catch { /* Subscriber failure cannot break coordinator state. */ }
    }
  }
}
