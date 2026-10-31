import { describe, expect, it } from 'vitest'
import { createFailureFactAccumulator, createIncidentPolicy, createIncidentScopeIssuer, unclassifiedFailureFact, incidentPresentationDecision } from '../../../src/diagnostics/incident'
import { FailureEvidenceHistory, MAX_FAILURE_OBSERVATION_BYTES, MAX_FAILURE_OBSERVATIONS } from '../../../src/diagnostics/failure/history'
import { createBrowserDiagnosticsComposition } from '../../../src/application/diagnostics/composition'
import { createIncidentReporter } from '../../../src/diagnostics/incident/reporter'
import { MAX_PENDING_INCIDENT_CONTEXTS } from '../../../src/diagnostics/incident/pending-scopes'
import { createIncidentRecordProjector, createRuntimeRunIdentity } from '../../../src/diagnostics/export/projector'
import { normalizeFileTransferFailure, FileOutputError } from '../../../src/transfer/job/failures'
import { isBoundedDiagnosticExceptionProjection, MAX_EXCEPTION_TEXT_BYTES } from '../../../src/diagnostics/exception'

function fact(message: string, lane = 1) {
  return unclassifiedFailureFact({ stage: 'content_read', recoveryDisposition: 'terminal',
    error: new Error(message), operation: 'read', context: { lane },
  })
}

