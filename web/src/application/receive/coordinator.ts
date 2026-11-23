import type { CatalogEntry } from '../../catalog/records'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { ArtifactOffers, ReceiveOperationDisplay } from '../../output/model'
import { ORIGINAL_FILE_MEDIA_TYPE, triggerBrowserDownload } from '../../output/backends/browser-handoff'
import {
  planArtifactOffers,
  type BrowserOutputWindow,
  type OutputSessionHandle,
  type StartBackendTaskInput,
} from '../../output/backends/contract'
import { startFSADirectoryPicker } from '../../output/backends/fsa'
import type { OPFSEnvironment } from '../../output/backends/opfs'
import type { OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveTaskRecord } from '../../output/store/types'
import { isAbortError } from '../../transfer/fault'
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
import { ALLOWED_ADMISSION, admitRetainedAction, blockedAdmission } from './admission'
import { BrowserCapabilityCache } from './capability-cache'
import type {
  ActiveActivationSnapshot,
  ReceiveAction,
  ReceiveAdmission,
  ReceiveBlockReason,
  ReceiveRuntimeSnapshot,
  RetainedTaskSnapshot,
  SingleFileDownloadOutcome,
} from './contracts'
import { ReceiveStorage, type ReceiveStoreAccess } from './receive-storage'
import { matchingRetainedTask, resolveRetainedCandidates, retainedTaskEnvironment } from './retained'
import { RetainedTaskRunner } from './retained-runner'
import { openNewOutputSession } from './session-opener'
import { ReceiveSessionRunner, type SessionRunCallbacks } from './session-runner'
import { ShareTaskIndex } from './share-task-index'
import { singleFilePickerFloorBytes } from './single-file'
import {
  startSingleFileDownloadFlow,
  type SingleFileDownloadOptions,
  type SingleFileLaunchHost,
  type SingleFileLaunchRequest,
} from './single-file-launch'
import { ReceiveTaskState } from './task-state'
import type { BeforeUnloadEventTarget } from './unload-guard'
import type { ScreenWakeLockWindowTarget } from './wake-lock-guard'

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
  readonly #shareIndex = new ShareTaskIndex()
  readonly #task = new ReceiveTaskState()
  readonly #sessions: ReceiveSessionRunner
  readonly #retained: RetainedTaskRunner
  #offerEpoch = 0n
  #offerAbort: AbortController | undefined
  #offers: ArtifactOffers | null = null
  #selectionSpec: SelectionSpec | null = null
  #selectionPolicy: FrozenSelectionPolicy | null = null
  #activation: ActiveActivationSnapshot | null = null
  #activationToken: symbol | null = null
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
      isDisposed: () => this.#disposed,
      onStateChange: () => this.#publish(),
    })
    this.#sessions = new ReceiveSessionRunner({
      windowPort: options.windowPort,
      task: this.#task,
      ...(options.outputTrace === undefined ? {} : { outputTrace: options.outputTrace }),
      isDisposed: () => this.#disposed,
      publish: () => this.#publish(),
      onSettledRecord: record => this.#shareIndex.upsert(record),
      onSessionFinished: () => { void this.loadRetained() },
      reportError: error => this.#reportError(error),
      dispatchAsyncError: (error, custom) => this.#dispatchAsyncError(error, custom),
    })
    this.#retained = new RetainedTaskRunner({
      storage: this.#storage,
      windowPort: options.windowPort,
      ...(options.opfsEnv === undefined ? {} : { opfsEnv: options.opfsEnv }),
      ...(options.outputTrace === undefined ? {} : { outputTrace: options.outputTrace }),
      signal: this.#lifetimeAbort.signal,
      task: this.#task,
      currentJoinedShare: options.currentJoinedShare,
      activeSessionTaskId: () => this.#sessions.activeTaskId,
      isDisposed: () => this.#disposed,
      publish: () => this.#publish(),
      adoptMatchingRetained: () => this.#adoptMatchingRetained(),
      onRecordsLoaded: records => this.#shareIndex.syncFromAllRecords(records),
      onRecordUpdated: record => this.#shareIndex.upsert(record),
      onRecordRemoved: taskId => this.#shareIndex.remove(taskId),
      clearActiveTask: () => this.startNewDownload(),
      closeDetached: session => this.#sessions.closeDetached(session),
      runSession: input => this.#sessions.run(input),
      dispatchAsyncError: (error, custom) => this.#dispatchAsyncError(error, custom),
    })
    this.#snapshot = this.#buildSnapshot()
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  readonly getSnapshot = (): ReceiveRuntimeSnapshot => this.#snapshot

  get active(): boolean {
    return this.#sessions.active || this.#activation !== null
  }

  get activeTaskId(): string | null {
    return this.#task.taskId
  }

  get canReleaseCurrent(): boolean {
    return !this.#sessions.active && this.#task.canRelease
  }

  get canRetainCurrent(): boolean {
    return !this.#sessions.active && this.#task.canRetain
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
    if (this.#capabilities.state === 'probing' && this.#capabilities.value === undefined) {
      void this.#capabilities.ensure().catch(() => undefined)
    }
    this.#shareIndex.preload(
      shareInstance,
      typeof safeIndexedDB(this.#options.windowPort)?.open === 'function',
      this.#storage,
      () => this.#disposed,
      () => this.#publish(),
    )
  }

  startBlockedReason(): ReceiveBlockReason | null {
    if (this.#options.currentJoinedShare() === undefined) return 'connectFirst'
    if (this.#activation !== null || this.#retained.snapshot.pending !== null) return 'busy'
    if (this.#sessions.active || !this.#task.canStartNew) return 'downloading'
    return null
  }

  shareNavigationBlockedReason(): ReceiveBlockReason | null {
    if (this.#sessions.active || this.#activation !== null) return 'downloading'
    if (this.#retained.snapshot.pending !== null) return 'busy'
    return null
  }

  activeActionAdmission(action: ReceiveAction): ReceiveAdmission {
    if (this.#task.lifecycle === null || this.#task.backend === null) return blockedAdmission('actionUnavailable')
    if (this.#task.interruption !== null || (this.#retained.snapshot.pending !== null && action !== 'pause' && action !== 'stop')) {
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
    if (this.#sessions.activeTaskId === task.taskId) return blockedAdmission('busy')
    return this.#admitRetained(task, action, this.active)
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
    return this.#retained.load()
  }

  ensureRetained(): Promise<void> {
    return this.#retained.ensure()
  }

  saveLocalBlob(blob: Blob, suggestedName: string): boolean {
    if (this.#disposed) return false
    try {
      triggerBrowserDownload(this.#options.windowPort, blob, suggestedName, ORIGINAL_FILE_MEDIA_TYPE)
      return true
    } catch {
      return false
    }
  }

  completeLocalBlobDownload(
    blob: Blob,
    suggestedName: string,
    display?: ReceiveOperationDisplay,
  ): boolean {
    if (this.#disposed || this.startBlockedReason() !== null) return false
    try {
      triggerBrowserDownload(this.#options.windowPort, blob, suggestedName, ORIGINAL_FILE_MEDIA_TYPE)
    } catch (error) {
      this.#reportError(error)
      return false
    }
    this.#dismissedShareInstance = null
    this.#task.completeLocalBlob({
      taskId: createTaskId(),
      shareInstance: this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null,
      selectionKey: this.#offers?.selectionKey ?? null,
      suggestedName,
      display,
      now: Date.now(),
    })
    this.#publish()
    return true
  }

  startDownload(choiceId: string, display?: ReceiveOperationDisplay): boolean {
    if (this.#disposed || this.startBlockedReason() !== null) return false
    const joined = this.#options.currentJoinedShare()
    const offers = this.#offers
    const spec = this.#selectionSpec
    if (joined === undefined || offers === null || offers.kind !== 'ready' || spec === null) return false
    const selection = this.#selectionPolicy ?? policyFromSelectionSpec(spec)
    const allChoices = [offers.primary, ...offers.alternatives, ...(offers.zip === null ? [] : [offers.zip])]
    const choice = allChoices.find(candidate => candidate.choiceId === choiceId)
    if (choice === undefined) return false
    const launched = this.#launchChoice(
      { joined, choice, spec, selection, key: offers.selectionKey, display },
      undefined,
      true,
    )
    return launched.kind === 'started' || launched.kind === 'waiting-picker'
  }

  startSingleFileDownload(
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    directory: BrowseDirectory,
    options?: SingleFileDownloadOptions,
  ): SingleFileDownloadOutcome {
    return startSingleFileDownloadFlow(this.#singleFileHost(), entry, directory, options)
  }

  cancelActivation(): void {
    if (this.#activation?.kind !== 'waiting-picker') return
    this.#clearActivation()
    this.#publish()
  }

  startNewDownload(): void {
    if (this.#sessions.active || this.#activation !== null) return
    this.#dismissedShareInstance = this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null
    this.#task.clear()
    this.#publish()
  }

  performActiveAction(action: ReceiveAction): void {
    if (!this.activeActionAdmission(action).allowed) return
    if ((action === 'pause' || action === 'stop') && this.#sessions.interrupt(action)) return
    if (this.#task.taskId !== null) {
      const retained = this.#retained.snapshot.tasks.find(task => task.taskId === this.#task.taskId)
      if (retained !== undefined) {
        this.performRetainedAction(retained, action)
        return
      }
      void this.#retained.runAction(this.#task.taskId, action)
    }
  }

  performRetainedAction(task: RetainedTaskSnapshot, action: ReceiveAction): void {
    if (!this.retainedActionAdmission(task, action).allowed) return
    void this.#retained.runAction(task.taskId, action)
  }

  suspendForNewShare(): void {
    this.#offerAbort?.abort()
    this.#offers = null
    this.#selectionSpec = null
    this.#selectionPolicy = null
    this.#clearActivation()
    this.#shareIndex.reset()
    this.#dismissedShareInstance = null
    if (!this.#sessions.active) this.#task.clear()
    this.#publish()
  }

  dispose(): Promise<void> {
    if (this.#disposed) return this.#storage.close()
    this.#disposed = true
    this.#listeners.clear()
    this.#lifetimeAbort.abort()
    this.#offerAbort?.abort()
    this.#clearActivation()
    this.#shareIndex.reset()
    this.#sessions.dispose()
    this.#snapshot = this.#buildSnapshot()
    return this.#storage.close()
  }

  #singleFileHost(): SingleFileLaunchHost {
    return {
      isDisposed: () => this.#disposed,
      startBlockedReason: () => this.startBlockedReason(),
      currentJoinedShare: () => this.#options.currentJoinedShare(),
      capabilitiesState: () => this.#capabilities.state,
      capabilitiesValue: () => this.#capabilities.value,
      retryCapabilities: () => this.#capabilities.retry(),
      ensureCapabilities: () => { void this.#capabilities.ensure().catch(() => undefined) },
      preloadShareTasks: shareInstance => this.preloadShareTasks(shareInstance),
      pendingShareIndex: shareInstance => this.#shareIndex.pendingPromiseFor(shareInstance),
      shareIndexRecords: (shareInstance, key) => this.#shareIndex.recordsFor(shareInstance, key),
      retainedTasks: () => this.#retained.snapshot.tasks,
      hasPendingRetained: () => this.#retained.snapshot.pending !== null,
      currentSettledCandidate: (shareInstance, key) =>
        this.#task.currentSettledCandidate(shareInstance, key, this.#sessions.active),
      admitCandidate: (task, action, ignoreActivation) =>
        this.#admitRetained(task, action, ignoreActivation ? this.#sessions.active : this.active),
      claimActivation: snapshot => this.#claimActivation(snapshot),
      ownsActivation: token => this.#ownsActivation(token),
      clearActivationAndPublish: () => { this.#clearActivation(); this.#publish() },
      clearActivationOnly: () => this.#clearActivation(),
      normalizeRecords: (token, records) => this.#normalizeCandidateRecords(token, records),
      runRetainedSave: (taskId, options) => {
        void this.#retained.runAction(taskId, 'save', {
          ...(options?.onExported === undefined ? {} : { onCompleted: options.onExported }),
          ...(options?.onAsyncError === undefined ? {} : { onError: options.onAsyncError }),
        })
      },
      runRetainedContinue: (taskId, options) => {
        void this.#retained.runAction(taskId, 'continue', {
          ...(options?.onActivated === undefined ? {} : { onActivated: options.onActivated }),
          ...(options?.onAsyncError === undefined ? {} : { onError: options.onAsyncError }),
        })
      },
      launchChoice: (request, options) => this.#launchChoice(request, launchCallbacks(options), false),
      openWithActivation: (token, _activation, request, options) =>
        this.#storage.run(access => this.#openDownload(access, request, token, undefined, launchCallbacks(options))),
      dispatchAsyncError: (error, custom) => this.#dispatchAsyncError(error, custom),
    }
  }

  async #normalizeCandidateRecords(
    token: symbol,
    records: readonly ReceiveTaskRecord[],
  ): Promise<readonly RetainedTaskSnapshot[]> {
    let normalized: readonly RetainedTaskSnapshot[] = Object.freeze([])
    await this.#storage.run(async access => {
      const store = await access.openStoreOptional()
      if (store === undefined || !this.#ownsActivation(token)) return
      const env = retainedTaskEnvironment(store, this.#options, this.#lifetimeAbort.signal)
      normalized = await resolveRetainedCandidates(env, records, this.#sessions.activeTaskId)
    })
    return normalized
  }

  #admitRetained(task: RetainedTaskSnapshot, action: ReceiveAction, hasActiveDownload: boolean): ReceiveAdmission {
    const shareInstance = this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null
    return admitRetainedAction(task, action, {
      currentShareInstance: shareInstance,
      hasActiveDownload,
      hasPendingAction: this.#retained.snapshot.pending !== null,
      connectionUnavailable: this.#connectionUnavailable,
      shareEnded: this.#shareEnded,
    })
  }

  #claimActivation(snapshot: ActiveActivationSnapshot): Readonly<{ token: symbol; activation: ActiveActivationSnapshot }> {
    const token = Symbol('receive-activation')
    const activation = Object.freeze(snapshot)
    this.#activationToken = token
    this.#activation = activation
    this.#publish()
    return { token, activation }
  }

  #clearActivation(): void {
    this.#activation = null
    this.#activationToken = null
  }

  #ownsActivation(token: symbol): boolean {
    return !this.#disposed && this.#activationToken === token
  }

  #launchChoice(
    request: SingleFileLaunchRequest,
    callbacks: SessionRunCallbacks | undefined,
    reportSyncPickerError: boolean,
  ): Extract<SingleFileDownloadOutcome, { kind: 'started' | 'waiting-picker' | 'failed' }> {
    const token = Symbol('receive-activation')
    const singleFileId = request.choice.artifact.singleFileId
    const activation: ActiveActivationSnapshot = Object.freeze({
      kind: request.choice.backend === 'fsa' ? 'waiting-picker' : 'committing',
      choiceId: request.choice.choiceId,
      suggestedName: request.choice.suggestedName,
      ...(singleFileId === undefined ? {} : { singleFileId }),
    })
    this.#activationToken = token
    this.#activation = activation
    let pickedFolder: Promise<FileSystemDirectoryHandle> | undefined
    if (activation.kind === 'waiting-picker') {
      try {
        pickedFolder = startFSADirectoryPicker(this.#options.windowPort)
      } catch (error) {
        if (this.#ownsActivation(token)) {
          this.#clearActivation()
          if (reportSyncPickerError) this.#reportError(error)
        }
        return Object.freeze({ kind: 'failed', error })
      }
    }
    this.#publish()
    void this.#executeNewDownload(request, token, activation, pickedFolder, callbacks)
    return Object.freeze({ kind: activation.kind === 'waiting-picker' ? 'waiting-picker' : 'started' })
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
    request: SingleFileLaunchRequest,
    token: symbol,
    activation: ActiveActivationSnapshot,
    pickedFolder: Promise<FileSystemDirectoryHandle> | undefined,
    callbacks: SessionRunCallbacks | undefined,
  ): Promise<void> {
    let parentHandle: FileSystemDirectoryHandle | undefined
    if (pickedFolder !== undefined) {
      try {
        parentHandle = await pickedFolder
      } catch (error) {
        if (!this.#ownsActivation(token)) return
        this.#clearActivation()
        this.#publish()
        if (!isAbortError(error)) this.#dispatchAsyncError(error, callbacks?.onAsyncError)
        return
      }
      if (!this.#ownsActivation(token)) return
      this.#activation = Object.freeze({ ...activation, kind: 'committing' })
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
    if (!this.#ownsActivation(token)) return
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
      if (!this.#ownsActivation(token)) return
      this.#clearActivation()
      this.#publish()
      this.#dispatchAsyncError(error, callbacks?.onAsyncError)
      return
    }

    if (!this.#ownsActivation(token)) return this.#sessions.closeDetached(session)
    this.#clearActivation()
    await this.#sessions.run({
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

  #activeActions(): readonly ReceiveAction[] {
    return this.#task.activeActions(this.#sessions.waitingForLiveJob)
  }

  #buildSnapshot(): ReceiveRuntimeSnapshot {
    const shareInstance = this.#options.currentJoinedShare()?.descriptor.shareInstanceId ?? null
    const pickerPending =
      this.#capabilities.state === 'ready' &&
      shareInstance !== null &&
      this.#shareIndex.isLoadingFor(shareInstance)
        ? singleFilePickerFloorBytes(this.#capabilities.value)
        : null
    return Object.freeze({
      offers: this.#offers,
      capabilitiesState: this.#capabilities.state,
      singleFilePickerPendingFromBytes: pickerPending,
      activation: this.#activation,
      activeTaskId: this.#task.activeTaskId(),
      activeSingleFileId: this.#activation?.singleFileId ?? this.#task.activeSingleFileId(),
      activeSelectionKey: this.#task.selectionKey,
      backend: this.#task.backend,
      lifecycle: this.#task.lifecycle,
      display: this.#task.display,
      safeRenamed: this.#task.safeRenamed,
      sourceRevisionFailures: this.#task.sourceRevisionFailures,
      outageCheckpointed: this.#sessions.outageCheckpointed,
      progress: this.#task.progress,
      actions: this.#activeActions(),
      interruption: this.#task.interruption,
      retained: this.#retained.snapshot,
    })
  }

  #adoptMatchingRetained(): void {
    if (this.#task.lifecycle !== null || this.#sessions.active || this.#activation !== null) return
    const joined = this.#options.currentJoinedShare()
    const offers = this.#offers
    if (joined === undefined || offers === null) return
    const shareInstance = joined.descriptor.shareInstanceId
    if (this.#dismissedShareInstance === shareInstance) return
    const match = matchingRetainedTask(this.#retained.snapshot.tasks, shareInstance, offers.selectionKey)
    if (match !== undefined) this.#task.adoptRetained(match)
  }

  #publish(): void {
    if (this.#disposed) return
    this.#snapshot = this.#buildSnapshot()
    for (const listener of this.#listeners) {
      try { listener() } catch { /* Subscriber failure cannot break coordinator state. */ }
    }
  }
}

function launchCallbacks(options: SingleFileDownloadOptions | undefined): SessionRunCallbacks {
  return {
    ...(options?.onActivated === undefined ? {} : { onActivated: options.onActivated }),
    ...(options?.onAsyncError === undefined ? {} : { onAsyncError: options.onAsyncError }),
  }
}

/** Restricted browser contexts (e.g. private iframes) throw SecurityError on the `indexedDB` getter itself. */
function safeIndexedDB(windowPort: BrowserOutputWindow): IDBFactory | undefined {
  try {
    return windowPort.indexedDB
  } catch {
    return undefined
  }
}
