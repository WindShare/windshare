import { expect, it } from 'vitest'
import { summarizeFailureFact } from '../../../src/diagnostics/failure/summary'
import { projectFailureFactV2 } from '../../../src/diagnostics/export/projector'
import { unclassifiedFailureFact } from '../../../src/diagnostics/incident/fact'

it('charges only the first reduction and preserves the summary through later byte budgets', () => {
  const fact = projectFailureFactV2(unclassifiedFailureFact({
    stage: 'content_read', recoveryDisposition: 'terminal',
    error: new Error('read failed', { cause: new Error('connection lost') }),
    operation: 'read', context: { lane: 7 },
  }))
  const first = summarizeFailureFact(fact)
  expect(first.evidenceDetailsOmitted).toBe(1)
  expect(first.fact.evidence?.exception.message).toContain('connection lost')
  expect(first.fact.evidence?.exception.cause).toBeNull()
  expect(first.fact.evidence?.context).toEqual({})
  expect(first.fact.evidence?.id).toBe(fact.evidence?.id)
  const second = summarizeFailureFact(first.fact)
  expect(second.evidenceDetailsOmitted).toBe(0)
  expect(second.fact).toBe(first.fact)
  expect(fact.evidence?.exception.cause?.message).toBe('connection lost')
  expect(fact.evidence?.context).toEqual({ lane: 7 })
})

it('does not count facts that carry no exception evidence', () => {
  const fact = projectFailureFactV2(unclassifiedFailureFact({ stage: 'content_read', recoveryDisposition: 'terminal' }))
  const summary = summarizeFailureFact(fact)
  expect(summary.evidenceDetailsOmitted).toBe(0)
  expect(summary.fact).toBe(fact)
})
