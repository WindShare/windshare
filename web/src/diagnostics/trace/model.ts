import type {
  DirectZipCoordinationPayloadV1,
  DirectZipMemberRollbackPayloadV1,
  DirectZipMilestonePayloadV1,
} from './direct-zip-payload'
import type { ProtocolOperationPayloadV2 } from './protocol-payload'
import type { BrowserDeliveryPayloadV1 } from './browser-delivery-payload'
import type { LaneTransitionPayloadV1 } from './lane-payload'
import type { ReceiverExperiencePayloadV1 } from './experience-payload'
import type { TraceCapacityPolicy } from './capacity'
import type { CheckpointPayloadV1 } from './checkpoint-payload'
import type { RetainedActionPayloadV1, RetainedInventoryPayloadV1 } from './retained-payload'
import type {
  CapacityWaitTransitionPayloadV1,
  PerformancePhasePayloadV1,
  PerformanceSummaryPayloadV1,
  TransferProgressPayloadV1,
} from './transfer-payload'
import type { CorrelationV1 } from '../export/correlation-v1'
import type {
  DiagnosticEventEnvelopeV2,
  LifecycleStateV1,
  PeerFailureCodeV1,
} from '../export/incident-record-v2'
import type { IncidentScopeKind } from '../incident/scope'

export type {
  DirectZipCoordinationPayloadV1,
  DirectZipMemberRollbackPayloadV1,
  DirectZipMilestonePayloadV1,
} from './direct-zip-payload'

export const TRACE_CAPTURE_STATES = Object.freeze([
  'idle',
  'recording_pre_failure',
  'recording_post_failure',
  'sealed',
] as const)

export type TraceCaptureState = (typeof TRACE_CAPTURE_STATES)[number]

export const TRACE_SEAL_REASONS = Object.freeze([
  'manual_disable',
  'expired',
  'scope_terminal',
  'post_failure_silence',
  'capacity_exhausted',
] as const)

export type TraceSealReason = (typeof TRACE_SEAL_REASONS)[number]

export const TRACE_EVENT_NAMES_V2 = Object.freeze([
  'join_transition',
  'receiver_experience',
  'browse_transition',
  'preview_transition',
  'projection_transition',
  'authority_transition',
  'protocol_operation',
  'content_scheduling',
  'request_scheduling',
  'operation_recovery',
  'peer_attempt',
  'peer_recovery',
  'lane_transition',
  'receive_transition',
  'lifecycle_action_transition',
  'transfer_progress',
  'performance_phase',
  'performance_summary',
  'output_reservation',
  'output_write',
  'checkpoint',
  'settlement',
  'publication',
  'continuation',
  'reopen',
  'cleanup',
  'browser_delivery',
  'direct_zip_milestone',
  'direct_zip_coordination',
  'direct_zip_member_rollback',
  'retained_inventory',
  'retained_action',
  'incident_marker',
] as const)

export type TraceEventNameV2 = (typeof TRACE_EVENT_NAMES_V2)[number]

export type TraceDomainEventNameV2 = Exclude<TraceEventNameV2, 'incident_marker'>

type ArtifactKindV1 = 'original_file' | 'directory_tree' | 'zip_archive'
type PlanKindV1 =
  | 'direct_tree'
  | 'direct_atomic'
  | 'workspace_then_publish'
  | 'portable_handoff'
  | 'direct_resumable_zip'

type AuthorityActivationContextV1 = Readonly<{
  activation_id: string
  authenticated_share_instance_id: string
  selection_digest: string
  observed_protocol_session_id: string
  projection_epoch: string
  observation_revision: string
  artifact_kind: ArtifactKindV1
  plan_kind: PlanKindV1
}>

type AuthorityActivationTransitionV1 = AuthorityActivationContextV1 & (
  | Readonly<{ transition: 'activation_started' }>
  | Readonly<{
      transition: 'prerequisite_waiting'
      waiting_for: 'authority' | 'resolution' | 'authority_and_resolution'
    }>
  | Readonly<{
      transition: 'retry_required'
      retryable_discovery_reason:
        | 'catalog_temporarily_unavailable'
        | 'receiver_reconnecting'
        | 'generation_replay_interrupted'
    }>
  | Readonly<{ transition: 'artifact_resolved' }>
  | Readonly<{
      transition: 'semantic_invalidated'
      invalidation_reason:
        | 'selection_changed'
        | 'selection_empty'
        | 'artifact_shape_incompatible'
        | 'semantic_route_unavailable'
        | 'hard_limit_exceeded'
        | 'authenticated_share_instance_changed'
        | 'installed_route_changed'
        | 'caller_cancelled'
    }>
  | Readonly<{ transition: 'commit_started' }>
  | Readonly<{
      transition: 'commit_pre_cut_retry'
      receiver_operation_id?: string
    }>
  | Readonly<{
      transition: 'commit_bound_operation'
      receiver_operation_id: string
    }>
  | Readonly<{
      transition: 'commit_owned_effects'
      receiver_operation_id: string
    }>
  | Readonly<{
      transition: 'cleanup_completed'
      receiver_operation_id?: string
    }>
  | Readonly<{
      transition: 'cleanup_failed'
      receiver_operation_id: string
      failed_stage: 'settlement' | 'detach'
    }>
)

