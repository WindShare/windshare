import type { CatalogEntry } from '../../catalog/records'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { ArtifactOffers, ReceiveOperationDisplay } from '../../output/model'
import { ORIGINAL_FILE_MEDIA_TYPE, triggerBrowserDownload } from '../../output/backends/browser-handoff'
import {
  planArtifactOffers,
  safeIndexedDB,
  type BrowserOutputWindow,
  type OutputSessionHandle,
  type StartBackendTaskInput,
} from '../../output/backends/contract'
import { startFSADirectoryPicker } from '../../output/backends/fsa'
import type { OPFSEnvironment } from '../../output/backends/opfs'
import { handoffLocalPortableBlob, persistPortableHistory } from '../../output/backends/portable'
import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import type { SelectionShape } from '../../transfer/discovery'
import { classifyNativeError, isAbortError } from '../../transfer/fault'
import {
  artifactRootPath,
  createTaskId,
  policyFromSelectionSpec,
  selectionKey,
  selectionSpecFromPolicy,
  type SelectionSpec,
} from '../../transfer/selection'
import { initialReceiveState } from '../../transfer/state'
import type { BrowseDirectory, JoinedBrowserShare } from '../browser-share/gateway'
import { ALLOWED_ADMISSION, admitRetainedAction, blockedAdmission, canForgetState } from './admission'
import { BrowserCapabilityCache } from './capability-cache'
import type {
  ReceiveAction,
  ReceiveAdmission,
  ReceiveBlockReason,
  ReceiveRuntimeSnapshot,
  RetainedTaskSnapshot,
  SingleFileDownloadOutcome,
} from './contracts'
import { ExecutionState, WorkspaceTask, type LocalRetainedActionKind } from './execution-state'
import { ReceiveStorage, type ReceiveStoreAccess } from './receive-storage'
import { matchingRetainedTask } from './retained'
import { RetainedTaskActions, type RetainedActionCallbacks } from './retained-actions'
import { RetainedTaskInventory } from './retained-inventory'
import { openNewOutputSession } from './session-opener'
import { ReceiveSessionRunner, type SessionRunCallbacks } from './session-runner'
import { singleFilePickerFloorBytes } from './single-file'
import {
  startSingleFileDownloadFlow,
  type SingleFileDownloadOptions,
  type SingleFileLaunchContext,
  type SingleFileLaunchRequest,
} from './single-file-launch'
import type { BeforeUnloadEventTarget } from './unload-guard'
import type { ScreenWakeLockWindowTarget } from './wake-lock-guard'

export type { RetainedActionCallbacks } from './retained-actions'
export type { SingleFileDownloadOptions } from './single-file-launch'

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

/**
 * Single application orchestrator for browser receive operations.
 * Owns offer planning, picker-synchronous activation, active TransferJob lifecycle,
 * and retained task inventory over the unified `ReceiveStore`.
 */
export class ReceiveCoordinator {
  readonly #options: ReceiveCoordinatorOptions
  readonly #listeners = new Set<() => void>()
  readonly #lifetimeAbort = new AbortController()
  readonly #storage: ReceiveStorage
  readonly #capabilities: BrowserCapabilityCache
  readonly #execution = new ExecutionState()
  readonly #workspace = new WorkspaceTask()
  readonly #inventory: RetainedTaskInventory
  readonly #sessions: ReceiveSessionRunner
  readonly #retainedActions: RetainedTaskActions
  readonly #singleFileContext: SingleFileLaunchContext
  #offerEpoch = 0n
  #offerAbort: AbortController | undefined
  #offers: ArtifactOffers | null = null
  #lastShape: SelectionShape | null = null
  #selectionSpec: SelectionSpec | null = null
  #selectionPolicy: FrozenSelectionPolicy | null = null
  #dismissedShareInstance: string | null = null
  #connectionUnavailable = false
  #shareEnded = false
  #snapshot: ReceiveRuntimeSnapshot
  #disposed = false

