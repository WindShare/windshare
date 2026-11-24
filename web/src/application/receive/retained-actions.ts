import type { BrowserOutputWindow, OutputSessionHandle } from '../../output/backends/contract'
import type { OPFSEnvironment } from '../../output/backends/opfs'
import type { OutputTraceSource } from '../../output/diagnostics'
import { TaskBusyError } from '../../output/store/tab-lock'
import { isPermissionDeniedError } from '../../transfer/fault'
import { artifactRootPath, policyFromSelectionSpec } from '../../transfer/selection'
import type { JoinedBrowserShare } from '../browser-share/gateway'
import type { ExecutionState, LocalRetainedActionKind, WorkspaceTask } from './execution-state'
import type { ReceiveStorage, ReceiveStoreAccess } from './receive-storage'
import { executeRetainedTaskAction, reopenRetainedTask, retainedTaskEnvironment } from './retained'
import type { RetainedTaskInventory } from './retained-inventory'
import type { SessionRunInput } from './session-runner'
import { ReceiveBlockedError } from './single-file'

export interface RetainedActionCallbacks {
  readonly onActivated?: () => void
  readonly onCompleted?: () => void
  readonly onError?: (error: unknown) => void
}

export interface RetainedTaskActionsOptions {
  readonly storage: ReceiveStorage
  readonly inventory: RetainedTaskInventory
  readonly execution: ExecutionState
  readonly workspace: WorkspaceTask
  readonly windowPort: BrowserOutputWindow
  readonly opfsEnv?: OPFSEnvironment
  readonly outputTrace?: OutputTraceSource
  readonly signal: AbortSignal
  readonly currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly isDisposed: () => boolean
  readonly publish: () => void
  readonly clearActiveTask: () => void
  readonly closeDetached: (session: OutputSessionHandle) => Promise<void>
  readonly runSession: (input: SessionRunInput) => Promise<void>
  readonly refreshStorageEstimate: () => void
  readonly dispatchAsyncError: (error: unknown, custom?: (error: unknown) => void) => void
}

/**
 * Executes retained-task commands under `ExecutionState` ownership:
 * - `continue` claims the exclusive transfer slot and transitions it into `running`.
 * - `save` / `save-partial` / `discard` / `forget` claim the orthogonal local-action slot
 *   so users can manage other retained records while a transfer is active.
 */
export class RetainedTaskActions {
  readonly #options: RetainedTaskActionsOptions

  constructor(options: RetainedTaskActionsOptions) {
    this.#options = options
  }

