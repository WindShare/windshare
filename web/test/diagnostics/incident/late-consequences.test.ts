import { describe, expect, it, vi } from 'vitest'
import { normalizeFileTransferFailure } from '../../../src/transfer/job/failures'
import { createIncidentReporter } from '../../../src/diagnostics/incident/reporter'
import {
  createIncidentPolicy, createIncidentScopeIssuer, excludedPresentationDecision,
  incidentPresentationDecision, unclassifiedFailureFact, type IncidentPolicy,
} from '../../../src/diagnostics/incident'
import { createIncidentRecordProjector, createRuntimeRunIdentity, projectDiagnosticsHealthV1 } from '../../../src/diagnostics/export/projector'
import { MAX_INCIDENT_CONSEQUENCE_RECORD_BYTES } from '../../../src/diagnostics/incident/consequences'
import { FailureEvidenceHistory } from '../../../src/diagnostics/failure/history'
import { DiagnosticsEvidenceReader } from '../../../src/diagnostics/evidence'
import { createDiagnosticBundleV2, projectDiagnosticsStatusV2, type DiagnosticBundleIncidentLineV2 } from '../../../src/diagnostics/export/diagnostic-bundle-v2'
import { DiagnosticBundleNdjsonEncoder } from '../../../src/diagnostics/export/ndjson'
import { traceStatus } from '../export/test-support'

