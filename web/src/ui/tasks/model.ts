import type {
  ReceiveAction,
  ReceiveBlockReason,
  ReceiveSettlingPhase,
  RetainedReadiness,
  RetainedTaskSnapshot,
} from '../../application/receive/contracts'
import type { ReceiveOperationDisplay, SaveBackendKind } from '../../output/model'
import type { ReceiveLifecycleState, SafeRenamedSummary } from '../../transfer/state'
import type { ReceiverProgress } from '../model'

export type TaskRecoveryReadiness = RetainedReadiness

export interface TaskBlocking {
  readonly kind: 'reconnecting' | 'unavailable' | 'sender-capacity' | 'relay-bandwidth' | 'share-ended' | 'access-denied' | 'storage'
  readonly description?: string
}

export type TaskActionTarget =
  | Readonly<{ kind: 'active'; action: ReceiveAction }>
  | Readonly<{ kind: 'retained'; task: RetainedTaskSnapshot; action: ReceiveAction }>

export interface TaskAction<Reason extends string = string> {
  readonly id: string
  readonly label: string
  readonly destructive: boolean
  readonly disabledReason: Reason | null
  readonly consequence: string | null
  readonly target: TaskActionTarget
}

export type TaskCompleteness = 'unknown' | 'incomplete' | 'partial' | 'complete'
export type TaskPublication = 'unpublished' | 'browser-handoff' | 'saved'
export type TaskSafeNameRestoreState = 'none' | 'active' | 'paused' | 'available'
export type TaskDetailSection = 'overview' | 'adjusted-names'
export type TaskStage =
  | 'preparing'
  | 'downloading'
  | 'waiting'
  | 'paused'
  | 'finishing'
  | 'ready-to-save'
  | 'handed-to-browser'
  | 'saved'
  | 'needs-action'
  | 'cancelled'
  | 'failed'

export interface TaskFacts {
  readonly lifecycle: ReceiveLifecycleState
  readonly backend: SaveBackendKind
  readonly display: ReceiveOperationDisplay | null
  readonly actions: readonly TaskAction<ReceiveBlockReason>[]
  readonly fallbackForget?: TaskAction<ReceiveBlockReason> | null
  readonly blocking: TaskBlocking | null
  readonly readiness: TaskRecoveryReadiness
  readonly completeness: TaskCompleteness
  readonly publication: TaskPublication
  readonly progress: ReceiverProgress | null
  readonly safeRenamed: SafeRenamedSummary | null
  readonly sourceOmittedEntries: bigint
  readonly details: readonly string[]
  readonly outageCheckpointed: boolean
  readonly settling: ReceiveSettlingPhase | null
}

export interface TaskProgressPresentation {
  readonly mode: 'indeterminate' | 'determinate'
  readonly percentage: number | null
  readonly sampleIdentity: string
  readonly receivedObjectBytes: bigint
  readonly writtenBytes: bigint
  readonly remainingBytes: bigint | null
  readonly status: string | null
  readonly label: string
  readonly details: readonly string[]
}

export interface TaskPresentationTransition {
  readonly name: 'receiver.task.presentation'
  readonly operation_id: string
  readonly generation: bigint
  readonly stage: TaskStage
  readonly reason: string
  readonly completeness: TaskCompleteness
  readonly publication: TaskPublication
  readonly attention: boolean
  readonly fingerprint: string
}

export interface TaskRemovalPresentation {
  readonly summary: string
  readonly hint: string
  readonly fallbackForget: TaskAction | null
}

export interface TaskPresentation {
  readonly operationId: string
  readonly generation: bigint
  readonly objectLabel: string
  readonly destinationLabel: string | null
  readonly createdAtMilliseconds: number | null
  readonly elapsedMilliseconds: number | null
  readonly stage: TaskStage
  readonly headline: string
  readonly description: string
  readonly tone: 'neutral' | 'positive' | 'warning' | 'critical'
  readonly attention: boolean
  readonly progress: TaskProgressPresentation | null
  readonly primaryAction: TaskAction | null
  readonly secondaryActions: readonly TaskAction[]
  readonly destructiveActions: readonly TaskAction[]
  readonly removal: TaskRemovalPresentation | null
  readonly details: readonly string[]
  readonly safeRenamed: SafeRenamedSummary | null
  readonly safeNameRestore: TaskSafeNameRestoreState
  readonly sourceOmittedEntries: bigint
  readonly completeness: TaskCompleteness
  readonly publication: TaskPublication
  readonly transition: TaskPresentationTransition
}
