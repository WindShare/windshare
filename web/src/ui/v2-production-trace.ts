import type { PersistedReceiveOperationReopenTraceEvent } from '../output/resume/reopen-authority'
import type { WorkspaceStageTraceEvent } from '../output/workspace/stages'
import type { V2ReceiverTraceEvent } from './v2-controller'

const TRACE_LABEL = 'windshare.receive'
const SAFE_TRACE_FIELDS = Object.freeze([
  'projection_epoch',
  'current_projection_epoch',
  'stale_projection_epoch',
  'event_class',
  'shape_proof',
  'discovery_state',
  'file_count_lower_bound',
  'directory_count_lower_bound',
  'byte_count_lower_bound',
  'unsettled_target_count',
  'retryable_discovery_reason',
  'retained_shape_proof',
  'artifact_kind',
  'layout_class',
  'plan_kind',
  'admitted_directory_count',
  'directory_failure_reason',
  'completed_file_count',
  'completed_bytes',
  'entry_count',
  'file_count',
  'directory_count',
  'raw_bytes',
  'selected_bytes',
  'metadata_bytes',
  'admission_kind',
  'artifact_bytes',
  'unique_raw_bytes',
  'package_bytes',
  'peak_temporary_bytes',
  'durable_metadata_bytes',
  'peak_owned_bytes',
  'limit_class',
  'preparation_admission_reason',
  'package_failure_reason',
  'expires_at_ms',
  'publication_route',
  'external_attempt_reason',
  'needs_attention_reason',
  'prior_state',
  'attempt_kind',
  'package_digest_present',
  'object_url_lease_ms',
  'retryable_until_present',
  'retryable_until_ms',
  'resumable_stage',
  'prior_stable_state',
  'cleanup_generation',
  'removed_object_count',
  'removed_record_count',
  'tree_outcome',
  'success_count',
  'failure_count',
  'visibility',
  'offered_artifact_kinds',
  'offered_plan_kinds',
  'primary_artifact_kind',
  'offer_unavailable_reason',
  'hard_limit_class',
  'operation_count',
  'lifecycle_generation',
  'continuation',
  'retained_action',
] as const)
const OPAQUE_ID_FIELDS = Object.freeze([
  ['operation_id', 'has_operation_id'],
  ['receive_intent_digest', 'has_receive_intent_digest'],
  ['protocol_session_id', 'has_protocol_session_id'],
  ['transfer_job_id', 'has_transfer_job_id'],
  ['output_session_id', 'has_output_session_id'],
  ['preparation_id', 'has_preparation_id'],
  ['preparation_digest', 'has_preparation_digest'],
  ['sealed_materialization_digest', 'has_sealed_materialization_digest'],
  ['package_digest', 'has_package_digest'],
  ['layout_digest', 'has_layout_digest'],
  ['publication_attempt_id', 'has_publication_attempt_id'],
  ['attempt_id', 'has_attempt_id'],
  ['lease_id', 'has_lease_id'],
] as const)

export type V2ProductionTraceEvent =
  | V2ReceiverTraceEvent
  | WorkspaceStageTraceEvent
  | PersistedReceiveOperationReopenTraceEvent

type PrivacySafeTraceValue =
  | string
  | number
  | bigint
  | boolean
  | readonly string[]

export type PrivacySafeV2ReceiverTrace = Readonly<Record<string, PrivacySafeTraceValue>>

export interface V2StructuredTraceConsole {
  info(label: string, event: PrivacySafeV2ReceiverTrace): void
}

/**
 * Production diagnostics use an allowlist so a future trace field cannot
 * accidentally expose capability material, file names, paths, or exception text.
 */
export function privacySafeV2ReceiverTrace(
  event: V2ProductionTraceEvent,
): PrivacySafeV2ReceiverTrace {
  const input = event as unknown as Readonly<Record<string, unknown>>
  const safe: Record<string, PrivacySafeTraceValue> = { name: event.name }
  for (const field of SAFE_TRACE_FIELDS) {
    const value = input[field]
    if (isSafeScalar(value)) {
      safe[field] = value
    } else if (isStringList(value)) {
      safe[field] = Object.freeze([...value])
    }
  }
  for (const [field, presenceField] of OPAQUE_ID_FIELDS) {
    if (typeof input[field] === 'string' && input[field].length > 0) {
      safe[presenceField] = true
    }
  }
  return Object.freeze(safe)
}

export function createPrivacySafeV2ReceiverTraceSink(
  consolePort: V2StructuredTraceConsole,
): (event: V2ProductionTraceEvent) => void {
  return (event) => {
    consolePort.info(TRACE_LABEL, privacySafeV2ReceiverTrace(event))
  }
}

function isSafeScalar(value: unknown): value is string | number | bigint | boolean {
  return typeof value === 'string' || typeof value === 'number' ||
    typeof value === 'bigint' || typeof value === 'boolean'
}

function isStringList(value: unknown): value is readonly string[] {
  return Array.isArray(value) && value.every((entry) => typeof entry === 'string')
}