describe('late incident consequences', () => {
  it('coalesces a cancellation burst without evicting the root, unrelated evidence, or trace markers', () => {
    const fixture = setup({ maxIncidentHistoryRecords: 2 })
    const first = root(fixture, 'unrelated failure')
    const stopped = root(fixture, 'root failure')
    const retained = fixture.reporter.history.snapshot()
    const abort = new AbortController()
    abort.abort(new Error('stop requested'))
    const concurrent = new Error('independent cleanup failure')
    for (let index = 0; index < 256; index++) {
      expect(normalizeFileTransferFailure(concurrent, {
        signal: abort.signal, incidentScope: stopped.handle,
      })).toEqual({ kind: 'canceled', diagnostic: abort.signal.reason })
    }
    expect(fixture.reporter.history.snapshot()).toEqual(retained)
    expect(fixture.reporter.history.last()).toBe(retained[1])
    expect(retained[0]?.payload.trigger.evidence?.exception.message).toBe('unrelated failure')
    const supplement = fixture.reporter.history.consequencesSnapshot()[0]!
    expect(supplement.fact_count).toBe('256')
    expect(supplement.consequences).toHaveLength(1)
    expect(supplement.consequences[0]).toMatchObject({ count: '256', representative: {
      evidence: { exception: { message: 'independent cleanup failure' } },
    } })
    expect(fixture.reporter.health.incidentHealthSnapshot().incidentHistoryEvictionCount).toBe(0n)
    expect(fixture.console.error).toHaveBeenCalledTimes(3)
    expect(fixture.trace.signal).toHaveBeenCalledTimes(2)
    first.facts.record(cleanup('independent cleanup failure'), 'consequence')
    expect(fixture.reporter.history.consequencesSnapshot().map(record => record.fact_count)).toEqual(['1', '256'])
  })

  it('keeps pre-seal aggregation and explicit user cancellation semantics', () => {
    const fixture = setup()
    const scope = fixture.reporter.openScope('receive')
    const trigger = scope.facts.record(cleanup('root'), 'contributor')
    for (let index = 0; index < 64; index++) scope.facts.record(cleanup('cleanup'), 'consequence')
    fixture.reporter.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', trigger))
    scope.close()
    expect(fixture.reporter.history.last()?.payload.consequences[0]?.count).toBe('64')
    expect(fixture.reporter.history.consequencesSnapshot()).toEqual([])
    const stopped = fixture.reporter.openScope('receive')
    fixture.reporter.submitDecision(stopped.handle, excludedPresentationDecision('receive', 'user_stopped'))
    stopped.close()
    const abort = new AbortController()
    abort.abort(new Error('stop'))
    normalizeFileTransferFailure(new Error('cleanup after user stop'), { signal: abort.signal, incidentScope: stopped.handle })
    expect(fixture.reporter.history.snapshot()).toHaveLength(1)
    expect(fixture.reporter.history.consequencesSnapshot()).toEqual([])
    expect(fixture.evidence.snapshot().records.some(record =>
      record.fact.evidence?.exception.message === 'cleanup after user stop')).toBe(true)
  })

  it('separates classifications and causes and counts overflow without charging it again on replacement', () => {
    const fixture = setup({ maxConsequenceBucketsPerIncident: 5 })
    const scope = root(fixture)
    for (let index = 0; index < 6; index++) scope.facts.record(cleanup('cause-' + index), 'consequence')
    scope.facts.record(cleanup('cause-0', 'settlement'), 'consequence')
    scope.facts.record(cleanup('cause-0', 'output_write'), 'consequence')
    scope.facts.record(cleanup('cause-0'), 'consequence')
    const supplement = fixture.reporter.history.consequencesSnapshot()[0]!
    expect(supplement.consequences).toHaveLength(5)
    expect(supplement.fact_count).toBe('9')
    expect(supplement.overflow_fact_count).toBe('3')
    expect(supplement.consequences.find(bucket => bucket.representative.stage === 'settlement')?.count).toBe('1')
    expect(fixture.reporter.health.incidentHealthSnapshot().factOverflowCount).toBe(3n)
    expect(supplement.consequences.reduce((sum, bucket) => sum + BigInt(bucket.count),
      BigInt(supplement.overflow_fact_count))).toBe(9n)
  })

  it('evicts supplements with their roots and never resurrects an evicted incident', () => {
    const fixture = setup({ maxIncidentHistoryRecords: 1 })
    const first = root(fixture)
    first.facts.record(cleanup('first cleanup'), 'consequence')
    const oldCut = fixture.reporter.history.consequencesSnapshot()
    root(fixture, 'next root')
    expect(fixture.reporter.history.last()?.sequence).toBe('2')
    expect(fixture.reporter.history.consequencesSnapshot()).toEqual([])
    first.facts.record(cleanup('after eviction'), 'consequence')
    expect(fixture.reporter.history.last()?.sequence).toBe('2')
    expect(fixture.reporter.history.consequencesSnapshot()).toEqual([])
    expect(oldCut[0]?.fact_count).toBe('1')
    expect(fixture.reporter.health.incidentHealthSnapshot().incidentHistoryEvictionCount).toBe(1n)
  })

  it('keeps detached cumulative snapshots and expires from the original seal time', () => {
    const fixture = setup({ maxLateIncidentLinkAgeMilliseconds: 10 })
    const scope = root(fixture)
    fixture.advance(2n)
    scope.facts.record(cleanup('cleanup'), 'consequence')
    const first = fixture.reporter.history.consequencesSnapshot()[0]!
    fixture.advance(7n)
    scope.facts.record(cleanup('cleanup'), 'consequence')
    const second = fixture.reporter.history.consequencesSnapshot()[0]!
    expect(first.fact_count).toBe('1')
    expect(second.fact_count).toBe('2')
    expect(second.first_observed.elapsed_ms).toBe('2')
    expect(second.last_observed.elapsed_ms).toBe('9')
    fixture.advance(1n)
    scope.facts.record(cleanup('expired cleanup'), 'consequence')
    expect(fixture.reporter.history.consequencesSnapshot()[0]).toBe(second)
    expect(fixture.reporter.health.incidentHealthSnapshot().lateLinkEvictionCount).toBe(1n)
  })

  it('bounds large cause samples and accounts for every fact after byte pruning', () => {
    const fixture = setup({ maxIncidentRecordBytes: 2_000 })
    const scope = root(fixture)
    for (let index = 0; index < 4; index++) {
      scope.facts.record(cleanup(index + '界'.repeat(4_000)), 'consequence')
    }
    const supplement = fixture.reporter.history.consequencesSnapshot()[0]!
    const bytes = new TextEncoder().encode(JSON.stringify(supplement)).byteLength
    expect(bytes).toBeLessThanOrEqual(Math.min(2_000, MAX_INCIDENT_CONSEQUENCE_RECORD_BYTES))
    expect(supplement.evidence_details_omitted).toBeGreaterThan(0)
    expect(supplement.consequences.reduce((sum, bucket) => sum + BigInt(bucket.count),
      BigInt(supplement.overflow_fact_count))).toBe(4n)
    expect(supplement.overflow_fact_count).not.toBe('0')
    const overflow = fixture.reporter.health.incidentHealthSnapshot().factOverflowCount
    scope.facts.record(cleanup('3' + '界'.repeat(4_000)), 'consequence')
    expect(fixture.reporter.health.incidentHealthSnapshot().factOverflowCount).toBe(overflow + 1n)
    expect(fixture.reporter.history.snapshot()).toHaveLength(1)
  })

  it('exports refreshed supplements without invalidating earlier evidence snapshots', () => {
    const fixture = setup()
    const scope = root(fixture)
    const initial = exportInput(fixture)
    const reader = new DiagnosticsEvidenceReader(initial.identity, () => initial.time)
    const before = reader.read(initial)
    const original = before.export()
    scope.facts.record(cleanup('cleanup'), 'consequence')
    const first = reader.read(exportInput(fixture))
    const firstExport = first.export()
    expect(first).not.toBe(before)
    scope.facts.record(cleanup('cleanup'), 'consequence')
    const second = reader.read(exportInput(fixture))
    expect(second).not.toBe(first)
    expect(reader.read(exportInput(fixture))).toBe(second)
    const exported = incidentLines(second.export())
    expect(exported).toHaveLength(1)
    expect(exported[0]?.record.sequence).toBe('1')
    expect(exported[0]?.record.payload.fact_count).toBe('1')
    expect(exported[0]?.late_consequences?.fact_count).toBe('2')
    expect(first.export()).toBe(firstExport)
    expect(incidentLines(firstExport)[0]?.late_consequences?.fact_count).toBe('1')
    expect(before.export()).toBe(original)
  })

  it('preserves roots and late counts when the total export budget trims samples', () => {
    const fixture = setup()
    const scope = root(fixture, 'root explanation')
    scope.facts.record(cleanup('late explanation ' + '界'.repeat(4_000)), 'consequence')
    const input = exportInput(fixture)
    const capacity = 3_000
    const encoded = new DiagnosticBundleNdjsonEncoder(capacity).encode(createDiagnosticBundleV2(input))
    expect(new TextEncoder().encode(encoded).byteLength).toBeLessThanOrEqual(capacity)
    const line = incidentLines(encoded)[0]!
    expect(line.record.payload.trigger.evidence?.exception.message).toBe('root explanation')
    expect(line.late_consequences?.fact_count).toBe('1')
    expect(line.late_consequences?.overflow_fact_count).toBe('1')
    expect(line.late_consequences?.consequences).toEqual([])
    expect(input.lateConsequences[0]?.consequences).toHaveLength(1)
    expect(() => createDiagnosticBundleV2({ ...input, incidents: [] })).toThrow(/owning incident/)
    expect(() => createDiagnosticBundleV2({ ...input, lateConsequences: [
      ...input.lateConsequences, ...input.lateConsequences,
    ] })).toThrow(/owning incident/)
  })
})

