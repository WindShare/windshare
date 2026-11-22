import { encodeBase64Url } from '../../crypto/bytes'
import type { ReceiveCounts } from '../../contracts/relay-delivery-observation'
import type { TraceEntry } from '../../diagnostics/runtime'
import { formatDiagnosticText } from '../../security/diagnostic-formatter'
import type {
  ConnectionRecoveryTraceEvent,
  ContentRetryTraceEvent,
  ContentSchedulingTraceEvent,
  LeaseRetirementTraceEvent,
  ProtocolErrorContent,
  ProtocolTraceEvent,
  RelayDeliveryTraceEvent,
  RelayHeartbeatTraceEvent,
  SenderVerificationTraceEvent,
} from '../../session/diagnostics'
import type { DiagnosticBytesIdentity, ProtocolCorrelation } from '../../session/identities'

const IDENTITY_BYTES = 16
const UINT32_MAX = 0xffff_ffff
const FAILURE_DETAIL_MAX_CHARS = 1_024
const TEXT_MAX_CHARS = 2_048
const FAILURE_DETAIL_FORMAT = Object.freeze({
  maxDepth: 6,
  maxEntries: 8,
  maxStringCharacters: 256,
})

export type CorrelationRecord = Readonly<{
  protocol_session_id?: string
  protocol_operation_id?: string
  peer_path_id?: string
  peer_attempt_id?: string
  lane_id?: number
  lane_epoch?: number
}>

/** Projects protocol identities into canonical cross-runtime (Go↔TS) wire text. */
export function projectCorrelation(
  correlation: ProtocolCorrelation | undefined,
): CorrelationRecord | undefined {
  if (correlation === undefined) return undefined
  if (correlation.protocolOperationId !== undefined && correlation.protocolSessionId === undefined) {
    throw new TypeError('Protocol operation correlation requires a protocol session identity')
  }
  if (correlation.peerAttemptId !== undefined && correlation.peerPathId === undefined) {
    throw new TypeError('Peer attempt correlation requires a peer path identity')
  }
  const session = encodeNonZeroIdentity(correlation.protocolSessionId, 'protocol session')
  const operation = correlation.protocolOperationId?.toDecimalString()
  const peerPath = encodeNonZeroIdentity(correlation.peerPathId, 'peer path')
  const peerAttempt = encodeNonZeroIdentity(correlation.peerAttemptId, 'peer attempt')
  const lane = validateLane(correlation.lane)
  const record: Record<string, string | number> = {}
  if (session !== undefined) record.protocol_session_id = session
  if (operation !== undefined) record.protocol_operation_id = operation
  if (peerPath !== undefined) record.peer_path_id = peerPath
  if (peerAttempt !== undefined) record.peer_attempt_id = peerAttempt
  if (lane !== undefined) {
    record.lane_id = lane.id
    record.lane_epoch = lane.epoch
  }
  return Object.keys(record).length === 0 ? undefined : Object.freeze(record as CorrelationRecord)
}

