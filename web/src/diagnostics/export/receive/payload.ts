import {
  booleanValue, canonicalIdentity, decimalFields, decimalUint64, exactKeys,
  member, recordValue, uint32, type UnknownRecord,
} from '../trace-payload-validation'

export const ARTIFACT_KINDS = ['original_file', 'directory_tree', 'zip_archive'] as const
export const PLAN_KINDS = [
  'direct_tree',
  'direct_atomic',
  'workspace_then_publish',
  'portable_handoff',
  'direct_resumable_zip',
] as const

function validateRevisionReadmission(payload: UnknownRecord): void {
  exactKeys(payload, [
    'transition', 'operation_id', 'transfer_job_id', 'file_id', 'previous_revision', 'current_revision',
    'decision', 'comparison_reason', 'retained_bytes', 'reused_bytes', 'elapsed_ms',
  ], [], 'file revision readmission')
  for (const field of ['operation_id', 'transfer_job_id', 'file_id', 'previous_revision', 'current_revision']) {
    canonicalIdentity(payload[field], field)
  }
  member(payload.decision, [
    'readmit', 'readmitted', 'observed-change', 'evidence-unavailable', 'retry-limit',
    'admission-failed', 'settlement-failed', 'canceled',
  ], 'file revision readmission decision')
  member(payload.comparison_reason, [
    'same-revision', 'metadata-agreement', 'saved-ranges-compared', 'context-changed',
    'size-changed', 'metadata-changed', 'metadata-unavailable',
  ], 'file revision comparison reason')
  decimalFields(payload, ['retained_bytes', 'reused_bytes'], 'file revision readmission')
  if (typeof payload.elapsed_ms !== 'number' || !Number.isFinite(payload.elapsed_ms) || payload.elapsed_ms < 0) {
    throw new TypeError('file revision readmission elapsed time must be nonnegative')
  }
}

export function validateReceive(payload: UnknownRecord): void {
  if (payload.transition === 'download_connectivity') {
    exactKeys(payload, ['transition', 'transfer_job_id', 'connectivity'], [], 'download connectivity')
    canonicalIdentity(payload.transfer_job_id, 'download transfer job')
    validateDownloadConnectivity(recordValue(payload.connectivity, 'download connectivity'))
    return
  }
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
    case 'file_revision_readmission':
      validateRevisionReadmission(payload)
      return
    case 'discovery_scheduling':
      exactKeys(payload, [
        'transition', 'operation_id', 'transfer_job_id', 'queue', 'decision',
        'pending_items', 'metadata_bytes', 'maximum_items', 'maximum_metadata_bytes',
      ], [], 'discovery scheduling payload')
      canonicalIdentity(payload.operation_id, 'discovery operation ID')
      canonicalIdentity(payload.transfer_job_id, 'discovery transfer job ID')
      member(payload.queue, ['generations', 'zip_members'], 'discovery queue')
      member(payload.decision, ['waiting', 'resumed', 'cancelled', 'complete'], 'discovery scheduling decision')
      decimalFields(payload, ['pending_items', 'metadata_bytes', 'maximum_items', 'maximum_metadata_bytes'],
        'discovery scheduling')
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

function validateDownloadConnectivity(payload: UnknownRecord): void {
  const bytes = ['direct_bytes', 'turn_bytes', 'application_relay_bytes', 'unknown_bytes'] as const
  exactKeys(payload, ['download_id', 'first_direct_elapsed_ms', ...bytes, 'direct_fraction',
    'fallback_stall_ms', 'incomplete', 'final'], [], 'download connectivity')
  if (typeof payload.download_id !== 'string' || !/^[0-9a-f-]{36}$/.test(payload.download_id)) {
    throw new TypeError('download identity must be a UUID')
  }
  decimalFields(payload, bytes, 'download bytes')
  booleanValue(payload.incomplete, 'download incomplete')
  booleanValue(payload.final, 'download final')
  for (const key of ['first_direct_elapsed_ms', 'fallback_stall_ms'] as const) {
    if (key === 'first_direct_elapsed_ms' && payload[key] === null) continue
    if (typeof payload[key] !== 'number' || !Number.isFinite(payload[key]) || payload[key] < 0) {
      throw new TypeError('download elapsed time must be nonnegative')
    }
  }
  const fraction = payload.direct_fraction
  if (fraction !== null && (typeof fraction !== 'number' || !Number.isFinite(fraction) || fraction < 0 || fraction > 1)) {
    throw new TypeError('download direct fraction is invalid')
  }
  if (payload.incomplete && fraction !== null) throw new TypeError('incomplete download cannot assert an exact fraction')
}
