import type { ReceiveOperationDisplay, SaveBackendKind } from '../../output/model'
import type { ReceiveTaskRecord } from '../../output/store/types'
import { isAbortError } from '../../transfer/fault'
import type { TransferJob } from '../../transfer/job'
import { EMPTY_TRANSFER_PROGRESS, type TransferProgress } from '../../transfer/progress'
import {
  isActiveReceiveState,
  transitionReceiveState,
  type ReceiveLifecycleState,
  type SafeRenamedSummary,
  type SourceRevisionFailures,
} from '../../transfer/state'
import { actionsForState, canReleaseCurrentTask, canSavePartialState, canStartNewDownload } from './admission'
import type {
  ActiveActivationSnapshot,
  PendingRetainedAction,
  ReceiveAction,
  ReceiveBlockReason,
} from './contracts'

export type LocalRetainedActionKind = 'save' | 'save-partial' | 'discard' | 'forget'

export type TransferSlot =
  | Readonly<{ kind: 'idle' }>
  | Readonly<{ kind: 'resolving-reuse'; token: symbol; choiceId: string; suggestedName: string; singleFileId: string }>
  | Readonly<{ kind: 'waiting-picker'; token: symbol; choiceId: string; suggestedName: string; singleFileId?: string }>
  | Readonly<{ kind: 'opening'; token: symbol; choiceId: string; suggestedName: string; singleFileId?: string }>
  | Readonly<{ kind: 'resuming'; token: symbol; taskId: string; singleFileId?: string }>
  | Readonly<{ kind: 'running'; token: symbol; taskId: string; singleFileId: string | null; job?: TransferJob }>

export type LocalActionSlot = Readonly<{
  token: symbol
  taskId: string
  action: LocalRetainedActionKind
  singleFileId?: string
}>

const IDLE_TRANSFER_SLOT: TransferSlot = Object.freeze({ kind: 'idle' })

/**
 * Owns the two orthogonal concurrency domains in the browser receiver:
 * 1. The exclusive transfer slot (`idle` -> activation/resuming -> `running`), which
 *    prevents concurrent `TransferJob` sessions and tracks single-file activation ownership.
 * 2. The local retained-storage action slot (`save`, `save-partial`, `discard`, `forget`),
 *    which may run alongside an active transfer on a different `taskId`.
 */
export class ExecutionState {
  #transfer: TransferSlot = IDLE_TRANSFER_SLOT
  #localAction: LocalActionSlot | null = null

  get transfer(): TransferSlot {
    return this.#transfer
  }

  get localAction(): LocalActionSlot | null {
    return this.#localAction
  }

  get isTransferActive(): boolean {
    return this.#transfer.kind !== 'idle'
  }

  get isRunning(): boolean {
    return this.#transfer.kind === 'running'
  }

  get activeTransferTaskId(): string | null {
    if (this.#transfer.kind === 'running' || this.#transfer.kind === 'resuming') {
      return this.#transfer.taskId
    }
    return null
  }

  /** True while this tab owns an active transfer, resume, or local retained action on `taskId`. */
  ownsTask(taskId: string): boolean {
    return this.activeTransferTaskId === taskId || this.#localAction?.taskId === taskId
  }

  get runningJob(): TransferJob | undefined {
    return this.#transfer.kind === 'running' ? this.#transfer.job : undefined
  }

  get hasLocalAction(): boolean {
    return this.#localAction !== null
  }

