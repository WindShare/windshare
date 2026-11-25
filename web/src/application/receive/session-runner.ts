import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { ReceiveOperationDisplay } from '../../output/model'
import type { OutputSessionHandle } from '../../output/backends/contract'
import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveTaskRecord } from '../../output/store/types'
import type { TransferJobResult } from '../../transfer/job'
import type { TransferProgress } from '../../transfer/progress'
import type { ReceiveLifecycleState, SafeRenamedSummary } from '../../transfer/state'
import type { JoinedBrowserShare } from '../browser-share/gateway'
import { DisconnectCheckpointGuard } from './disconnect-checkpoint-guard'
import type { ExecutionState, WorkspaceTask } from './execution-state'
import { closeOutputSession, closeUnactivatedOutputSession } from './session-opener'
import { BeforeUnloadGuard, type BeforeUnloadEventTarget } from './unload-guard'
import { ScreenWakeLockGuard, type ScreenWakeLockWindowTarget } from './wake-lock-guard'

const PROGRESS_THROTTLE_MS = 100

export interface SessionRunCallbacks {
  readonly onActivated?: () => void
  readonly onAsyncError?: (error: unknown) => void
}

export interface SessionRunInput {
  readonly transferToken: symbol
  readonly joined: JoinedBrowserShare
  readonly session: OutputSessionHandle
  readonly selection: FrozenSelectionPolicy
  readonly activeSelectionKey: string
  readonly rootPath: readonly string[]
  readonly singleFileId: string | null
  readonly display: ReceiveOperationDisplay | null
  readonly initialLifecycle: ReceiveLifecycleState
  readonly callbacks?: SessionRunCallbacks
}

interface PendingFailure {
  readonly error: unknown
  readonly preActivation: boolean
}

/**
 * Owns the active `TransferJob` lifecycle, browser leave/wake-lock/disconnect guards,
 * throttled progress publication, and settlement under `ExecutionState` ownership.
 */
export class ReceiveSessionRunner {
  readonly #unloadGuard: BeforeUnloadGuard
  readonly #wakeLockGuard: ScreenWakeLockGuard
  readonly #execution: ExecutionState
  readonly #workspace: WorkspaceTask
  readonly #outputTrace: OutputTraceSource | undefined
  readonly #isDisposed: () => boolean
  readonly #publish: () => void
  readonly #onSettledRecord: (record: ReceiveTaskRecord) => void
  readonly #onSessionFinished: () => void
  readonly #reportError: (error: unknown) => void
  readonly #dispatchAsyncError: (error: unknown, custom?: (error: unknown) => void) => void
  #disconnectGuard: DisconnectCheckpointGuard | undefined
  #progressTimer: ReturnType<typeof setTimeout> | undefined

  constructor(options: Readonly<{
    windowPort: Partial<BeforeUnloadEventTarget> & ScreenWakeLockWindowTarget
    execution: ExecutionState
    workspace: WorkspaceTask
    outputTrace?: OutputTraceSource
    isDisposed: () => boolean
    publish: () => void
    onSettledRecord: (record: ReceiveTaskRecord) => void
    onSessionFinished: () => void
    reportError: (error: unknown) => void
    dispatchAsyncError: (error: unknown, custom?: (error: unknown) => void) => void
  }>) {
    this.#unloadGuard = new BeforeUnloadGuard(options.windowPort)
    this.#wakeLockGuard = new ScreenWakeLockGuard(options.windowPort)
    this.#execution = options.execution
    this.#workspace = options.workspace
    this.#outputTrace = options.outputTrace
    this.#isDisposed = options.isDisposed
    this.#publish = options.publish
    this.#onSettledRecord = options.onSettledRecord
    this.#onSessionFinished = options.onSessionFinished
    this.#reportError = options.reportError
    this.#dispatchAsyncError = options.dispatchAsyncError
  }

