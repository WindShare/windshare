import type { V2BlockSchedulingObservation } from '../../content/v2-lane-set'
import type { V2ContentSchedulingTraceEvent, V2ProtocolTraceSource } from '../../session/v2-diagnostics'
import type { FailureIdentity } from '../incident/fact'
import { projectCorrelationV1 } from '../export/correlation-v1'
import type { TraceEventObservationV2 } from './model'

export function traceContentScheduling(
  fact: V2BlockSchedulingObservation,
  protocolSessionId: FailureIdentity<'protocol_session'>,
  source: V2ProtocolTraceSource | undefined,
): void {
  source?.current?.({
    eventName: 'content_scheduling',
    correlation: { protocolSessionId, lane: { id: fact.laneId, epoch: fact.laneEpoch } },
    dispatchSequence: fact.dispatchSequence,
    fileId: fact.fileId, localBlockIndex: fact.localBlockIndex,
    route: fact.route, purpose: fact.purpose,
    expectedMilliseconds: fact.expectedMilliseconds,
    pendingBytes: fact.pendingBytes, bytesPerSecond: fact.bytesPerSecond,
  })
}

export function projectContentScheduling(event: V2ContentSchedulingTraceEvent): TraceEventObservationV2 {
  const correlation = projectCorrelationV1(event.correlation)
  if (correlation === undefined) throw new TypeError('Content scheduling requires session correlation')
  return Object.freeze({
    eventName: 'content_scheduling',
    correlation,
    payload: Object.freeze({
      dispatch_sequence: String(event.dispatchSequence),
      file_id: event.fileId, block_index: String(event.localBlockIndex),
      route: event.route, purpose: event.purpose,
      expected_ms: Math.ceil(event.expectedMilliseconds),
      pending_bytes: String(event.pendingBytes),
      bytes_per_second: Math.round(event.bytesPerSecond),
    }),
  })
}