  constructor(options: ReceiveCoordinatorOptions) {
    this.#options = options
    this.#storage = new ReceiveStorage(safeIndexedDB(options.windowPort), options.databaseName)
    this.#capabilities = new BrowserCapabilityCache({
      windowPort: options.windowPort,
      signal: this.#lifetimeAbort.signal,
      ...(options.outputTrace === undefined ? {} : { outputTrace: options.outputTrace }),
      isDisposed: () => this.#disposed,
      onStateChange: () => this.#publish(),
      onRecovered: () => this.#handleCapabilitiesRecovered(),
      onStorageEstimateChanged: () => this.#replanOffersForUpdatedStorage(),
    })
    this.#inventory = new RetainedTaskInventory({
      storage: this.#storage,
      windowPort: options.windowPort,
      ...(options.opfsEnv === undefined ? {} : { opfsEnv: options.opfsEnv }),
      ...(options.outputTrace === undefined ? {} : { outputTrace: options.outputTrace }),
      signal: this.#lifetimeAbort.signal,
      isDisposed: () => this.#disposed,
      isLocalTask: taskId => this.#execution.ownsTask(taskId),
      onFullLoaded: () => this.#adoptMatchingRetained(),
      onChange: () => this.#publish(),
    })
    this.#sessions = new ReceiveSessionRunner({
      windowPort: options.windowPort,
      execution: this.#execution,
      workspace: this.#workspace,
      ...(options.outputTrace === undefined ? {} : { outputTrace: options.outputTrace }),
      isDisposed: () => this.#disposed,
      publish: () => this.#publish(),
      onSettledRecord: record => this.#inventory.upsertRecord(record),
      onSessionFinished: () => {
        void this.#capabilities.refreshStorageEstimate()
        void this.loadRetained()
      },
      reportError: error => this.#reportError(error),
      dispatchAsyncError: (error, custom) => this.#dispatchAsyncError(error, custom),
    })
    this.#retainedActions = new RetainedTaskActions({
      storage: this.#storage,
      inventory: this.#inventory,
      execution: this.#execution,
      workspace: this.#workspace,
      windowPort: options.windowPort,
      ...(options.opfsEnv === undefined ? {} : { opfsEnv: options.opfsEnv }),
      ...(options.outputTrace === undefined ? {} : { outputTrace: options.outputTrace }),
      signal: this.#lifetimeAbort.signal,
      currentJoinedShare: options.currentJoinedShare,
      isDisposed: () => this.#disposed,
      publish: () => this.#publish(),
      clearActiveTask: () => this.startNewDownload(),
      closeDetached: session => this.#sessions.closeDetached(session),
      runSession: input => this.#sessions.run(input),
      refreshStorageEstimate: () => { void this.#capabilities.refreshStorageEstimate() },
      dispatchAsyncError: (error, custom) => this.#dispatchAsyncError(error, custom),
    })
    this.#singleFileContext = this.#createSingleFileContext()
    this.#snapshot = this.#buildSnapshot()
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  readonly getSnapshot = (): ReceiveRuntimeSnapshot => this.#snapshot

  get active(): boolean {
    return this.#execution.isTransferActive
  }

  get activeTaskId(): string | null {
    return this.#workspace.taskId
  }

  get canReleaseCurrent(): boolean {
    return !this.#execution.isRunning && this.#workspace.canRelease
  }

  get canRetainCurrent(): boolean {
    return !this.#execution.isRunning && this.#workspace.canRetain
  }

  observeConnection(kind: 'idle' | 'connected' | 'reconnecting' | 'unavailable' | 'ended'): void {
    const unavailable = kind === 'unavailable' || kind === 'ended'
    const ended = kind === 'ended'
    if (this.#connectionUnavailable === unavailable && this.#shareEnded === ended) return
    this.#connectionUnavailable = unavailable
    this.#shareEnded = ended
    this.#publish()
  }

  preloadShareTasks(shareInstance: string): void {
    if (this.#disposed) return
    if (this.#capabilities.state === 'failed') {
      this.#capabilities.retry()
    } else if (this.#capabilities.state === 'probing' && this.#capabilities.value === undefined) {
      void this.#capabilities.ensure().catch(() => undefined)
    } else if (this.#capabilities.state === 'ready') {
      void this.#capabilities.refreshStorageEstimate()
    }
    this.#inventory.preloadShare(
      shareInstance,
      typeof safeIndexedDB(this.#options.windowPort)?.open === 'function',
    )
  }

  startBlockedReason(): ReceiveBlockReason | null {
    return this.#execution.startBlockedReason(
      this.#options.currentJoinedShare() !== undefined,
      this.#workspace.canStartNew,
    )
  }

  shareNavigationBlockedReason(): ReceiveBlockReason | null {
    return this.#execution.shareNavigationBlockedReason()
  }

  activeActionAdmission(action: ReceiveAction): ReceiveAdmission {
    if (this.#workspace.lifecycle === null || this.#workspace.backend === null) {
      return blockedAdmission('actionUnavailable')
    }
    if (
      this.#workspace.interruption !== null ||
      (this.#execution.pendingRetained !== null && action !== 'pause' && action !== 'stop')
    ) {
      return blockedAdmission('busy')
    }
    const supported = this.#activeActions().includes(action) ||
      (!this.#sessions.waitingForLiveJob && action === 'forget' &&
        canForgetState(this.#workspace.lifecycle, this.#workspace.backend))
    if (!supported) return blockedAdmission('actionUnavailable')
    if (action === 'continue') {
      if (this.#shareEnded) return blockedAdmission('shareEnded')
      if (this.#connectionUnavailable) return blockedAdmission('connectionUnavailable')
    }
    return ALLOWED_ADMISSION
  }

  retainedActionAdmission(task: RetainedTaskSnapshot, action: ReceiveAction): ReceiveAdmission {
    if (this.#execution.activeTransferTaskId === task.taskId) return blockedAdmission('busy')
    return this.#admitRetained(task, action, this.#execution.isTransferActive)
  }

  refreshOffers(joined: JoinedBrowserShare): void {
    if (this.#disposed) return
    this.preloadShareTasks(joined.descriptor.shareInstanceId)
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
    this.#lastShape = null
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
    return this.#inventory.loadAll()
  }

  ensureRetained(): Promise<void> {
    return this.#inventory.ensureAll()
  }

  saveLocalBlob(blob: Blob, suggestedName: string): boolean {
    if (this.#disposed) return false
    try {
      triggerBrowserDownload(this.#options.windowPort, blob, suggestedName, ORIGINAL_FILE_MEDIA_TYPE)
      return true
    } catch (error) {
      this.#traceLocalBlobFailure(error)
      return false
    }
  }

  completeLocalBlobDownload(
    choiceId: string,
    blob: Blob,
    display?: ReceiveOperationDisplay,
  ): boolean {
    const resolved = this.#resolveReadyChoice(choiceId)
    if (
      resolved === undefined ||
      resolved.choice.backend !== 'portable' ||
      resolved.choice.artifact.kind !== 'original-file'
    ) {
      return false
    }
    const now = Date.now()
    const taskId = createTaskId()
    let record: ReturnType<typeof handoffLocalPortableBlob>
    try {
      record = handoffLocalPortableBlob({
        task: Object.freeze({
          taskId,
          selection: resolved.spec,
          selectionKey: resolved.key,
          artifact: resolved.choice.artifact,
          lifecycle: initialReceiveState(taskId, now),
          ...(display === undefined ? {} : { display }),
        }),
        blob,
        windowPort: this.#options.windowPort,
        ...(this.#options.outputTrace === undefined ? {} : { trace: this.#options.outputTrace }),
        now,
      })
    } catch (error) {
      this.#traceLocalBlobFailure(error)
      this.#reportError(error)
      return false
    }
    this.#dismissedShareInstance = null
    this.#workspace.completeSettled(record)
    this.#inventory.upsertRecord(record)
    this.#publish()
    void this.#storage.runTaskWrite(record.taskId, async access => {
      const store = await access.openStoreOptional()
      await persistPortableHistory(store, record, this.#options.outputTrace)
    }).catch(() => undefined)
    return true
  }

  startDownload(choiceId: string, display?: ReceiveOperationDisplay): boolean {
    const resolved = this.#resolveReadyChoice(choiceId)
    if (resolved === undefined) return false
    const selection = this.#selectionPolicy ?? policyFromSelectionSpec(resolved.spec)
    const launched = this.#launchChoice(
      { ...resolved, selection, display },
      undefined,
      true,
    )
    return launched.kind === 'started' || launched.kind === 'waiting-picker'
  }

  #resolveReadyChoice(choiceId: string) {
    if (this.#disposed || this.startBlockedReason() !== null) return undefined
    const joined = this.#options.currentJoinedShare()
    const offers = this.#offers
    const spec = this.#selectionSpec
    if (joined === undefined || offers === null || offers.kind !== 'ready' || spec === null) return undefined
    const allChoices = [offers.primary, ...offers.alternatives, ...(offers.zip === null ? [] : [offers.zip])]
    const choice = allChoices.find(candidate => candidate.choiceId === choiceId)
    if (choice === undefined) return undefined
    return { joined, choice, spec, key: offers.selectionKey }
  }

  startSingleFileDownload(
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    directory: BrowseDirectory,
    options?: SingleFileDownloadOptions,
  ): SingleFileDownloadOutcome {
    return startSingleFileDownloadFlow(this.#singleFileContext, entry, directory, options)
  }

  cancelActivation(): void {
    if (!this.#execution.cancelWaitingPicker()) return
    this.#publish()
  }

  startNewDownload(): void {
    if (this.#execution.isTransferActive) return
    this.#dismissedShareInstance = this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null
    this.#workspace.clear()
    this.#publish()
  }

  performActiveAction(action: ReceiveAction, callbacks?: RetainedActionCallbacks): void {
    if (!this.activeActionAdmission(action).allowed) return
    if ((action === 'pause' || action === 'stop') && this.#sessions.interrupt(action)) return
    const taskId = this.#workspace.taskId
    if (taskId === null) return
    const retained = this.#inventory.tasks.find(task => task.taskId === taskId)
    if (retained !== undefined) {
      this.performRetainedAction(retained, action, callbacks)
      return
    }
    this.#dispatchRetainedAction(taskId, action, callbacks)
  }

  performRetainedAction(task: RetainedTaskSnapshot, action: ReceiveAction, callbacks?: RetainedActionCallbacks): void {
    if (!this.retainedActionAdmission(task, action).allowed) return
    this.#dispatchRetainedAction(task.taskId, action, callbacks)
  }

  suspendForNewShare(): void {
    this.#offerAbort?.abort()
    this.#offers = null
    this.#lastShape = null
    this.#selectionSpec = null
    this.#selectionPolicy = null
    this.#execution.clearPreRunningTransfer()
    this.#inventory.resetShare()
    this.#dismissedShareInstance = null
    if (!this.#execution.isRunning) this.#workspace.clear()
    this.#publish()
  }

  dispose(): Promise<void> {
    if (this.#disposed) return this.#storage.close()
    this.#disposed = true
    this.#listeners.clear()
    this.#lifetimeAbort.abort()
    this.#offerAbort?.abort()
    this.#lastShape = null
    this.#sessions.dispose()
    this.#execution.dispose()
    this.#workspace.clearActiveIfRunning()
    this.#inventory.resetShare()
    this.#snapshot = this.#buildSnapshot()
    return this.#storage.close()
  }

  #createSingleFileContext(): SingleFileLaunchContext {
    return {
      execution: this.#execution,
      inventory: this.#inventory,
      capabilities: this.#capabilities,
      retainedActions: this.#retainedActions,
      ...(this.#options.outputTrace === undefined ? {} : { outputTrace: this.#options.outputTrace }),
      isDisposed: () => this.#disposed,
      currentJoinedShare: () => this.#options.currentJoinedShare(),
      startBlockedReason: () => this.startBlockedReason(),
      preloadShareTasks: shareInstance => this.preloadShareTasks(shareInstance),
      admitCandidate: (task, action, ignoreTransferSlot) =>
        this.#admitRetained(task, action, ignoreTransferSlot ? this.#execution.isRunning : this.#execution.isTransferActive),
      publish: () => this.#publish(),
      launchChoice: (request, options) => this.#launchChoice(request, launchCallbacks(options), false),
      openWithTransferToken: (token, request, options) =>
        this.#storage.run(access => this.#openDownload(access, request, token, undefined, launchCallbacks(options))),
      dispatchAsyncError: (error, custom) => this.#dispatchAsyncError(error, custom),
    }
  }

  #dispatchRetainedAction(
    taskId: string,
    action: ReceiveAction,
    callbacks: RetainedActionCallbacks | undefined,
  ): void {
    if (action === 'continue') {
      void this.#retainedActions.runContinue(taskId, undefined, callbacks)
      return
    }
    if (isLocalRetainedAction(action)) {
      void this.#retainedActions.runLocalAction(taskId, action, undefined, callbacks)
    }
  }

  #admitRetained(task: RetainedTaskSnapshot, action: ReceiveAction, hasActiveDownload: boolean): ReceiveAdmission {
    const shareInstance = this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null
    return admitRetainedAction(task, action, {
      currentShareInstance: shareInstance,
      hasActiveDownload,
      hasPendingAction: this.#execution.pendingRetained !== null,
      connectionUnavailable: this.#connectionUnavailable,
      shareEnded: this.#shareEnded,
    })
  }

  #launchChoice(
    request: SingleFileLaunchRequest,
    callbacks: SessionRunCallbacks | undefined,
    reportSyncPickerError: boolean,
  ): Extract<SingleFileDownloadOutcome, { kind: 'started' | 'waiting-picker' | 'failed' }> {
    const kind = request.choice.backend === 'fsa' ? 'waiting-picker' : 'opening'
    const token = this.#execution.claimLaunch(
      kind,
      request.choice.choiceId,
      request.choice.suggestedName,
      request.choice.artifact.singleFileId,
    )
    if (token === null) {
      return Object.freeze({ kind: 'failed', error: new Error('Another receive operation is already active') })
    }
    let pickedFolder: Promise<FileSystemDirectoryHandle> | undefined
    if (kind === 'waiting-picker') {
      try {
        pickedFolder = startFSADirectoryPicker(this.#options.windowPort)
      } catch (error) {
        if (this.#execution.releaseTransferIfOwned(token) && reportSyncPickerError) {
          this.#reportError(error)
        }
        return Object.freeze({ kind: 'failed', error })
      }
    }
    this.#publish()
    void this.#executeNewDownload(request, token, pickedFolder, callbacks)
    return Object.freeze({ kind: kind === 'waiting-picker' ? 'waiting-picker' : 'started' })
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
        this.#capabilities.ensure(),
        joined.inspectSelection(frozen, signal),
      ])
      if (signal.aborted || this.#disposed || epoch !== this.#offerEpoch) return
      this.#lastShape = shape
      this.#offers = planArtifactOffers({ epoch, selectionKey: key, shape, capabilities })
      this.#publish()
    } catch (error) {
      if (signal.aborted || this.#disposed || epoch !== this.#offerEpoch) return
      this.#lastShape = null
      this.#offers = Object.freeze({ kind: 'retry-required', epoch, selectionKey: key })
      this.#publish()
      if (!isAbortError(error)) this.#reportError(error)
    }
  }

  #handleCapabilitiesRecovered(): void {
    if (this.#disposed || this.#offers?.kind !== 'retry-required') return
    const joined = this.#options.currentJoinedShare()
    if (joined !== undefined) this.refreshOffers(joined)
  }

  #replanOffersForUpdatedStorage(): void {
    if (this.#disposed || this.#lastShape === null || this.#offers === null) return
    const capabilities = this.#capabilities.value
    if (capabilities === undefined) return
    this.#offers = planArtifactOffers({
      epoch: this.#offerEpoch,
      selectionKey: this.#offers.selectionKey,
      shape: this.#lastShape,
      capabilities,
    })
    this.#publish()
  }

  async #executeNewDownload(
    request: SingleFileLaunchRequest,
    token: symbol,
    pickedFolder: Promise<FileSystemDirectoryHandle> | undefined,
    callbacks: SessionRunCallbacks | undefined,
  ): Promise<void> {
    let parentHandle: FileSystemDirectoryHandle | undefined
    if (pickedFolder !== undefined) {
      try {
        parentHandle = await pickedFolder
      } catch (error) {
        if (!this.#execution.releaseTransferIfOwned(token)) return
        this.#publish()
        if (!isAbortError(error)) this.#dispatchAsyncError(error, callbacks?.onAsyncError)
        return
      }
      if (!this.#execution.transitionToOpening(token)) return
      this.#publish()
    }
    await this.#storage.run(access => this.#openDownload(access, request, token, parentHandle, callbacks))
  }

  async #openDownload(
    access: ReceiveStoreAccess,
    request: SingleFileLaunchRequest,
    token: symbol,
    parentHandle: FileSystemDirectoryHandle | undefined,
    callbacks: SessionRunCallbacks | undefined,
  ): Promise<void> {
    if (!this.#execution.ownsTransfer(token)) return
    const { joined, choice, spec, selection, key, display } = request
    const taskId = createTaskId()
    const initial = initialReceiveState(taskId)
    const taskInput: StartBackendTaskInput = Object.freeze({
      taskId, selection: spec, selectionKey: key, artifact: choice.artifact, lifecycle: initial,
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
        ...access,
        signal: this.#lifetimeAbort.signal,
      })
    } catch (error) {
      if (!this.#execution.releaseTransferIfOwned(token)) return
      this.#publish()
      this.#dispatchAsyncError(error, callbacks?.onAsyncError)
      return
    }

    if (!this.#execution.ownsTransfer(token)) return this.#sessions.closeDetached(session)
    await this.#sessions.run({
      transferToken: token,
      joined,
      session,
      selection,
      activeSelectionKey: key,
      rootPath: artifactRootPath(choice.artifact),
      singleFileId: choice.artifact.singleFileId ?? null,
      display: display ?? null,
      initialLifecycle: initial,
      ...(callbacks === undefined ? {} : { callbacks }),
    })
  }

  #dispatchAsyncError(error: unknown, custom?: (error: unknown) => void): void {
    if (this.#disposed) return
    if (custom !== undefined) custom(error)
    else this.#reportError(error)
  }

  #reportError(error: unknown): void {
    if (!this.#disposed) this.#options.onActionError?.(error)
  }

  #traceLocalBlobFailure(error: unknown): void {
    emitOutputTrace(this.#options.outputTrace, 'publication', {
      backend: 'portable',
      transition: 'local_blob_failed',
      error_class: classifyNativeError(error),
    })
  }

  #activeActions(): readonly ReceiveAction[] {
    return this.#workspace.activeActions(this.#sessions.waitingForLiveJob)
  }

  #buildSnapshot(): ReceiveRuntimeSnapshot {
    const shareInstance = this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null
    const pickerPending =
      this.#capabilities.state === 'ready' &&
      shareInstance !== null &&
      this.#inventory.isShareLoading(shareInstance)
        ? singleFilePickerFloorBytes(this.#capabilities.value)
        : null
    return Object.freeze({
      offers: this.#offers,
      capabilitiesState: this.#capabilities.state,
      singleFilePickerPendingFromBytes: pickerPending,
      activation: this.#execution.activation,
      activeTaskId: this.#execution.isRunning ? this.#workspace.activeTaskId() : null,
      activeSingleFileId: this.#execution.activeSingleFileId,
      activeSelectionKey: this.#workspace.selectionKey,
      backend: this.#workspace.backend,
      lifecycle: this.#workspace.lifecycle,
      display: this.#workspace.display,
      safeRenamed: this.#workspace.safeRenamed,
      sourceRevisionFailures: this.#workspace.sourceRevisionFailures,
      outageCheckpointed: this.#sessions.outageCheckpointed,
      progress: this.#workspace.progress,
      actions: this.#activeActions(),
      interruption: this.#workspace.interruption,
      retained: this.#inventory.snapshot(this.#execution.pendingRetained),
    })
  }

  #adoptMatchingRetained(): void {
    if (this.#workspace.lifecycle !== null || this.#execution.isTransferActive) return
    const joined = this.#options.currentJoinedShare()
    const offers = this.#offers
    if (joined === undefined || offers === null) return
    const shareInstance = joined.descriptor.shareInstanceId
    if (this.#dismissedShareInstance === shareInstance) return
    const match = matchingRetainedTask(this.#inventory.tasks, shareInstance, offers.selectionKey)
    if (match !== undefined) this.#workspace.adoptRetained(match)
  }

  #publish(): void {
    if (this.#disposed) return
    this.#snapshot = this.#buildSnapshot()
    for (const listener of this.#listeners) {
      try { listener() } catch { /* Subscriber failure cannot break coordinator state. */ }
    }
  }
}

function isLocalRetainedAction(action: ReceiveAction): action is LocalRetainedActionKind {
  return action === 'save' || action === 'save-partial' || action === 'discard' || action === 'forget'
}

function launchCallbacks(options: SingleFileDownloadOptions | undefined): SessionRunCallbacks {
  return {
    ...(options?.onActivated === undefined ? {} : { onActivated: options.onActivated }),
    ...(options?.onAsyncError === undefined ? {} : { onAsyncError: options.onAsyncError }),
  }
}
