import { projectContentScheduling } from '../diagnostics/trace/content-scheduling'
import { projectProtocolErrorContentV2 } from '../diagnostics/export/protocol-error-v2'
import { TRACE_FAILURE_DETAIL_MAX_CHARACTERS } from '../diagnostics/trace/lane-payload'
import { formatDiagnosticText } from '../security/diagnostic-formatter'
import type { V2ProtocolTraceEvent } from '../session/v2-diagnostics'
import type { TraceEventObservationV2, TraceEventPayloadByNameV2 } from '../diagnostics/trace/model'
import { correlatedObservation, decimal, requiredCorrelation } from '../diagnostics/export/trace-observation'

const FAILURE_DETAIL_FORMAT = Object.freeze({
  maxDepth: 6,
  maxEntries: 8,
  maxStringCharacters: 256,
})

export function projectProtocolTraceEvent(
  event: V2ProtocolTraceEvent,
): TraceEventObservationV2 {
  const correlation = requiredCorrelation(event.correlation)
  if (event.eventName === 'request_scheduling') {
    return correlatedObservation(event.eventName, correlation, {
      request_sequence: decimal(event.sequence), request_kind: event.kind, route: event.route,
      transition: event.transition, expected_ms: Math.ceil(event.expectedMilliseconds),
      elapsed_ms: Math.ceil(event.elapsedMilliseconds), pending_requests: event.pendingRequests,
    })
  }
  if (event.eventName === 'content_scheduling') return projectContentScheduling(event)
  if (event.eventName === 'operation_recovery') {
    return correlatedObservation(event.eventName, correlation, {
      operation_sequence: decimal(event.operationSequence),
      generation_id: decimal(event.generationId),
      availability_revision: decimal(event.availabilityRevision),
      lane_count: decimal(event.laneCount),
      unchanged_availability_retries: decimal(event.unchangedAvailabilityRetries),
      ...(event.transition === 'wait_for_availability'
        ? { transition: event.transition, delay_ms: event.delayMilliseconds }
        : { transition: event.transition }),
    })
  }
  if (event.eventName === 'lane_transition') {
    switch (event.transition) {
      case 'admission_rejected':
        return correlatedObservation(event.eventName, correlation, {
          transition: event.transition,
          rejection_code: event.rejectionCode,
          retry_after_ms: event.retryAfterMilliseconds,
        })
      case 'detached':
        return correlatedObservation(event.eventName, correlation, {
          transition: event.transition,
          detachment_class: event.detachmentClass,
          // Error causes are non-enumerable; preserving them here makes an
          // authenticated failure distinguishable from ordinary transport loss.
          ...(event.failure === undefined ? {} : {
            failure_detail: formatDiagnosticText(event.failure, FAILURE_DETAIL_FORMAT)
              .slice(0, TRACE_FAILURE_DETAIL_MAX_CHARACTERS),
          }),
        })
      default:
        return correlatedObservation(event.eventName, correlation, {
          transition: event.transition,
        })
    }
  }

  switch (event.transition) {
    case 'admission_waiting':
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
        capacity: event.capacity,
        active_operations: decimal(event.activeOperations),
        tracked_operations: decimal(event.trackedOperations),
      })
    case 'response_received':
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
        response_kind: event.responseKind,
      })
    case 'cancelled':
    case 'late_response_discarded':
      return correlatedObservation(event.eventName, correlation, projectRetiredOperation(event))
    case 'authenticated_failure':
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
        protocol_error: projectProtocolErrorContentV2(event.protocolError),
      })
    case 'settled':
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
        settlement: event.settlement,
      })
    default:
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
      })
  }
}

function projectRetiredOperation(
  event: Extract<V2ProtocolTraceEvent, { transition: 'cancelled' | 'late_response_discarded' }>,
): TraceEventPayloadByNameV2['protocol_operation'] {
  const request = event.request === undefined ? {} : { request: {
    lease_id: event.request.leaseId,
    ...(event.request.blocks === undefined ? {} : { blocks: {
      first_index: decimal(event.request.blocks.firstIndex), count: event.request.blocks.count,
    } }),
  } }
  if (event.transition === 'cancelled') return {
    transition: event.transition, request_kind: event.requestKind,
    cancellation_reason: event.cancellationReason, ...request,
  }
  return {
    transition: event.transition, request_kind: event.requestKind,
    response_kind: event.responseKind, settlement: event.settlement, ...request,
    ...(event.cancellationReason === undefined ? {} : { cancellation_reason: event.cancellationReason }),
    ...(event.protocolError === undefined ? {} : { protocol_error: projectProtocolErrorContentV2(event.protocolError) }),
  }
}
