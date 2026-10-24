import type { ConnectionRecoveryTraceEvent, ProtocolTraceSource } from '../session/diagnostics'

export type RecoveryObservation = Pick<ConnectionRecoveryTraceEvent,
  'attempt' | 'phase' | 'transition' | 'delayMilliseconds' | 'waitReason' | 'failure'>

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