type AttemptTransitionV1 =
  | 'started'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'stale_replacement'

type ProjectionShapeProofV1 =
  | 'unknown'
  | 'none'
  | 'single_file'
  | 'tree'

type OutputBackendV1 = 'file_system_access' | 'origin_private' | 'portable'

/**
 * The V2 payload map is deliberately closed at the diagnostics boundary. Domain
 * adapters may discard richer product state, but they cannot add open strings or
 * authority-bearing values to an exported event.
 */
type SnakeStage<Value extends string> = Value extends `${infer Head}-${infer Tail}` ? `${Head}_${SnakeStage<Tail>}` : Value
interface PeerAttemptSummaryV1 {
  readonly last_completed_stage: SnakeStage<import('../../connectivity/diagnostics').V2BrowserConnectivityAttemptStage>
  readonly attempt_elapsed_ms: number
  readonly stage_elapsed_ms: number
  readonly deadline_expired: boolean
}

export interface TraceEventPayloadByNameV2 {
  readonly receiver_experience: ReceiverExperiencePayloadV1
  readonly join_transition: Readonly<{
    transition: 'started' | 'joined' | 'failed' | 'stale_replacement'
  }>
  readonly browse_transition:
    | Readonly<{ transition: AttemptTransitionV1 }>
    | Readonly<{ transition: 'page_loaded'; entry_count: string }>
  readonly preview_transition: Readonly<{
    attempt: 'open' | 'seek' | 'media'
    transition: AttemptTransitionV1 | 'closed'
  }>
  readonly projection_transition:
    | Readonly<{ transition: 'started'; projection_epoch: string }>
    | Readonly<{
        transition: 'refined'
        projection_epoch: string
        shape_proof: ProjectionShapeProofV1
        discovery_state: 'idle' | 'discovering' | 'bounded' | 'retryable_failure' | 'complete'
        file_count_lower_bound: string
        directory_count_lower_bound: string
        byte_count_lower_bound: string
        unsettled_target_count: string
      }>
    | Readonly<{
        transition: 'proven'
        projection_epoch: string
        shape_proof: Exclude<ProjectionShapeProofV1, 'unknown'>
        layout_basis_class:
          | 'unsettled'
          | 'complete_directory'
          | 'directory_selection'
          | 'synthetic_selection'
      }>
    | Readonly<{
        transition: 'retryable_failure'
        projection_epoch: string
        shape_proof: ProjectionShapeProofV1
        retryable_discovery_reason:
          | 'catalog_temporarily_unavailable'
          | 'receiver_reconnecting'
          | 'generation_replay_interrupted'
      }>
    | Readonly<{
        transition: 'retry_started'
        projection_epoch: string
        retained_shape_proof: ProjectionShapeProofV1
      }>
    | Readonly<{
        transition: 'stale_event_dropped'
        current_projection_epoch: string
        stale_projection_epoch: string
        event_class: 'catalog_evidence' | 'discovery_result'
      }>
  readonly authority_transition:
    | Readonly<{
        transition: 'offers_computed'
        projection_epoch: string
        shape_proof: ProjectionShapeProofV1
        offered_artifact_kinds: readonly ArtifactKindV1[]
        offered_plan_kinds: readonly PlanKindV1[]
        primary_artifact_kind: ArtifactKindV1
      }>
    | Readonly<{
        transition: 'offers_disabled'
        projection_epoch: string
        shape_proof: ProjectionShapeProofV1
        reason:
          | 'shape_unsettled'
          | 'selection_empty'
          | 'discovery_retry_required'
          | 'no_safe_destination'
          | 'permission_denied'
          | 'capability_changed'
          | 'portable_limit_exceeded'
          | 'workspace_limit_exceeded'
        hard_limit_class?: 'portable_artifact' | 'workspace_job' | 'workspace_process'
      }>
    | Readonly<{
        transition: 'stale_event_dropped'
        current_projection_epoch: string
        stale_projection_epoch: string
        event_class: 'capability_result' | 'artifact_action' | 'authority_result'
      }>
    | AuthorityActivationTransitionV1
  readonly request_scheduling: Readonly<{
    request_sequence: string
    request_kind: 'open_revisions' | 'list_children' | 'renew_lease' | 'release_lease'
    route: 'application-relay' | 'direct' | 'turn'
    transition: 'dispatched' | 'completed' | 'failed' | 'cancelled'
    expected_ms: number
    elapsed_ms: number
    pending_requests: number
  }>
  readonly content_scheduling: Readonly<{
    dispatch_sequence: string
    file_id: string
    block_index: string
    route: 'application-relay' | 'direct' | 'turn'
    purpose: 'content' | 'probe' | 'rescue'
    expected_ms: number
    pending_bytes: string
    bytes_per_second: number
  }>
  readonly operation_recovery: Readonly<{
    operation_sequence: string
    generation_id: string
    availability_revision: string
    lane_count: string
    unchanged_availability_retries: string
  }> & (
    | Readonly<{ transition: 'retry_available_lanes' | 'wait_for_generation' | 'exhausted' }>
    | Readonly<{ transition: 'wait_for_availability'; delay_ms: number }>
  )
  readonly protocol_operation: ProtocolOperationPayloadV2
  readonly peer_attempt:
    | Readonly<{ stage: 'provider_fact'; fact: import('../../connectivity/peer-set/provider-facts').PeerProviderFact }>
    | Readonly<{
        stage: 'started'
        wave_ordinal: string
        wave_attempt_ordinal: string
        session_attempt_ordinal: string
      }>
    | Readonly<{
        stage: 'negotiation_deadline_armed' | 'negotiation_deadline_expired'
        deadline_budget_ms: number
      }>
    | Readonly<{
        stage: 'offer_created' | 'offer_sent' | 'answer_received' | 'datachannel_open'
        local_candidates_emitted: string
        remote_candidates_accepted: string
      }>
    | Readonly<{
        stage: 'admission_deadline_armed' | 'admission_deadline_expired'
        deadline_budget_ms: number
      }>
    | Readonly<{ stage: 'grant_requested'; requested_lane_id: number }>
    | Readonly<{
        stage:
          | 'grant_received'
          | 'lane_hello_sent'
          | 'admission_response_received'
          | 'lane_attached'
      }>
    | Readonly<{ stage: 'admitted'; summary?: PeerAttemptSummaryV1 }>
    | Readonly<{
        stage: 'admission_response_settled'
        settlement:
          | Readonly<{ disposition: 'accepted' }>
          | Readonly<{
              disposition: 'rejected'
              rejection_code: number
              retry_after_ms: number
            }>
      }>
    | Readonly<{
        stage: 'failed'
        summary?: PeerAttemptSummaryV1
        failure?: import('../../connectivity/v2-peer-failure').V2PeerAttemptFailure
        failed_at_stage:
          | 'negotiation_deadline_armed'
          | 'negotiation_deadline_expired'
          | 'offer_created'
          | 'offer_sent'
          | 'answer_received'
          | 'datachannel_open'
          | 'admission_deadline_armed'
          | 'admission_deadline_expired'
          | 'grant_requested'
          | 'grant_received'
          | 'lane_hello_sent'
          | 'admission_response_received'
          | 'admission_response_settled'
          | 'lane_attached'
          | 'admitted'
        failure_scope: 'attempt-transient' | 'path-terminal' | 'session-terminal'
        code: PeerFailureCodeV1
        retryable: boolean
      }>
  readonly peer_recovery:
    | Readonly<{
        stage: 'wave_started' | 'wave_rearmed'
        wave_ordinal: string
        trigger: 'activation' | 'network_change' | 'detachment'
      }>
    | Readonly<{
        stage: 'retry_decided'
        wave_ordinal: string
        decision: 'retry_attempt' | 'stop_path' | 'stop_session'
        reason:
          | 'local_transient'
          | 'grant_expired'
          | 'admission_limited'
          | 'local_policy'
          | 'local_contract'
          | 'peer_operation_final'
          | 'lane_rejection_final'
          | 'untyped_failure'
          | 'session_terminal'
        authenticated_retry_after_ms: number
      }>
    | Readonly<{
        stage: 'backoff_scheduled'
        wave_ordinal: string
        retry_ordinal: string
        local_delay_ms: number
        authenticated_retry_after_ms: number
        effective_delay_ms: number
      }>
    | Readonly<{
        stage: 'attempt_replaced'
        wave_ordinal: string
      }>
    | Readonly<{
        stage: 'wave_quiesced'
        wave_ordinal: string
        reason: 'wave_attempt_budget' | 'wave_elapsed_budget'
      }>
    | Readonly<{ stage: 'peer_detached' }>
    | Readonly<{
        stage: 'session_budget_exhausted'
        reason: 'session_attempt_budget' | 'session_elapsed_budget'
      }>
    | Readonly<{
        stage: 'path_stopped'
        reason:
          | 'local_policy'
          | 'local_contract'
          | 'peer_operation_final'
          | 'lane_rejection_final'
          | 'untyped_failure'
      }>
    | Readonly<{
        stage: 'session_stopped'
        reason:
          | 'runtime_closed'
          | 'generation_retired'
          | 'binding_conflict'
          | 'continuation_conflict'
          | 'protocol_failure'
      }>
  readonly lane_transition: LaneTransitionPayloadV1
  readonly receive_transition:
    | Readonly<{
        transition: 'discovery_scheduling'
        operation_id: string
        transfer_job_id: string
        queue: 'generations' | 'zip_members'
        decision: 'waiting' | 'resumed' | 'cancelled' | 'complete'
        pending_items: string
        metadata_bytes: string
        maximum_items: string
        maximum_metadata_bytes: string
      }>
    | Readonly<{
        transition: 'download_connectivity'
        transfer_job_id: string
        connectivity: import('./transfer-payload').DownloadConnectivitySnapshot
      }>
    | Readonly<{
        transition: 'intent_frozen'
        artifact_kind: ArtifactKindV1
        layout_class:
          | 'original_file'
          | 'directory_tree_single_file'
          | 'directory_tree_result_root'
          | 'directory_tree_catalog_root'
          | 'zip_result_root'
        plan_kind: PlanKindV1
      }>
    | Readonly<{
        transition: 'directory_admitted'
        admitted_directory_count: string
        layout_class:
          | 'directory_tree_single_file'
          | 'directory_tree_result_root'
          | 'directory_tree_catalog_root'
          | 'zip_result_root'
      }>
    | Readonly<{ transition: 'materialization_started'; plan_kind: PlanKindV1 }>
    | Readonly<{
        transition: 'materialization_failed'
        plan_kind: PlanKindV1
        failure_reason:
          | 'file_open_failed'
          | 'source_revision_changed'
          | 'content_read_failed'
          | 'output_write_failed'
          | 'output_commit_failed'
          | 'directory_finalize_failed'
        completed_file_count: string
        completed_bytes: string
      }>
    | Readonly<{
        transition: 'materialization_completed'
        entry_count: string
        file_count: string
        directory_count: string
        raw_bytes: string
      }>
    | Readonly<{
        transition: 'tree_finalized'
        outcome: 'published' | 'partial_directory' | 'discarded'
        success_count: string
        failure_count: string
      }>
    | Readonly<{
        transition: 'worker_consequence_observed'
        worker_family: 'discovery' | 'prepared_files'
        failure_source: 'producer' | 'worker' | 'abort' | 'queue_close' | 'queue_abort'
        failure_source_index?: number
        operation_id: string
        transfer_job_id: string
        protocol_session_id?: string
        protocol_generation?: number
        output_session_id?: string
      }>
    | CapacityWaitTransitionPayloadV1
  readonly lifecycle_action_transition: Readonly<{
    transition: 'started' | 'completed' | 'failed' | 'excluded'
    action:
      | 'pause'
      | 'stop'
      | 'continue'
      | 'save'
      | 'save_staged_files'
      | 'cleanup_staging'
      | 'discard_incomplete_staging'
      | 'redownload'
      | 'change_location'
      | 'discard'
      | 'delete'
    lifecycle_state?: LifecycleStateV1
  }>
  readonly transfer_progress: TransferProgressPayloadV1
  readonly performance_phase: PerformancePhasePayloadV1
  readonly performance_summary: PerformanceSummaryPayloadV1
  readonly output_reservation: Readonly<{
    backend: OutputBackendV1
    transition: 'started' | 'acquired' | 'reopened' | 'failed'
  }>
  readonly output_write: Readonly<{
    backend: OutputBackendV1
    transition:
      | 'transaction_started'
      | 'transaction_failed'
      | 'transaction_committed'
      | 'commit_failed'
  }>
  readonly checkpoint: CheckpointPayloadV1
  readonly settlement: Readonly<{
    backend: OutputBackendV1
    transition: 'started' | 'completed' | 'failed' | 'ownership_unknown'
    outcome?:
      | 'published'
      | 'partial_directory'
      | 'resumable_receive'
      | 'discarded'
      | 'needs_attention'
  }>
  readonly publication: Readonly<{
    backend: OutputBackendV1
    transition: 'started' | 'committed' | 'not_committed' | 'unknown'
  }>
  readonly continuation: Readonly<{
    backend: OutputBackendV1
    transition: 'paused' | 'resumed' | 'admission_failed'
  }>
  readonly reopen: Readonly<{
    backend: Exclude<OutputBackendV1, 'portable'>
    transition: 'started' | 'authorized' | 'failed'
  }>
  readonly cleanup: Readonly<{
    backend: OutputBackendV1
    transition: 'started' | 'completed' | 'retryable_failure' | 'ownership_unknown' | 'failed'
    operation_id?: string
    receive_intent_digest?: string
    lifecycle_generation?: string
    cleanup_kind?: 'published_metadata'
  }>
  readonly direct_zip_milestone: DirectZipMilestonePayloadV1
  readonly browser_delivery: BrowserDeliveryPayloadV1
  readonly direct_zip_coordination: DirectZipCoordinationPayloadV1
  readonly direct_zip_member_rollback: DirectZipMemberRollbackPayloadV1
  readonly retained_inventory: RetainedInventoryPayloadV1
  readonly retained_action: RetainedActionPayloadV1
  readonly incident_marker: Readonly<{
    incident_sequence: string
    root_incident_sequence?: string
    scope: Readonly<{
      scope_kind: IncidentScopeKind
      scope_sequence: string
    }>
  }>
}

