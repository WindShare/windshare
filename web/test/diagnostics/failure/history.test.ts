import { describe, expect, it } from 'vitest'
import { FailureEvidenceHistory, MAX_FAILURE_OBSERVATIONS, MAX_FAILURE_OBSERVATION_BYTES } from '../../../src/diagnostics/failure/history'
import { FAILURE_STAGES, unclassifiedFailureFact, type FailureStage } from '../../../src/diagnostics/incident/fact'
import { createIncidentScopeIssuer, type FailureFactRelation } from '../../../src/diagnostics/incident/scope'

function fact(message: string, stage: FailureStage = 'content_read') {
  const error = new Error(message)
  error.stack = ''
  return unclassifiedFailureFact({ stage, recoveryDisposition: 'terminal', error, operation: 'read' })
}

function causes(history: FailureEvidenceHistory): string[] {
  return [...new Set(history.snapshot().records.map(row => row.fact.evidence?.exception.message ?? 'unknown'))]
}

describe('failure history cause admission', () => {
  it('shares cause capacity across relations while preserving their counts and latest scopes', () => {
    const history = new FailureEvidenceHistory()
    const scope = createIncidentScopeIssuer().open('receive')
    const record = (message: string, relation: FailureFactRelation) => {
      const value = fact(message)
      const ref = scope.facts.record(value, relation)
      history.record(value, relation, ref)
      return ref
    }
    try {
      record('A', 'contributor')
      record('B', 'contributor')
      record('C', 'contributor')
      record('C', 'consequence')
      record('D', 'contributor')
      const before = history.snapshot()
      const repeated = record('C', 'contributor')
      expect(causes(history).sort()).toEqual(['A', 'B', 'C', 'D'])
      expect(history.snapshot().records).toHaveLength(5)
      expect(history.snapshot().omitted).toBe('0')
      const roles = history.snapshot().records.filter(row => row.fact.evidence?.exception.message === 'C')
      expect(roles.map(row => [row.relation, row.count])).toEqual([['consequence', '1'], ['contributor', '2']])
      expect(roles[1]?.scope).toEqual({
        scope_kind: 'receive', scope_sequence: scope.identity.scopeSequence.toString(),
        fact_sequence: repeated.factSequence.toString(),
      })
      expect(before.records.find(row => row.fact.evidence?.exception.message === 'C' && row.relation === 'contributor')?.count).toBe('1')
    } finally { scope.close() }
  })

  it.each([undefined, 'contributor', 'consequence'] as const)('refreshes cause recency through relation %s', relation => {
    const history = new FailureEvidenceHistory()
    for (const message of ['A', 'B', 'C', 'D']) history.record(fact(message), 'contributor')
    history.record(fact('A'), relation)
    history.record(fact('E'), 'contributor')
    expect(causes(history).sort()).toEqual(['A', 'C', 'D', 'E'])
    expect(history.snapshot().omitted).toBe('1')
  })

  it('evicts every relation of the least recent cause and accounts all repeated observations', () => {
    const history = new FailureEvidenceHistory()
    for (let index = 0; index < 2; index++) history.record(fact('A'), 'contributor')
    for (let index = 0; index < 3; index++) history.record(fact('A'), 'consequence')
    for (const message of ['B', 'C', 'D', 'E']) history.record(fact(message), 'contributor')
    expect(causes(history)).toEqual(['B', 'C', 'D', 'E'])
    expect(history.snapshot().records).toHaveLength(4)
    expect(history.snapshot().omitted).toBe('5')
  })

  it('treats evidence-free observations as one cause across relations', () => {
    const history = new FailureEvidenceHistory()
    const unknown = unclassifiedFailureFact({ stage: 'content_read', recoveryDisposition: 'terminal' })
    history.record(unknown, 'contributor')
    history.record(unknown, 'consequence')
    for (const message of ['A', 'B', 'C']) history.record(fact(message), 'contributor')
    expect(causes(history)).toEqual(['unknown', 'A', 'B', 'C'])
    expect(history.snapshot().omitted).toBe('0')
    history.record(fact('D'), 'contributor')
    expect(causes(history)).toEqual(['A', 'B', 'C', 'D'])
    expect(history.snapshot().omitted).toBe('2')
  })

  it.each([0, 4000])('preserves global record and byte bounds with %i-byte messages and mixed relations', length => {
    const history = new FailureEvidenceHistory()
    for (const stage of FAILURE_STAGES) {
      const value = fact('failure' + 'x'.repeat(length), stage)
      history.record(value)
      history.record(value, 'contributor')
      history.record(value, 'consequence')
    }
    const snapshot = history.snapshot()
    expect(snapshot.records.length).toBeLessThanOrEqual(MAX_FAILURE_OBSERVATIONS)
    expect(snapshot.retainedBytes).toBeLessThanOrEqual(MAX_FAILURE_OBSERVATION_BYTES)
    expect(BigInt(snapshot.omitted) + snapshot.records.reduce((sum, row) => sum + BigInt(row.count), 0n))
      .toBe(BigInt(FAILURE_STAGES.length * 3))
    expect(BigInt(snapshot.omitted)).toBeGreaterThan(0n)
  })
})
