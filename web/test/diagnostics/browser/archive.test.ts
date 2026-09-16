import { describe, expect, it } from 'vitest'
import {
  DIAGNOSTICS_ARCHIVE_MAX_AGE_MS,
  DIAGNOSTICS_ARCHIVE_MAX_FILE_BYTES,
  summarizeDiagnosticCapture,
  retainDiagnosticCaptures,
  type DiagnosticCaptureSummary,
  type SavedDiagnosticCapture,
} from '../../../src/diagnostics/browser/archive'
import { createDiagnosticFile } from '../../../src/diagnostics/browser/file'

const RUN_ID = 'AQAAAAAAAAAAAAAAAAAAAA'
const NOW = DIAGNOSTICS_ARCHIVE_MAX_AGE_MS * 2

describe('diagnostic archive retention', () => {
  it('keeps the newest three summaries, rejects malformed/expired/future records, and deduplicates IDs', () => {
    const retained = retainDiagnosticCaptures([
      summary('expired', NOW - DIAGNOSTICS_ARCHIVE_MAX_AGE_MS),
      summary('future', NOW + 1), null, { id: 'invalid' },
      summary('old', NOW - 4), summary('third', NOW - 3), summary('second', NOW - 2),
      summary('newest', NOW), summary('newest', NOW - 5),
    ], NOW)
    expect(retained.map(item => item.id)).toEqual(['newest', 'second', 'third'])
  })

  it('bounds total bytes using summaries without retaining or encoding bodies', () => {
    const sixMiB = 6 * 1_024 * 1_024
    expect(retainDiagnosticCaptures([
      summary('one', NOW, sixMiB), summary('two', NOW - 1, sixMiB), summary('three', NOW - 2, sixMiB),
      summary('oversized', NOW, DIAGNOSTICS_ARCHIVE_MAX_FILE_BYTES + 1),
    ], NOW).map(item => item.id)).toEqual(['one', 'two'])
  })

  it('counts UTF-8 once when admitting a saved capture', () => {
    expect(summarizeDiagnosticCapture(capture('utf8', '界')).byteLength).toBe(3)
    const oversized = '界'.repeat(Math.floor(DIAGNOSTICS_ARCHIVE_MAX_FILE_BYTES / 3) + 1)
    expect(() => summarizeDiagnosticCapture(capture('oversized', oversized))).toThrow(RangeError)
  })

  it('names exports with the original run identity and timestamp while preserving NDJSON', () => {
    const text = JSON.stringify({ line_type: 'bundle_header', runtime_run_id: RUN_ID, time: '2026-09-16T01:02:03Z' }) + '\n'
    const file = createDiagnosticFile(text)
    expect(file.name).toBe('windshare-diagnostics-2026-09-16T01-02-03-000Z-' + RUN_ID + '.ndjson')
    expect(file.text).toBe(text)
    expect(() => createDiagnosticFile('{}\n')).toThrow()
  })
})

function summary(id: string, savedAt: number, byteLength = 8): DiagnosticCaptureSummary {
  return { id, savedAt, scope: '/share', byteLength }
}

function capture(id: string, text: string): SavedDiagnosticCapture {
  return { id, savedAt: NOW, scope: '/share', file: { name: 'diagnostics.ndjson', runtimeRunId: RUN_ID, text } }
}
