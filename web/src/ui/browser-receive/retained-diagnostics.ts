import type {
  OutputDiagnosticBackend,
  OutputDiagnosticsPorts,
  OutputFailureSinks,
  OutputTraceSource,
} from '../../output/diagnostics'

export function diagnosticsOption(
  backend: OutputDiagnosticBackend,
  trace: OutputTraceSource | undefined,
  failures?: OutputFailureSinks,
): { readonly diagnostics?: OutputDiagnosticsPorts } {
  const diagnostics = diagnosticsFor(backend, trace, failures)
  return diagnostics === undefined ? Object.freeze({}) : Object.freeze({ diagnostics })
}

export function diagnosticsFor(
  backend: OutputDiagnosticBackend,
  trace: OutputTraceSource | undefined,
  failures?: OutputFailureSinks,
): OutputDiagnosticsPorts | undefined {
  if (trace === undefined && failures === undefined) return undefined
  return Object.freeze({
    backend,
    ...(failures === undefined ? {} : { failures }),
    ...(trace === undefined ? {} : { trace }),
  })
}
