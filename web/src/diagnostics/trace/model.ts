import type { TraceCapacityPolicy } from './capacity'
import type { CorrelationV1 } from '../export/correlation-v1'
import type {
  DiagnosticEventEnvelopeV1,
  LifecycleStateV1,
  PeerFailureCodeV1,
  ProtocolFailureV1,
} from '../export/incident-record-v1'
import type { ProtocolMessageKindV1 } from '../incident/fact'
import type { IncidentScopeKind } from '../incident/scope'

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

export const TRACE_EVENT_NAMES_V1 = Object.freeze([
  'join_transition',
  'browse_transition',
  'preview_transition',
  'projection_transition',
  'authority_transition',
  'protocol_operation',
  'peer_attempt',
  'peer_recovery',
  'lane_transition',
  'receive_transition',
  'lifecycle_action_transition',
  'transfer_progress',
  'output_reservation',
  'output_write',
  'checkpoint',
  'settlement',
  'publication',
  'continuation',
  'reopen',
  'cleanup',
  'retained_inventory',
  'retained_action',
  'incident_marker',
] as const)

export type TraceEventNameV1 = (typeof TRACE_EVENT_NAMES_V1)[number]

export type TraceDomainEventNameV1 = Exclude<TraceEventNameV1, 'incident_marker'>

type ArtifactKindV1 = 'original_file' | 'directory_tree' | 'zip_archive'
type PlanKindV1 =
  | 'direct_tree'
  | 'direct_atomic'
  | 'workspace_then_publish'
  | 'portable_handoff'

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
 * The V1 payload map is deliberately closed at the diagnostics boundary. Domain
 * adapters may discard richer product state, but they cannot add open strings or
 * authority-bearing values to an exported event.
 */
export interface TraceEventPayloadByNameV1 {
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
        discovery_state: 'idle' | 'discovering' | 'retryable_failure' | 'complete'
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
  readonly protocol_operation:
    | Readonly<{
        transition: 'request_sent' | 'request_send_failed' | 'cancelled'
        request_kind: ProtocolMessageKindV1
      }>
    | Readonly<{
        transition: 'response_received'
        request_kind: ProtocolMessageKindV1
        response_kind: ProtocolMessageKindV1
      }>
    | Readonly<{
        transition: 'authenticated_failure'
        request_kind: ProtocolMessageKindV1
        protocol_failure: ProtocolFailureV1
      }>
    | Readonly<{
        transition: 'settled'
        request_kind: ProtocolMessageKindV1
        settlement: 'remote_final' | 'local_cancel' | 'session_terminal'
      }>
  readonly peer_attempt:
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
          | 'admitted'
      }>
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
        failure_scope: 'attempt' | 'session'
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
  readonly lane_transition:
    | Readonly<{
        transition:
          | 'attached'
          | 'grant_requested'
          | 'grant_received'
          | 'hello_sent'
          | 'admission_accepted'
          | 'installed'
      }>
    | Readonly<{
        transition: 'admission_rejected'
        rejection_code: number
        retry_after_ms: number
      }>
    | Readonly<{
        transition: 'detached'
        detachment_class: 'closed' | 'physical_failure' | 'authenticated_failure'
      }>
  readonly receive_transition:
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
  readonly lifecycle_action_transition: Readonly<{
    transition: 'started' | 'completed' | 'failed' | 'excluded'
    action:
      | 'pause'
      | 'stop'
      | 'continue'
      | 'save'
      | 'redownload'
      | 'change_location'
      | 'discard'
      | 'delete'
      | 'expiry'
    lifecycle_state?: LifecycleStateV1
  }>
  readonly transfer_progress: Readonly<{
    discovered_files: string
    discovered_bytes: string
    written_bytes: string
    completed_files: string
    completed_bytes: string
    file_errors: string
    selection_errors: string
    failed_directories: string
    content_lanes: number
    discovery: 'open' | 'complete' | 'failed'
    partial: boolean
  }>
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
  readonly checkpoint: Readonly<{
    backend: Exclude<OutputBackendV1, 'portable'>
    transition: 'restored' | 'persisted' | 'quarantined' | 'failed'
    decision?: 'absent' | 'installed' | 'exact' | 'revision_conflict' | 'ownership_conflict' | 'invalid'
  }>
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
  }>
  readonly retained_inventory:
    | Readonly<{ transition: 'load_started' | 'load_failed' }>
    | Readonly<{ transition: 'load_completed'; operation_count: string }>
  readonly retained_action: Readonly<{
    transition: 'started' | 'completed' | 'failed' | 'excluded'
    action: 'continue' | 'save' | 'redownload' | 'discard' | 'delete'
    continuation:
      | 'resume_receive'
      | 'resume_package'
      | 'save_artifact'
      | 'retry_download'
      | 'cleanup_expired'
      | 'retry_cleanup'
      | 'needs_attention'
  }>
  readonly incident_marker: Readonly<{
    incident_sequence: string
    root_incident_sequence?: string
    scope: Readonly<{
      scope_kind: IncidentScopeKind
      scope_sequence: string
    }>
  }>
}

export type TraceEventPayloadV1 = TraceEventPayloadByNameV1[TraceEventNameV1]

type CorrelatedTraceEventNameV1 =
  | 'protocol_operation'
  | 'peer_attempt'
  | 'peer_recovery'
  | 'lane_transition'

export type TraceEventObservationV1 = {
  readonly [Name in TraceDomainEventNameV1]: Readonly<{
    eventName: Name
    payload: TraceEventPayloadByNameV1[Name]
  } & (Name extends CorrelatedTraceEventNameV1
    ? { correlation: CorrelationV1 }
    : { correlation?: CorrelationV1 })>
}[TraceDomainEventNameV1]

export type TraceEventRecordV1 = {
  readonly [Name in TraceEventNameV1]: Readonly<
    DiagnosticEventEnvelopeV1<TraceEventPayloadByNameV1[Name]> & {
      readonly level: 'debug'
      readonly event: Name
    }
  >
}[TraceEventNameV1]

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
  | Readonly<{ kind: 'event'; event: Event; eventName: Exclude<TraceEventNameV1, 'incident_marker'> }>
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
