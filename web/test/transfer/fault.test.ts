import { describe, expect, it } from 'vitest'

import {
  BoundaryFaultError,
  CatalogFaultCode,
  CheckpointFaultCode,
  FaultDomain,
  FaultRetirement,
  FaultScope,
  OutputFaultCode,
  SessionFaultCode,
  SourceFaultCode,
  authorizeFileRetirement,
  catalogFault,
  checkpointFault,
  compareFaults,
  consumeFileRetirementAuthorization,
  dependencyContractFault,
  faultRequiresAttention,
  isFault,
  joinFaults,
  normalizeBoundaryError,
  outputFault,
  promoteFaultScope,
  sessionFault,
  sourceFault,
  type Fault,
} from '../../src/transfer/fault'

describe('closed transfer fault values', () => {
  it('validates and freezes every domain, scope, and domain-specific code', () => {
    const scopes = Object.values(FaultScope)
    const values = scopes.flatMap(scope => [
      ...Object.values(SourceFaultCode).map(code => sourceFault(scope, code)),
      ...Object.values(CatalogFaultCode).map(code => catalogFault(scope, code)),
      ...Object.values(SessionFaultCode).map(code => sessionFault(scope, code)),
      ...Object.values(OutputFaultCode).map(code => outputFault(scope, code)),
      ...Object.values(CheckpointFaultCode).map(code => checkpointFault(scope, code)),
    ])

    for (const fault of values) {
      expect(isFault(fault)).toBe(true)
      expect(Object.isFrozen(fault)).toBe(true)
      expect(scopes).toContain(fault.scope)
    }
    expect(new Set(values.map(fault => fault.domain))).toEqual(new Set(Object.values(FaultDomain)))
    expect(isFault({ domain: FaultDomain.Source, scope: 'unknown', code: SourceFaultCode.Permanent })).toBe(false)
    expect(isFault({ domain: FaultDomain.Source, scope: FaultScope.FileLocal, code: 'unknown' })).toBe(false)
    expect(isFault({ domain: 'unknown', scope: FaultScope.FileLocal, code: SourceFaultCode.Permanent })).toBe(false)
    expect(() => sourceFault('unknown' as FaultScope, SourceFaultCode.Permanent)).toThrow(TypeError)
    expect(() => joinFaults({} as Fault)).toThrow(TypeError)
  })

  it('joins permutations by severity followed by stable domain and code order', () => {
    const file = outputFault(FaultScope.FileLocal, OutputFaultCode.Contract)
    const directory = catalogFault(FaultScope.DirectoryLocal, CatalogFaultCode.InvalidGeneration)
    const output = checkpointFault(FaultScope.OutputPause, CheckpointFaultCode.StateIO)
    const terminal = sourceFault(FaultScope.SessionTerminal, SourceFaultCode.Unavailable)
    for (const permutation of permutations([file, directory, output, terminal])) {
      expect(joinFaults(...permutation)).toEqual(terminal)
    }

    const sourceTie = sourceFault(FaultScope.OutputPause, SourceFaultCode.Permanent)
    const checkpointTie = checkpointFault(FaultScope.OutputPause, CheckpointFaultCode.Busy)
    expect(joinFaults(sourceTie, checkpointTie)).toEqual(checkpointTie)
    const lowCode = checkpointFault(FaultScope.OutputPause, CheckpointFaultCode.Busy)
    const highCode = checkpointFault(FaultScope.OutputPause, CheckpointFaultCode.StateIO)
    expect(joinFaults(highCode, lowCode)).toEqual(highCode)
    expect(joinFaults()).toBeUndefined()
    expect(joinFaults(file, file)).toEqual(file)
    expect(joinFaults(joinFaults(file, output)!, terminal)).toEqual(
      joinFaults(file, joinFaults(output, terminal)!),
    )
    expect(compareFaults(file, output)).toBeLessThan(0)
    expect(compareFaults(output, file)).toBeGreaterThan(0)
    expect(compareFaults(file, file)).toBe(0)
  })
})