export function projectProtocolTraceEvent(event: ProtocolTraceEvent): TraceEntry {
  const correlation = projectCorrelation(event.correlation)
  const envelope = correlation === undefined ? {} : { correlation }
  switch (event.eventName) {
    case 'connection_recovery':
      return { event: event.eventName, ...envelope, payload: projectConnectionRecovery(event) }
    case 'relay_heartbeat':
      return { event: event.eventName, ...envelope, payload: projectRelayHeartbeat(event) }
    case 'relay_delivery':
      return { event: event.eventName, ...envelope, payload: projectRelayDelivery(event) }
    case 'sender_verification':
      return { event: event.eventName, ...envelope, payload: projectSenderVerification(event) }
    case 'lease_retirement':
      return { event: event.eventName, ...envelope, payload: projectLeaseRetirement(event) }
    case 'request_scheduling':
      return {
        event: event.eventName,
        ...envelope,
        payload: {
          request_sequence: String(event.sequence),
          request_kind: event.kind,
          route: event.route,
          transition: event.transition,
          expected_ms: Math.max(0, Math.round(event.expectedMilliseconds)),
          elapsed_ms: Math.max(0, Math.round(event.elapsedMilliseconds)),
          pending_requests: event.pendingRequests,
        },
      }
    case 'content_scheduling':
      return { event: event.eventName, ...envelope, payload: projectContentScheduling(event) }
    case 'content_retry':
      return { event: event.eventName, ...envelope, payload: projectContentRetry(event) }
    case 'operation_recovery':
      return {
        event: event.eventName,
        ...envelope,
        payload: {
          operation_sequence: String(event.operationSequence),
          generation_id: String(event.generationId),
          availability_revision: String(event.availabilityRevision),
          lane_count: String(event.laneCount),
          unchanged_availability_retries: String(event.unchangedAvailabilityRetries),
          transition: event.transition,
          ...(event.transition === 'wait_for_availability'
            ? { delay_ms: Math.max(0, Math.round(event.delayMilliseconds)) }
            : {}),
        },
      }
    case 'lane_transition':
      return { event: event.eventName, ...envelope, payload: projectLaneTransition(event) }
    case 'protocol_operation':
      return { event: event.eventName, ...envelope, payload: projectProtocolOperation(event) }
  }
}

function projectProtocolOperation(
  event: Extract<ProtocolTraceEvent, { eventName: 'protocol_operation' }>,
): Readonly<Record<string, unknown>> {
  switch (event.transition) {
    case 'admission_waiting':
      return {
        transition: event.transition,
        request_kind: event.requestKind,
        capacity: event.capacity,
        active_operations: String(event.activeOperations),
        tracked_operations: String(event.trackedOperations),
      }
    case 'response_received':
    case 'retired_response_discarded':
      return {
        transition: event.transition,
        request_kind: event.requestKind,
        response_kind: event.responseKind,
      }
    case 'cancelled':
      return {
        transition: event.transition,
        request_kind: event.requestKind,
        cancellation_reason: event.cancellationReason,
        ...projectRequestMeta(event.request),
        ...(event.blockWait === undefined ? {} : {
          block_wait: {
            phase: event.blockWait.phase,
            waited_ms: String(Math.max(0, Math.floor(event.blockWait.waitedMilliseconds))),
            queue_progress: String(event.blockWait.queueProgress),
          },
        }),
      }
    case 'late_response_discarded':
      return {
        transition: event.transition,
        request_kind: event.requestKind,
        response_kind: event.responseKind,
        settlement: event.settlement,
        ...projectRequestMeta(event.request),
        ...(event.cancellationReason === undefined ? {} : { cancellation_reason: event.cancellationReason }),
        ...(event.protocolError === undefined ? {} : { protocol_error: projectProtocolError(event.protocolError) }),
      }
    case 'authenticated_failure':
      return {
        transition: event.transition,
        request_kind: event.requestKind,
        protocol_error: projectProtocolError(event.protocolError),
      }
    case 'settled':
      return {
        transition: event.transition,
        request_kind: event.requestKind,
        settlement: event.settlement,
      }
    default:
      return {
        transition: event.transition,
        request_kind: event.requestKind,
        ...(event.failure === undefined ? {} : { failure_detail: formatFailure(event.failure) }),
      }
  }
}

function projectLaneTransition(
  event: Extract<ProtocolTraceEvent, { eventName: 'lane_transition' }>,
): Readonly<Record<string, unknown>> {
  if (event.transition === 'admission_rejected') {
    return {
      transition: event.transition,
      rejection_code: event.rejectionCode,
      retry_after_ms: event.retryAfterMilliseconds,
    }
  }
  if (event.transition === 'detached') {
    if (event.termination.kind === 'closed') {
      return {
        transition: event.transition,
        detachment_class: 'closed',
        close_reason: event.termination.reason,
      }
    }
    return {
      transition: event.transition,
      detachment_class: event.termination.kind,
      failure_detail: formatFailure(event.termination.failure),
    }
  }
  return { transition: event.transition }
}

