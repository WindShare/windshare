export type DirectZipCoordinationPayloadV1 = Readonly<{
  operation_id: string
  scope: 'parent_access' | 'namespace' | 'target'
  transition: 'waiting' | 'acquired' | 'released' | 'failed'
  lock_name?: string
  native_error_name?: string
}>

export type DirectZipMemberRollbackPayloadV1 = Readonly<{
  operation_id: string
  session_id: string
  candidate_id: string
  phase: 'requested' | 'persisted' | 'recovering' | 'completed' | 'failed'
  old_committed_length: string
  new_committed_length: string
  retained_selected_payload_bytes: string
  member_ordinal: string
  source_change_reason?:
    | 'file_id_changed'
    | 'revision_changed'
    | 'size_changed'
    | 'range_authority_changed'
  native_error_name?: string
}>

export type DirectZipMilestonePayloadV1 = Readonly<{
  operation_id: string
  session_id: string
  plan_kind: 'direct_resumable_zip'
  milestone:
    | 'session_started'
    | 'session_restored'
    | 'session_paused'
    | 'session_resumed'
    | 'session_settled'
    | 'session_stopped'
    | 'permission_query'
    | 'permission_request'
    | 'candidate_persist'
    | 'exact_name_lookup'
    | 'exact_name_create'
    | 'bootstrap_write'
    | 'bootstrap_close'
    | 'snapshot'
    | 'epoch_open'
    | 'epoch_write'
    | 'epoch_truncate'
    | 'epoch_close'
    | 'epoch_abort'
    | 'range_proof'
    | 'cleanup_delete'
    | 'cleanup_observe'
    | 'epoch_opened'
    | 'member_admitted'
    | 'member_resumed'
    | 'checkpoint_policy_decided'
    | 'candidate_staged'
    | 'predecessor_verified'
    | 'epoch_close_observed'
    | 'candidate_resolved'
    | 'checkpoint_promoted'
    | 'closing_entered'
    | 'central_record_replayed'
    | 'completion_verified'
    | 'writer_gated'
    | 'writer_failed'
  checkpoint_phase: 'between_members' | 'inside_member' | 'closing'
  epoch_offset_class:
    | 'not_positioned'
    | 'member_header'
    | 'member_payload'
    | 'member_descriptor'
    | 'central_directory'
    | 'closing_tail'
  prefix_copy_decision:
    | 'not_evaluated'
    | 'admit'
    | 'decline_evidence_unavailable'
    | 'decline_prefix_copy_budget'
    | 'decline_cumulative_copy_budget'
  peak_space_decision:
    | 'not_evaluated'
    | 'within_budget'
    | 'confirmation_required'
    | 'destination_space_required'
    | 'evidence_unavailable'
  permission_decision: 'not_evaluated' | 'granted' | 'authorization_required'
  identity_decision:
    | 'not_evaluated'
    | 'verified'
    | 'target_verification_required'
    | 'restart_required'
    | 'needs_attention'
  space_decision:
    | 'not_evaluated'
    | 'admitted'
    | 'destination_space_required'
    | 'quota_exceeded'
    | 'native_effect_ambiguous'
  cleanup_decision:
    | 'not_evaluated'
    | 'not_requested'
    | 'retained'
    | 'deleted'
    | 'needs_attention'
  native_error_class?:
    | 'abort'
    | 'data'
    | 'invalid_state'
    | 'no_modification_allowed'
    | 'not_allowed'
    | 'not_found'
    | 'not_supported'
    | 'quota_exceeded'
    | 'security'
    | 'timeout'
    | 'type_error'
    | 'type_mismatch'
    | 'unknown'
}>
