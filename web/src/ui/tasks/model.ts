import type { ReceiveLifecycleState } from '../../output/workspace'
import type { ReceiveOperationDisplay } from '../../output/workspace/operation-display'
import type { CompatibleNameRepairPresentation } from '../compatible-name-repair-presentation'
import type { LifecycleUserAction } from '../v2-lifecycle-presentation'
import type { V2ReceiverProgress } from '../v2-model'
import type { V2DirectZipProgressSnapshot, V2RetainedReceiveAction, V2RetainedReceiveOperation } from '../v2-receive-runtime'

export type TaskRecoveryReadiness = import('../controller/retained-readiness').RetainedContinuationReadiness

export interface TaskBlocking {
  readonly kind: 'reconnecting' | 'unavailable' | 'sender-capacity' | 'share-ended' | 'storage'
  readonly description?: string
}

export type TaskActionTarget =
  | Readonly<{ kind: 'active'; action: LifecycleUserAction }>
  | Readonly<{ kind: 'retained'; operation: V2RetainedReceiveOperation; action: V2RetainedReceiveAction }>

export interface TaskAction {
  readonly id: string
  readonly label: string
  readonly destructive: boolean
  readonly disabledReason: string | null
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
  readonly actions: readonly TaskAction[]
  readonly blocking: TaskBlocking | null
  readonly readiness: TaskRecoveryReadiness
  readonly completeness: TaskCompleteness
  readonly publication: TaskPublication
  readonly progress: V2ReceiverProgress | null
  readonly browserDelivery?: import('../../output/browser-delivery/retained').BrowserDeliveryResumeSummary | null
  readonly directZipProgress: V2DirectZipProgressSnapshot | null
  readonly fidelity: CompatibleNameRepairPresentation | null
  readonly details: readonly string[]
  readonly interruption: 'pause' | 'stop' | 'finish' | null
  readonly execution: Readonly<{ kind: 'active' }> |
    Readonly<{ kind: 'retained'; continuation: import('../../output/resume/descriptor').ReceiveOperationContinuation }> |
    Readonly<{ kind: 'local-finalization' }>
}

export interface TaskProgressPresentation {
  readonly mode: 'indeterminate' | 'determinate'
  readonly percentage: number | null
  readonly sampleIdentity: string
  readonly receivedBytes: bigint
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
  readonly fidelity: CompatibleNameRepairPresentation | null
  readonly completeness: TaskCompleteness
  readonly publication: TaskPublication
  readonly transition: TaskPresentationTransition
}