function projectConnectionRecovery(event: ConnectionRecoveryTraceEvent): Readonly<Record<string, unknown>> {
  return {
    generation_id: String(event.generationId),
    attempt: String(event.attempt),
    phase: event.phase,
    transition: event.transition,
    ...(event.shareId === undefined ? {} : { share_id: event.shareId.slice(0, TEXT_MAX_CHARS) }),
    ...(event.shareInstanceId === undefined ? {} : { share_instance_id: event.shareInstanceId.slice(0, TEXT_MAX_CHARS) }),
    ...(event.relayBase === undefined ? {} : { relay_base: event.relayBase.slice(0, TEXT_MAX_CHARS) }),
    ...(event.delayMilliseconds === undefined ? {} : { delay_ms: Math.max(0, Math.round(event.delayMilliseconds)) }),
    ...(event.waitReason === undefined ? {} : { wait_reason: event.waitReason }),
    ...(event.failure === undefined ? {} : { failure_detail: formatFailure(event.failure) }),
  }
}

function projectRelayHeartbeat(event: RelayHeartbeatTraceEvent): Readonly<Record<string, unknown>> {
  return {
    connection_id: String(event.connectionId),
    relay_base: event.relayBase.slice(0, TEXT_MAX_CHARS),
    round: String(event.round),
    stage: event.stage,
    buffered_bytes: String(event.bufferedBytes),
    elapsed_ms: Math.max(0, Math.round(event.elapsedMilliseconds)),
    timeout_ms: event.timeoutMilliseconds,
    ...(event.generationId === undefined ? {} : { generation_id: String(event.generationId) }),
    ...(event.shareId === undefined ? {} : { share_id: event.shareId.slice(0, TEXT_MAX_CHARS) }),
    ...(event.shareInstanceId === undefined ? {} : { share_instance_id: event.shareInstanceId.slice(0, TEXT_MAX_CHARS) }),
  }
}

function projectRelayDelivery(event: RelayDeliveryTraceEvent): Readonly<Record<string, unknown>> {
  const progress = event.progress
  return {
    connection_id: event.connectionId,
    relay_base: event.relayBase.slice(0, TEXT_MAX_CHARS),
    action: event.action,
    capacity: deliveryCounts(event.capacity),
    held: deliveryCounts(event.usage.held),
    outstanding: deliveryCounts(event.usage.outstanding),
    pending_consumed: deliveryCounts(event.usage.pendingConsumed),
    retained_sessions: String(event.usage.retainedSessions),
    ...(event.sessionId === undefined ? {} : { relay_session_id: event.sessionId }),
    ...(event.shareId === undefined ? {} : { share_id: event.shareId }),
    ...(event.shareInstanceId === undefined ? {} : { share_instance_id: event.shareInstanceId }),
    ...(event.failure === undefined ? {} : { failure: event.failure }),
    ...(progress === undefined ? {} : {
      progress: {
        phase: progress.phase,
        received: deliveryCounts(progress.received),
        consumed: deliveryCounts(progress.consumed),
        discarded: deliveryCounts(progress.discarded),
        consumed_prefix: deliveryCounts(progress.consumedPrefix),
      },
    }),
  }
}

function projectContentScheduling(event: ContentSchedulingTraceEvent): Readonly<Record<string, unknown>> {
  return {
    dispatch_sequence: String(event.dispatchSequence),
    file_id: event.fileId,
    block_index: String(event.localBlockIndex),
    route: event.route,
    purpose: event.purpose,
    expected_ms: Math.max(0, Math.round(event.expectedMilliseconds)),
    pending_bytes: String(event.pendingBytes),
    bytes_per_second: Math.round(event.bytesPerSecond),
    ...(event.deliveryEvidence === undefined ? {} : { delivery_evidence: event.deliveryEvidence }),
    ...(event.rescueReason === undefined ? {} : { rescue_reason: event.rescueReason }),
    ...(event.admission === undefined ? {} : {
      admission: {
        waited_ms: Math.max(0, Math.round(event.admission.waitedMilliseconds)),
        unfinished_bytes: String(event.admission.unfinishedBytes),
        ahead_bytes: String(Math.round(event.admission.aheadBytes)),
        bytes_per_second: Math.round(event.admission.bytesPerSecond),
      },
    }),
  }
}

