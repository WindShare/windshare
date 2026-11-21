import type {
  TraceDomainEventNameV2,
  TraceEventPayloadByNameV2,
} from '../trace/model'
import {
  validateLane,
  validatePeerAttempt,
  validatePeerRecovery,
  validateProtocolOperation,
  validateOperationRecovery,
  validateConnectionRecovery,
  validateRelayHeartbeat,
  validateRelayDelivery,
  validateSenderVerification,
  validateContentScheduling,
  validateContentRetry,
  validateRequestScheduling,
  validateLeaseRetirement,
} from './trace-payload-protocol'
import {
  validateAuthority,
  validateBrowse,
  validateCheckpoint,
  validateCleanup,
  validateContinuation,
  validateJoin,
  validateReceiverExperience,
  validateLifecycleAction,
  validateOutputReservation,
  validateStoragePersistence,
  validateOutputWrite,
  validatePreview,
  validateProjection,
  validatePublication,
  validateReopen,
  validateRetainedAction,
  validateRetainedInventory,
  validateSettlement,
  validateTransferProgress,
} from './trace-payload-product'
import {
  validateDirectZipCoordination,
  validateDirectZipMemberRollback,
  validateDirectZipMilestone,
} from './trace-payload-direct-zip'
import {
  validatePerformancePhase,
  validatePerformanceSummary,
} from './trace-payload-performance'
import { exactKeys, integerBetween, member, recordValue, type UnknownRecord } from './trace-payload-validation'
import { validateBrowserDelivery } from './trace-payload-browser-delivery'
import { validateReceive } from './receive/payload'

export { validateCorrelationV1 } from './trace-payload-validation'

const EVENT_PAYLOAD_VALIDATORS = Object.freeze({
  network_directory: validateNetworkDirectory,
  authority_blocks: validateAuthorityBlocks,
  catalog_storage: validateCatalogStorage,
  join_transition: validateJoin,
  receiver_experience: validateReceiverExperience,
  browse_transition: validateBrowse,
  preview_transition: validatePreview,
  projection_transition: validateProjection,
  authority_transition: validateAuthority,
  protocol_operation: validateProtocolOperation,
  content_scheduling: validateContentScheduling,
  content_retry: validateContentRetry,
  request_scheduling: validateRequestScheduling,
  lease_retirement: validateLeaseRetirement,
  operation_recovery: validateOperationRecovery,
  connection_recovery: validateConnectionRecovery,
  relay_heartbeat: validateRelayHeartbeat,
  relay_delivery: validateRelayDelivery,
  sender_verification: validateSenderVerification,
  peer_attempt: validatePeerAttempt,
  peer_recovery: validatePeerRecovery,
  lane_transition: validateLane,
  receive_transition: validateReceive,
  lifecycle_action_transition: validateLifecycleAction,
  transfer_progress: validateTransferProgress,
  performance_phase: validatePerformancePhase,
  performance_summary: validatePerformanceSummary,
  storage_persistence: validateStoragePersistence,
  output_reservation: validateOutputReservation,
  output_write: validateOutputWrite,
  checkpoint: validateCheckpoint,
  settlement: validateSettlement,
  publication: validatePublication,
  continuation: validateContinuation,
  reopen: validateReopen,
  cleanup: validateCleanup,
  direct_zip_milestone: validateDirectZipMilestone,
  browser_delivery: validateBrowserDelivery,
  direct_zip_coordination: validateDirectZipCoordination,
  direct_zip_member_rollback: validateDirectZipMemberRollback,
  retained_inventory: validateRetainedInventory,
  retained_action: validateRetainedAction,
} satisfies Record<TraceDomainEventNameV2, (payload: UnknownRecord) => void>)

function validateAuthorityBlocks(payload: UnknownRecord): void {
  exactKeys(payload, ['operation_id', 'endpoint', 'transition'], ['generation', 'version', 'failure_detail'], 'authority blocks')
  member(payload.transition, ['accepted', 'rejected', 'storage_fallback'], 'authority transition')
  for (const field of ['operation_id', 'endpoint', 'failure_detail']) {
    if (payload[field] !== undefined && (typeof payload[field] !== 'string' || payload[field].length > 2048)) throw new TypeError('Invalid authority trace text')
  }
  for (const field of ['generation', 'version']) {
    if (payload[field] !== undefined) integerBetween(payload[field], 0, Number.MAX_SAFE_INTEGER, field)
  }
}

function validateNetworkDirectory(payload: UnknownRecord): void {
  member(payload.transition, ['accepted', 'rejected', 'storage_fallback'], 'directory transition')
  if (payload.transition === 'storage_fallback') {
    exactKeys(payload, ['operation_id', 'transition', 'failure_detail'], [], 'network directory storage fallback')
  } else {
    exactKeys(payload, ['operation_id', 'mirror', 'transition'], ['tier', 'generation', 'serial', 'trusted_until_ms', 'failure_detail'], 'network directory')
    if (payload.tier !== undefined) member(payload.tier, ['live', 'fallback'], 'directory mirror tier')
  }
  for (const field of ['operation_id', 'mirror', 'failure_detail']) {
    if (payload[field] !== undefined && (typeof payload[field] !== 'string' || payload[field].length > 2048)) throw new TypeError('Invalid network trace text')
  }
  for (const field of ['generation', 'serial', 'trusted_until_ms']) {
    if (payload[field] !== undefined) integerBetween(payload[field], 0, Number.MAX_SAFE_INTEGER, field)
  }
}

function validateCatalogStorage(payload: UnknownRecord): void {
  exactKeys(payload, ['share_instance_id', 'transition', 'reason'], ['failure_detail'], 'catalog storage')
  member(payload.transition, ['storage_fallback'], 'catalog storage transition')
  member(payload.reason, ['storage_unavailable', 'storage_denied'], 'catalog storage reason')
  if (typeof payload.share_instance_id !== 'string' || payload.share_instance_id.length === 0 || payload.share_instance_id.length > 2048) {
    throw new TypeError('Invalid catalog storage share instance')
  }
  if (payload.failure_detail !== undefined && (typeof payload.failure_detail !== 'string' || payload.failure_detail.length > 2048)) {
    throw new TypeError('Invalid catalog storage trace text')
  }
}

export function validateTraceEventPayloadV2<Name extends TraceDomainEventNameV2>(
  eventName: Name,
  value: unknown,
): asserts value is TraceEventPayloadByNameV2[Name] {
  const payload = recordValue(value, `${eventName} payload`)
  if (!Object.hasOwn(EVENT_PAYLOAD_VALIDATORS, eventName)) throw new TypeError(`unhandled trace event ${String(eventName)}`)
  EVENT_PAYLOAD_VALIDATORS[eventName](payload)
}
