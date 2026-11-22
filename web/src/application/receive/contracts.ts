import type { ArtifactOffers, ReceiveOperationDisplay, SaveBackendKind } from '../../output/model'
import type { TransferProgress } from '../../transfer/progress'
import type {
  ReceiveLifecycleState,
  SafeRenamedSummary,
  SourceRevisionFailure,
  SourceRevisionFailures,
} from '../../transfer/state'

export type ReceiveBlockReason =
  | 'connectFirst'
  | 'emptySelection'
  | 'downloading'
  | 'busy'
  | 'matchingShare'
  | 'shareEnded'
  | 'connectionUnavailable'
  | 'activeInAnotherTab'
  | 'actionUnavailable'

export interface ReceiveAdmission {
  readonly allowed: boolean
  readonly reason: ReceiveBlockReason | null
}

export type ReceiveAction =
  | 'pause'
  | 'stop'
  | 'continue'
  | 'save'
  | 'save-partial'
  | 'discard'
  | 'forget'

export type RetainedReadiness =
  | 'local'
  | 'matching-share'
  | 'different-share'
  | 'original-link-required'

export interface RetainedTaskSnapshot {
  readonly taskId: string
  readonly shareInstance: string
  readonly selectionKey: string
  readonly backend: SaveBackendKind
  readonly lifecycle: ReceiveLifecycleState
  readonly display?: ReceiveOperationDisplay
  readonly safeRenamed?: SafeRenamedSummary
  readonly sourceRevisionFailures?: SourceRevisionFailures
  readonly actions: readonly ReceiveAction[]
  readonly activeElsewhere: boolean
  readonly canSavePartial: boolean
}

export interface PendingRetainedAction {
  readonly taskId: string
  readonly action: ReceiveAction
}

export interface RetainedInventorySnapshot {
  readonly kind: 'unloaded' | 'loading' | 'empty' | 'available' | 'failed'
  readonly tasks: readonly RetainedTaskSnapshot[]
  readonly pending: PendingRetainedAction | null
}

export const EMPTY_RETAINED_INVENTORY: RetainedInventorySnapshot = Object.freeze({
  kind: 'unloaded',
  tasks: Object.freeze([]),
  pending: null,
})

export interface ActiveActivationSnapshot {
  readonly kind: 'waiting-picker' | 'committing'
  readonly choiceId: string
  readonly suggestedName: string
}

export interface ReceiveRuntimeSnapshot {
  readonly offers: ArtifactOffers | null
  readonly activation: ActiveActivationSnapshot | null
  readonly activeTaskId: string | null
  readonly activeSelectionKey: string | null
  readonly backend: SaveBackendKind | null
  readonly lifecycle: ReceiveLifecycleState | null
  readonly display: ReceiveOperationDisplay | null
  readonly safeRenamed: SafeRenamedSummary | null
  readonly sourceRevisionFailures: SourceRevisionFailures | null
  readonly progress: TransferProgress
  readonly actions: readonly ReceiveAction[]
  readonly interruption: 'pause' | 'stop' | null
  readonly retained: RetainedInventorySnapshot
}

export interface ReceiveReplacementTarget {
  readonly task: RetainedTaskSnapshot
  readonly failure: SourceRevisionFailure
}

export type PreviewTraceEvent = Readonly<{
  name: 'preview_transition'
  transition: 'deferred'
  previewId: number
  shareInstanceId: string
  fileId: string
  error: unknown
}>

export type ReceiverTraceEvent =
  | PreviewTraceEvent
  | Readonly<{ name: 'join_transition'; transition: 'started' | 'joined' | 'failed' | 'stale_replacement' }>
  | Readonly<{
      name: 'receiver_experience'
      transition: 'task'
      operationId: string
      generation: bigint
      stage: string
      reason: string
      attention: boolean
      completeness: string
      publication: string
      elapsedMilliseconds: number | null
    }>
  | Readonly<{
      name: 'receiver_experience'
      transition: 'saving'
      projectionEpoch: bigint | null
      choiceId: string | null
      outcome: string | null
      reason: string
    }>
  | Readonly<{
      name: 'receiver_experience'
      transition: 'intent'
      action: string
      operationId: string | null
      generation: bigint
    }>
  | Readonly<{
      name: 'receiver_experience'
      transition: 'ownership'
      state: 'releasing' | 'released' | 'release-failed'
      operationId: string
      generation: bigint
      lifecycleKind: ReceiveLifecycleState['kind']
    }>
