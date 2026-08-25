import type {
  TraceDomainEventNameV1,
  TraceEventPayloadByNameV1,
} from '../trace/model'
import {
  validateLane,
  validatePeerAttempt,
  validatePeerRecovery,
  validateProtocolOperation,
} from './trace-payload-protocol'
import {
  validateAuthority,
  validateBrowse,
  validateCheckpoint,
  validateCleanup,
  validateContinuation,
  validateDirectZipMilestone,
  validateJoin,
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
  validatePerformancePhase,
  validatePerformanceSummary,
} from './trace-payload-performance'
import { recordValue } from './trace-payload-validation'

export { validateCorrelationV1 } from './trace-payload-validation'

export function validateTraceEventPayloadV1<Name extends TraceDomainEventNameV1>(
  eventName: Name,
  value: unknown,
): asserts value is TraceEventPayloadByNameV1[Name] {
  const payload = recordValue(value, `${eventName} payload`)
  switch (eventName) {
    case 'join_transition': validateJoin(payload); return
    case 'browse_transition': validateBrowse(payload); return
    case 'preview_transition': validatePreview(payload); return
    case 'projection_transition': validateProjection(payload); return
    case 'authority_transition': validateAuthority(payload); return
    case 'protocol_operation': validateProtocolOperation(payload); return
    case 'peer_attempt': validatePeerAttempt(payload); return
    case 'peer_recovery': validatePeerRecovery(payload); return
    case 'lane_transition': validateLane(payload); return
    case 'receive_transition': validateReceive(payload); return
    case 'lifecycle_action_transition': validateLifecycleAction(payload); return
    case 'transfer_progress': validateTransferProgress(payload); return
    case 'performance_phase': validatePerformancePhase(payload); return
    case 'performance_summary': validatePerformanceSummary(payload); return
    case 'output_reservation': validateOutputReservation(payload); return
    case 'output_write': validateOutputWrite(payload); return
    case 'checkpoint': validateCheckpoint(payload); return
    case 'settlement': validateSettlement(payload); return
    case 'publication': validatePublication(payload); return
    case 'continuation': validateContinuation(payload); return
    case 'reopen': validateReopen(payload); return
    case 'cleanup': validateCleanup(payload); return
    case 'direct_zip_milestone': validateDirectZipMilestone(payload); return
    case 'retained_inventory': validateRetainedInventory(payload); return
    case 'retained_action': validateRetainedAction(payload); return
    default: assertNever(eventName)
  }
}

function assertNever(value: never): never {
  throw new TypeError(`unhandled trace event ${String(value)}`)
}
