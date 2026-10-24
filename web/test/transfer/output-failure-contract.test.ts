import { describe, expect, it } from 'vitest'
import { faultFailureFact } from '../../src/diagnostics/incident'
import { BoundaryFaultError, FaultScope, OutputFaultCode, dependencyContractFault, outputFault } from '../../src/transfer/failure/fault'
import { OUTPUT_FAILURE, declareOutputFailure, declaredOutputFailure } from '../../src/transfer/failure/output'
import { normalizeV2FileTransferFailure, transferFileOutcomeEvidence, V2FileOutputError } from '../../src/transfer/job/failures'

function unfamiliarBackendFailure(): Error {
  const fault = outputFault(FaultScope.FileLocal, OutputFaultCode.NamespaceUnsafe)
  return Object.assign(new Error('backend-private exception'), {
    [OUTPUT_FAILURE]: declareOutputFailure(Object.freeze({
      fault,
      fact: faultFailureFact({ fault, stage: 'output_write', recoveryDisposition: 'none' }),
      materializationFailureReason: 'output-write-failed',
    }), { kind: 'occupied-unbound-destination' }),
  })
}

describe('output failure contract', () => {
  it('accepts a new backend without knowing its exception class', () => {
    const error = unfamiliarBackendFailure()
    const normalized = normalizeV2FileTransferFailure(error)
    expect(normalized).toMatchObject({
      kind: 'fault',
      fault: { domain: 'output', scope: 'file-local', code: 'namespace-unsafe' },
      fact: { stage: 'output_write', recoveryDisposition: 'none' },
      materializationFailureReason: 'output-write-failed',
    })
    if (normalized.kind !== 'fault') return
    expect(transferFileOutcomeEvidence(normalized.diagnostic)).toEqual({ kind: 'occupied-unbound-destination' })
    expect(normalizeV2FileTransferFailure(normalized.diagnostic)).toMatchObject({ fact: normalized.fact })
    expect(Object.isFrozen(declaredOutputFailure(error)?.classification)).toBe(true)
  })

  it('retains backend evidence through a write wrapper while respecting the caller operation', () => {
    const error = new V2FileOutputError('commit failed', 'output-commit-failed', unfamiliarBackendFailure())
    expect(normalizeV2FileTransferFailure(error)).toMatchObject({
      kind: 'fault',
      fault: { scope: 'file-local', code: 'namespace-unsafe' },
      fact: { stage: 'output_commit', recoveryDisposition: 'none' },
      materializationFailureReason: 'output-commit-failed',
    })
    expect(normalizeV2FileTransferFailure(error, { stage: 'settlement' })).toMatchObject({
      fact: { stage: 'settlement' },
    })
  })

  it('keeps explicit boundary authority ahead of nested backend facts', () => {
    const fault = outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous)
    const error = new BoundaryFaultError(fault)
    Object.defineProperty(error, 'cause', { value: unfamiliarBackendFailure() })
    expect(normalizeV2FileTransferFailure(error)).toMatchObject({
      fault,
      fact: { recoveryDisposition: 'resumable_receive' },
    })
  })

  it('honors cancellation and rejects unreviewed declarations and name matching', () => {
    const controller = new AbortController()
    controller.abort(new Error('receiver paused'))
    expect(normalizeV2FileTransferFailure(unfamiliarBackendFailure(), { signal: controller.signal }))
      .toMatchObject({ kind: 'canceled', diagnostic: controller.signal.reason })
    const named = Object.assign(new Error('not an authority'), { name: 'NativeOutputInitializationError' })
    const malformed = Object.assign(new Error('invalid contract'), { [OUTPUT_FAILURE]: { classification: {} } })
    for (const error of [named, malformed]) {
      expect(normalizeV2FileTransferFailure(error)).toMatchObject({ fault: dependencyContractFault() })
    }
  })
})
