import { expect, it } from 'vitest'
import { captureFailureEvidence } from '../../../src/diagnostics/failure/evidence'
import { DiagnosticBundleNdjsonEncoder } from '../../../src/diagnostics/export/ndjson'
import { createDiagnosticBundleV2, projectDiagnosticsStatusV2, type DiagnosticBundleIncidentLineV2 } from '../../../src/diagnostics/export/diagnostic-bundle-v2'
import { deepFreezeJson } from '../../../src/diagnostics/export/json'
import { diagnosticsHealthV1, incidentRecord, TEST_BUNDLE_IDENTITY, traceStatus } from './test-support'

it('reserves every nominated trigger before secondary detail under the total UTF-8 export budget', () => {
  const capacity = 6_000
  const incidents = ['1', '2'].map(sequence => {
    const base = incidentRecord(sequence)
    const error = new Error('root-' + sequence, { cause: new Error('界'.repeat(1500)) })
    error.stack = 'stack'.repeat(1000)
    const evidence = captureFailureEvidence(error, { operation: 'catalog.validate', context: { directoryId: sequence } })
    return deepFreezeJson({ ...base, payload: { ...base.payload,
      trigger: { ...base.payload.trigger, evidence },
    } })
  })
  const health = diagnosticsHealthV1()
  const bundle = createDiagnosticBundleV2({
    identity: TEST_BUNDLE_IDENTITY, time: '2026-10-31T00:00:00Z', incidents,
    status: projectDiagnosticsStatusV2(traceStatus(), health), healthAtExport: health,
  })
  const encoded = new DiagnosticBundleNdjsonEncoder(capacity).encode(bundle)
  expect(new TextEncoder().encode(encoded).byteLength).toBeLessThanOrEqual(capacity)
  const lines = encoded.trimEnd().split('\n').map(line => JSON.parse(line))
  expect(lines[0].export_omitted.incidents_summarized).toBeGreaterThan(0)
  const roots = lines.slice(1) as DiagnosticBundleIncidentLineV2[]
  expect(roots[0]?.record.payload.trigger.evidence?.exception.message).toContain('root-1')
  expect(roots[1]?.record.payload.trigger.evidence?.exception.message).toContain('root-2')
  expect(roots.some(line => line.record.payload.trigger.evidence?.exception.message?.includes('cause:'))).toBe(true)
  expect(roots.some(line => line.record.payload.trigger.evidence?.exception.truncated.includes('summary'))).toBe(true)
  expect(incidents[0]!.payload.trigger.evidence.exception.cause?.message).not.toBeNull()
})
