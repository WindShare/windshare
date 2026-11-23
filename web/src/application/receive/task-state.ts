import type { ReceiveOperationDisplay, SaveBackendKind } from '../../output/model'
import type { ReceiveTaskRecord } from '../../output/store/types'
import { isAbortError } from '../../transfer/fault'
import { EMPTY_TRANSFER_PROGRESS, type TransferProgress } from '../../transfer/progress'
import {
  initialReceiveState,
  isActiveReceiveState,
  transitionReceiveState,
  type ReceiveLifecycleState,
  type SafeRenamedSummary,
  type SourceRevisionFailures,
} from '../../transfer/state'
import { actionsForState, canReleaseCurrentTask, canSavePartialState, canStartNewDownload } from './admission'
import type { ReceiveAction, RetainedTaskSnapshot } from './contracts'

/**
 * Encapsulates the current workspace task fields managed by `ReceiveCoordinator`
 * so session startup, settlement, local-blob handoff, and resets stay consistent.
 */
export class ReceiveTaskState {
  taskId: string | null = null
  shareInstance: string | null = null
  selectionKey: string | null = null
  singleFileId: string | null = null
  backend: SaveBackendKind | null = null
  isZipArchive = false
  lifecycle: ReceiveLifecycleState | null = null
  display: ReceiveOperationDisplay | null = null
  safeRenamed: SafeRenamedSummary | null = null
  sourceRevisionFailures: SourceRevisionFailures | null = null
  progress: TransferProgress = EMPTY_TRANSFER_PROGRESS
  interruption: 'pause' | 'stop' | null = null

  get canRelease(): boolean {
    return canReleaseCurrentTask(this.lifecycle)
  }

  get canRetain(): boolean {
    return this.lifecycle?.kind === 'paused'
  }

  get canStartNew(): boolean {
    return canStartNewDownload(this.lifecycle)
  }

  clear(): void {
    this.taskId = null
    this.shareInstance = null
    this.selectionKey = null
    this.singleFileId = null
    this.backend = null
    this.isZipArchive = false
    this.lifecycle = null
    this.display = null
    this.safeRenamed = null
    this.sourceRevisionFailures = null
    this.progress = EMPTY_TRANSFER_PROGRESS
    this.interruption = null
  }

  beginSession(input: Readonly<{
    taskId: string
    shareInstance: string
    selectionKey: string
    singleFileId: string | null
    backend: SaveBackendKind
    display: ReceiveOperationDisplay | null
    initialLifecycle: ReceiveLifecycleState
  }>): void {
    const downloading = input.initialLifecycle.kind === 'downloading'
      ? input.initialLifecycle
      : transitionReceiveState(input.initialLifecycle, { kind: 'downloading', phase: 'receiving' })
    this.taskId = input.taskId
    this.shareInstance = input.shareInstance
    this.selectionKey = input.selectionKey
    this.singleFileId = input.singleFileId
    this.backend = input.backend
    this.isZipArchive = false
    this.lifecycle = downloading
    this.display = input.display
    this.safeRenamed = null
    this.sourceRevisionFailures = null
    this.interruption = null
    this.progress = EMPTY_TRANSFER_PROGRESS
  }

  applySettled(settled: ReceiveTaskRecord): void {
    this.isZipArchive = settled.artifact.kind === 'zip-archive'
    this.lifecycle = settled.lifecycle
    this.display = settled.display ?? this.display
    this.safeRenamed = settled.safeRenamed ?? null
    this.sourceRevisionFailures = settled.sourceRevisionFailures ?? null
    this.interruption = null
  }

  markFailed(error: unknown): void {
    this.interruption = null
    if (this.lifecycle === null) return
    this.lifecycle = transitionReceiveState(this.lifecycle, {
      kind: 'failed',
      reason: isAbortError(error) ? 'cancelled' : 'error',
      ...(error instanceof Error ? { detail: error.message } : {}),
    })
  }

  adoptRetained(match: RetainedTaskSnapshot): void {
    this.taskId = match.taskId
    this.shareInstance = match.shareInstance
    this.selectionKey = match.selectionKey
    this.singleFileId = null
    this.backend = match.backend
    this.isZipArchive = match.canSavePartial
    this.lifecycle = match.lifecycle
    this.display = match.display ?? null
    this.safeRenamed = match.safeRenamed ?? null
    this.sourceRevisionFailures = match.sourceRevisionFailures ?? null
  }

  completeLocalBlob(input: Readonly<{
    taskId: string
    shareInstance: string | null
    selectionKey: string | null
    suggestedName: string
    display: ReceiveOperationDisplay | undefined
    now: number
  }>): void {
    const started = transitionReceiveState(initialReceiveState(input.taskId, input.now), {
      kind: 'download-started',
      retained: false,
      fileName: input.suggestedName,
    }, input.now)
    const destinationLabel = Object.freeze({ kind: 'browser-downloads' as const })
    this.taskId = input.taskId
    this.shareInstance = input.shareInstance
    this.selectionKey = input.selectionKey
    this.singleFileId = null
    this.backend = 'portable'
    this.isZipArchive = false
    this.lifecycle = started
    this.display = Object.freeze(input.display === undefined
      ? { objectLabel: input.suggestedName, destinationLabel, createdAtMilliseconds: input.now }
      : { ...input.display, destinationLabel })
    this.safeRenamed = null
    this.sourceRevisionFailures = null
    this.progress = EMPTY_TRANSFER_PROGRESS
    this.interruption = null
  }

  currentSettledCandidate(shareInstance: string, key: string, hasLiveSession: boolean): RetainedTaskSnapshot | undefined {
    if (
      hasLiveSession ||
      this.taskId === null ||
      this.shareInstance !== shareInstance ||
      this.selectionKey !== key ||
      this.backend === null ||
      this.lifecycle === null
    ) {
      return undefined
    }
    const exportable = this.backend === 'opfs' && (
      this.lifecycle.kind === 'ready-to-save' ||
      (this.lifecycle.kind === 'download-started' && this.lifecycle.retained)
    )
    if (!exportable && this.lifecycle.kind !== 'paused') return undefined
    const canSavePartial = canSavePartialState(this.lifecycle, this.backend, this.isZipArchive)
    return Object.freeze({
      taskId: this.taskId,
      shareInstance,
      selectionKey: key,
      backend: this.backend,
      lifecycle: this.lifecycle,
      ...(this.display === null ? {} : { display: this.display }),
      ...(this.safeRenamed === null ? {} : { safeRenamed: this.safeRenamed }),
      ...(this.sourceRevisionFailures === null ? {} : { sourceRevisionFailures: this.sourceRevisionFailures }),
      actions: actionsForState(this.lifecycle, this.backend, canSavePartial),
      activeElsewhere: false,
      canSavePartial,
    })
  }

  activeActions(hasSessionWithoutLiveJob: boolean): readonly ReceiveAction[] {
    if (hasSessionWithoutLiveJob) return Object.freeze([])
    const canSavePartial = canSavePartialState(this.lifecycle, this.backend, this.isZipArchive)
    return this.lifecycle === null || this.backend === null || this.interruption !== null
      ? Object.freeze([])
      : actionsForState(this.lifecycle, this.backend, canSavePartial)
  }

  activeTaskId(): string | null {
    return this.lifecycle !== null && isActiveReceiveState(this.lifecycle) ? this.taskId : null
  }

  activeSingleFileId(): string | null {
    return this.lifecycle !== null && isActiveReceiveState(this.lifecycle) ? this.singleFileId : null
  }
}
