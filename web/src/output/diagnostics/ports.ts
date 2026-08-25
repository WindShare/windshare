import type { OutputFailureSinks } from './facts'
import {
  createPerformanceSummaryObservations,
  type PerformanceSummaryObservations,
} from './performance-summary'
import type { OutputDiagnosticBackend, OutputTraceSource } from './trace'
import type { TraceClock } from '../../diagnostics/trace/ports'
import type { PerformanceCorrelationProjectionInput } from '../../diagnostics/trace/transfer-payload'

/**
 * Output code receives only reviewed failure sinks and a revocable trace source.
 * Neither port owns retry, settlement, checkpoint, publication, or cleanup policy.
 */
export interface OutputDiagnosticsPorts {
  readonly backend: OutputDiagnosticBackend
  readonly failures?: OutputFailureSinks
  readonly trace?: OutputTraceSource
  readonly performance?: PerformanceSummaryObservations
}

export function bindOutputPerformanceSummary(
  diagnostics: OutputDiagnosticsPorts | undefined,
  correlation: PerformanceCorrelationProjectionInput,
  clock: TraceClock,
): OutputDiagnosticsPorts | undefined {
  if (diagnostics?.trace === undefined) return diagnostics
  try {
    return Object.freeze({
      ...diagnostics,
      performance: createPerformanceSummaryObservations({
        correlation,
        clock,
        trace: diagnostics.trace,
      }),
    })
  } catch {
    // Invalid or unavailable telemetry must not reject output authority acquisition.
    return diagnostics
  }
}
