import { describe, expect, it } from 'vitest'
import { isFailureFact, unclassifiedFailureFact } from '../../../src/diagnostics/incident/fact'
import { MAX_INCIDENT_EXCEPTION_TEXT_BYTES } from '../../../src/diagnostics/incident/exception-evidence'

const context = { stage: 'authority_activation', recoveryDisposition: 'terminal' } as const

describe('unclassified exception evidence', () => {
  it('snapshots exception text without retaining the mutable thrown object', () => {
    const error = new Error('initial failure', { cause: new Error('initial cause') })
    error.stack = 'initial stack'
    const fact = unclassifiedFailureFact({ ...context, error })
    error.message = 'changed'
    error.stack = 'changed'
    expect(isFailureFact(fact)).toBe(true)
    expect(fact.payload.unclassified.exception).toMatchObject({
      errorName: 'Error', message: 'initial failure', stack: 'initial stack', cause: 'Error: initial cause',
    })
    expect(Object.isFrozen(fact.payload.unclassified.exception)).toBe(true)
  })

  it('bounds multibyte message and stack evidence before retention', () => {
    const error = new TypeError('错误😀'.repeat(MAX_INCIDENT_EXCEPTION_TEXT_BYTES))
    error.stack = error.message
    const fact = unclassifiedFailureFact({ ...context, error })
    const evidence = fact.payload.unclassified.exception!
    for (const text of [evidence.message!, evidence.stack!]) {
      expect(new TextEncoder().encode(text).byteLength).toBeLessThanOrEqual(MAX_INCIDENT_EXCEPTION_TEXT_BYTES)
      expect(text).not.toContain('\uFFFD')
      expect(text.length).toBeGreaterThan(0)
    }
    expect(isFailureFact(fact)).toBe(true)
  })

  it('distinguishes absent evidence from a thrown undefined value', () => {
    expect(unclassifiedFailureFact(context).payload.unclassified.exception).toBeNull()
    expect(unclassifiedFailureFact({ ...context, error: undefined }).payload.unclassified.exception)
      .toMatchObject({ thrownType: 'undefined', thrownValue: 'undefined' })
  })

  it('captures hostile thrown values without affecting product control flow', () => {
    const error = new Proxy({}, { get() { throw new Error('unreadable property') } })
    const fact = unclassifiedFailureFact({ ...context, error })
    expect(isFailureFact(fact)).toBe(true)
    expect(fact.payload.unclassified.exception).toMatchObject({
      thrownType: 'object', thrownValue: '[unprintable thrown value]',
    })
  })

  it('rejects forged, unbounded or mutable retained evidence', () => {
    const fact = unclassifiedFailureFact({ ...context, error: new Error('failure') })
    for (const exception of [
      { ...fact.payload.unclassified.exception },
      Object.freeze({ ...fact.payload.unclassified.exception, message: 'x'.repeat(MAX_INCIDENT_EXCEPTION_TEXT_BYTES + 1) }),
      Object.freeze({ ...fact.payload.unclassified.exception, thrownType: 'invented' }),
      Object.freeze({ ...fact.payload.unclassified.exception, raw: new Error() }),
    ]) {
      const forged = Object.freeze({ ...fact, payload: Object.freeze({
        unclassified: Object.freeze({ exception }),
      }) })
      expect(isFailureFact(forged)).toBe(false)
    }
  })
})