  get activation(): ActiveActivationSnapshot | null {
    switch (this.#transfer.kind) {
      case 'waiting-picker':
        return Object.freeze({
          kind: 'waiting-picker',
          choiceId: this.#transfer.choiceId,
          suggestedName: this.#transfer.suggestedName,
          ...(this.#transfer.singleFileId === undefined ? {} : { singleFileId: this.#transfer.singleFileId }),
        })
      case 'resolving-reuse':
      case 'opening':
        return Object.freeze({
          kind: 'committing',
          choiceId: this.#transfer.choiceId,
          suggestedName: this.#transfer.suggestedName,
          ...(this.#transfer.singleFileId === undefined ? {} : { singleFileId: this.#transfer.singleFileId }),
        })
      default:
        return null
    }
  }

  get pendingRetained(): PendingRetainedAction | null {
    if (this.#localAction !== null) {
      return Object.freeze({ taskId: this.#localAction.taskId, action: this.#localAction.action })
    }
    if (this.#transfer.kind === 'resuming') {
      return Object.freeze({ taskId: this.#transfer.taskId, action: 'continue' })
    }
    return null
  }

  get activeSingleFileId(): string | null {
    if (this.#transfer.kind !== 'idle') return this.#transfer.singleFileId ?? null
    return this.#localAction?.singleFileId ?? null
  }

  startBlockedReason(hasJoinedShare: boolean, canStartNewTask: boolean): ReceiveBlockReason | null {
    if (!hasJoinedShare) return 'connectFirst'
    if (this.#transfer.kind === 'running') return 'downloading'
    if (this.#transfer.kind !== 'idle' || this.#localAction !== null) return 'busy'
    if (!canStartNewTask) return 'downloading'
    return null
  }

  shareNavigationBlockedReason(): ReceiveBlockReason | null {
    if (
      this.#transfer.kind === 'running' ||
      this.#transfer.kind === 'waiting-picker' ||
      this.#transfer.kind === 'opening' ||
      this.#transfer.kind === 'resolving-reuse'
    ) {
      return 'downloading'
    }
    if (this.#transfer.kind === 'resuming' || this.#localAction !== null) return 'busy'
    return null
  }

  ownsTransfer(token: symbol): boolean {
    return this.#transfer.kind !== 'idle' && this.#transfer.token === token
  }

  ownsLocalAction(token: symbol): boolean {
    return this.#localAction?.token === token
  }

  claimResolvingReuse(choiceId: string, suggestedName: string, singleFileId: string): symbol | null {
    if (this.#transfer.kind !== 'idle' || this.#localAction !== null) return null
    const token = Symbol('receive-transfer')
    this.#transfer = Object.freeze({ kind: 'resolving-reuse', token, choiceId, suggestedName, singleFileId })
    return token
  }

  claimLaunch(
    kind: 'waiting-picker' | 'opening',
    choiceId: string,
    suggestedName: string,
    singleFileId?: string,
  ): symbol | null {
    if (this.#transfer.kind !== 'idle' || this.#localAction !== null) return null
    const token = Symbol('receive-transfer')
    this.#transfer = Object.freeze({
      kind,
      token,
      choiceId,
      suggestedName,
      ...(singleFileId === undefined ? {} : { singleFileId }),
    })
    return token
  }

  claimResuming(taskId: string, singleFileId?: string): symbol | null {
    if (this.#transfer.kind !== 'idle' || this.#localAction !== null) return null
    const token = Symbol('receive-transfer')
    this.#transfer = Object.freeze({
      kind: 'resuming',
      token,
      taskId,
      ...(singleFileId === undefined ? {} : { singleFileId }),
    })
    return token
  }

  transitionToOpening(token: symbol): boolean {
    if (
      (this.#transfer.kind !== 'waiting-picker' && this.#transfer.kind !== 'resolving-reuse') ||
      this.#transfer.token !== token
    ) {
      return false
    }
    this.#transfer = Object.freeze({
      kind: 'opening',
      token,
      choiceId: this.#transfer.choiceId,
      suggestedName: this.#transfer.suggestedName,
      ...(this.#transfer.singleFileId === undefined ? {} : { singleFileId: this.#transfer.singleFileId }),
    })
    return true
  }

  transitionToResuming(token: symbol, taskId: string, singleFileId?: string): boolean {
    if (this.#transfer.kind !== 'resolving-reuse' || this.#transfer.token !== token || this.#localAction !== null) {
      return false
    }
    const resolvedFileId = singleFileId ?? this.#transfer.singleFileId
    this.#transfer = Object.freeze({
      kind: 'resuming',
      token,
      taskId,
      ...(resolvedFileId === undefined ? {} : { singleFileId: resolvedFileId }),
    })
    return true
  }

  transitionToRunning(token: symbol, taskId: string, singleFileId: string | null): boolean {
    if (
      (this.#transfer.kind !== 'opening' && this.#transfer.kind !== 'resuming') ||
      this.#transfer.token !== token
    ) {
      return false
    }
    this.#transfer = Object.freeze({
      kind: 'running',
      token,
      taskId,
      singleFileId,
    })
    return true
  }

  attachRunningJob(token: symbol, job: TransferJob): boolean {
    if (this.#transfer.kind !== 'running' || this.#transfer.token !== token) return false
    this.#transfer = Object.freeze({ ...this.#transfer, job })
    return true
  }

  releaseTransferIfOwned(token: symbol): boolean {
    if (!this.ownsTransfer(token)) return false
    this.#transfer = IDLE_TRANSFER_SLOT
    return true
  }

  cancelWaitingPicker(): boolean {
    if (this.#transfer.kind !== 'waiting-picker') return false
    this.#transfer = IDLE_TRANSFER_SLOT
    return true
  }

  clearPreRunningTransfer(): void {
    if (this.#transfer.kind !== 'running') {
      this.#transfer = IDLE_TRANSFER_SLOT
    }
  }

  claimLocalAction(
    taskId: string,
    action: LocalRetainedActionKind,
    singleFileId?: string,
  ): symbol | null {
    if (this.#localAction !== null || this.activeTransferTaskId === taskId) return null
    const token = Symbol('receive-local-action')
    this.#localAction = Object.freeze({
      token,
      taskId,
      action,
      ...(singleFileId === undefined ? {} : { singleFileId }),
    })
    return token
  }

  /** Atomically converts an in-flight single-file lookup into a local export action without flickering UI state. */
  handoffTransferToLocal(
    transferToken: symbol,
    taskId: string,
    action: LocalRetainedActionKind,
    singleFileId?: string,
  ): symbol | null {
    if (this.#transfer.kind !== 'resolving-reuse' || this.#transfer.token !== transferToken) return null
    if (this.#localAction !== null) return null
    const resolvedFileId = singleFileId ?? this.#transfer.singleFileId
    const localToken = Symbol('receive-local-action')
    this.#transfer = IDLE_TRANSFER_SLOT
    this.#localAction = Object.freeze({
      token: localToken,
      taskId,
      action,
      ...(resolvedFileId === undefined ? {} : { singleFileId: resolvedFileId }),
    })
    return localToken
  }

  releaseLocalIfOwned(token: symbol): boolean {
    if (!this.ownsLocalAction(token)) return false
    this.#localAction = null
    return true
  }

  dispose(): void {
    this.#transfer = IDLE_TRANSFER_SLOT
    this.#localAction = null
  }
}

/**
 * Encapsulates the current workspace task presentation state orthogonally from
 * `ExecutionState`, so cancelled pickers or pre-activation failures preserve any
 * prior settled/paused workspace card untouched.
 */
export class WorkspaceTask {
  #taskId: string | null = null
  #shareInstance: string | null = null
  #selectionKey: string | null = null
  #backend: SaveBackendKind | null = null
  #isZipArchive = false
  #lifecycle: ReceiveLifecycleState | null = null
  #display: ReceiveOperationDisplay | null = null
  #safeRenamed: SafeRenamedSummary | null = null
  #sourceRevisionFailures: SourceRevisionFailures | null = null
  #progress: TransferProgress = EMPTY_TRANSFER_PROGRESS
  #interruption: 'pause' | 'stop' | null = null

  get taskId(): string | null { return this.#taskId }
  get shareInstance(): string | null { return this.#shareInstance }
  get selectionKey(): string | null { return this.#selectionKey }
  get backend(): SaveBackendKind | null { return this.#backend }
  get lifecycle(): ReceiveLifecycleState | null { return this.#lifecycle }
  get display(): ReceiveOperationDisplay | null { return this.#display }
  get safeRenamed(): SafeRenamedSummary | null { return this.#safeRenamed }
  get sourceRevisionFailures(): SourceRevisionFailures | null { return this.#sourceRevisionFailures }
  get progress(): TransferProgress { return this.#progress }
  get interruption(): 'pause' | 'stop' | null { return this.#interruption }

  get canRelease(): boolean {
    return canReleaseCurrentTask(this.#lifecycle)
  }

  get canRetain(): boolean {
    return this.#lifecycle?.kind === 'paused'
  }

  get canStartNew(): boolean {
    return canStartNewDownload(this.#lifecycle)
  }

  clear(): void {
    this.#taskId = null
    this.#shareInstance = null
    this.#selectionKey = null
    this.#backend = null
    this.#isZipArchive = false
    this.#lifecycle = null
    this.#display = null
    this.#safeRenamed = null
    this.#sourceRevisionFailures = null
    this.#progress = EMPTY_TRANSFER_PROGRESS
    this.#interruption = null
  }

  clearActiveIfRunning(): void {
    if (this.#lifecycle !== null && isActiveReceiveState(this.#lifecycle)) {
      this.clear()
    }
  }

  beginActive(input: Readonly<{
    taskId: string
    shareInstance: string
    selectionKey: string
    backend: SaveBackendKind
    display: ReceiveOperationDisplay | null
    initialLifecycle: ReceiveLifecycleState
  }>): void {
    const downloading = input.initialLifecycle.kind === 'downloading'
      ? input.initialLifecycle
      : transitionReceiveState(input.initialLifecycle, { kind: 'downloading', phase: 'receiving' })
    this.#taskId = input.taskId
    this.#shareInstance = input.shareInstance
    this.#selectionKey = input.selectionKey
    this.#backend = input.backend
    this.#isZipArchive = false
    this.#lifecycle = downloading
    this.#display = input.display
    this.#safeRenamed = null
    this.#sourceRevisionFailures = null
    this.#interruption = null
    this.#progress = EMPTY_TRANSFER_PROGRESS
  }

  observeProgress(progress: TransferProgress): void {
    this.#progress = progress
  }

  markInterruption(action: 'pause' | 'stop'): void {
    this.#interruption = action
  }

  markFinalizing(): void {
    if (this.#lifecycle?.kind !== 'downloading') return
    this.#lifecycle = transitionReceiveState(this.#lifecycle, { kind: 'downloading', phase: 'finalizing' })
  }

  applySettled(settled: ReceiveTaskRecord): void {
    this.#isZipArchive = settled.artifact.kind === 'zip-archive'
    this.#lifecycle = settled.lifecycle
    this.#display = settled.display ?? this.#display
    this.#safeRenamed = settled.safeRenamed ?? null
    this.#sourceRevisionFailures = settled.sourceRevisionFailures ?? null
    this.#interruption = null
  }

  applyUpdatedLifecycle(lifecycle: ReceiveLifecycleState): void {
    this.#lifecycle = lifecycle
  }

  markFailed(error: unknown): void {
    this.#interruption = null
    if (this.#lifecycle === null) return
    this.#lifecycle = transitionReceiveState(this.#lifecycle, {
      kind: 'failed',
      reason: isAbortError(error) ? 'cancelled' : 'error',
      ...(error instanceof Error ? { detail: error.message } : {}),
    })
  }

  completeSettled(record: ReceiveTaskRecord): void {
    this.#taskId = record.taskId
    this.#shareInstance = record.shareInstance
    this.#selectionKey = record.selectionKey
    this.#backend = record.backend
    this.#isZipArchive = record.artifact.kind === 'zip-archive'
    this.#lifecycle = record.lifecycle
    this.#display = record.display ?? null
    this.#safeRenamed = record.safeRenamed ?? null
    this.#sourceRevisionFailures = record.sourceRevisionFailures ?? null
    this.#progress = EMPTY_TRANSFER_PROGRESS
    this.#interruption = null
  }

  activeActions(hasSessionWithoutLiveJob: boolean): readonly ReceiveAction[] {
    if (hasSessionWithoutLiveJob) return Object.freeze([])
    const canSavePartial = canSavePartialState(this.#lifecycle, this.#backend, this.#isZipArchive)
    return this.#lifecycle === null || this.#backend === null || this.#interruption !== null
      ? Object.freeze([])
      : actionsForState(this.#lifecycle, this.#backend, canSavePartial)
  }

  activeTaskId(): string | null {
    return this.#lifecycle !== null && isActiveReceiveState(this.#lifecycle) ? this.#taskId : null
  }
}
