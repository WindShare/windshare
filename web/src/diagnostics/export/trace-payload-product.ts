import {
  booleanValue,
  decimalFields,
  decimalUint64,
  exactKeys,
  member,
  memberList,
  uint32,
  type UnknownRecord,
} from './trace-payload-validation'

const ARTIFACT_KINDS = ['original_file', 'directory_tree', 'zip_archive'] as const
const PLAN_KINDS = [
  'direct_tree',
  'direct_atomic',
  'workspace_then_publish',
  'portable_handoff',
  'direct_resumable_zip',
] as const
const ATTEMPT_TRANSITIONS = [
  'started',
  'completed',
  'failed',
  'cancelled',
  'stale_replacement',
] as const
const PROJECTION_SHAPE_PROOFS = ['unknown', 'none', 'single_file', 'tree'] as const
const PROVEN_PROJECTION_SHAPE_PROOFS = ['none', 'single_file', 'tree'] as const
const OUTPUT_BACKENDS = ['file_system_access', 'origin_private', 'portable'] as const
const CHECKPOINT_BACKENDS = ['file_system_access', 'origin_private'] as const
const CANONICAL_IDENTITY = /^[A-Za-z0-9_-]{21}[AQgw]$/
const CANONICAL_DIGEST = /^[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$/
const ZERO_IDENTITY = 'A'.repeat(22)
const ZERO_DIGEST = 'A'.repeat(43)
const AUTHORITY_ACTIVATION_CONTEXT_KEYS = [
  'activation_id',
  'authenticated_share_instance_id',
  'selection_digest',
  'observed_protocol_session_id',
  'projection_epoch',
  'observation_revision',
  'artifact_kind',
  'plan_kind',
] as const
const LIFECYCLE_STATES = [
  'intent_frozen',
  'preparing',
  'receiving',
  'resumable_receive',
  'finalizing_tree',
  'committing_atomic',
  'materialization_sealed',
  'packaging',
  'resumable_package',
  'artifact_sealed',
  'waiting_to_save',
  'publishing_managed',
  'handing_off',
  'published',
  'download_started',
  'partial_directory',
  'restart_required',
  'discarded',
  'expired',
  'needs_attention',
  'authorization_required',
  'target_verification_required',
  'destination_space_required',
] as const

export function validateJoin(payload: UnknownRecord): void {
  exactKeys(payload, ['transition'], [], 'join_transition payload')
  member(payload.transition, ['started', 'joined', 'failed', 'stale_replacement'],
    'join_transition transition')
}

export function validateBrowse(payload: UnknownRecord): void {
  if (payload.transition === 'page_loaded') {
    exactKeys(payload, ['transition', 'entry_count'], [], 'browse_transition page_loaded payload')
    decimalUint64(payload.entry_count, 'browse_transition entry_count')
    return
  }
  exactKeys(payload, ['transition'], [], 'browse_transition attempt payload')
  member(payload.transition, ATTEMPT_TRANSITIONS, 'browse_transition transition')
}

export function validatePreview(payload: UnknownRecord): void {
  exactKeys(payload, ['attempt', 'transition'], [], 'preview_transition payload')
  member(payload.attempt, ['open', 'seek', 'media'], 'preview_transition attempt')
  member(payload.transition, [...ATTEMPT_TRANSITIONS, 'closed'], 'preview_transition transition')
}

export function validateProjection(payload: UnknownRecord): void {
  switch (payload.transition) {
    case 'started':
      exactKeys(payload, ['transition', 'projection_epoch'], [], 'projection started payload')
      decimalUint64(payload.projection_epoch, 'projection epoch')
      return
    case 'refined':
      exactKeys(payload, [
        'transition', 'projection_epoch', 'shape_proof', 'discovery_state',
        'file_count_lower_bound', 'directory_count_lower_bound', 'byte_count_lower_bound',
        'unsettled_target_count',
      ], [], 'projection refined payload')
      decimalUint64(payload.projection_epoch, 'projection epoch')
      member(payload.shape_proof, PROJECTION_SHAPE_PROOFS, 'projection shape proof')
      member(payload.discovery_state,
        ['idle', 'discovering', 'retryable_failure', 'complete'],
        'projection discovery state')
      decimalFields(payload, [
        'file_count_lower_bound', 'directory_count_lower_bound', 'byte_count_lower_bound',
        'unsettled_target_count',
      ], 'projection refined')
      return
    case 'proven':
      exactKeys(payload, [
        'transition', 'projection_epoch', 'shape_proof', 'layout_basis_class',
      ], [], 'projection proven payload')
      decimalUint64(payload.projection_epoch, 'projection epoch')
      member(payload.shape_proof, PROVEN_PROJECTION_SHAPE_PROOFS, 'projection shape proof')
      member(payload.layout_basis_class, [
        'unsettled', 'complete_directory', 'directory_selection', 'synthetic_selection',
      ], 'projection layout basis class')
      return
    case 'retryable_failure':
      exactKeys(payload, [
        'transition', 'projection_epoch', 'shape_proof', 'retryable_discovery_reason',
      ], [], 'projection retryable_failure payload')
      decimalUint64(payload.projection_epoch, 'projection epoch')
      member(payload.shape_proof, PROJECTION_SHAPE_PROOFS, 'projection shape proof')
      member(payload.retryable_discovery_reason, [
        'catalog_temporarily_unavailable',
        'receiver_reconnecting',
        'generation_replay_interrupted',
      ], 'projection retryable discovery reason')
      return
    case 'retry_started':
      exactKeys(payload, [
        'transition', 'projection_epoch', 'retained_shape_proof',
      ], [], 'projection retry_started payload')
      decimalUint64(payload.projection_epoch, 'projection epoch')
      member(payload.retained_shape_proof, PROJECTION_SHAPE_PROOFS,
        'projection retained shape proof')
      return
    case 'stale_event_dropped':
      exactKeys(payload, [
        'transition', 'current_projection_epoch', 'stale_projection_epoch', 'event_class',
      ], [], 'projection stale_event_dropped payload')
      decimalUint64(payload.current_projection_epoch, 'current projection epoch')
      decimalUint64(payload.stale_projection_epoch, 'stale projection epoch')
      member(payload.event_class, ['catalog_evidence', 'discovery_result'],
        'projection event class')
      return
    default:
      throw new TypeError('projection_transition discriminant is invalid')
  }
}

export function validateAuthority(payload: UnknownRecord): void {
  switch (payload.transition) {
    case 'offers_computed':
      exactKeys(payload, [
        'transition', 'projection_epoch', 'shape_proof', 'offered_artifact_kinds',
        'offered_plan_kinds', 'primary_artifact_kind',
      ], [], 'authority offers_computed payload')
      decimalUint64(payload.projection_epoch, 'authority projection epoch')
      member(payload.shape_proof, PROJECTION_SHAPE_PROOFS, 'authority shape proof')
      memberList(payload.offered_artifact_kinds, ARTIFACT_KINDS,
        'authority offered artifact kinds')
      memberList(payload.offered_plan_kinds, PLAN_KINDS, 'authority offered plan kinds')
      member(payload.primary_artifact_kind, ARTIFACT_KINDS, 'authority primary artifact kind')
      return
    case 'offers_disabled':
      exactKeys(payload, [
        'transition', 'projection_epoch', 'shape_proof', 'reason',
      ], ['hard_limit_class'], 'authority offers_disabled payload')
      decimalUint64(payload.projection_epoch, 'authority projection epoch')
      member(payload.shape_proof, PROJECTION_SHAPE_PROOFS, 'authority shape proof')
      member(payload.reason, [
        'shape_unsettled', 'selection_empty', 'discovery_retry_required',
        'no_safe_destination', 'permission_denied', 'capability_changed',
        'portable_limit_exceeded', 'workspace_limit_exceeded',
      ], 'authority disabled reason')
      if (payload.hard_limit_class !== undefined) {
        member(payload.hard_limit_class,
          ['portable_artifact', 'workspace_job', 'workspace_process'],
          'authority hard limit class')
      }
      return
    case 'stale_event_dropped':
      exactKeys(payload, [
        'transition', 'current_projection_epoch', 'stale_projection_epoch', 'event_class',
      ], [], 'authority stale_event_dropped payload')
      decimalUint64(payload.current_projection_epoch, 'current projection epoch')
      decimalUint64(payload.stale_projection_epoch, 'stale projection epoch')
      member(payload.event_class,
        ['capability_result', 'artifact_action', 'authority_result'],
        'authority event class')
      return
    case 'activation_started':
    case 'artifact_resolved':
    case 'commit_started':
      validateAuthorityActivationContext(payload)
      return
    case 'commit_pre_cut_retry':
    case 'cleanup_completed':
      validateAuthorityActivationContext(payload, [], ['receiver_operation_id'])
      if (payload.receiver_operation_id !== undefined) {
        canonicalIdentity(payload.receiver_operation_id, 'authority receiver operation ID')
      }
      return
    case 'prerequisite_waiting':
      validateAuthorityActivationContext(payload, ['waiting_for'])
      member(payload.waiting_for, ['authority', 'resolution', 'authority_and_resolution'],
        'authority waiting prerequisite')
      return
    case 'retry_required':
      validateAuthorityActivationContext(payload, ['retryable_discovery_reason'])
      member(payload.retryable_discovery_reason, [
        'catalog_temporarily_unavailable',
        'receiver_reconnecting',
        'generation_replay_interrupted',
      ], 'authority retryable discovery reason')
      return
    case 'semantic_invalidated':
      validateAuthorityActivationContext(payload, ['invalidation_reason'])
      member(payload.invalidation_reason, [
        'selection_changed',
        'selection_empty',
        'artifact_shape_incompatible',
        'semantic_route_unavailable',
        'hard_limit_exceeded',
        'authenticated_share_instance_changed',
        'installed_route_changed',
        'caller_cancelled',
      ], 'authority invalidation reason')
      return
    case 'commit_bound_operation':
    case 'commit_owned_effects':
      validateAuthorityActivationContext(payload, ['receiver_operation_id'])
      canonicalIdentity(payload.receiver_operation_id, 'authority receiver operation ID')
      return
    case 'cleanup_failed':
      validateAuthorityActivationContext(payload, ['receiver_operation_id', 'failed_stage'])
      canonicalIdentity(payload.receiver_operation_id, 'authority receiver operation ID')
      member(payload.failed_stage, ['settlement', 'detach'], 'authority cleanup failed stage')
      return
    default:
      throw new TypeError('authority_transition discriminant is invalid')
  }
}

function validateAuthorityActivationContext(
  payload: UnknownRecord,
  required: readonly string[] = [],
  optional: readonly string[] = [],
): void {
  exactKeys(payload, ['transition', ...AUTHORITY_ACTIVATION_CONTEXT_KEYS, ...required], optional,
    'authority activation payload')
  canonicalIdentity(payload.activation_id, 'authority activation ID')
  canonicalIdentity(payload.authenticated_share_instance_id,
    'authority authenticated share-instance ID')
  canonicalDigest(payload.selection_digest, 'authority selection digest')
  canonicalIdentity(payload.observed_protocol_session_id,
    'authority observed ProtocolSession ID')
  decimalUint64(payload.projection_epoch, 'authority projection epoch')
  decimalUint64(payload.observation_revision, 'authority observation revision')
  member(payload.artifact_kind, ARTIFACT_KINDS, 'authority artifact kind')
  member(payload.plan_kind, PLAN_KINDS, 'authority plan kind')
}

function canonicalIdentity(value: unknown, field: string): void {
  if (typeof value !== 'string' || !CANONICAL_IDENTITY.test(value) || value === ZERO_IDENTITY) {
    throw new TypeError(`${field} must be a canonical non-zero 16-byte base64url identity`)
  }
}

function canonicalDigest(value: unknown, field: string): void {
  if (typeof value !== 'string' || !CANONICAL_DIGEST.test(value) || value === ZERO_DIGEST) {
    throw new TypeError(`${field} must be a canonical non-zero 32-byte base64url digest`)
  }
}

export function validateReceive(payload: UnknownRecord): void {
  switch (payload.transition) {
    case 'intent_frozen':
      exactKeys(payload, ['transition', 'artifact_kind', 'layout_class', 'plan_kind'], [],
        'receive intent payload')
      member(payload.artifact_kind, ARTIFACT_KINDS, 'receive artifact kind')
      member(payload.layout_class, [
        'original_file', 'directory_tree_single_file', 'directory_tree_result_root',
        'directory_tree_catalog_root', 'zip_result_root',
      ], 'receive layout class')
      member(payload.plan_kind, PLAN_KINDS, 'receive plan kind')
      return
    case 'directory_admitted':
      exactKeys(payload, ['transition', 'admitted_directory_count', 'layout_class'], [],
        'receive directory admission payload')
      decimalUint64(payload.admitted_directory_count, 'admitted directory count')
      member(payload.layout_class, [
        'directory_tree_single_file', 'directory_tree_result_root',
        'directory_tree_catalog_root', 'zip_result_root',
      ], 'receive directory layout class')
      return
    case 'materialization_started':
      exactKeys(payload, ['transition', 'plan_kind'], [], 'materialization started payload')
      member(payload.plan_kind, PLAN_KINDS, 'materialization plan kind')
      return
    case 'materialization_failed':
      exactKeys(payload, [
        'transition', 'plan_kind', 'failure_reason', 'completed_file_count',
        'completed_bytes',
      ], [], 'materialization failed payload')
      member(payload.plan_kind, PLAN_KINDS, 'materialization plan kind')
      member(payload.failure_reason, [
        'file_open_failed', 'source_revision_changed', 'content_read_failed',
        'output_write_failed', 'output_commit_failed', 'directory_finalize_failed',
      ], 'materialization failure reason')
      decimalFields(payload, ['completed_file_count', 'completed_bytes'],
        'materialization failure')
      return
    case 'materialization_completed':
      exactKeys(payload, [
        'transition', 'entry_count', 'file_count', 'directory_count', 'raw_bytes',
      ], [], 'materialization completed payload')
      decimalFields(payload, ['entry_count', 'file_count', 'directory_count', 'raw_bytes'],
        'materialization completed')
      return
    case 'tree_finalized':
      exactKeys(payload, ['transition', 'outcome', 'success_count', 'failure_count'], [],
        'tree finalized payload')
      member(payload.outcome, ['published', 'partial_directory', 'discarded'],
        'tree finalization outcome')
      decimalFields(payload, ['success_count', 'failure_count'], 'tree finalized')
      return
    case 'worker_consequence_observed': {
      exactKeys(payload, [
        'transition', 'worker_family', 'failure_source', 'operation_id', 'transfer_job_id',
      ], [
        'failure_source_index', 'protocol_session_id', 'protocol_generation', 'output_session_id',
      ], 'worker consequence payload')
      member(payload.worker_family, ['discovery', 'prepared_files'], 'worker family')
      member(payload.failure_source, [
        'producer', 'worker', 'abort', 'queue_close', 'queue_abort',
      ], 'worker consequence source')
      const indexed = payload.failure_source === 'worker' ||
        payload.failure_source === 'queue_close' ||
        payload.failure_source === 'queue_abort'
      if (indexed !== (payload.failure_source_index !== undefined)) {
        throw new TypeError('worker consequence source index disagrees with its source')
      }
      if (payload.failure_source_index !== undefined) {
        uint32(payload.failure_source_index, 'worker consequence source index')
      }
      canonicalIdentity(payload.operation_id, 'worker consequence operation ID')
      canonicalIdentity(payload.transfer_job_id, 'worker consequence transfer job ID')
      const hasProtocolSession = payload.protocol_session_id !== undefined
      const hasProtocolGeneration = payload.protocol_generation !== undefined
      if (hasProtocolSession !== hasProtocolGeneration) {
        throw new TypeError('worker consequence protocol context must be complete')
      }
      if (hasProtocolSession) {
        canonicalIdentity(payload.protocol_session_id, 'worker consequence protocol session ID')
        if (uint32(payload.protocol_generation, 'worker consequence protocol generation') === 0) {
          throw new RangeError('worker consequence protocol generation must be positive')
        }
      }
      if (payload.output_session_id !== undefined) {
        canonicalIdentity(payload.output_session_id, 'worker consequence output session ID')
      }
      return
    }
    case 'capacity_retry_scheduled':
    case 'capacity_retry_succeeded':
    case 'capacity_wait_budget_paused':
    case 'capacity_wait_cancelled':
    case 'capacity_generation_replaced':
      exactKeys(payload, [
        'transition', 'capacity_wait_id', 'capacity_surface', 'receive_operation_id',
        'transfer_job_id', 'protocol_session_id', 'protocol_operation_id', 'attempt',
        'sender_hint_ms', 'jitter_ms', 'delay_ms', 'accumulated_wait_ms', 'active_waiters',
      ], [], 'capacity wait payload')
      canonicalIdentity(payload.capacity_wait_id, 'capacity wait ID')
      member(payload.capacity_surface, ['revision_open', 'block_range'], 'capacity wait surface')
      canonicalIdentity(payload.receive_operation_id, 'capacity receive operation ID')
      canonicalIdentity(payload.transfer_job_id, 'capacity transfer job ID')
      canonicalIdentity(payload.protocol_session_id, 'capacity ProtocolSession ID')
      canonicalIdentity(payload.protocol_operation_id, 'capacity protocol operation ID')
      decimalUint64(payload.attempt, 'capacity wait attempt')
      uint32(payload.sender_hint_ms, 'capacity sender hint')
      uint32(payload.jitter_ms, 'capacity jitter')
      uint32(payload.delay_ms, 'capacity delay')
      uint32(payload.accumulated_wait_ms, 'capacity accumulated wait')
      uint32(payload.active_waiters, 'capacity active waiters')
      return
    default:
      throw new TypeError('receive_transition discriminant is invalid')
  }
}

export function validateLifecycleAction(payload: UnknownRecord): void {
  exactKeys(payload, ['transition', 'action'], ['lifecycle_state'],
    'lifecycle_action_transition payload')
  member(payload.transition, ['started', 'completed', 'failed', 'excluded'],
    'lifecycle action transition')
  member(payload.action, [
    'pause', 'stop', 'continue', 'save', 'redownload', 'change_location',
    'discard', 'delete', 'expiry',
  ], 'lifecycle action')
  if (payload.lifecycle_state !== undefined) {
    member(payload.lifecycle_state, LIFECYCLE_STATES, 'lifecycle state')
  }
}

export function validateTransferProgress(payload: UnknownRecord): void {
  const decimalKeys = [
    'discovered_files', 'discovered_bytes', 'written_bytes', 'completed_files',
    'completed_bytes', 'file_errors', 'selection_errors', 'failed_directories',
    'capacity_waiting_files', 'capacity_accumulated_wait_ms', 'capacity_wait_attempts',
  ] as const
  exactKeys(payload, [
    ...decimalKeys, 'content_lanes', 'capacity_wait_visible',
    'discovery', 'partial',
  ], [],
    'transfer_progress payload')
  decimalFields(payload, decimalKeys, 'transfer progress')
  uint32(payload.content_lanes, 'transfer progress content lanes')
  booleanValue(payload.capacity_wait_visible, 'capacity wait visibility')
  member(payload.discovery, ['open', 'complete', 'failed'], 'transfer discovery state')
  booleanValue(payload.partial, 'transfer partial flag')
}

export function validateOutputReservation(payload: UnknownRecord): void {
  validateOutputPair(payload, OUTPUT_BACKENDS, [
    'started', 'acquired', 'reopened', 'failed',
  ], 'output_reservation')
}

export function validateOutputWrite(payload: UnknownRecord): void {
  validateOutputPair(payload, OUTPUT_BACKENDS, [
    'transaction_started', 'transaction_failed', 'transaction_committed', 'commit_failed',
  ], 'output_write')
}

export function validateCheckpoint(payload: UnknownRecord): void {
  exactKeys(payload, ['backend', 'transition'], ['decision'], 'checkpoint payload')
  member(payload.backend, CHECKPOINT_BACKENDS, 'checkpoint backend')
  member(payload.transition, [
    'restored', 'persisted', 'quarantined', 'failed',
  ], 'checkpoint transition')
  if (payload.decision !== undefined) {
    member(payload.decision, [
      'absent', 'installed', 'exact', 'revision_conflict', 'ownership_conflict', 'invalid',
    ], 'checkpoint decision')
  }
}

export function validateSettlement(payload: UnknownRecord): void {
  exactKeys(payload, ['backend', 'transition'], ['outcome'], 'settlement payload')
  member(payload.backend, OUTPUT_BACKENDS, 'settlement backend')
  member(payload.transition, ['started', 'completed', 'failed', 'ownership_unknown'],
    'settlement transition')
  if (payload.outcome !== undefined) {
    member(payload.outcome, [
      'published', 'partial_directory', 'resumable_receive', 'discarded', 'needs_attention',
    ], 'settlement outcome')
  }
}

export function validatePublication(payload: UnknownRecord): void {
  validateOutputPair(payload, OUTPUT_BACKENDS, [
    'started', 'committed', 'not_committed', 'unknown',
  ], 'publication')
}

export function validateContinuation(payload: UnknownRecord): void {
  validateOutputPair(payload, OUTPUT_BACKENDS, [
    'paused', 'resumed', 'admission_failed',
  ], 'continuation')
}

export function validateReopen(payload: UnknownRecord): void {
  validateOutputPair(payload, CHECKPOINT_BACKENDS, [
    'started', 'authorized', 'failed',
  ], 'reopen')
}

export function validateCleanup(payload: UnknownRecord): void {
  validateOutputPair(payload, OUTPUT_BACKENDS, [
    'started', 'completed', 'retryable_failure', 'ownership_unknown', 'failed',
  ], 'cleanup')
}

export function validateDirectZipMilestone(payload: UnknownRecord): void {
  exactKeys(payload, [
    'operation_id',
    'session_id',
    'plan_kind',
    'milestone',
    'checkpoint_phase',
    'epoch_offset_class',
    'prefix_copy_decision',
    'peak_space_decision',
    'permission_decision',
    'identity_decision',
    'space_decision',
    'cleanup_decision',
  ], ['native_error_class'], 'direct_zip_milestone payload')
  canonicalIdentity(payload.operation_id, 'direct ZIP operation ID')
  canonicalIdentity(payload.session_id, 'direct ZIP session ID')
  member(payload.plan_kind, ['direct_resumable_zip'], 'direct ZIP plan kind')
  member(payload.milestone, [
    'session_started', 'session_restored', 'session_paused', 'session_resumed',
    'session_settled', 'session_stopped', 'permission_query', 'permission_request',
    'candidate_persist', 'exact_name_lookup', 'exact_name_create', 'bootstrap_write',
    'bootstrap_close', 'snapshot', 'epoch_open', 'epoch_write', 'epoch_truncate',
    'epoch_close', 'epoch_abort', 'range_proof', 'cleanup_delete', 'cleanup_observe',
    'epoch_opened', 'member_admitted', 'member_resumed', 'checkpoint_policy_decided',
    'candidate_staged', 'predecessor_verified', 'epoch_close_observed',
    'candidate_resolved', 'checkpoint_promoted', 'closing_entered',
    'central_record_replayed', 'completion_verified', 'writer_gated', 'writer_failed',
  ], 'direct ZIP milestone')
  member(payload.checkpoint_phase, ['between_members', 'inside_member', 'closing'],
    'direct ZIP checkpoint phase')
  member(payload.epoch_offset_class, [
    'not_positioned', 'member_header', 'member_payload', 'member_descriptor',
    'central_directory', 'closing_tail',
  ], 'direct ZIP epoch offset class')
  member(payload.prefix_copy_decision, [
    'not_evaluated', 'admit', 'decline_evidence_unavailable',
    'decline_prefix_copy_budget', 'decline_cumulative_copy_budget',
  ], 'direct ZIP prefix-copy decision')
  member(payload.peak_space_decision, [
    'not_evaluated', 'within_budget', 'confirmation_required',
    'destination_space_required', 'evidence_unavailable',
  ], 'direct ZIP peak-space decision')
  member(payload.permission_decision, [
    'not_evaluated', 'granted', 'authorization_required',
  ], 'direct ZIP permission decision')
  member(payload.identity_decision, [
    'not_evaluated', 'verified', 'target_verification_required',
    'restart_required', 'needs_attention',
  ], 'direct ZIP identity decision')
  member(payload.space_decision, [
    'not_evaluated', 'admitted', 'destination_space_required',
    'quota_exceeded', 'native_effect_ambiguous',
  ], 'direct ZIP space decision')
  member(payload.cleanup_decision, [
    'not_evaluated', 'not_requested', 'retained', 'deleted', 'needs_attention',
  ], 'direct ZIP cleanup decision')
  if (payload.native_error_class !== undefined) {
    member(payload.native_error_class, [
      'abort', 'data', 'invalid_state', 'no_modification_allowed', 'not_allowed',
      'not_found', 'not_supported', 'quota_exceeded', 'security', 'timeout',
      'type_error', 'type_mismatch', 'unknown',
    ], 'direct ZIP native error class')
  }
}

export function validateRetainedInventory(payload: UnknownRecord): void {
  if (payload.transition === 'load_completed') {
    exactKeys(payload, ['transition', 'operation_count'], [], 'retained inventory completed payload')
    decimalUint64(payload.operation_count, 'retained inventory operation count')
    return
  }
  exactKeys(payload, ['transition'], [], 'retained inventory payload')
  member(payload.transition, ['load_started', 'load_failed'], 'retained inventory transition')
}

export function validateRetainedAction(payload: UnknownRecord): void {
  exactKeys(payload, ['transition', 'action', 'continuation'], [], 'retained action payload')
  member(payload.transition, ['started', 'completed', 'failed', 'excluded'],
    'retained action transition')
  member(payload.action, ['continue', 'catch-up', 'save', 'redownload', 'discard', 'delete'],
    'retained action')
  member(payload.continuation, [
    'resume_receive', 'pending_catch_up', 'restoration_available',
    'resume_package', 'save_artifact', 'retry_download',
    'cleanup_expired', 'retry_cleanup', 'needs_attention',
  ], 'retained action continuation')
}

function validateOutputPair(
  payload: UnknownRecord,
  backends: readonly string[],
  transitions: readonly string[],
  eventName: string,
): void {
  exactKeys(payload, ['backend', 'transition'], [], `${eventName} payload`)
  member(payload.backend, backends, `${eventName} backend`)
  member(payload.transition, transitions, `${eventName} transition`)
}
