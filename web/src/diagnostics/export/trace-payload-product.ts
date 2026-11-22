import { PROJECTION_DISCOVERY_STATES } from '../trace/model'
import { CHECKPOINT_TRACE_DECISIONS } from '../trace/checkpoint-payload'
import { isBoundedDiagnosticExceptionProjection, MAX_EXCEPTION_TEXT_BYTES, type DiagnosticExceptionProjection } from '../exception'
import { deepFreezeJson } from './json'
import { STORAGE_PERSISTENCE_TRANSITIONS } from '../trace/storage-persistence-payload'
import {
  RETAINED_ACTION_TRANSITIONS,
  RETAINED_ACTIONS,
  RETAINED_CONTINUATIONS,
} from '../trace/retained-payload'
import {
  booleanValue,
  canonicalIdentity,
  decimalFields,
  decimalUint64,
  exactKeys,
  member,
  memberList,
  uint32,
  type UnknownRecord,
} from './trace-payload-validation'
import { ARTIFACT_KINDS, PLAN_KINDS } from './receive/payload'

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
const CANONICAL_DIGEST = /^[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$/
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
  'preparing',
  'downloading',
  'paused',
  'ready_to_save',
  'download_started',
  'saved',
  'partial',
  'failed',
] as const

const MAXIMUM_EXPERIENCE_TRACE_FIELD_LENGTH = 512

export function validateReceiverExperience(payload: UnknownRecord): void {
  switch (payload.transition) {
    case 'task':
      exactKeys(payload, ['transition', 'operation_id', 'generation', 'stage', 'reason',
        'attention', 'completeness', 'publication', 'elapsed_ms'], [], 'receiver task payload')
      if (payload.elapsed_ms !== null) decimalUint64(payload.elapsed_ms, 'receiver task elapsed milliseconds')
      decimalUint64(payload.generation, 'receiver task generation')
      booleanValue(payload.attention, 'receiver task attention')
      break
    case 'saving':
      exactKeys(payload, ['transition', 'projection_epoch', 'choice_id', 'outcome', 'reason'], [], 'receiver saving payload')
      if (payload.projection_epoch !== null) decimalUint64(payload.projection_epoch, 'receiver saving epoch')
      break
    case 'intent':
      exactKeys(payload, ['transition', 'action', 'operation_id', 'generation'], [], 'receiver intent payload')
      decimalUint64(payload.generation, 'receiver intent generation')
      break
    case 'ownership':
      exactKeys(payload, ['transition', 'operation_id', 'generation', 'state', 'lifecycle_state'], [], 'receiver ownership payload')
      decimalUint64(payload.generation, 'receiver ownership generation')
      member(payload.state, ['releasing', 'released', 'release-failed'], 'receiver ownership state')
      member(payload.lifecycle_state, LIFECYCLE_STATES, 'receiver ownership lifecycle')
      break
    default: throw new TypeError('unknown receiver experience transition')
  }
  for (const [key, value] of Object.entries(payload)) {
    if (key === 'attention' || value === null) continue
    if (typeof value !== 'string' || value.length > MAXIMUM_EXPERIENCE_TRACE_FIELD_LENGTH) {
      throw new TypeError('receiver experience fields must be bounded text')
    }
  }
}

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
  if (payload.transition === 'deferred') {
    exactKeys(payload, ['attempt', 'transition', 'intent', 'preview_id', 'share_instance_id', 'file_id', 'exception'],
      [], 'preview_transition deferred payload')
    member(payload.attempt, ['open'], 'deferred preview attempt')
    member(payload.intent, ['automatic_photo'], 'deferred preview intent')
    decimalUint64(payload.preview_id, 'deferred preview identity')
    canonicalIdentity(payload.share_instance_id, 'deferred preview share instance')
    canonicalIdentity(payload.file_id, 'deferred preview file')
    const exception = deepFreezeJson(payload.exception) as DiagnosticExceptionProjection
    if (!isBoundedDiagnosticExceptionProjection(exception, MAX_EXCEPTION_TEXT_BYTES)) {
      throw new TypeError('deferred preview exception must be a bounded diagnostic snapshot')
    }
    return
  }
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
      member(payload.discovery_state, PROJECTION_DISCOVERY_STATES, 'projection discovery state')
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

function canonicalDigest(value: unknown, field: string): void {
  if (typeof value !== 'string' || !CANONICAL_DIGEST.test(value) || value === ZERO_DIGEST) {
    throw new TypeError(`${field} must be a canonical non-zero 32-byte base64url digest`)
  }
}

