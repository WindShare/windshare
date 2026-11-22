import type { BlockRetryObservation, BlockSchedulingObservation } from '../content/lane-set'
import type { ReceiveLedgerEvent } from '../contracts/relay-delivery-observation'
import type { ConnectionRecoveryTraceEvent, ProtocolTraceSource } from '../session/diagnostics'
import type { ProtocolCorrelation, ProtocolSessionIdentity } from '../session/identities'
import type { RelayHeartbeatTrace } from '../transport/relay/heartbeat'

export type RecoveryObservation = Pick<ConnectionRecoveryTraceEvent,
  'attempt' | 'phase' | 'transition' | 'delayMilliseconds' | 'waitReason' | 'failure'>

interface RelayTraceContext {
  readonly correlation: ProtocolCorrelation
  readonly generationId?: number
  readonly shareId?: string
  readonly shareInstanceId?: string
}

export function observeRecovery(
  source: ProtocolTraceSource | undefined,
  context: Omit<ConnectionRecoveryTraceEvent, keyof RecoveryObservation | 'eventName'>,
  observation: RecoveryObservation,
): void {
  try {
    source?.current?.({ eventName: 'connection_recovery', ...context, ...observation })
  } catch {
    // Observability cannot change a connection's lifetime or its retry decision.
  }
}

export function emitRelayHeartbeat(
  source: ProtocolTraceSource | undefined,
  event: RelayHeartbeatTrace,
  relayBase: string,
  context: () => RelayTraceContext,
): void {
  try {
    const observer = source?.current
    if (observer !== undefined) observer({ eventName: 'relay_heartbeat', ...event, ...context(), relayBase })
  } catch {
    // Capture is passive even before a protocol generation has been admitted.
  }
}

export function emitRelayDelivery(
  source: ProtocolTraceSource | undefined,
  event: ReceiveLedgerEvent,
  relayBase: string,
  context: () => RelayTraceContext,
): void {
  try {
    const observer = source?.current
    if (observer !== undefined) observer({ eventName: 'relay_delivery', ...event, ...context(), relayBase })
  } catch {
    // Receive ownership cannot depend on a diagnostic observer.
  }
}

export function traceContentScheduling(
  fact: BlockSchedulingObservation,
  protocolSessionId: ProtocolSessionIdentity,
  source: ProtocolTraceSource | undefined,
): void {
  source?.current?.({
    eventName: 'content_scheduling',
    correlation: { protocolSessionId, lane: { id: fact.laneId, epoch: fact.laneEpoch } },
    dispatchSequence: fact.dispatchSequence,
    fileId: fact.fileId,
    localBlockIndex: fact.localBlockIndex,
    route: fact.route,
    purpose: fact.purpose,
    expectedMilliseconds: fact.expectedMilliseconds,
    ...(fact.deliveryEvidence === undefined ? {} : { deliveryEvidence: fact.deliveryEvidence }),
    ...(fact.rescueReason === undefined ? {} : { rescueReason: fact.rescueReason }),
    pendingBytes: fact.pendingBytes,
    bytesPerSecond: fact.bytesPerSecond,
    ...(fact.admission === undefined ? {} : { admission: fact.admission }),
  })
}

export function traceContentRetry(
  fact: BlockRetryObservation,
  protocolSessionId: ProtocolSessionIdentity,
  source: ProtocolTraceSource | undefined,
): void {
  source?.current?.({
    ...fact,
    eventName: 'content_retry',
    correlation: {
      protocolSessionId,
      ...(fact.transition === 'retrying' ? { lane: { id: fact.laneId, epoch: fact.laneEpoch } } : {}),
    },
  })
}
