import type { DiagnosticFile } from './file'

export const DIAGNOSTICS_ARCHIVE_MAX_AGE_MS = 24 * 60 * 60 * 1_000
export const DIAGNOSTICS_ARCHIVE_MAX_CAPTURES = 3
export const DIAGNOSTICS_ARCHIVE_MAX_FILE_BYTES = 12 * 1_024 * 1_024
export const DIAGNOSTICS_ARCHIVE_MAX_TOTAL_BYTES = 16 * 1_024 * 1_024

export interface DiagnosticCaptureSummary {
  readonly id: string
  readonly scope: string
  readonly savedAt: number
  readonly byteLength: number
}

export interface SavedDiagnosticCapture {
  readonly id: string
  readonly scope: string
  readonly savedAt: number
  readonly file: DiagnosticFile
}

export interface DiagnosticsArchiveStore {
  list(): Promise<readonly DiagnosticCaptureSummary[]>
  readFile(id: string): Promise<DiagnosticFile | null>
  save(capture: SavedDiagnosticCapture): Promise<readonly DiagnosticCaptureSummary[]>
  remove(id: string): Promise<readonly DiagnosticCaptureSummary[]>
}

export function isSavedDiagnosticCapture(value: unknown): value is SavedDiagnosticCapture {
  if (typeof value !== 'object' || value === null) return false
  const candidate = value as Partial<SavedDiagnosticCapture>
  const file = candidate.file
  return typeof candidate.id === 'string' && typeof candidate.scope === 'string' &&
    Number.isSafeInteger(candidate.savedAt) && candidate.savedAt! >= 0 &&
    typeof file === 'object' && file !== null &&
    typeof file.name === 'string' && /^[A-Za-z0-9_.-]+\.ndjson$/u.test(file.name) &&
    typeof file.runtimeRunId === 'string' && /^[A-Za-z0-9_-]{22}$/u.test(file.runtimeRunId) &&
    typeof file.text === 'string' && file.text.length <= DIAGNOSTICS_ARCHIVE_MAX_FILE_BYTES
}

export function summarizeDiagnosticCapture(capture: SavedDiagnosticCapture): DiagnosticCaptureSummary {
  if (!isSavedDiagnosticCapture(capture)) throw new TypeError('Invalid diagnostic capture')
  // Count UTF-8 once at admission; listing and retention never need the log body.
  const byteLength = new TextEncoder().encode(capture.file.text).byteLength
  if (byteLength > DIAGNOSTICS_ARCHIVE_MAX_FILE_BYTES) {
    throw new RangeError('Diagnostic capture exceeds the archive file limit')
  }
  return Object.freeze({ id: capture.id, scope: capture.scope, savedAt: capture.savedAt, byteLength })
}

export function isDiagnosticCaptureSummary(value: unknown): value is DiagnosticCaptureSummary {
  if (typeof value !== 'object' || value === null) return false
  const candidate = value as Partial<DiagnosticCaptureSummary>
  return typeof candidate.id === 'string' && typeof candidate.scope === 'string' &&
    Number.isSafeInteger(candidate.savedAt) && candidate.savedAt! >= 0 &&
    Number.isSafeInteger(candidate.byteLength) && candidate.byteLength! >= 0 &&
    candidate.byteLength! <= DIAGNOSTICS_ARCHIVE_MAX_FILE_BYTES
}

/** Prune by bytes as well as count: incident evidence can outweigh the trace ring. */
export function retainDiagnosticCaptures(
  candidates: readonly unknown[],
  now: number,
): readonly DiagnosticCaptureSummary[] {
  const fresh = candidates.filter(isDiagnosticCaptureSummary)
    .filter(capture => capture.savedAt <= now && now - capture.savedAt < DIAGNOSTICS_ARCHIVE_MAX_AGE_MS)
    .sort((left, right) => right.savedAt - left.savedAt)
  const retained: DiagnosticCaptureSummary[] = []
  let bytes = 0
  for (const capture of fresh) {
    if (bytes + capture.byteLength > DIAGNOSTICS_ARCHIVE_MAX_TOTAL_BYTES ||
        retained.some(previous => previous.id === capture.id)) continue
    retained.push(capture)
    bytes += capture.byteLength
    if (retained.length === DIAGNOSTICS_ARCHIVE_MAX_CAPTURES) break
  }
  return Object.freeze(retained)
}
