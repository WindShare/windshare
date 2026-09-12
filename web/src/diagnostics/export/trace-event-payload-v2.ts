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
  validateContentScheduling,
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
  validateOutputWrite,
  validatePreview,
  validateProjection,
  validatePublication,
  validateReceive,
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
import { recordValue, type UnknownRecord } from './trace-payload-validation'
import { validateBrowserDelivery } from './trace-payload-browser-delivery'

export { validateCorrelationV1 } from './trace-payload-validation'

const EVENT_PAYLOAD_VALIDATORS = Object.freeze({
  join_transition: validateJoin,
  receiver_experience: validateReceiverExperience,
  browse_transition: validateBrowse,
  preview_transition: validatePreview,
  projection_transition: validateProjection,
  authority_transition: validateAuthority,
  protocol_operation: validateProtocolOperation,
  content_scheduling: validateContentScheduling,
  request_scheduling: validateRequestScheduling,
  lease_retirement: validateLeaseRetirement,
  operation_recovery: validateOperationRecovery,
  peer_attempt: validatePeerAttempt,
  peer_recovery: validatePeerRecovery,
  lane_transition: validateLane,
  receive_transition: validateReceive,
  lifecycle_action_transition: validateLifecycleAction,
  transfer_progress: validateTransferProgress,
  performance_phase: validatePerformancePhase,
  performance_summary: validatePerformanceSummary,
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

export function validateTraceEventPayloadV2<Name extends TraceDomainEventNameV2>(
  eventName: Name,
  value: unknown,
): asserts value is TraceEventPayloadByNameV2[Name] {
  const payload = recordValue(value, `${eventName} payload`)
  if (!Object.hasOwn(EVENT_PAYLOAD_VALIDATORS, eventName)) throw new TypeError(`unhandled trace event ${String(eventName)}`)
  EVENT_PAYLOAD_VALIDATORS[eventName](payload)
}
