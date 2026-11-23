import type { BrowserOutputWindow, OutputSessionHandle } from '../../output/backends/contract'
import type { OPFSEnvironment } from '../../output/backends/opfs'
import type { OutputTraceSource } from '../../output/diagnostics'
import { TaskBusyError } from '../../output/store/tab-lock'
import type { ReceiveTaskRecord } from '../../output/store/types'
import { isPermissionDeniedError } from '../../transfer/fault'
import { artifactRootPath, policyFromSelectionSpec } from '../../transfer/selection'
import type { JoinedBrowserShare } from '../browser-share/gateway'
import {
  EMPTY_RETAINED_INVENTORY,
  type PendingRetainedAction,
  type ReceiveAction,
  type RetainedInventorySnapshot,
} from './contracts'
import type { ReceiveStorage, ReceiveStoreAccess } from './receive-storage'
import {
  executeRetainedTaskAction,
  reopenRetainedTask,
  resolveRetainedCandidates,
  retainedTaskEnvironment,
} from './retained'
import type { SessionRunInput } from './session-runner'
import { ReceiveBlockedError } from './single-file'
import type { ReceiveTaskState } from './task-state'

export interface RetainedActionCallbacks {
  readonly onActivated?: () => void
  readonly onCompleted?: () => void
  readonly onError?: (error: unknown) => void
}

/**
 * Owns retained-history loading and serialized retained task actions
 * (`continue`, `save`, `save-partial`, `discard`, `forget`) over `ReceiveStorage`.
 */
export class RetainedTaskRunner {
  readonly #storage: ReceiveStorage
  readonly #windowPort: BrowserOutputWindow
  readonly #opfsEnv: OPFSEnvironment | undefined
  readonly #outputTrace: OutputTraceSource | undefined
  readonly #signal: AbortSignal
  readonly #task: ReceiveTaskState
  readonly #currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly #activeSessionTaskId: () => string | null
  readonly #isDisposed: () => boolean
  readonly #publish: () => void
  readonly #adoptMatchingRetained: () => void
  readonly #onRecordsLoaded: (records: readonly ReceiveTaskRecord[]) => void
  readonly #onRecordUpdated: (record: ReceiveTaskRecord) => void
  readonly #onRecordRemoved: (taskId: string) => void
  readonly #clearActiveTask: () => void
  readonly #closeDetached: (session: OutputSessionHandle) => Promise<void>
  readonly #runSession: (input: SessionRunInput) => Promise<void>
  readonly #dispatchAsyncError: (error: unknown, custom?: (error: unknown) => void) => void
  #snapshot: RetainedInventorySnapshot = EMPTY_RETAINED_INVENTORY

  constructor(options: Readonly<{
    storage: ReceiveStorage
    windowPort: BrowserOutputWindow
    opfsEnv?: OPFSEnvironment
    outputTrace?: OutputTraceSource
    signal: AbortSignal
    task: ReceiveTaskState
    currentJoinedShare: () => JoinedBrowserShare | undefined
    activeSessionTaskId: () => string | null
    isDisposed: () => boolean
    publish: () => void
    adoptMatchingRetained: () => void
    onRecordsLoaded: (records: readonly ReceiveTaskRecord[]) => void
    onRecordUpdated: (record: ReceiveTaskRecord) => void
    onRecordRemoved: (taskId: string) => void
    clearActiveTask: () => void
    closeDetached: (session: OutputSessionHandle) => Promise<void>
    runSession: (input: SessionRunInput) => Promise<void>
    dispatchAsyncError: (error: unknown, custom?: (error: unknown) => void) => void
  }>) {
    this.#storage = options.storage
    this.#windowPort = options.windowPort
    this.#opfsEnv = options.opfsEnv
    this.#outputTrace = options.outputTrace
    this.#signal = options.signal
    this.#task = options.task
    this.#currentJoinedShare = options.currentJoinedShare
    this.#activeSessionTaskId = options.activeSessionTaskId
    this.#isDisposed = options.isDisposed
    this.#publish = options.publish
    this.#adoptMatchingRetained = options.adoptMatchingRetained
    this.#onRecordsLoaded = options.onRecordsLoaded
    this.#onRecordUpdated = options.onRecordUpdated
    this.#onRecordRemoved = options.onRecordRemoved
    this.#clearActiveTask = options.clearActiveTask
    this.#closeDetached = options.closeDetached
    this.#runSession = options.runSession
    this.#dispatchAsyncError = options.dispatchAsyncError
  }

  get snapshot(): RetainedInventorySnapshot {
    return this.#snapshot
  }

  load(): Promise<void> {
    return this.#storage.run(access => this.#load(access))
  }

  async ensure(): Promise<void> {
    if (this.#snapshot.kind === 'unloaded' || this.#snapshot.kind === 'failed') {
      await this.load()
    }
  }

