import type { BlockRetryObservation, BlockSchedulingObservation } from '../../content/lane-set'
import type { ContentRetryTraceEvent, ContentSchedulingTraceEvent, ProtocolTraceSource } from '../../session/diagnostics'
import type { FailureIdentity } from '../incident/fact'
import { projectCorrelationV1 } from '../export/correlation-v1'
import { correlatedObservation, durationMilliseconds, requiredCorrelation } from '../export/trace-observation'
import type { TraceEventObservationV2 } from './model'

export type ContentSchedulingPayload = Readonly<{
  admission?: Readonly<{ waited_ms: number; unfinished_bytes: string; ahead_bytes: string; bytes_per_second: number }>
  dispatch_sequence: string
  file_id: string
  block_index: string
  route: 'application-relay' | 'direct' | 'turn'
  purpose: 'content' | 'probe' | 'sample' | 'rescue'
  delivery_evidence?: 'unknown' | 'recent' | 'stale'
  rescue_reason?: import('../../content/scheduling/exploration').RescueReason
  expected_ms: number
  pending_bytes: string
  bytes_per_second: number
}>

export type ContentRetryPayload = Readonly<{
  first_dispatch_sequence: string
  file_id: string
  block_index: string
  attempts: number
  maximum_attempts: number
}> & (
  | Readonly<{
      transition: 'retrying'
      dispatch_sequence: string
      route: 'application-relay' | 'direct' | 'turn'
      successful_blocks: string
    }> & (
      | Readonly<{ reason: 'untried_lane' }>
      | Readonly<{ reason: 'recovered_lane'; successful_blocks_at_failure: string }>
    )
  | Readonly<{ transition: 'exhausted'; reason: 'attempt_limit' | 'no_recovered_lane' }>
)

export function traceContentRetry(
  fact: BlockRetryObservation,
  protocolSessionId: FailureIdentity<'protocol_session'>,
  source: ProtocolTraceSource | undefined,
): void {
  source?.current?.({
    ...fact, eventName: 'content_retry',
    correlation: { protocolSessionId,
      ...(fact.transition === 'retrying' ? { lane: { id: fact.laneId, epoch: fact.laneEpoch } } : {}) },
  })
}

export function projectContentRetry(event: ContentRetryTraceEvent): TraceEventObservationV2 {
  const context = { first_dispatch_sequence: String(event.firstDispatchSequence),
    file_id: event.fileId, block_index: String(event.localBlockIndex),
    attempts: event.attempts, maximum_attempts: event.maximumAttempts }
  return correlatedObservation('content_retry', requiredCorrelation(event.correlation),
    event.transition === 'exhausted'
      ? { ...context, transition: event.transition, reason: event.reason }
      : { ...context, transition: event.transition,
        dispatch_sequence: String(event.dispatchSequence), route: event.route,
        successful_blocks: String(event.successfulBlocks),
        ...(event.reason === 'recovered_lane'
          ? { reason: event.reason, successful_blocks_at_failure: String(event.successfulBlocksAtFailure) }
          : { reason: event.reason }) })
}

export function traceContentScheduling(
  fact: BlockSchedulingObservation,
  protocolSessionId: FailureIdentity<'protocol_session'>,
  source: ProtocolTraceSource | undefined,
): void {
  source?.current?.({
    eventName: 'content_scheduling',
    correlation: { protocolSessionId, lane: { id: fact.laneId, epoch: fact.laneEpoch } },
    dispatchSequence: fact.dispatchSequence,
    fileId: fact.fileId, localBlockIndex: fact.localBlockIndex,
    route: fact.route, purpose: fact.purpose,
    expectedMilliseconds: fact.expectedMilliseconds,
    ...(fact.deliveryEvidence === undefined ? {} : { deliveryEvidence: fact.deliveryEvidence }),
    ...(fact.rescueReason === undefined ? {} : { rescueReason: fact.rescueReason }),
    pendingBytes: fact.pendingBytes, bytesPerSecond: fact.bytesPerSecond,
    ...(fact.admission === undefined ? {} : { admission: fact.admission }),
  })
}

export function projectContentScheduling(event: ContentSchedulingTraceEvent): TraceEventObservationV2 {
  const correlation = projectCorrelationV1(event.correlation)
  if (correlation === undefined) throw new TypeError('Content scheduling requires session correlation')
  return Object.freeze({
    eventName: 'content_scheduling',
    correlation,
    payload: Object.freeze({
      dispatch_sequence: String(event.dispatchSequence),
      file_id: event.fileId, block_index: String(event.localBlockIndex),
      route: event.route, purpose: event.purpose,
      expected_ms: durationMilliseconds(event.expectedMilliseconds),
      ...(event.deliveryEvidence === undefined ? {} : { delivery_evidence: event.deliveryEvidence }),
      ...(event.rescueReason === undefined ? {} : { rescue_reason: event.rescueReason }),
      pending_bytes: String(event.pendingBytes),
      bytes_per_second: Math.round(event.bytesPerSecond),
      ...(event.admission === undefined ? {} : { admission: Object.freeze({
        waited_ms: durationMilliseconds(event.admission.waitedMilliseconds),
        unfinished_bytes: String(event.admission.unfinishedBytes),
        ahead_bytes: String(Math.round(event.admission.aheadBytes)),
        bytes_per_second: Math.round(event.admission.bytesPerSecond),
      }) }),
    }),
  })
}
