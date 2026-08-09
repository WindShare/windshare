import { describe, expect, it } from 'vitest'
import { directoryId, fileId } from '../../src/catalog/model'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  MAXIMUM_RETAINED_TRANSFER_FAILURES,
  TransferFailureAccumulator,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import { identityText } from './v2-job-fixture'

describe('transfer worker settlement', () => {
  it('keeps worker completion separate from plan lifecycle', () => {
    expect(transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)).toEqual({
      status: 'Succeeded',
      failures: [],
      failureCount: 0,
      omittedFailureCount: 0,
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
})
