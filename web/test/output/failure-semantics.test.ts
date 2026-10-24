import { describe, expect, it } from 'vitest'
import { OriginCapacityDataError } from '../../src/output/origin-private/capacity/errors'
import { NativeOutputInitializationError } from '../../src/output/origin-private/native-object/errors'
import { CheckpointLineageDecisionError, DestinationCollisionError, SourceRevisionChangedError } from '../../src/output/persistent-tree/errors'
import { declaredOutputFailure } from '../../src/transfer/failure/output'
import { normalizeV2FileTransferFailure, transferFileOutcomeEvidence } from '../../src/transfer/job/failures'

describe('output boundary semantics', () => {
  it('retains native exception identity, cause, diagnostic stage and retryability', () => {
    const cause = new Error('Worker script could not be loaded')
    const startup = new NativeOutputInitializationError(cause)
    expect(startup.cause).toBe(cause)
    const normalized = normalizeV2FileTransferFailure(startup)
    expect(normalized).toMatchObject({
      fault: { domain: 'output', scope: 'output-pause', code: 'state-io' },
      fact: { stage: 'output_initialization', recoveryDisposition: 'retryable' },
    })
    expect(JSON.stringify(declaredOutputFailure(startup))).toContain(cause.message)

    const detail = 'store=workspace-budget-claims id=operation field=occupiedBytes: expected u64 bigint; actual=NaN'
    const capacity = new OriginCapacityDataError(detail)
    expect(capacity).toBeInstanceOf(DOMException)
    expect(normalizeV2FileTransferFailure(capacity)).toMatchObject({
      fault: { domain: 'output', scope: 'output-pause', code: 'state-io' },
      fact: { stage: 'output_reservation', recoveryDisposition: 'resumable_receive' },
    })
    expect(JSON.stringify(declaredOutputFailure(capacity))).toContain(detail)
  })

  it.each([
    ['revision-conflict', 'unsafe-install'],
    ['ownership-conflict', 'ownership-mismatch'],
    ['invalid', 'corrupt-record'],
  ] as const)('maps checkpoint %s before it reaches transfer', (decision, code) => {
    const error = new CheckpointLineageDecisionError(decision)
    expect(normalizeV2FileTransferFailure(error)).toMatchObject({
      fault: { domain: 'checkpoint', scope: 'file-local', code },
      fact: { stage: 'output_write', recoveryDisposition: 'none' },
    })
    expect(transferFileOutcomeEvidence(error)).toEqual({ kind: 'checkpoint-decision', decision })
  })

  it('preserves collision and source-drift evidence through contextual causes', () => {
    const collision = new DestinationCollisionError()
    const drift = new SourceRevisionChangedError()
    expect(normalizeV2FileTransferFailure(collision)).toMatchObject({
      fault: { domain: 'output', scope: 'file-local', code: 'namespace-unsafe' },
    })
    const wrapped = new Error('file admission failed', { cause: drift })
    expect(normalizeV2FileTransferFailure(wrapped)).toMatchObject({
      fault: { domain: 'source', scope: 'file-local', code: 'revision-changed' },
    })
    expect(transferFileOutcomeEvidence(wrapped)).toEqual({ kind: 'authenticated-source-drift' })
    expect(transferFileOutcomeEvidence(collision)).toEqual({ kind: 'occupied-unbound-destination' })
  })
})
