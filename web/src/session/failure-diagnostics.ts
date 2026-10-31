import { captureFailureEvidence, type FailureContextValue, type FailureEvidence } from '../diagnostics/failure/evidence'
import type { FailureCorrelation } from '../diagnostics/incident/failure-correlation'
import type { ProtocolTraceSource } from './diagnostics'

export function observeProtocolFailure(
  source: ProtocolTraceSource | undefined, error: unknown, operation: string,
  correlation: FailureCorrelation, context?: Readonly<Record<string, FailureContextValue>>, relation?: 'consequence',
): FailureEvidence {
  const evidence = captureFailureEvidence(error, { operation, ...(context === undefined ? {} : { context }) })
  try {
    source?.failure?.(error, operation, correlation, relation)
  } catch { /* Failure evidence cannot affect lane or session authority. */ }
  return evidence
}
