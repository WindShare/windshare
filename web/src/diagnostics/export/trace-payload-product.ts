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
    case 'authority_acquired':
    case 'intent_frozen':
    case 'activation_failed':
    case 'stale_replacement':
    case 'authority_invalidated':
      exactKeys(payload, ['transition'], ['artifact_kind', 'plan_kind'],
        'authority activation payload')
      if (payload.artifact_kind !== undefined) {
        member(payload.artifact_kind, ARTIFACT_KINDS, 'authority artifact kind')
      }
      if (payload.plan_kind !== undefined) {
        member(payload.plan_kind, PLAN_KINDS, 'authority plan kind')
      }
      return
    default:
      throw new TypeError('authority_transition discriminant is invalid')
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
  ] as const
  exactKeys(payload, [...decimalKeys, 'content_lanes', 'discovery', 'partial'], [],
    'transfer_progress payload')
  decimalFields(payload, decimalKeys, 'transfer progress')
  uint32(payload.content_lanes, 'transfer progress content lanes')
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
  member(payload.action, ['continue', 'save', 'redownload', 'discard', 'delete'],
    'retained action')
  member(payload.continuation, [
    'resume_receive', 'resume_package', 'save_artifact', 'retry_download',
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