export type TraceEventPayloadV2 = TraceEventPayloadByNameV2[TraceEventNameV2]

type CorrelatedTraceEventNameV2 =
  | 'protocol_operation'
  | 'operation_recovery'
  | 'content_scheduling'
  | 'request_scheduling'
  | 'peer_attempt'
  | 'peer_recovery'
  | 'lane_transition'

export type TraceEventObservationV2 = {
  readonly [Name in TraceDomainEventNameV2]: Readonly<{
    eventName: Name
    payload: TraceEventPayloadByNameV2[Name]
  } & (Name extends CorrelatedTraceEventNameV2
    ? { correlation: CorrelationV1 }
    : { correlation?: CorrelationV1 })>
}[TraceDomainEventNameV2]

export type TraceEventRecordV2 = {
  readonly [Name in TraceEventNameV2]: Readonly<
    DiagnosticEventEnvelopeV2<TraceEventPayloadByNameV2[Name]> & {
      readonly level: 'debug'
      readonly event: Name
    }
  >
}[TraceEventNameV2]

export type TraceHealthCounter =
  | 'droppedCount'
  | 'overwrittenCount'
  | 'sampledCount'
  | 'coalescedCount'

export interface TraceHealthSnapshot {
  readonly droppedCount: bigint
  readonly overwrittenCount: bigint
  readonly sampledCount: bigint
  readonly coalescedCount: bigint
}