describe('fault boundary normalization', () => {
  it('excludes cancellation and defaults unknown errors to dependency-contract output pause', () => {
    const normalized = checkpointFault(FaultScope.OutputPause, CheckpointFaultCode.CorruptRecord)
    const diagnostic = new Error('native checkpoint decoder detail')
    const typed = new BoundaryFaultError(normalized, undefined, { cause: diagnostic })
    expect(normalizeBoundaryError(typed)).toEqual({ kind: 'fault', fault: normalized })
    expect(typed.cause).toBe(diagnostic)

    expect(normalizeBoundaryError(new Error('untyped collaborator failure'))).toEqual({
      kind: 'fault',
      fault: dependencyContractFault(),
    })
    expect(normalizeBoundaryError(undefined)).toEqual({ kind: 'success' })

    const canceled = new AbortController()
    canceled.abort(new DOMException('Canceled', 'AbortError'))
    expect(normalizeBoundaryError(typed, canceled.signal)).toEqual({ kind: 'canceled' })
    expect(normalizeBoundaryError(undefined, canceled.signal)).toEqual({ kind: 'canceled' })
    expect(normalizeBoundaryError(new DOMException('Canceled', 'AbortError'))).toEqual({ kind: 'canceled' })
    expect(normalizeBoundaryError(new DOMException('Timed out', 'TimeoutError'))).toEqual({ kind: 'canceled' })
    expect(normalizeBoundaryError({ name: 'AbortError' })).toEqual({
      kind: 'fault',
      fault: dependencyContractFault(),
    })
  })

  it('authorizes retirement only for the explicit file-local source allowlist', () => {
    const permanent = authorizeFileRetirement(
      sourceFault(FaultScope.FileLocal, SourceFaultCode.Permanent),
    )
    const invalidated = authorizeFileRetirement(
      sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionInvalidated),
    )
    expect(permanent?.retirement).toBe(FaultRetirement.PermanentSource)
    expect(invalidated?.retirement).toBe(FaultRetirement.InvalidatedRevision)
    expect(consumeFileRetirementAuthorization(permanent)).toBe(FaultRetirement.PermanentSource)
    expect(() => consumeFileRetirementAuthorization(permanent)).toThrow(/already consumed/u)
    expect(() => consumeFileRetirementAuthorization({
      retirement: FaultRetirement.PermanentSource,
    })).toThrow(/allowlisted authorization/u)

    const notAuthorized = [
      sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable),
      sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged),
      sourceFault(FaultScope.OutputPause, SourceFaultCode.Permanent),
      dependencyContractFault(),
    ]
    for (const fault of notAuthorized) expect(authorizeFileRetirement(fault)).toBeUndefined()
  })

  it('promotes preservation faults and marks only ownership ambiguity for attention', () => {
    const transient = sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable)
    expect(promoteFaultScope(transient, FaultScope.OutputPause)).toEqual(
      sourceFault(FaultScope.OutputPause, SourceFaultCode.Unavailable),
    )
    expect(promoteFaultScope(transient, FaultScope.FileLocal)).toEqual(transient)

    expect(faultRequiresAttention(checkpointFault(
      FaultScope.OutputPause,
      CheckpointFaultCode.OwnershipMismatch,
    ))).toBe(true)
    expect(faultRequiresAttention(outputFault(
      FaultScope.OutputPause,
      OutputFaultCode.MutationAmbiguous,
    ))).toBe(true)
    expect(faultRequiresAttention(dependencyContractFault())).toBe(false)
  })
})

function permutations<Value>(values: readonly Value[]): Value[][] {
  if (values.length === 0) return [[]]
  return values.flatMap((value, index) => {
    const remainder = [...values.slice(0, index), ...values.slice(index + 1)]
    return permutations(remainder).map(suffix => [value, ...suffix])
  })
}
