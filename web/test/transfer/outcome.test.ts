import { describe, expect, it } from 'vitest'
import { directoryId, fileId } from '../../src/catalog/model'
import {
  FaultScope,
  OutputFaultCode,
  SourceFaultCode,
  outputFault,
  sourceFault,
} from '../../src/transfer/fault'
import type { Fault } from '../../src/transfer/fault'
import type { MaterializationFailureReason } from '../../src/transfer/job/contract'
import { normalizedV2FileTransferFault } from '../../src/transfer/job/failures'
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
    accumulator.record({
      kind: 'file',
      fileId: fileId(identityText(4)),
      classification: classification(
        sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable),
      ),
    }, 4n)
    expect(() => transferWorkerSettlement('Succeeded', accumulator.snapshot()))
      .toThrow(/cannot contain failures/)
  })

  it('retains bounded representatives while preserving exact count and trigger', () => {
    const accumulator = new TransferFailureAccumulator()
    for (let index = 0; index < MAXIMUM_RETAINED_TRANSFER_FAILURES + 3; index += 1) {
      accumulator.record({
        kind: 'directory',
        directoryId: directoryId(identityText((index % 200) + 1)),
        classification: classification(
          sourceFault(FaultScope.DirectoryLocal, SourceFaultCode.Unavailable),
        ),
      }, BigInt(index + 1))
    }
    const snapshot = accumulator.snapshot()
    expect(snapshot.failures).toHaveLength(MAXIMUM_RETAINED_TRANSFER_FAILURES)
    expect(snapshot.failureCount).toBe(MAXIMUM_RETAINED_TRANSFER_FAILURES + 3)
    expect(snapshot.omittedFailureCount).toBe(3)
    expect(snapshot.trigger?.fault).toEqual(
      sourceFault(FaultScope.DirectoryLocal, SourceFaultCode.Unavailable),
    )
  })

  it('nominates the same authority and lowest stable ordinal in every arrival order', () => {
    const lowOrdinal = classification(
      outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
      'output-write-failed',
    )
    const highOrdinal = classification(
      outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
      'output-commit-failed',
    )
    const lessSevere = classification(
      sourceFault(FaultScope.FileLocal, SourceFaultCode.Permanent),
    )
    const entries = [
      {
        failure: {
          kind: 'file' as const,
          fileId: fileId(identityText(9)),
          classification: highOrdinal,
        },
        ordinal: 9n,
      },
      {
        failure: {
          kind: 'file' as const,
          fileId: fileId(identityText(2)),
          classification: lowOrdinal,
        },
        ordinal: 2n,
      },
      {
        failure: {
          kind: 'file' as const,
          fileId: fileId(identityText(1)),
          classification: lessSevere,
        },
        ordinal: 1n,
      },
    ]

    for (const order of [entries, [...entries].reverse(), [entries[1]!, entries[2]!, entries[0]!]]) {
      const accumulator = new TransferFailureAccumulator()
      for (const entry of order) accumulator.record(entry.failure, entry.ordinal)
      expect(accumulator.snapshot().trigger).toBe(lowOrdinal)
    }
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
        classification: classification(
          sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable),
        ),
      }, BigInt(index), outcomes[index % outcomes.length])
    }
    const settlement = transferWorkerSettlement('CompletedWithErrors', accumulator.snapshot())
    expect(settlement.fileFailureCount).toBe(MAXIMUM_RETAINED_TRANSFER_FAILURES + outcomes.length)
    expect(Object.values(settlement.fileOutcomes).reduce((sum, count) => sum + count, 0))
      .toBe(settlement.fileFailureCount)
    expect(settlement.omittedFailureCount).toBe(outcomes.length)
  })
})

function classification(
  fault: Fault,
  materializationFailureReason: MaterializationFailureReason = 'content-read-failed',
) {
  return normalizedV2FileTransferFault(fault, {
    materializationFailureReason,
  }).diagnostic.classification
}
