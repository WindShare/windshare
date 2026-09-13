import type { FailureCorrelation } from '../incident/fact'
import { projectCorrelationV1 } from '../export/correlation-v1'
import { decimal } from '../export/trace-observation'
import { formatDiagnosticText } from '../../security/diagnostic-formatter'
import type { V2ConnectionRecoveryTraceEvent, V2ProtocolTraceSource, V2RelayHeartbeatTraceEvent } from '../../session/v2-diagnostics'
import type { RelayHeartbeatTrace } from '../../transport/relay/heartbeat'
import type { TraceEventObservationV2 } from './model'
import { TRACE_FAILURE_DETAIL_MAX_CHARACTERS } from './lane-payload'

const FAILURE_DETAIL_FORMAT = Object.freeze({ maxDepth: 6, maxEntries: 8, maxStringCharacters: 256 })
export const CONNECTION_TRACE_TEXT_MAX_CHARACTERS = 2_048

type ConnectionContext = Readonly<{
  correlation: FailureCorrelation
  generationId?: number
  shareId?: string
  shareInstanceId?: string
}>

export function emitRelayHeartbeat(
  source: V2ProtocolTraceSource | undefined,
  event: RelayHeartbeatTrace,
  relayBase: string,
  context: () => ConnectionContext,
): void {
  try {
    const observer = source?.current
    if (observer === undefined) return
    observer({ eventName: 'relay_heartbeat', ...event, ...context(), relayBase })
  } catch {
    // Capture is passive even before a protocol generation has been admitted.
  }
}

export function projectConnectionTrace(
  event: V2ConnectionRecoveryTraceEvent | V2RelayHeartbeatTraceEvent,
): TraceEventObservationV2 {
  const correlation = projectCorrelationV1(event.correlation)
  const identity = {
    ...(event.generationId === undefined ? {} : { generation_id: decimal(event.generationId) }),
    ...(event.shareId === undefined ? {} : { share_id: boundedText(event.shareId) }),
    ...(event.shareInstanceId === undefined ? {} : { share_instance_id: boundedText(event.shareInstanceId) }),
  }
  const envelope = correlation === undefined ? {} : { correlation }
  if (event.eventName === 'relay_heartbeat') return {
    eventName: event.eventName, ...envelope,
    payload: {
      ...identity, connection_id: decimal(event.connectionId), relay_base: boundedText(event.relayBase), round: decimal(event.round), stage: event.stage,
      buffered_bytes: decimal(event.bufferedBytes),
      elapsed_ms: Math.max(0, Math.ceil(event.elapsedMilliseconds)), timeout_ms: event.timeoutMilliseconds,
    },
  }
  return {
    eventName: event.eventName, ...envelope,
    payload: {
      ...identity, generation_id: decimal(event.generationId), attempt: decimal(event.attempt), phase: event.phase, transition: event.transition,
      ...(event.relayBase === undefined ? {} : { relay_base: boundedText(event.relayBase) }),
      ...(event.delayMilliseconds === undefined ? {} : { delay_ms: event.delayMilliseconds }),
      ...(event.failure === undefined ? {} : {
        failure_detail: formatDiagnosticText(event.failure, FAILURE_DETAIL_FORMAT).slice(0, TRACE_FAILURE_DETAIL_MAX_CHARACTERS),
      }),
    },
  }
}

function boundedText(value: string): string { return value.slice(0, CONNECTION_TRACE_TEXT_MAX_CHARACTERS) }
