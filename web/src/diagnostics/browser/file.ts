export const DIAGNOSTICS_MIME_TYPE = 'application/x-ndjson'

export interface DiagnosticFile {
  readonly name: string
  readonly text: string
  readonly runtimeRunId: string
}

export function createDiagnosticFile(text: string): DiagnosticFile {
  const header: unknown = JSON.parse(text.slice(0, text.indexOf('\n')))
  if (typeof header !== 'object' || header === null ||
      !('runtime_run_id' in header) || typeof header.runtime_run_id !== 'string' ||
      !/^[A-Za-z0-9_-]{22}$/u.test(header.runtime_run_id) ||
      !('time' in header) || typeof header.time !== 'string' ||
      !Number.isFinite(Date.parse(header.time))) {
    throw new TypeError('Diagnostic export has no valid bundle identity')
  }
  const timestamp = new Date(header.time).toISOString().replaceAll(/[:.]/gu, '-')
  return Object.freeze({
    name: `windshare-diagnostics-${timestamp}-${header.runtime_run_id}.ndjson`,
    runtimeRunId: header.runtime_run_id,
    text,
  })
}