function setup(overrides: Partial<IncidentPolicy> = {}) {
  const policy = createIncidentPolicy(overrides)
  let elapsedMs = 0n
  const console = { error: vi.fn() }
  const trace = { signal: vi.fn() }
  const evidence = new FailureEvidenceHistory()
  const reporter = createIncidentReporter({
    policy, evidence,
    projector: createIncidentRecordProjector({ policy,
      runtimeRunId: createRuntimeRunIdentity(new Uint8Array(16).fill(1)),
      build: { version: '0.0.0', mode: 'test' }, secureContext: true,
    }),
    timeSource: { capture: () => ({ time: new Date(Number(elapsedMs)).toISOString(), elapsedMs }) },
    consoleSink: console, traceSignals: trace,
    scopeIssuer: createIncidentScopeIssuer({ policy, clock: { elapsedMilliseconds: () => Number(elapsedMs) },
      scheduler: { schedule: () => ({ cancel() {} }) },
    }),
  })
  return { reporter, console, trace, evidence, advance: (amount: bigint) => { elapsedMs += amount } }
}

function root(fixture: ReturnType<typeof setup>, message = 'root') {
  const scope = fixture.reporter.openScope('receive')
  const trigger = scope.facts.record(cleanup(message), 'contributor')
  fixture.reporter.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', trigger))
  scope.close()
  return scope
}

function cleanup(message: string, stage: 'cleanup' | 'settlement' | 'output_write' = 'cleanup') {
  const error = new Error(message)
  error.stack = 'deterministic cleanup stack'
  return unclassifiedFailureFact({ stage, recoveryDisposition: 'none', error, operation: 'cleanup' })
}

function exportInput(fixture: ReturnType<typeof setup>) {
  const incidents = fixture.reporter.history.snapshot()
  const first = incidents[0]!
  const health = projectDiagnosticsHealthV1(fixture.reporter.health.incidentHealthSnapshot())
  return {
    identity: { build: first.payload.build, runtime: first.payload.runtime, runtimeRunId: first.runtime_run_id },
    time: '2026-10-31T00:00:00Z', incidents,
    lateConsequences: fixture.reporter.history.consequencesSnapshot(),
    status: projectDiagnosticsStatusV2(traceStatus(), health), healthAtExport: health,
  }
}

function incidentLines(text: string): DiagnosticBundleIncidentLineV2[] {
  return text.trimEnd().split('\n').map(line => JSON.parse(line)).filter(line => line.line_type === 'incident')
}
