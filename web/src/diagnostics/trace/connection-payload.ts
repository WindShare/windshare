import type { FailureCorrelation } from '../incident/fact'
import { projectCorrelationV1 } from '../export/correlation-v1'
import { decimal, durationMilliseconds } from '../export/trace-observation'
import { formatDiagnosticText } from '../../security/diagnostic-formatter'
import type { V2ConnectionRecoveryTraceEvent, V2ProtocolTraceSource, V2RelayHeartbeatTraceEvent, V2RelayDeliveryTraceEvent } from '../../session/v2-diagnostics'
import type { RelayHeartbeatTrace } from '../../transport/relay/heartbeat'
import type { ReceiveCounts, ReceiveLedgerEvent } from '../../transport/relay/receive-ledger'
import type { TraceEventObservationV2 } from './model'
import { TRACE_FAILURE_DETAIL_MAX_CHARACTERS } from './lane-payload'

export interface ConnectionTracePayloads {
  readonly connection_recovery: Readonly<{
    generation_id: string
    share_id?: string
    share_instance_id?: string
    relay_base?: string
    attempt: string
    phase: 'initial' | 'fast' | 'waiting'
    transition: 'attempt_started' | 'attempt_failed' | 'waiting' | 'connected' | 'terminal' | 'retry_requested'
    delay_ms?: number
    wait_reason?: 'backoff' | 'capacity' | 'server'
    failure_detail?: string
  }>
  readonly relay_heartbeat: Readonly<{
    connection_id: string
    generation_id?: string
    share_id?: string
    share_instance_id?: string
    relay_base: string
    round: string
    stage: 'probe' | 'acknowledged' | 'failed'
    buffered_bytes: string
    elapsed_ms: number
    timeout_ms: number
  }>
  readonly relay_delivery: Readonly<{
    connection_id: string
    relay_base: string
    relay_session_id?: string
    share_id?: string
    share_instance_id?: string
    action: import('../../transport/relay/receive-ledger/types').ReceiveAction
    failure?: import('../../transport/relay/receive-ledger/types').ReceiveFailure
    capacity: Readonly<{ frames: string; encoded_bytes: string }>
    held: Readonly<{ frames: string; encoded_bytes: string }>
    outstanding: Readonly<{ frames: string; encoded_bytes: string }>
    pending_consumed: Readonly<{ frames: string; encoded_bytes: string }>
    retained_sessions: string
    progress?: Readonly<{
      phase: import('../../transport/relay/receive-ledger/types').ReceivePhase
      received: Readonly<{ frames: string; encoded_bytes: string }>
      consumed: Readonly<{ frames: string; encoded_bytes: string }>
      discarded: Readonly<{ frames: string; encoded_bytes: string }>
      consumed_prefix: Readonly<{ frames: string; encoded_bytes: string }>
    }>
  }>
}

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

export function emitRelayDelivery(
  source: V2ProtocolTraceSource | undefined,
  event: ReceiveLedgerEvent,
  relayBase: string,
  context: () => ConnectionContext,
): void {
  try {
    const observer = source?.current
    if (observer !== undefined) observer({ eventName: 'relay_delivery', ...event, ...context(), relayBase })
  } catch {
    // Receive ownership cannot depend on a diagnostic observer.
  }
}

export function projectRelayDeliveryTrace(event: V2RelayDeliveryTraceEvent): TraceEventObservationV2 {
  const correlation = projectCorrelationV1(event.correlation)
  const progress = event.progress
  return {
    eventName: 'relay_delivery', ...(correlation === undefined ? {} : { correlation }),
    payload: {
      connection_id: event.connectionId, relay_base: boundedText(event.relayBase), action: event.action,
      capacity: deliveryCounts(event.capacity),
      ...(event.sessionId === undefined ? {} : { relay_session_id: event.sessionId }),
      ...(event.shareId === undefined ? {} : { share_id: event.shareId }),
      ...(event.shareInstanceId === undefined ? {} : { share_instance_id: event.shareInstanceId }),
      ...(event.failure === undefined ? {} : { failure: event.failure }),
      held: deliveryCounts(event.usage.held), outstanding: deliveryCounts(event.usage.outstanding),
      pending_consumed: deliveryCounts(event.usage.pendingConsumed), retained_sessions: decimal(event.usage.retainedSessions),
      ...(progress === undefined ? {} : { progress: {
        phase: progress.phase, received: deliveryCounts(progress.received), consumed: deliveryCounts(progress.consumed),
        discarded: deliveryCounts(progress.discarded), consumed_prefix: deliveryCounts(progress.consumedPrefix),
      } }),
    },
  }
}

function deliveryCounts(counts: ReceiveCounts): { frames: string; encoded_bytes: string } {
  return { frames: decimal(counts.frames), encoded_bytes: decimal(counts.encodedBytes) }
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
      elapsed_ms: durationMilliseconds(Math.max(0, event.elapsedMilliseconds)), timeout_ms: event.timeoutMilliseconds,
    },
  }
  return {
    eventName: event.eventName, ...envelope,
    payload: {
      ...identity, generation_id: decimal(event.generationId), attempt: decimal(event.attempt), phase: event.phase, transition: event.transition,
      ...(event.relayBase === undefined ? {} : { relay_base: boundedText(event.relayBase) }),
      ...(event.delayMilliseconds === undefined ? {} : { delay_ms: durationMilliseconds(event.delayMilliseconds) }),
      ...(event.waitReason === undefined ? {} : { wait_reason: event.waitReason }),
      ...(event.failure === undefined ? {} : {
        failure_detail: formatDiagnosticText(event.failure, FAILURE_DETAIL_FORMAT).slice(0, TRACE_FAILURE_DETAIL_MAX_CHARACTERS),
      }),
    },
  }
}

function boundedText(value: string): string { return value.slice(0, CONNECTION_TRACE_TEXT_MAX_CHARACTERS) }
