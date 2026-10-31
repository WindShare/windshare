import { describe, expect, it } from 'vitest'
import { faultFailureFact, isFailureFact, unclassifiedFailureFact } from '../../../src/diagnostics/incident/fact'
import { MAX_EXCEPTION_BYTES, MAX_EXCEPTION_TEXT_BYTES, projectDiagnosticException } from '../../../src/diagnostics/exception'
import { captureFailureEvidence } from '../../../src/diagnostics/failure/evidence'
import { catalogFault, CatalogFaultCode, FaultScope } from '../../../src/transfer/failure/fault'

const context = { stage: 'authority_activation', recoveryDisposition: 'terminal' } as const

describe('classification-independent exception evidence', () => {
  it('retains immutable messages, stacks and cause branches on typed facts', () => {
    const error = new Error('initial failure', { cause: new Error('initial cause') })
    error.stack = 'initial stack'
    const fact = faultFailureFact({ ...context,
      fault: catalogFault(FaultScope.SessionTerminal, CatalogFaultCode.InvalidGeneration), error,
      operation: 'catalog.accept', context: { directory: 'directory-a', expectedPage: 2, actualPage: 3 },
    })
    error.message = 'changed'
    error.stack = 'changed'
    expect(isFailureFact(fact)).toBe(true)
    expect(fact.kind).toBe('fault')
    expect(fact.evidence).toMatchObject({ operation: 'catalog.accept', context: { expectedPage: 2, actualPage: 3 },
      exception: { errorName: 'Error', message: 'initial failure', stack: 'initial stack', cause: { message: 'initial cause' } },
    })
    expect(Object.isFrozen(fact.evidence!.exception.cause)).toBe(true)
  })

  it('bounds escaped multibyte text and the entire exception graph', () => {
    const large = '\u754c\u{1f680}\u0000'.repeat(MAX_EXCEPTION_TEXT_BYTES)
    let error: Error = new Error(large)
    for (let index = 0; index < 12; index++) error = new AggregateError(Array(20).fill(error), large, { cause: error })
    error.stack = large
    const snapshot = projectDiagnosticException(error)
    expect(new TextEncoder().encode(JSON.stringify(snapshot)).byteLength).toBeLessThanOrEqual(MAX_EXCEPTION_BYTES)
    expect(snapshot.omittedBranches).toBeGreaterThan(0)
    expect(snapshot.truncated.length).toBeGreaterThan(0)
    expect(snapshot.message).not.toContain('\uFFFD')
  })

  it('distinguishes unavailable evidence, thrown undefined, cycles and inaccessible properties', () => {
    expect(unclassifiedFailureFact(context).evidence).toBeUndefined()
    expect(unclassifiedFailureFact({ ...context, error: undefined }).evidence!.exception)
      .toMatchObject({ thrownType: 'undefined', thrownValue: 'undefined' })
    const error = new Error('cycle')
    error.cause = error
    const snapshot = projectDiagnosticException(error)
    expect(snapshot.truncated).toContain('cause:cycle')
    const hostile = new Proxy({}, { get() { throw new Error('unreadable') } })
    expect(projectDiagnosticException(hostile)).toMatchObject({
      thrownType: 'object', thrownValue: '[unprintable thrown value]',
    })
    expect(projectDiagnosticException(hostile).unavailable).toContain('cause')
  })

  it('reuses originating evidence through wrappers and aggregate branches', () => {
    const cause = new Error('disk permission denied')
    const outputSnapshot = projectDiagnosticException(cause)
    const original = captureFailureEvidence(cause, { operation: 'output.write' })
    expect(original.exception).toBe(outputSnapshot)
    const wrapper = new Error('failed to publish', { cause })
    expect(captureFailureEvidence(wrapper, { operation: 'transfer' })).toBe(original)
    const aggregate = captureFailureEvidence(new AggregateError([wrapper, new Error('cleanup failed')], 'settlement failed'), { operation: 'settlement' })
    expect(aggregate.exception.errors[0]).toBe(original.exception)
    expect(aggregate.exception.errors[1]?.message).toBe('cleanup failed')
  })

  it('groups diagnostic causes independently of occurrence identities and exception ordering', () => {
    const first = captureFailureEvidence(new Error('same cause'), { operation: 'read', context: { lane: 1 } })
    const second = captureFailureEvidence(new Error('same cause'), { operation: 'read', context: { lane: 9 } })
    const different = captureFailureEvidence(new Error('different cause'), { operation: 'read' })
    expect(first.signature).toBe(second.signature)
    expect(first.signature).not.toBe(different.signature)
    const aggregate = (errors: string[]) => captureFailureEvidence(
      new AggregateError(errors.map(message => new Error(message)), 'branches'), { operation: 'read' },
    ).signature
    expect(aggregate(['a', 'b'])).toBe(aggregate(['b', 'a']))
  })

  it('rejects forged or mutable evidence independently of the classification payload', () => {
    const fact = unclassifiedFailureFact({ ...context, error: new Error('failure') })
    expect(isFailureFact(Object.freeze({ ...fact, evidence: { ...fact.evidence } }))).toBe(false)
    expect(isFailureFact(Object.freeze({ ...fact, evidence: Object.freeze({ ...fact.evidence, raw: new Error() }) }))).toBe(false)
  })
})
