import { createBrowserDiagnosticsComposition } from '../../src/application/diagnostics/composition'
import { BrowserDiagnosticsSession } from '../../src/diagnostics/browser/session'
import { summarizeDiagnosticCapture, type SavedDiagnosticCapture } from '../../src/diagnostics/browser/archive'
import { FileOutputError, normalizeFileTransferFailure } from '../../src/transfer/job/failures'
import { incidentPresentationDecision } from '../../src/diagnostics/incident'

const FAILURE_COUNT = 512
const BATCH_SIZE = 16
const DISTINCT_CAUSES = 32
const CAUSE_TEXT_REPEATS = 512

export async function runFailureEvidenceWorkload(traceEnabled: boolean) {
  const diagnostics = createBrowserDiagnosticsComposition({
    build: { version: '0.0.0', mode: 'test' }, secureContext: true,
    consoleSink: { error: () => undefined },
  })
  let saves = 0
  let serializedBytes = 0
  let archive: SavedDiagnosticCapture | undefined
  const session = new BrowserDiagnosticsSession({
    runtime: diagnostics.runtime, observeCapture: diagnostics.observeEvidence,
    pageUrl: location.href,
    archive: {
      list: async () => [],
      readFile: async () => archive?.file ?? null,
      remove: async () => { archive = undefined; return [] },
      save: async capture => {
        saves++
        serializedBytes += new TextEncoder().encode(capture.file.text).byteLength
        archive = capture
        return [summarizeDiagnosticCapture(capture)]
      },
    },
  })
  if (traceEnabled) session.enable()
  const scope = diagnostics.incidents.openScope('receive')
  const batchMilliseconds: number[] = []
  let nominated: ReturnType<typeof normalizeFileTransferFailure> | undefined
  const started = performance.now()
  for (let start = 0; start < FAILURE_COUNT; start += BATCH_SIZE) {
    const batchStart = performance.now()
    for (let offset = 0; offset < BATCH_SIZE; offset++) {
      const index = start + offset
      const cause = new AggregateError([
        new Error('cause ' + (index % DISTINCT_CAUSES) + ' detail'.repeat(CAUSE_TEXT_REPEATS)),
        new DOMException('storage unavailable', 'NotAllowedError'),
      ], 'provider failed')
      nominated = normalizeFileTransferFailure(
        new FileOutputError('native write rejected', 'output-write-failed', cause),
        { incidentScope: scope.handle, operation: 'output.write', context: { file: 'file-' + index } },
      )
    }
    batchMilliseconds.push(performance.now() - batchStart)
    await new Promise(resolve => setTimeout(resolve, 0))
  }
  if (nominated?.kind !== 'fault' || nominated.factRef === undefined) throw new Error('Missing nominated trigger')
  diagnostics.incidents.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', nominated.factRef))
  scope.close()
  await session.checkpoint()
  const evidence = diagnostics.runtime.inspectFailureEvidence()
  const result = {
    traceEnabled, failures: FAILURE_COUNT, elapsedMilliseconds: performance.now() - started,
    maximumBatchMilliseconds: Math.max(...batchMilliseconds),
    retainedRecords: evidence.records.length, retainedBytes: evidence.retainedBytes,
    omittedObservations: evidence.omitted, saves, serializedBytes,
    archiveBytes: archive === undefined ? 0 : new TextEncoder().encode(archive.file.text).byteLength,
    retainedTrigger: diagnostics.runtime.inspectLastFailure()?.payload.trigger.evidence?.exception.message,
  }
  session.dispose()
  return result
}

declare global {
  interface Window { runFailureEvidenceWorkload: typeof runFailureEvidenceWorkload }
}
window.runFailureEvidenceWorkload = runFailureEvidenceWorkload
