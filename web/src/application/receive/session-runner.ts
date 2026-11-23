import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { ReceiveOperationDisplay } from '../../output/model'
import type { OutputSessionHandle } from '../../output/backends/contract'
import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveTaskRecord } from '../../output/store/types'
import type { TransferJob, TransferJobResult } from '../../transfer/job'
import type { TransferProgress } from '../../transfer/progress'
import { transitionReceiveState, type ReceiveLifecycleState } from '../../transfer/state'
import type { JoinedBrowserShare } from '../browser-share/gateway'
import { DisconnectCheckpointGuard } from './disconnect-checkpoint-guard'
import { closeOutputSession } from './session-opener'
import type { ReceiveTaskState } from './task-state'
import { BeforeUnloadGuard, type BeforeUnloadEventTarget } from './unload-guard'
import { ScreenWakeLockGuard, type ScreenWakeLockWindowTarget } from './wake-lock-guard'

const PROGRESS_THROTTLE_MS = 100

export interface SessionRunCallbacks {
  readonly onActivated?: () => void
  readonly onAsyncError?: (error: unknown) => void
}

export interface SessionRunInput {
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

interface ActiveReceiveSession {
  readonly taskId: string
  job?: TransferJob
}

/**
 * Owns the active `TransferJob` lifecycle, browser leave/wake-lock/disconnect guards,
 * throttled progress publication, and settlement.
 */
export class ReceiveSessionRunner {
  readonly #unloadGuard: BeforeUnloadGuard
  readonly #wakeLockGuard: ScreenWakeLockGuard
  readonly #task: ReceiveTaskState
  readonly #outputTrace: OutputTraceSource | undefined
  readonly #isDisposed: () => boolean
  readonly #publish: () => void
  readonly #onSettledRecord: (record: ReceiveTaskRecord) => void
  readonly #onSessionFinished: () => void
  readonly #reportError: (error: unknown) => void
  readonly #dispatchAsyncError: (error: unknown, custom?: (error: unknown) => void) => void
  #session: ActiveReceiveSession | null = null
  #disconnectGuard: DisconnectCheckpointGuard | undefined
  #progressTimer: ReturnType<typeof setTimeout> | undefined

  constructor(options: Readonly<{
    windowPort: Partial<BeforeUnloadEventTarget> & ScreenWakeLockWindowTarget
    task: ReceiveTaskState
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
    this.#task = options.task
    this.#outputTrace = options.outputTrace
    this.#isDisposed = options.isDisposed
    this.#publish = options.publish
    this.#onSettledRecord = options.onSettledRecord
    this.#onSessionFinished = options.onSessionFinished
    this.#reportError = options.reportError
    this.#dispatchAsyncError = options.dispatchAsyncError
  }

  get active(): boolean {
    return this.#session !== null
  }

  get activeTaskId(): string | null {
    return this.#session?.taskId ?? null
  }

  get waitingForLiveJob(): boolean {
    return this.#session !== null && (this.#session.job === undefined || this.#task.lifecycle?.kind !== 'downloading')
  }

  get outageCheckpointed(): boolean {
    return this.#disconnectGuard?.checkpointed ?? false
  }

  interrupt(action: 'pause' | 'stop'): boolean {
    const job = this.#session?.job
    if (job === undefined) return false
    this.#task.interruption = action
    if (action === 'pause') job.pause('user')
    else job.stop()
    this.#closeDisconnectGuard()
    this.#publish()
    return true
  }

  closeDetached(session: OutputSessionHandle): Promise<void> {
    this.#traceDetached(session)
    return closeOutputSession(session, this.#outputTrace)
  }

  dispose(): void {
    this.#unloadGuard.dispose()
    this.#wakeLockGuard.dispose()
    this.#session?.job?.detach()
    this.#closeDisconnectGuard()
    this.#flushProgressNow()
  }

  async run(input: SessionRunInput): Promise<void> {
    const { joined, session, selection, activeSelectionKey, rootPath, singleFileId, display, initialLifecycle, callbacks } = input
    if (this.#isDisposed()) return this.closeDetached(session)
    let connectivity: ReturnType<JoinedBrowserShare['beginDownloadConnectivity']> | undefined
    const owner: ActiveReceiveSession = { taskId: session.taskId }
    this.#session = owner
    this.#task.beginSession({
      taskId: session.taskId,
      shareInstance: joined.descriptor.shareInstanceId,
      selectionKey: activeSelectionKey,
      singleFileId,
      backend: session.backend,
      display,
      initialLifecycle,
    })

    let activated = false
    try {
      connectivity = joined.beginDownloadConnectivity()
      const job = joined.createTransferJob(session.transferOutput, connectivity, {
        selection,
        rootPath,
        onProgress: progress => this.#observeProgress(progress),
        onUncheckpointedWrite: () => this.#disconnectGuard?.invalidate(),
      })
      owner.job = job
      if (session.transferOutput.capabilities.resumable) {
        this.#disconnectGuard = new DisconnectCheckpointGuard(joined, {
          job,
          taskId: session.taskId,
          changed: () => this.#publish(),
          ...(this.#outputTrace === undefined ? {} : { trace: this.#outputTrace }),
        })
      }
      this.#unloadGuard.setArmed(true)
      this.#wakeLockGuard.setArmed(true)
      this.#publish()
      activated = true
      try { callbacks?.onActivated?.() } catch { /* Callback failure cannot break transfer execution. */ }
      const result = await job.run()
      this.#closeDisconnectGuard()
      await this.#settle(session, result)
    } catch (error) {
      if (!activated && callbacks?.onAsyncError !== undefined) {
        this.#task.clear()
        this.#dispatchAsyncError(error, callbacks.onAsyncError)
      } else {
        this.#task.markFailed(error)
        this.#reportError(error)
      }
    } finally {
      this.#closeDisconnectGuard()
      this.#unloadGuard.setArmed(false)
      this.#wakeLockGuard.setArmed(false)
      await closeOutputSession(session, this.#outputTrace)
      try { connectivity?.close() } catch { /* Best-effort connectivity release. */ }
      this.#session = null
      this.#task.singleFileId = null
      this.#publish()
      if (!this.#isDisposed()) this.#onSessionFinished()
    }
  }

  async #settle(session: OutputSessionHandle, result: TransferJobResult): Promise<void> {
    this.#flushProgressNow()
    if (result.status === 'detached') {
      this.#traceDetached(session)
      return
    }
    if (this.#task.lifecycle?.kind === 'downloading') {
      this.#task.lifecycle = transitionReceiveState(this.#task.lifecycle, { kind: 'downloading', phase: 'finalizing' })
      this.#publish()
    }
    const settled = await session.settle(result)
    this.#onSettledRecord(settled)
    this.#task.applySettled(settled)
  }

  #observeProgress(progress: TransferProgress): void {
    this.#task.progress = progress
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