  get waitingForLiveJob(): boolean {
    return this.#execution.isRunning &&
      (this.#execution.runningJob === undefined || this.#workspace.lifecycle?.kind !== 'downloading')
  }

  get outageCheckpointed(): boolean {
    return this.#disconnectGuard?.checkpointed ?? false
  }

  interrupt(action: 'pause' | 'stop'): boolean {
    const job = this.#execution.runningJob
    if (job === undefined) return false
    this.#workspace.markInterruption(action)
    if (action === 'pause') job.pause('user')
    else job.stop()
    this.#closeDisconnectGuard()
    this.#publish()
    return true
  }

  async closeUnactivated(session: OutputSessionHandle): Promise<void> {
    await closeUnactivatedOutputSession(session, this.#outputTrace)
    if (!this.#isDisposed()) this.#onSessionFinished()
  }

  dispose(): void {
    this.#unloadGuard.dispose()
    this.#wakeLockGuard.dispose()
    this.#execution.runningJob?.detach()
    this.#closeDisconnectGuard()
    this.#flushProgressNow()
  }

  async run(input: SessionRunInput): Promise<void> {
    const { transferToken, joined, session, selection, activeSelectionKey, rootPath, singleFileId, display, initialLifecycle, callbacks } = input
    if (this.#isDisposed() || !this.#execution.transitionToRunning(transferToken, session.taskId, singleFileId)) {
      return this.closeUnactivated(session)
    }

    let connectivity: ReturnType<JoinedBrowserShare['beginDownloadConnectivity']> | undefined
    let unsubscribeSafeRenamed: (() => void) | undefined
    let activated = false
    let failure: PendingFailure | undefined
    try {
      connectivity = joined.beginDownloadConnectivity()
      const job = joined.createTransferJob(session.transferOutput, connectivity, {
        selection,
        rootPath,
        onProgress: progress => this.#observeProgress(progress),
        onUncheckpointedWrite: () => this.#disconnectGuard?.invalidate(),
      })
      if (!this.#execution.attachRunningJob(transferToken, job)) {
        return
      }
      if (session.transferOutput.capabilities.resumable) {
        this.#disconnectGuard = new DisconnectCheckpointGuard(joined, {
          job,
          taskId: session.taskId,
          changed: () => this.#publish(),
          ...(this.#outputTrace === undefined ? {} : { trace: this.#outputTrace }),
        })
      }
      // Defer mutating workspace state until connectivity, job, and guards succeed
      // so pre-activation failures leave any prior settled/paused TaskCard intact.
      this.#workspace.beginActive({
        taskId: session.taskId,
        shareInstance: joined.descriptor.shareInstanceId,
        selectionKey: activeSelectionKey,
        backend: session.backend,
        display,
        initialLifecycle,
      })
      this.#workspace.observeSafeRenamed(session.transferOutput.safeRenamedSummary?.() ?? null)
      unsubscribeSafeRenamed = session.subscribeSafeRenamed?.(summary => this.#observeSafeRenamed(summary))
      this.#unloadGuard.setArmed(true)
      this.#wakeLockGuard.setArmed(true)
      this.#publish()
      activated = true
      try { callbacks?.onActivated?.() } catch { /* Callback failure cannot break transfer execution. */ }
      const result = await job.run()
      unsubscribeSafeRenamed?.()
      unsubscribeSafeRenamed = undefined
      this.#closeDisconnectGuard()
      await this.#settle(session, result)
    } catch (error) {
      failure = { error, preActivation: !activated }
      if (activated) this.#workspace.markFailed(error)
    } finally {
      unsubscribeSafeRenamed?.()
      this.#closeDisconnectGuard()
      this.#unloadGuard.setArmed(false)
      this.#wakeLockGuard.setArmed(false)
      if (activated) await closeOutputSession(session, this.#outputTrace)
      else await closeUnactivatedOutputSession(session, this.#outputTrace)
      try { connectivity?.close() } catch { /* Best-effort connectivity release. */ }
      this.#execution.releaseTransferIfOwned(transferToken)
      this.#publish()
      this.#finishRun(failure, callbacks)
    }
  }

  #finishRun(failure: PendingFailure | undefined, callbacks: SessionRunCallbacks | undefined): void {
    if (this.#isDisposed()) return
    if (failure !== undefined) {
      if (failure.preActivation) this.#dispatchAsyncError(failure.error, callbacks?.onAsyncError)
      else this.#reportError(failure.error)
    }
    this.#onSessionFinished()
  }

  async #settle(session: OutputSessionHandle, result: TransferJobResult): Promise<void> {
    this.#flushProgressNow()
    if (result.status === 'detached') {
      this.#traceDetached(session)
      return
    }
    if (this.#workspace.lifecycle?.kind === 'downloading') {
      this.#workspace.markFinalizing()
      this.#publish()
    }
    const settled = await session.settle(result)
    this.#onSettledRecord(settled)
    this.#workspace.applySettled(settled)
  }

  #observeProgress(progress: TransferProgress): void {
    this.#workspace.observeProgress(progress)
    this.#scheduleThrottledPublish()
  }

  #observeSafeRenamed(summary: SafeRenamedSummary): void {
    if (this.#workspace.observeSafeRenamed(summary)) {
      this.#scheduleThrottledPublish()
    }
  }

  #scheduleThrottledPublish(): void {
    if (this.#progressTimer !== undefined) return
    this.#progressTimer = setTimeout(() => {
      this.#progressTimer = undefined
      if (!this.#isDisposed()) this.#publish()
    }, PROGRESS_THROTTLE_MS)
  }

  #flushProgressNow(): void {
    if (this.#progressTimer !== undefined) {
      clearTimeout(this.#progressTimer)
      this.#progressTimer = undefined
    }
  }

  #closeDisconnectGuard(): void {
    this.#disconnectGuard?.close()
    this.#disconnectGuard = undefined
  }

  #traceDetached(session: OutputSessionHandle): void {
    emitOutputTrace(this.#outputTrace, 'settlement', { task_id: session.taskId, transition: 'detached' })
  }
}