  runAction(taskId: string, action: ReceiveAction, callbacks?: RetainedActionCallbacks): Promise<void> {
    if (this.#isDisposed() || this.#snapshot.pending !== null) return Promise.resolve()
    const pending: PendingRetainedAction = Object.freeze({ taskId, action })
    this.#snapshot = Object.freeze({ ...this.#snapshot, pending })
    this.#publish()
    return this.#storage.run(access => this.#execute(access, pending, callbacks))
  }

  async #load(access: ReceiveStoreAccess): Promise<void> {
    if (this.#isDisposed()) return
    const store = await access.openStoreOptional()
    if (this.#isDisposed()) return
    if (store === undefined) {
      this.#onRecordsLoaded(Object.freeze([]))
      this.#snapshot = Object.freeze({ kind: 'empty', tasks: Object.freeze([]), pending: null })
      this.#publish()
      return
    }
    this.#snapshot = Object.freeze({ ...this.#snapshot, kind: 'loading' })
    this.#publish()
    try {
      const env = retainedTaskEnvironment(store, this.#envPorts(), this.#signal)
      const records = await store.listTasks()
      if (this.#isDisposed()) return
      const tasks = await resolveRetainedCandidates(env, records, this.#activeSessionTaskId())
      if (this.#isDisposed()) return
      const survivingIds = new Set(tasks.map(task => task.taskId))
      this.#onRecordsLoaded(records.filter(record => survivingIds.has(record.taskId)))
      this.#snapshot = Object.freeze({
        kind: tasks.length === 0 ? 'empty' : 'available',
        tasks,
        pending: this.#snapshot.pending,
      })
      this.#adoptMatchingRetained()
      this.#publish()
    } catch {
      if (this.#isDisposed()) return
      this.#snapshot = Object.freeze({ kind: 'failed', tasks: Object.freeze([]), pending: null })
      this.#publish()
    }
  }

  async #execute(
    access: ReceiveStoreAccess,
    pending: PendingRetainedAction,
    callbacks?: RetainedActionCallbacks,
  ): Promise<void> {
    try {
      const store = await access.openStoreOptional()
      if (this.#isDisposed()) return
      if (store === undefined) {
        if (pending.action === 'forget' && this.#task.taskId === pending.taskId) {
          this.#clearPending(pending)
          this.#clearActiveTask()
          return
        }
        await access.requireStore()
        return
      }
      const env = retainedTaskEnvironment(store, this.#envPorts(), this.#signal)
      if (pending.action === 'continue') {
        await this.#executeContinue(env, pending, callbacks)
        return
      }
      await this.#executeLocalAction(env, pending, callbacks)
    } catch (error) {
      await this.#handleError(pending, error, callbacks)
    }
  }

  async #executeContinue(
    env: ReturnType<typeof retainedTaskEnvironment>,
    pending: PendingRetainedAction,
    callbacks: RetainedActionCallbacks | undefined,
  ): Promise<void> {
    const { joined, task, session } = await reopenRetainedTask(env, pending.taskId, this.#currentJoinedShare)
    if (this.#isDisposed()) return this.#closeDetached(session)
    this.#clearPending(pending)
    await this.#runSession({
      joined,
      session,
      selection: policyFromSelectionSpec(task.selection),
      activeSelectionKey: task.selectionKey,
      rootPath: artifactRootPath(task.artifact),
      singleFileId: task.artifact.singleFileId ?? null,
      display: task.display ?? null,
      initialLifecycle: task.lifecycle,
      callbacks: {
        ...(callbacks?.onActivated === undefined ? {} : { onActivated: callbacks.onActivated }),
        ...(callbacks?.onError === undefined ? {} : { onAsyncError: callbacks.onError }),
      },
    })
  }

  async #executeLocalAction(
    env: ReturnType<typeof retainedTaskEnvironment>,
    pending: PendingRetainedAction,
    callbacks: RetainedActionCallbacks | undefined,
  ): Promise<void> {
    const { taskId, action } = pending
    if (action === 'continue') return
    const outcome = await executeRetainedTaskAction(env, taskId, action)
    if (this.#isDisposed()) return
    this.#clearPending(pending)
    if (action === 'forget' || action === 'discard') this.#onRecordRemoved(taskId)
    else if (outcome.updated !== undefined) this.#onRecordUpdated(outcome.updated)
    if (this.#task.taskId === taskId) {
      if (action === 'forget' || action === 'discard') this.#clearActiveTask()
      else if (outcome.updated !== undefined) {
        this.#task.lifecycle = outcome.updated.lifecycle
        this.#publish()
      }
    }
    if (action === 'save') {
      try { callbacks?.onCompleted?.() } catch { /* Callback failure cannot break retained refresh. */ }
    }
    await this.load()
  }

  async #handleError(
    pending: PendingRetainedAction,
    error: unknown,
    callbacks: RetainedActionCallbacks | undefined,
  ): Promise<void> {
    if (this.#isDisposed()) return
    this.#clearPending(pending)
    if (this.#isEphemeralPortableForget(pending, error)) {
      this.#clearActiveTask()
      return
    }
    if (pending.action === 'continue' || isPermissionDeniedError(error) || error instanceof TaskBusyError) {
      await this.load()
    }
    this.#publish()
    const mapped = error instanceof TaskBusyError ? new ReceiveBlockedError('activeInAnotherTab') : error
    this.#dispatchAsyncError(mapped, callbacks?.onError)
  }

  #isEphemeralPortableForget(pending: PendingRetainedAction, error: unknown): boolean {
    return pending.action === 'forget' &&
      this.#task.taskId === pending.taskId &&
      this.#task.backend === 'portable' &&
      error instanceof DOMException &&
      error.name === 'NotFoundError'
  }

  #clearPending(pending: PendingRetainedAction): void {
    if (this.#snapshot.pending === pending) {
      this.#snapshot = Object.freeze({ ...this.#snapshot, pending: null })
    }
  }

  #envPorts() {
    return {
      windowPort: this.#windowPort,
      ...(this.#opfsEnv === undefined ? {} : { opfsEnv: this.#opfsEnv }),
      ...(this.#outputTrace === undefined ? {} : { outputTrace: this.#outputTrace }),
    }
  }
}
