import { describe, expect, it } from 'vitest'
import { directoryId, fileId } from '../../src/catalog/model'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  MAXIMUM_RETAINED_TRANSFER_FAILURES,
  TransferFailureAccumulator,
  projectTransferFileOutcome,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import { identityText } from './v2-job-fixture'

describe('transfer worker settlement', () => {
  it('keeps worker completion separate from plan lifecycle', () => {
    expect(transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)).toEqual({
      status: 'Succeeded',
      failures: [],
      failureCount: 0,
      fileFailureCount: 0,
      omittedFailureCount: 0,
      fileOutcomes: {
        sourceDriftFiles: 0,
        revisionConflictFiles: 0,
        checkpointInvalidFiles: 0,
        ownedObjectUnknownFiles: 0,
        collisionFiles: 0,
        failedFiles: 0,
      },
    })
    expect(transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY).status).toBe('Paused')
  })

  it('rejects complete-with-errors without evidence and success with evidence', () => {
    expect(() => transferWorkerSettlement('CompletedWithErrors', EMPTY_TRANSFER_FAILURE_SUMMARY))
      .toThrow(/require failure evidence/)
    const accumulator = new TransferFailureAccumulator()
    accumulator.record({ kind: 'file', fileId: fileId(identityText(4)), reason: new Error('x') })
    expect(() => transferWorkerSettlement('Succeeded', accumulator.snapshot()))
      .toThrow(/cannot contain failures/)
  })

  it('retains bounded diagnostics while preserving the exact count', () => {
    const accumulator = new TransferFailureAccumulator()
    for (let index = 0; index < MAXIMUM_RETAINED_TRANSFER_FAILURES + 3; index += 1) {
      accumulator.record({
        kind: 'directory',
        directoryId: directoryId(identityText((index % 200) + 1)),
        reason: index,
      })
    }
    const snapshot = accumulator.snapshot()
    expect(snapshot.failures).toHaveLength(MAXIMUM_RETAINED_TRANSFER_FAILURES)
    expect(snapshot.failureCount).toBe(MAXIMUM_RETAINED_TRANSFER_FAILURES + 3)
    expect(snapshot.omittedFailureCount).toBe(3)
  })

  it.each([
    [{ kind: 'authenticated-source-drift' }, 'source-drift'],
    [{ kind: 'checkpoint-decision', decision: 'revision-conflict' }, 'revision-conflict'],
    [{ kind: 'checkpoint-decision', decision: 'ownership-conflict' }, 'owned-object-unknown'],
    [{ kind: 'checkpoint-decision', decision: 'invalid' }, 'checkpoint-invalid'],
    [{ kind: 'occupied-unbound-destination' }, 'destination-collision'],
    [{ kind: 'residual-failure' }, 'failed'],
  ] as const)('projects %o to %s', (evidence, expected) => {
    expect(projectTransferFileOutcome(evidence)).toBe(expected)
  })

  it('keeps exact outcome counts independent of bounded diagnostics', () => {
    const accumulator = new TransferFailureAccumulator()
    const outcomes = [
      'source-drift',
      'revision-conflict',
      'checkpoint-invalid',
      'owned-object-unknown',
      'destination-collision',
      'failed',
    ] as const
    for (let index = 0; index < MAXIMUM_RETAINED_TRANSFER_FAILURES + outcomes.length; index += 1) {
      accumulator.record({
        kind: 'file',
        fileId: fileId(identityText((index % 200) + 1)),
        reason: new Error('bounded diagnostic'),
      }, outcomes[index % outcomes.length])
    }
    const settlement = transferWorkerSettlement('CompletedWithErrors', accumulator.snapshot())
    expect(settlement.fileFailureCount).toBe(MAXIMUM_RETAINED_TRANSFER_FAILURES + outcomes.length)
    expect(Object.values(settlement.fileOutcomes).reduce((sum, count) => sum + count, 0))
      .toBe(settlement.fileFailureCount)
    expect(settlement.omittedFailureCount).toBe(outcomes.length)
  })
})
