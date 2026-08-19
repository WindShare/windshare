import type { OutputFailureSinks } from './facts'
import type { OutputDiagnosticBackend, OutputTraceSource } from './trace'

/**
 * Output code receives only reviewed failure sinks and a revocable trace source.
 * Neither port owns retry, settlement, checkpoint, publication, or cleanup policy.
 */
export interface OutputDiagnosticsPorts {
  readonly backend: OutputDiagnosticBackend
  readonly failures?: OutputFailureSinks
  readonly trace?: OutputTraceSource
}
