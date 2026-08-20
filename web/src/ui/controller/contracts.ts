import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type {
  IncidentScopeHandle,
  IncidentScopeKind,
  IncidentScopeOwner,
  PresentationDecision,
} from '../../diagnostics/incident'
import type { ReceiveLifecycleState } from '../../output/workspace'
import type {
  OfferComputedDecision,
  OfferDisabledDecision,
  ProjectionEpoch,
  RetryableDiscoveryReason,
} from '../../output/planning'
import type { ArtifactSpec, MaterializationPlan } from '../../transfer/intent'
import type { ProjectionTraceEvent } from '../../transfer/projection'
import type { TransferTraceEvent } from '../../transfer/v2-job'
import type { LifecycleUserAction } from '../v2-lifecycle-presentation'
import type { V2DiagnosticFormatter, V2SecurityMilestone } from '../v2-capability-lifecycle'
import type {
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'
import type { V2ActivationInvalidationReason } from './activation-model'

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

export type V2AuthorityOfferTraceEvent =
  | Readonly<{
      name: 'authority_transition'
      transition: 'offers_computed'
      projectionEpoch: ProjectionEpoch
      shapeProof: OfferComputedDecision['shape_proof']
      offeredArtifactKinds: OfferComputedDecision['offered_artifact_kinds']
      offeredPlanKinds: OfferComputedDecision['offered_plan_kinds']
      primaryArtifactKind: OfferComputedDecision['primary_artifact_kind']
    }>
  | Readonly<{
      name: 'authority_transition'
      transition: 'offers_disabled'
      projectionEpoch: ProjectionEpoch
      shapeProof: OfferDisabledDecision['shape_proof']
      reason: OfferDisabledDecision['offer_unavailable_reason']
      hardLimitClass?: NonNullable<OfferDisabledDecision['hard_limit_class']>
    }>
  | Readonly<{
      name: 'authority_transition'
      transition: 'stale_event_dropped'
      currentProjectionEpoch: ProjectionEpoch
      staleProjectionEpoch: ProjectionEpoch
      eventClass: 'capability_result' | 'artifact_action' | 'authority_result'
    }>

interface V2AuthorityActivationTraceContext {
  readonly name: 'authority_transition'
  readonly activationId: string
  readonly authenticatedShareInstanceId: string
  readonly selectionDigest: string
  readonly observedProtocolSessionId: string
  readonly projectionEpoch: ProjectionEpoch
  readonly observationRevision: number
  readonly artifactKind: ArtifactSpec['kind']
  readonly planKind: MaterializationPlan['kind']
}

export type V2AuthorityActivationTraceEvent = V2AuthorityActivationTraceContext & (
  | Readonly<{ transition: 'activation_started' }>
  | Readonly<{
      transition: 'prerequisite_waiting'
      waitingFor: 'authority' | 'resolution' | 'authority-and-resolution'
    }>
  | Readonly<{
      transition: 'retry_required'
      retryableDiscoveryReason: RetryableDiscoveryReason
    }>
  | Readonly<{ transition: 'artifact_resolved' }>
  | Readonly<{
      transition: 'semantic_invalidated'
      invalidationReason: V2ActivationInvalidationReason
    }>
  | Readonly<{ transition: 'commit_started' }>
  | Readonly<{
      transition: 'commit_pre_cut_retry'
      receiverOperationId?: string
    }>
  | Readonly<{
      transition: 'commit_bound_operation'
      receiverOperationId: string
    }>
  | Readonly<{
      transition: 'commit_owned_effects'
      receiverOperationId: string
    }>
  | Readonly<{
      transition: 'cleanup_completed'
      receiverOperationId?: string
    }>
  | Readonly<{
      transition: 'cleanup_failed'
      receiverOperationId: string
      failedStage: 'settlement' | 'detach'
    }>
)

export type V2ControllerWorkflowTraceEvent =
  | Readonly<{
      name: 'join_transition'
      transition: 'started' | 'joined' | 'failed' | 'stale_replacement'
    }>
  | V2AuthorityActivationTraceEvent
  | Readonly<{
      name: 'lifecycle_action_transition'
      transition: 'started' | 'completed' | 'failed' | 'excluded'
      action: LifecycleUserAction | 'expiry'
      lifecycleKind?: ReceiveLifecycleState['kind']
    }>

export type V2ReceiverTraceEvent =
  | ProjectionTraceEvent
  | TransferTraceEvent
  | V2AuthorityOfferTraceEvent
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