function projectContentRetry(event: ContentRetryTraceEvent): Readonly<Record<string, unknown>> {
  const base = {
    first_dispatch_sequence: String(event.firstDispatchSequence),
    file_id: event.fileId,
    block_index: String(event.localBlockIndex),
    attempts: event.attempts,
    maximum_attempts: event.maximumAttempts,
    transition: event.transition,
    reason: event.reason,
  }
  if (event.transition === 'exhausted') return base
  return {
    ...base,
    dispatch_sequence: String(event.dispatchSequence),
    route: event.route,
    successful_blocks: String(event.successfulBlocks),
    ...(event.reason === 'recovered_lane'
      ? { successful_blocks_at_failure: String(event.successfulBlocksAtFailure) }
      : {}),
  }
}

function projectLeaseRetirement(event: LeaseRetirementTraceEvent): Readonly<Record<string, unknown>> {
  const base = { lease_id: event.leaseId, attempt: event.attempt, transition: event.transition }
  if (event.transition === 'retrying') return { ...base, failure_detail: formatFailure(event.failure) }
  if (event.transition === 'abandoned') {
    return { ...base, reason: event.reason, failure_detail: formatFailure(event.failure) }
  }
  return base
}

function projectSenderVerification(event: SenderVerificationTraceEvent): Readonly<Record<string, unknown>> {
  return {
    verifier_id: String(event.verifierId),
    share_id: event.shareId,
    backend: event.backend,
    transition: event.transition,
    reason: event.reason,
  }
}

function projectProtocolError(error: ProtocolErrorContent): Readonly<Record<string, unknown>> {
  return {
    wire_scope: error.scope,
    wire_code: error.code,
    retryable: error.retryable,
    ...(error.retryAfterMilliseconds === undefined ? {} : { retry_after_ms: error.retryAfterMilliseconds }),
  }
}

function projectRequestMeta(
  request: Readonly<{ leaseId: string; blocks?: Readonly<{ firstIndex: bigint; count: number }> }> | undefined,
): Readonly<Record<string, unknown>> {
  if (request === undefined) return {}
  return {
    request: {
      lease_id: request.leaseId,
      ...(request.blocks === undefined ? {} : {
        blocks: { first_index: String(request.blocks.firstIndex), count: request.blocks.count },
      }),
    },
  }
}

function deliveryCounts(counts: ReceiveCounts): Readonly<{ frames: string; encoded_bytes: string }> {
  return { frames: String(counts.frames), encoded_bytes: String(counts.encodedBytes) }
}

function formatFailure(error: unknown): string {
  return formatDiagnosticText(error, FAILURE_DETAIL_FORMAT).slice(0, FAILURE_DETAIL_MAX_CHARS)
}

function encodeNonZeroIdentity(
  identity: DiagnosticBytesIdentity | undefined,
  label: string,
): string | undefined {
  if (identity === undefined) return undefined
  const bytes = identity.copyBytes()
  if (bytes.byteLength !== IDENTITY_BYTES) {
    throw new RangeError(`${label} identity must contain exactly 16 bytes`)
  }
  if (!bytes.some(byte => byte !== 0)) return undefined
  return encodeBase64Url(bytes)
}

function validateLane(
  lane: ProtocolCorrelation['lane'],
): Readonly<{ id: number; epoch: number }> | undefined {
  if (lane === undefined) return undefined
  if (!Number.isInteger(lane.id) || lane.id < 0 || lane.id > UINT32_MAX ||
      !Number.isInteger(lane.epoch) || lane.epoch < 0 || lane.epoch > UINT32_MAX) {
    throw new RangeError('Lane correlation requires uint32 ID and epoch')
  }
  return lane
}
