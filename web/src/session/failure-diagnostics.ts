import type { ProtocolCorrelation } from './identities'
import type { ProtocolTraceSource } from './diagnostics'

export function observeProtocolFailure(
  source: ProtocolTraceSource | undefined,
  error: unknown,
  operation: string,
  correlation: ProtocolCorrelation,
  context?: Readonly<Record<string, unknown>>,
): void {
  try {
    source?.failure?.(error, operation, correlation, context)
  } catch {
    // Failure observation cannot affect lane or session authority.
  }
}
