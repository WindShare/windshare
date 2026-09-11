import type { RetainedActionPayloadV1 } from '../../diagnostics/trace/retained-payload'
import type { V2RetainedInventoryTraceEvent } from './contracts'

type RetainedActionTraceEvent = Extract<V2RetainedInventoryTraceEvent, {
  readonly retained_action: unknown
}>

const ACTION_TRANSITIONS = Object.freeze({
  'receive.inventory.action.started': 'started',
  'receive.inventory.action.completed': 'completed',
  'receive.inventory.action.failed': 'failed',
} as const)

// Keep this exhaustive: a new recovery route must remain exportable through the
// closed trace schema instead of disappearing when capture validates its payload.
const CONTINUATIONS = Object.freeze({
  'resume-receive': 'resume_receive',
  'resume-direct-zip': 'resume_receive',
  'reauthorize-direct-zip': 'resume_receive',
  'verify-direct-zip-target': 'resume_receive',
  'retry-direct-zip-space': 'resume_receive',
  'verify-direct-zip-completion': 'verify_direct_zip_completion',
  'pending-catch-up': 'pending_catch_up',
  'restoration-available': 'restoration_available',
  'history-only': 'history_only',
  'resume-package': 'resume_package',
  'resume-local-finalization': 'resume_local_finalization',
  'save-artifact': 'save_artifact',
  'retry-download': 'retry_download',
  'cleanup-incompatible': 'cleanup_incompatible',
  'retry-cleanup': 'retry_cleanup',
  'needs-attention': 'needs_attention',
} satisfies Record<RetainedActionTraceEvent['continuation'], RetainedActionPayloadV1['continuation']>)

export function projectRetainedActionPayload(
  event: RetainedActionTraceEvent,
): RetainedActionPayloadV1 {
  return Object.freeze({
    transition: ACTION_TRANSITIONS[event.name],
    action: event.retained_action,
    continuation: CONTINUATIONS[event.continuation],
  })
}
