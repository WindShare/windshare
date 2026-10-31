import { expect, it } from 'vitest'
import { createDiagnosticBundleV2, projectDiagnosticsStatusV2, type DiagnosticBundleIncidentLineV2 } from '../../../src/diagnostics/export/diagnostic-bundle-v2'
import { DiagnosticBundleNdjsonEncoder } from '../../../src/diagnostics/export/ndjson'
import { createIncidentRecordProjector, createRuntimeRunIdentity } from '../../../src/diagnostics/export/projector'
import { createFailureFactAccumulator } from '../../../src/diagnostics/incident/aggregate'
import { projectIncidentConsequencesV2 } from '../../../src/diagnostics/incident/consequences'
import { unclassifiedFailureFact } from '../../../src/diagnostics/incident/fact'
import { createIncidentPolicy } from '../../../src/diagnostics/incident/policy'
import { createIncidentScopeIssuer } from '../../../src/diagnostics/incident/scope'
import { diagnosticsHealthV1, incidentHealth, TEST_BUNDLE_IDENTITY, traceStatus } from './test-support'

const TIME = '2026-10-31T00:00:00Z'
const EXPORT_CAPACITY = 4500

function fact(message: string) {
  const error = new Error(message + 'x'.repeat(4000))
  error.stack = ''
  return unclassifiedFailureFact({ stage: 'content_read', recoveryDisposition: 'terminal', error, operation: 'read' })
}

function projectedIncident(maxIncidentRecordBytes: number) {
  const policy = createIncidentPolicy({ maxIncidentRecordBytes })
  const scope = createIncidentScopeIssuer().open('receive')
  try {
    const accumulator = createFailureFactAccumulator(scope.identity, policy)
    for (const relation of ['contributor', 'consequence'] as const) {
      for (const message of ['A', 'B']) accumulator.record(scope.facts.record(fact(relation + message), relation))
    }
    const trigger = scope.facts.record(fact('root'), 'contributor')
    accumulator.record(trigger)
    const bytes = new Uint8Array(16)
    bytes[0] = 1
    const projector = createIncidentRecordProjector({
      policy, runtimeRunId: createRuntimeRunIdentity(bytes), build: TEST_BUNDLE_IDENTITY.build, secureContext: true,
    })
    const record = projector.project({
      sequence: 1n, time: TIME, elapsedMs: 1n, presentation: { boundary: 'receive', outcome: 'failed' },
      facts: accumulator.seal(trigger), context: {}, health: incidentHealth(),
    }).record
    const late = createFailureFactAccumulator(scope.identity, policy)
    for (const message of ['late A', 'late B', 'late C']) late.record(scope.facts.record(fact(message), 'consequence'))
    const supplement = projectIncidentConsequencesV2({
      runtimeRunId: record.runtime_run_id, incidentSequence: 1n, scope: scope.identity,
      firstObserved: { time: TIME, elapsedMs: 2n }, lastObserved: { time: TIME, elapsedMs: 2n },
      facts: late.snapshotConsequences(), policy,
    }).record
    return { record, supplement }
  } finally { scope.close() }
}

it.each([
  { recordCapacity: 7000, projectedRootOmissions: 5, projectedLateOmissions: 3 },
  { recordCapacity: 65_536, projectedRootOmissions: 0, projectedLateOmissions: 0 },
])('counts each evidence detail once across record and export limits: $recordCapacity bytes', ({
  recordCapacity, projectedRootOmissions, projectedLateOmissions,
}) => {
  const { record, supplement } = projectedIncident(recordCapacity)
  expect(record.payload.evidence_details_omitted ?? 0).toBe(projectedRootOmissions)
  expect(supplement.evidence_details_omitted).toBe(projectedLateOmissions)
  const health = diagnosticsHealthV1()
  const bundle = createDiagnosticBundleV2({
    identity: TEST_BUNDLE_IDENTITY, time: TIME, incidents: [record], lateConsequences: [supplement],
    status: projectDiagnosticsStatusV2(traceStatus(), health), healthAtExport: health,
  })
  const encoder = new DiagnosticBundleNdjsonEncoder(EXPORT_CAPACITY)
  const encoded = encoder.encode(bundle)
  expect(encoder.encode(bundle)).toBe(encoded)
  expect(new TextEncoder().encode(encoded).byteLength).toBeLessThanOrEqual(EXPORT_CAPACITY)
  const lines = encoded.trimEnd().split('\n').map(line => JSON.parse(line))
  expect(lines[0].export_omitted.incidents_summarized).toBe(1)
  const exported = lines[1] as DiagnosticBundleIncidentLineV2
  expect(exported.record.payload.evidence_details_omitted).toBe(5)
  expect(exported.record.payload.overflow_fact_count).toBe('4')
  expect(exported.record.payload.trigger.evidence?.exception.message).toContain('root')
  expect(exported.late_consequences?.evidence_details_omitted).toBe(3)
  expect(exported.late_consequences?.overflow_fact_count).toBe('3')
  expect(record.payload.evidence_details_omitted ?? 0).toBe(projectedRootOmissions)
  expect(supplement.evidence_details_omitted).toBe(projectedLateOmissions)
})
