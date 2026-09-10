import {
  canonicalIdentity,
  exactKeys,
  member,
  type UnknownRecord,
} from './trace-payload-validation'

const MAXIMUM_DIRECT_ZIP_LOCK_NAME_LENGTH = 512
const MAXIMUM_DIRECT_ZIP_NATIVE_ERROR_NAME_LENGTH = 128

export function validateDirectZipCoordination(payload: UnknownRecord): void {
  exactKeys(payload, ['operation_id', 'scope', 'transition'],
    ['lock_name', 'native_error_name'], 'direct_zip_coordination payload')
  canonicalIdentity(payload.operation_id, 'direct ZIP operation ID')
  member(payload.scope, ['parent_access', 'namespace', 'target'], 'direct ZIP coordination scope')
  member(payload.transition, ['waiting', 'acquired', 'released', 'failed'],
    'direct ZIP coordination transition')
  for (const [key, maximumLength] of [
    ['lock_name', MAXIMUM_DIRECT_ZIP_LOCK_NAME_LENGTH],
    ['native_error_name', MAXIMUM_DIRECT_ZIP_NATIVE_ERROR_NAME_LENGTH],
  ] as const) {
    const value = payload[key]
    if (value !== undefined && (typeof value !== 'string' ||
        value.length === 0 || value.length > maximumLength)) {
      throw new TypeError(`direct ZIP coordination ${key} must be bounded nonempty text`)
    }
  }
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
