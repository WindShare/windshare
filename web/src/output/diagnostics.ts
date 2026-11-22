import type { FailureEvidence } from '../diagnostics/failure/evidence'
import type { RecoveryDisposition } from '../diagnostics/incident'
import type { SaveBackendKind } from './model/offers'

export type OutputDiagnosticBackend = 'file_system_access' | 'origin_private' | 'portable'

export interface OutputTraceEvent {
  readonly eventName:
    | 'output_reservation'
    | 'output_write'
    | 'checkpoint'
    | 'settlement'
    | 'publication'
    | 'continuation'
    | 'reopen'
    | 'cleanup'
  readonly payload: Readonly<Record<string, unknown>>
}

export interface OutputTraceSource {
  readonly current: ((event: OutputTraceEvent) => void) | undefined
}

export interface OutputFailureSink {
  record(error: unknown, input?: Readonly<{
    recoveryDisposition?: RecoveryDisposition
    operation?: string
    evidence?: FailureEvidence
  }>): void
}

export interface OutputFailureSinks {
  readonly outputWrite?: OutputFailureSink
  readonly reopen?: OutputFailureSink
  readonly continuation?: OutputFailureSink
  readonly publication?: OutputFailureSink
  readonly cleanup?: OutputFailureSink
}

export interface OutputDiagnosticsPorts {
  readonly backend: OutputDiagnosticBackend
  readonly trace?: OutputTraceSource
  readonly failures?: OutputFailureSinks
}

export function diagnosticBackendName(kind: SaveBackendKind): OutputDiagnosticBackend {
  if (kind === 'fsa') return 'file_system_access'
  if (kind === 'opfs') return 'origin_private'
  return 'portable'
}

export function emitOutputTrace(
  trace: OutputTraceSource | undefined,
  eventName: OutputTraceEvent['eventName'],
  payload: Readonly<Record<string, unknown>>,
): void {
  try {
    trace?.current?.(Object.freeze({ eventName, payload: Object.freeze({ ...payload }) }))
  } catch {
    // Observability cannot affect storage or transfer outcomes.
  }
}
