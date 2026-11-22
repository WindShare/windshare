import type { ReceiveBlockReason } from '../../application/receive/admission'
import type { ReceiveLifecycleState } from '../../transfer/lifecycle/state'
import type { ReceiveOperationDisplay } from '../../output/model/operation-display'
import type { SafeRenamedSummary } from '../../transfer/lifecycle/safe-names'
import type { LifecycleUserAction } from '../../application/receive/actions'
import type { ReceiverProgress } from '../model'
import type { RetainedReceiveAction, RetainedReceiveOperation } from '../../application/receive/contracts'

export type TaskRecoveryReadiness = import('../../application/receive/retained/readiness').RetainedContinuationReadiness

export interface TaskBlocking {
  readonly kind: 'reconnecting' | 'unavailable' | 'sender-capacity' | 'relay-bandwidth' | 'share-ended' | 'storage'
  readonly description?: string
}

export type TaskActionTarget =
  | Readonly<{ kind: 'active'; action: LifecycleUserAction }>
  | Readonly<{ kind: 'retained'; operation: RetainedReceiveOperation; action: RetainedReceiveAction }>

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
export type TaskStage = 'preparing' | 'downloading' | 'waiting' | 'paused' | 'finishing' |
  'ready-to-save' | 'handed-to-browser' | 'saved' | 'needs-action' | 'cancelled' | 'failed'

/** Display facts cannot grant a storage action: every action retains its owner-issued target. */
export interface TaskFacts {
  readonly lifecycle: ReceiveLifecycleState
  readonly display: ReceiveOperationDisplay | null
  readonly actions: readonly TaskAction<ReceiveBlockReason>[]
  readonly blocking: TaskBlocking | null
  readonly readiness: TaskRecoveryReadiness
  readonly completeness: TaskCompleteness
  readonly publication: TaskPublication
  readonly progress: ReceiverProgress | null
  readonly safeRenamed: SafeRenamedSummary | null
  readonly details: readonly string[]
  readonly interruption: 'pause' | 'stop' | 'finish' | null
  readonly execution: Readonly<{ kind: 'active' }> |
    Readonly<{ kind: 'retained'; continuation: import('../../output/model/continuation').ReceiveOperationContinuation }> |
    Readonly<{ kind: 'local-finalization' }>
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
  readonly details: readonly string[]
  readonly safeRenamed: SafeRenamedSummary | null
  readonly completeness: TaskCompleteness
  readonly publication: TaskPublication
  readonly transition: TaskPresentationTransition
}
