import type { DomainTraceSource } from '../../../diagnostics/trace/ports'
import type {
  CompatibleNameRepairSummary,
} from '../../../transfer/lifecycle/name-repair'
import type { IncidentScopeHandle, IncidentScopeKind, IncidentScopeOwner, PresentationDecision } from '../../../diagnostics/incident'
import type { ReceiveLifecycleState } from '../../../output/workspace'
import type { OfferComputedDecision, OfferDisabledDecision, ProjectionEpoch, RetryableDiscoveryReason } from '../../../output/planning'
import type { ArtifactSpec, MaterializationPlan } from '../../../transfer/intent'
import type { ProjectionTraceEvent } from '../../../transfer/projection'
import type { TransferTraceEvent } from '../../../transfer/job'
import type { LifecycleUserAction } from '../actions'
import type { RetainedReceiveAction, RetainedReceiveOperation } from '../contracts'
import type { ActivationInvalidationReason } from '../activation/model'

export type RetainedInventoryTraceEvent =
  | Readonly<{ name: 'receive.inventory.load.started' }>
  | Readonly<{ name: 'receive.inventory.load.completed'; operation_count: number }>
  | Readonly<{ name: 'receive.inventory.load.failed' }>
  | Readonly<{
      name:
        | 'receive.inventory.action.started'
        | 'receive.inventory.action.completed'
        | 'receive.inventory.action.failed'
      retained_action: RetainedReceiveAction
      continuation: RetainedReceiveOperation['continuation']
    }>

/**
 * Repaired output runtimes own this projection; ordinary runtimes omit it entirely so
 * their hot path never constructs a ledger reader, subscriber, or mapping snapshot.
 */
export interface ActiveCompatibleNameRepairProjection {
  /**
   * The source synchronously emits its latest durable summary, when one exists, before
   * returning the unsubscribe function. Later emissions represent monotonic durable state.
   */
  subscribe(listener: (summary: CompatibleNameRepairSummary) => void): () => void
}

/**
 * Implementations perform one operation-header point read. Undefined means there is no
 * user-visible repair summary; the boundary must never infer repair state from lifecycle data.
 */
export interface RetainedCompatibleNameRepairSource {
  readRepairSummary(
    operationId: string,
    signal: AbortSignal,
  ): PromiseLike<CompatibleNameRepairSummary | undefined>
}

export type AuthorityOfferTraceEvent =
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

interface AuthorityActivationTraceContext {
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

export type AuthorityActivationTraceEvent = AuthorityActivationTraceContext & (
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
      invalidationReason: ActivationInvalidationReason
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

export type ControllerWorkflowTraceEvent =
  | Readonly<{
      name: 'join_transition'
      transition: 'started' | 'joined' | 'failed' | 'stale_replacement'
    }>
  | AuthorityActivationTraceEvent
  | Readonly<{
      name: 'lifecycle_action_transition'
      transition: 'started' | 'completed' | 'failed' | 'excluded'
      action: LifecycleUserAction
      lifecycleKind?: ReceiveLifecycleState['kind']
      operationId?: string
      generation?: bigint
      artifactId?: string
      checkpointId?: string
      checkpointGeneration?: bigint
      settlement?: import('../../../transfer/lifecycle/interruption').ReceiveStorageSettlement
    }>

export type ReceiverExperienceTraceEvent =
  | Readonly<{ name: 'receiver_experience'; transition: 'ownership'; operationId: string; generation: bigint;
      state: 'releasing' | 'released' | 'release-failed'; lifecycleKind: ReceiveLifecycleState['kind'] }>
  | Readonly<{ name: 'receiver_experience'; transition: 'task'; operationId: string; generation: bigint;
      stage: string; reason: string; attention: boolean; completeness: string; publication: string;
      elapsedMilliseconds: number | null }>
  | Readonly<{ name: 'receiver_experience'; transition: 'saving'; projectionEpoch: bigint | null;
      choiceId: string | null; outcome: string | null; reason: string }>
  | Readonly<{ name: 'receiver_experience'; transition: 'intent'; action: string;
      operationId: string | null; generation: bigint }>

export type ReceiverTraceEvent =
  | ReceiverExperienceTraceEvent
  | ProjectionTraceEvent
  | TransferTraceEvent
  | AuthorityOfferTraceEvent
  | ControllerWorkflowTraceEvent
  | RetainedInventoryTraceEvent

export interface ReceiverIncidentPort {
  openScope(kind: IncidentScopeKind): IncidentScopeOwner
  submitDecision(scope: IncidentScopeHandle, decision: PresentationDecision): void
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

export interface ReceiveObservationOptions {
  readonly incidents?: ReceiverIncidentPort
  readonly trace?: DomainTraceSource<ReceiverTraceEvent>
}