  runContinue(
    taskId: string,
    callbacks?: RetainedActionCallbacks,
  ): Promise<void> {
    if (this.#options.isDisposed()) return Promise.resolve()
    const token = this.#options.execution.claimResuming(taskId)
    if (token === null) return Promise.resolve()
    this.#options.publish()
    return this.#options.storage.run(access => this.#executeContinue(access, token, taskId, callbacks))
  }

  runLocalAction(
    taskId: string,
    action: LocalRetainedActionKind,
    callbacks?: RetainedActionCallbacks,
  ): Promise<void> {
    if (this.#options.isDisposed()) return Promise.resolve()
    const token = this.#options.execution.claimLocalAction(taskId, action)
    if (token === null) return Promise.resolve()
    this.#options.publish()
    return this.#options.storage.run(access => this.#executeLocal(access, token, taskId, action, callbacks))
  }

  async #executeContinue(
    access: ReceiveStoreAccess,
    token: symbol,
    taskId: string,
    callbacks: RetainedActionCallbacks | undefined,
  ): Promise<void> {
    try {
      const store = (await access.openStoreOptional()) ?? (await access.requireStore())
      if (this.#options.isDisposed() || !this.#options.execution.ownsTransfer(token)) return
      const env = retainedTaskEnvironment(store, this.#envPorts(), this.#options.signal)
      const { joined, task, session } = await reopenRetainedTask(env, taskId, this.#options.currentJoinedShare)
      if (this.#options.isDisposed() || !this.#options.execution.ownsTransfer(token)) {
        await this.#options.closeDetached(session)
        return
      }
      let sessionInput: SessionRunInput
      try {
        sessionInput = {
          transferToken: token,
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
        }
      } catch (error) {
        await this.#options.closeDetached(session)
        throw error
      }
      await this.#options.runSession(sessionInput)
    } catch (error) {
      await this.#handleContinueError(token, taskId, error, callbacks)
    }
  }

  async #executeLocal(
    access: ReceiveStoreAccess,
    token: symbol,
    taskId: string,
    action: LocalRetainedActionKind,
    callbacks: RetainedActionCallbacks | undefined,
  ): Promise<void> {
    try {
      await access.waitForTaskWrites(taskId)
      if (this.#options.isDisposed() || !this.#options.execution.ownsLocalAction(token)) return
      let store = await access.openStoreOptional()
      if (this.#options.isDisposed() || !this.#options.execution.ownsLocalAction(token)) return
      if (store === undefined) {
        if (this.#isEphemeralPortableForgetTarget(taskId, action)) {
          this.#clearEphemeralPortableTask(token, taskId)
          return
        }
        store = await access.requireStore()
        if (this.#options.isDisposed() || !this.#options.execution.ownsLocalAction(token)) return
      }
      const env = retainedTaskEnvironment(store, this.#envPorts(), this.#options.signal)
      const outcome = await executeRetainedTaskAction(env, taskId, action)
      if (this.#options.isDisposed()) return
      this.#applyLocalOutcome(token, taskId, action, outcome.updated, callbacks)
      this.#options.refreshStorageEstimate()
      await this.#options.inventory.loadAll()
    } catch (error) {
      await this.#handleLocalError(token, taskId, action, error, callbacks)
    }
  }

  #applyLocalOutcome(
    token: symbol,
    taskId: string,
    action: LocalRetainedActionKind,
    updated: Awaited<ReturnType<typeof executeRetainedTaskAction>>['updated'],
    callbacks: RetainedActionCallbacks | undefined,
  ): void {
    const removing = action === 'forget' || action === 'discard'
    if (removing) this.#options.inventory.removeTask(taskId)
    else if (updated !== undefined) this.#options.inventory.upsertRecord(updated)

    // Release slot and publish before invoking callbacks so subscribers observe idle pending state.
    this.#options.execution.releaseLocalIfOwned(token)
    if (this.#options.workspace.taskId === taskId) {
      if (removing) {
        this.#options.clearActiveTask()
      } else if (updated !== undefined) {
        this.#options.workspace.applyUpdatedLifecycle(updated.lifecycle)
        this.#options.publish()
      } else {
        this.#options.publish()
      }
    } else {
      this.#options.publish()
    }

    if (action === 'save') {
      try { callbacks?.onCompleted?.() } catch { /* Callback failure cannot break retained refresh. */ }
    }
  }

  async #handleContinueError(
    token: symbol,
    taskId: string,
    error: unknown,
    callbacks: RetainedActionCallbacks | undefined,
  ): Promise<void> {
    if (this.#options.isDisposed()) return
    this.#options.execution.releaseTransferIfOwned(token)
    if (isNotFoundError(error)) this.#options.inventory.removeTask(taskId)
    this.#options.publish()
    await this.#options.inventory.loadAll()
    const mapped = error instanceof TaskBusyError ? new ReceiveBlockedError('activeInAnotherTab') : error
    this.#options.dispatchAsyncError(mapped, callbacks?.onError)
  }

  async #handleLocalError(
    token: symbol,
    taskId: string,
    action: LocalRetainedActionKind,
    error: unknown,
    callbacks: RetainedActionCallbacks | undefined,
  ): Promise<void> {
    if (this.#options.isDisposed()) return
    if (isNotFoundError(error) && this.#isEphemeralPortableForgetTarget(taskId, action)) {
      this.#clearEphemeralPortableTask(token, taskId)
      return
    }
    this.#options.execution.releaseLocalIfOwned(token)
    const notFound = isNotFoundError(error)
    if (notFound) this.#options.inventory.removeTask(taskId)
    this.#options.publish()
    if (notFound || isPermissionDeniedError(error) || error instanceof TaskBusyError) {
      await this.#options.inventory.loadAll()
    }
    const mapped = error instanceof TaskBusyError ? new ReceiveBlockedError('activeInAnotherTab') : error
    this.#options.dispatchAsyncError(mapped, callbacks?.onError)
  }

  #isEphemeralPortableForgetTarget(taskId: string, action: LocalRetainedActionKind): boolean {
    if (action !== 'forget') return false
    if (this.#options.workspace.taskId === taskId && this.#options.workspace.backend === 'portable') {
      return true
    }
    return this.#options.inventory.tasks.some(
      task => task.taskId === taskId && task.backend === 'portable',
    )
  }

  #clearEphemeralPortableTask(token: symbol, taskId: string): void {
    this.#options.inventory.removeTask(taskId)
    this.#options.execution.releaseLocalIfOwned(token)
    if (this.#options.workspace.taskId === taskId) {
      this.#options.clearActiveTask()
    } else {
      this.#options.publish()
    }
  }

  #envPorts() {
    return {
      windowPort: this.#options.windowPort,
      ...(this.#options.opfsEnv === undefined ? {} : { opfsEnv: this.#options.opfsEnv }),
      ...(this.#options.outputTrace === undefined ? {} : { outputTrace: this.#options.outputTrace }),
    }
  }
}

function isNotFoundError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'NotFoundError'
}