describe('bounded cause retention', () => {
  it('seals nominated triggers before evicting active aggregation under pressure', () => {
    const diagnostics = createBrowserDiagnosticsComposition({ build: { version: '0.0.0', mode: 'test' },
      secureContext: true, consoleSink: { error: () => undefined },
    })
    const scopes = Array.from({ length: MAX_PENDING_INCIDENT_CONTEXTS + 1 }, (_, index) => {
      const scope = diagnostics.incidents.openScope('receive')
      const trigger = scope.facts.record(fact('nominated-' + index), 'contributor')
      diagnostics.incidents.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', trigger))
      return scope
    })
    expect(diagnostics.runtime.inspectLastFailure()?.payload.trigger.evidence?.exception.message).toBe('nominated-0')
    scopes.forEach(scope => scope.close())
    expect(diagnostics.incidents.history.snapshot()).toHaveLength(scopes.length)
  })

  it('counts distinct causes within one classification and keeps the nominated overflow trigger', () => {
    const scope = createIncidentScopeIssuer().open('receive')
    const accumulator = createFailureFactAccumulator(scope.identity)
    const facts = [fact('first'), fact('first', 9), fact('second'), fact('third'), fact('fourth'), fact('root')]
    const refs = facts.map(value => scope.facts.record(value, 'contributor'))
    refs.forEach(ref => accumulator.record(ref))
    const result = accumulator.seal(refs[5]!)
    expect(result.trigger.fact).toBe(facts[5])
    expect(result.contributorBuckets).toHaveLength(4)
    expect(result.contributorBuckets.find(bucket => bucket.representative.evidence?.exception.message === 'first')?.count).toBe(2n)
    expect(result.contributorOverflowCount).toBe(0n)
    scope.close()
  })

  it('creates references for the receiving owner even when separate issuers reuse scope numbers', () => {
    const first = createIncidentScopeIssuer().open('receive')
    const second = createIncidentScopeIssuer().open('receive')
    const original = normalizeFileTransferFailure(new FileOutputError('write denied', 'output-write-failed'), { incidentScope: first.handle })
    expect(original.kind).toBe('fault')
    if (original.kind !== 'fault') return
    const moved = normalizeFileTransferFailure(original.diagnostic, { incidentScope: second.handle })
    expect(moved.kind).toBe('fault')
    if (moved.kind !== 'fault') return
    expect(moved.factRef).not.toBe(original.factRef)
    expect(moved.factRef?.scope).toBe(second.identity)
    expect(moved.fact.evidence).toBe(original.fact.evidence)
    const accumulator = createFailureFactAccumulator(second.identity)
    expect(() => accumulator.record(original.factRef!)).toThrow('another incident scope')
    accumulator.record(moved.factRef!)
    expect(accumulator.seal(moved.factRef!).trigger.fact.evidence).toBe(original.fact.evidence)
    const repeated = normalizeFileTransferFailure(moved.diagnostic, { incidentScope: second.handle })
    if (repeated.kind === 'fault') expect(repeated.factRef).toBe(moved.factRef)
    first.close(); second.close()
  })

  it('preserves independent cleanup evidence without giving it cancellation authority', () => {
    const recorded: string[] = []
    const scope = createIncidentScopeIssuer().open('receive', { factRecorded: value => recorded.push(value.relation) })
    const abort = new AbortController()
    abort.abort(new Error('user stopped'))
    const failure = normalizeFileTransferFailure(new Error('cleanup failed'), { signal: abort.signal, incidentScope: scope.handle })
    expect(failure.kind).toBe('canceled')
    expect(failure.diagnostic).toBe(abort.signal.reason)
    expect(recorded).toEqual(['consequence'])
    scope.close()
  })

  it('bounds a burst and exposes omitted observations while retaining the most recent explanations', () => {
    const history = new FailureEvidenceHistory()
    for (let index = 0; index < 256; index++) history.record(fact('cause-' + index + 'x'.repeat(4000), index))
    const snapshot = history.snapshot()
    expect(snapshot.records.length).toBeLessThanOrEqual(MAX_FAILURE_OBSERVATIONS)
    expect(snapshot.retainedBytes).toBeLessThanOrEqual(MAX_FAILURE_OBSERVATION_BYTES)
    expect(BigInt(snapshot.omitted) + snapshot.records.reduce((sum, row) => sum + BigInt(row.count), 0n)).toBe(256n)
    expect(snapshot.records.at(-1)?.fact.evidence?.context.lane).toBe(255)
    for (const row of snapshot.records) expect(isBoundedDiagnosticExceptionProjection(row.fact.evidence!.exception, MAX_EXCEPTION_TEXT_BYTES)).toBe(true)
  })

  it('preserves a trimmed root alongside bounded late evidence', () => {
    const policy = createIncidentPolicy({ maxIncidentHistoryRecords: 1, maxIncidentRecordBytes: 3000 })
    const projector = createIncidentRecordProjector({
      policy, runtimeRunId: createRuntimeRunIdentity(new Uint8Array(16).fill(1)),
      build: { version: '0.0.0', mode: 'test' }, secureContext: true,
    })
    const reporter = createIncidentReporter({
      policy, projector, timeSource: { capture: () => ({ time: '2026-10-31T00:00:00Z', elapsedMs: 1n }) },
      consoleSink: { error: () => undefined },
    })
    const scope = reporter.openScope('receive')
    const trigger = scope.facts.record(fact('root cause ' + 'x'.repeat(4000)), 'contributor')
    reporter.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', trigger))
    scope.close()
    expect(reporter.history.last()?.payload.trigger.evidence?.exception.message).toContain('root cause')
    expect(reporter.history.last()?.payload.evidence_details_omitted).toBeGreaterThan(0)
    scope.facts.record(fact('first cleanup'), 'consequence')
    scope.facts.record(fact('second cleanup'), 'consequence')
    const root = reporter.history.last()!
    const supplement = reporter.history.consequencesSnapshot()[0]!
    expect(reporter.history.snapshot()).toHaveLength(1)
    expect(root.payload.trigger.evidence?.exception.message).toContain('root cause')
    expect(supplement.consequences.map(bucket => bucket.representative.evidence?.exception.message))
      .toEqual(['first cleanup', 'second cleanup'])
    expect(new TextEncoder().encode(JSON.stringify(root)).byteLength).toBeLessThanOrEqual(3000)
    expect(new TextEncoder().encode(JSON.stringify(supplement)).byteLength).toBeLessThanOrEqual(3000)
  })
})
