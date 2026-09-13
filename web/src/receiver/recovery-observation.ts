import type { V2ConnectionRecoveryTraceEvent, V2ProtocolTraceSource } from '../session/v2-diagnostics'

export type RecoveryObservation = Pick<V2ConnectionRecoveryTraceEvent,
  'attempt' | 'phase' | 'transition' | 'delayMilliseconds' | 'failure'>

export function observeRecovery(
  source: V2ProtocolTraceSource | undefined,
  context: Omit<V2ConnectionRecoveryTraceEvent, keyof RecoveryObservation | 'eventName'>,
  observation: RecoveryObservation,
): void {
  try {
    source?.current?.({ eventName: 'connection_recovery', ...context, ...observation })
  } catch {
    // Observability cannot change a connection's lifetime or its retry decision.
  }
}