export type TraceCapturedValue<Event, Incident> =
  | Readonly<{ kind: 'event'; event: Event; eventName: Exclude<TraceEventNameV2, 'incident_marker'> }>
  | Readonly<{ kind: 'incident_marker'; incident: Incident; eventName: 'incident_marker' }>

export interface TraceCapturedEvent<Event, Incident> {
  readonly sequence: bigint
  readonly observedAtMilliseconds: number
  readonly elapsedMs: bigint
  readonly encodedBytes: number
  readonly value: TraceCapturedValue<Event, Incident>
}

export interface TraceCaptureSnapshot<Event, Incident> {
  readonly state: Exclude<TraceCaptureState, 'idle'>
  readonly captureGeneration: bigint
  readonly startedAtMilliseconds: number
  readonly sealReason?: TraceSealReason
  readonly retainedEventCount: bigint
  readonly retainedEventBytes: bigint
  readonly incidentMarkerCount: bigint
  readonly events: readonly TraceCapturedEvent<Event, Incident>[]
  readonly health: TraceHealthSnapshot
}

export interface TraceCoreStatus {
  readonly state: TraceCaptureState
  readonly enabled: boolean
  readonly captureGeneration: bigint
  readonly expiresAtMilliseconds?: number
  readonly sealReason?: TraceSealReason
  readonly capacity: TraceCapacityPolicy
  readonly retainedEventCount: bigint
  readonly retainedEventBytes: bigint
  readonly incidentMarkerCount: bigint
  readonly health: TraceHealthSnapshot
}
