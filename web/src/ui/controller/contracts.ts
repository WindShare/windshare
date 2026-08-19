import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type {
  IncidentScopeHandle,
  IncidentScopeKind,
  IncidentScopeOwner,
  PresentationDecision,
} from '../../diagnostics/incident'
import type { ReceiveLifecycleState } from '../../output/workspace'
import type { ArtifactSpec, MaterializationPlan } from '../../transfer/intent'
import type { ProjectionTraceEvent } from '../../transfer/projection'
import type { TransferTraceEvent } from '../../transfer/v2-job'
import type { LifecycleUserAction } from '../v2-lifecycle-presentation'
import type { V2DiagnosticFormatter, V2SecurityMilestone } from '../v2-capability-lifecycle'
import type { V2OutputTraceEvent } from '../v2-output'
import type {
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'

export type V2RetainedInventoryTraceEvent =
  | Readonly<{ name: 'receive.inventory.load.started' }>
  | Readonly<{ name: 'receive.inventory.load.completed'; operation_count: number }>
  | Readonly<{ name: 'receive.inventory.load.failed' }>
  | Readonly<{
      name:
        | 'receive.inventory.action.started'
        | 'receive.inventory.action.completed'
        | 'receive.inventory.action.failed'
      retained_action: V2RetainedReceiveAction
      continuation: V2RetainedReceiveOperation['continuation']
    }>

export type V2ControllerWorkflowTraceEvent =
  | Readonly<{
      name: 'join_transition'
      transition: 'started' | 'joined' | 'failed' | 'stale_replacement'
    }>
  | Readonly<{
      name: 'authority_transition'
      transition:
        | 'activation_started'
        | 'authority_acquired'
        | 'intent_frozen'
        | 'activation_failed'
        | 'stale_replacement'
        | 'authority_invalidated'
      artifactKind?: ArtifactSpec['kind']
      planKind?: MaterializationPlan['kind']
    }>
  | Readonly<{
      name: 'lifecycle_action_transition'
      transition: 'started' | 'completed' | 'failed' | 'excluded'
      action: LifecycleUserAction | 'expiry'
      lifecycleKind?: ReceiveLifecycleState['kind']
    }>

export type V2ReceiverTraceEvent =
  | ProjectionTraceEvent
  | TransferTraceEvent
  | V2OutputTraceEvent
  | V2ControllerWorkflowTraceEvent
  | V2RetainedInventoryTraceEvent

export interface V2ReceiverIncidentPort {
  openScope(kind: IncidentScopeKind): IncidentScopeOwner
  submitDecision(scope: IncidentScopeHandle, decision: PresentationDecision): void
}

export interface V2ReceiverControllerOptions {
  readonly diagnosticFormatter?: V2DiagnosticFormatter
  readonly onSecurityMilestone?: (milestone: V2SecurityMilestone) => void
  readonly incidents?: V2ReceiverIncidentPort
  readonly trace?: DomainTraceSource<V2ReceiverTraceEvent>
  readonly receive: V2ReceiveCompositionPort
}

export class ArtifactChoiceInvalidatedError extends Error {
  constructor() {
    super('The selected artifact action is no longer offered by current authority facts')
    this.name = 'ArtifactChoiceInvalidatedError'
  }
}

export class StaleReceiveBoundaryError extends DOMException {
  constructor() {
    super('A newer receive boundary replaced this action', 'AbortError')
  }
}