export function validateLifecycleAction(payload: UnknownRecord): void {
  exactKeys(payload, ['transition', 'action'], ['lifecycle_state', 'operation_id', 'generation', 'artifact_id',
    'checkpoint_id', 'checkpoint_generation', 'task_settlement', 'retention', 'cleanup'],
    'lifecycle_action_transition payload')
  member(payload.transition, ['started', 'completed', 'failed', 'excluded'],
    'lifecycle action transition')
  member(payload.action, [
    'pause', 'stop', 'continue', 'save', 'redownload', 'change_location',
    'discard', 'delete',
  ], 'lifecycle action')
  if (payload.lifecycle_state !== undefined) {
    member(payload.lifecycle_state, LIFECYCLE_STATES, 'lifecycle state')
  }
  if (payload.operation_id !== undefined) canonicalIdentity(payload.operation_id, 'lifecycle operation')
  for (const key of ['artifact_id', 'checkpoint_id']) {
    if (payload[key] !== undefined) canonicalDigest(payload[key], key)
  }
  for (const key of ['generation', 'checkpoint_generation']) {
    if (payload[key] !== undefined) decimalUint64(payload[key], key)
  }
  if (payload.task_settlement !== undefined) member(payload.task_settlement, ['continuable', 'ended', 'unsettled'], 'task settlement')
  if (payload.retention !== undefined) member(payload.retention,
    ['continuation', 'complete_results', 'delivered_results', 'browser_handoff', 'none', 'reconcile'], 'retention')
  if (payload.cleanup !== undefined) member(payload.cleanup, ['settled', 'retryable'], 'cleanup')
}

export function validateTransferProgress(payload: UnknownRecord): void {
  const decimalKeys = [
    'source_omitted_entries',
    'discovered_files', 'discovered_bytes', 'received_object_bytes', 'written_bytes', 'completed_files',
    'completed_bytes', 'file_errors', 'selection_errors', 'failed_directories',
    'capacity_waiting_files', 'capacity_accumulated_wait_ms', 'capacity_wait_attempts',
  ] as const
  exactKeys(payload, [
    ...decimalKeys, 'operation_id', 'transfer_job_id', 'content_lanes', 'capacity_wait_visible',
    'discovery', 'partial',
  ], [],
    'transfer_progress payload')
  canonicalIdentity(payload.operation_id, 'transfer progress operation ID')
  canonicalIdentity(payload.transfer_job_id, 'transfer progress job ID')
  decimalFields(payload, decimalKeys, 'transfer progress')
  uint32(payload.content_lanes, 'transfer progress content lanes')
  booleanValue(payload.capacity_wait_visible, 'capacity wait visibility')
  member(payload.discovery, ['open', 'complete', 'failed'], 'transfer discovery state')
  booleanValue(payload.partial, 'transfer partial flag')
}

export function validateStoragePersistence(payload: UnknownRecord): void {
  exactKeys(payload, ['operation_id', 'request_operation_id', 'transition'], [], 'storage_persistence payload')
  canonicalIdentity(payload.operation_id, 'storage persistence operation ID')
  canonicalIdentity(payload.request_operation_id, 'storage persistence request operation ID')
  member(payload.transition, STORAGE_PERSISTENCE_TRANSITIONS, 'storage persistence transition')
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
  if (payload.transition === 'authority_decision') {
    exactKeys(payload, [
      'backend', 'transition', 'authority', 'receive_operation_id', 'transfer_job_id',
      'output_session_id', 'materialization_relative_path', 'trigger', 'prefix_copy_bytes',
      'write_amplification_bytes', 'temporary_bytes', 'decision',
    ], [
      'checkpoint_ordinal', 'remaining_automatic_write_amplification_bytes', 'release_reason',
    ], 'checkpoint authority payload')
    member(payload.backend, ['file_system_access'], 'checkpoint authority backend')
    member(payload.authority, ['automatic_admission', 'preserving_capacity'],
      'checkpoint authority kind')
    canonicalIdentity(payload.receive_operation_id, 'checkpoint receive operation ID')
    canonicalIdentity(payload.transfer_job_id, 'checkpoint transfer job ID')
    canonicalIdentity(payload.output_session_id, 'checkpoint output session ID')
    if (!Array.isArray(payload.materialization_relative_path) ||
        payload.materialization_relative_path.some(segment =>
          typeof segment !== 'string' || segment.length === 0)) {
      throw new TypeError('checkpoint materialization path must contain non-empty segments')
    }
    member(payload.trigger, ['pending_bytes', 'pending_time', 'paused_file_recovery'],
      'checkpoint authority trigger')
    if (payload.checkpoint_ordinal !== undefined &&
        uint32(payload.checkpoint_ordinal, 'checkpoint ordinal') === 0) {
      throw new RangeError('checkpoint ordinal must be positive')
    }
    decimalFields(payload, [
      'prefix_copy_bytes', 'write_amplification_bytes', 'temporary_bytes',
    ], 'checkpoint authority cost')
    if (payload.remaining_automatic_write_amplification_bytes !== undefined) {
      decimalUint64(
        payload.remaining_automatic_write_amplification_bytes,
        'checkpoint remaining automatic write amplification',
      )
    }
    member(payload.decision, [
      'admitted', 'checkpoint_priority', 'prefix_copy_budget',
      'cumulative_write_amplification_budget', 'capacity_unavailable',
      'paused_recovery_queued', 'paused_recovery_admitted', 'committed', 'released',
    ], 'checkpoint authority decision')
    if ((payload.decision === 'released') !== (payload.release_reason !== undefined)) {
      throw new TypeError('checkpoint release reason must accompany exactly a released decision')
    }
    if (payload.release_reason !== undefined) {
      member(payload.release_reason, [
        'unused', 'capacity_unavailable', 'replacement_open_failed', 'writer_closed',
        'writer_aborted', 'file_committed', 'file_paused', 'file_retired', 'cancelled',
        'terminal_drain', 'automatic_handoff',
      ], 'checkpoint authority release reason')
    }
    return
  }
  if (payload.transition === 'source_comparison') return validateSourceComparison(payload)
  exactKeys(payload, ['backend', 'transition'], ['decision'], 'checkpoint payload')
  member(payload.backend, CHECKPOINT_BACKENDS, 'checkpoint backend')
  member(payload.transition, [
    'restored', 'persisted', 'quarantined', 'failed',
  ], 'checkpoint transition')
  if (payload.decision !== undefined) {
    member(payload.decision, CHECKPOINT_TRACE_DECISIONS, 'checkpoint decision')
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
  exactKeys(payload, ['backend', 'transition'], ['operation_id', 'retained_bytes'], 'continuation payload')
  member(payload.backend, OUTPUT_BACKENDS, 'continuation backend')
  member(payload.transition, ['paused', 'resumed', 'admission_failed', 'source_invalidated'], 'continuation transition')
  if (payload.operation_id !== undefined) canonicalIdentity(payload.operation_id, 'continuation operation ID')
  if (payload.retained_bytes !== undefined) decimalUint64(payload.retained_bytes, 'continuation retained bytes')
}

export function validateReopen(payload: UnknownRecord): void {
  if (payload.transition === 'receive_recovered') {
    exactKeys(payload, ['backend', 'transition', 'operation_id', 'lifecycle_generation',
      'checkpoint_count', 'verified_bytes'], [], 'receive recovery payload')
    member(payload.backend, ['origin_private'], 'receive recovery backend')
    canonicalIdentity(payload.operation_id, 'receive recovery operation ID')
    decimalUint64(payload.lifecycle_generation, 'receive recovery generation')
    member(payload.checkpoint_count, ['0', '1'], 'original receive checkpoint count')
    decimalUint64(payload.verified_bytes, 'receive recovery verified bytes')
    return
  }
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
  member(payload.transition, RETAINED_ACTION_TRANSITIONS,
    'retained action transition')
  member(payload.action, RETAINED_ACTIONS, 'retained action')
  member(payload.continuation, RETAINED_CONTINUATIONS, 'retained action continuation')
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

function validateSourceComparison(payload: UnknownRecord): void {
  exactKeys(payload, ['backend', 'transition', 'operation_id', 'file_id', 'previous_revision',
    'current_revision', 'stage', 'comparison', 'reason', 'reused_bytes', 'retained_bytes'],
  [], 'source comparison payload')
  member(payload.backend, CHECKPOINT_BACKENDS, 'comparison backend')
  for (const key of ['operation_id', 'file_id', 'previous_revision', 'current_revision']) {
    canonicalIdentity(payload[key], key)
  }
  member(payload.stage, ['accepted', 'retained'], 'comparison stage')
  member(payload.comparison, ['metadata-agreement', 'observed-change',
    'unavailable'], 'comparison evidence')
  member(payload.reason, ['same-revision', 'metadata-agreement',
    'size-changed', 'metadata-changed', 'metadata-unavailable'], 'comparison reason')
  decimalUint64(payload.reused_bytes, 'reused bytes')
  decimalUint64(payload.retained_bytes, 'retained bytes')
}
