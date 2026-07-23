import { describe, expect, it } from 'vitest'

import { directoryId, fileId } from '../../src/catalog/model'
import {
  MAXIMUM_RETAINED_TRANSFER_FAILURES,
  TransferFailureAccumulator,
  jobOutcome,
} from '../../src/transfer/outcome'

const MILLION_FAILURE_OBSERVATIONS = 1_000_000

describe('bounded transfer failure evidence', () => {
  it('retains bounded details with exact million-scale total and omitted counts', () => {
    const failures = new TransferFailureAccumulator()
    const repeatedFile = fileId('failed-file')
    for (let index = 0; index < MILLION_FAILURE_OBSERVATIONS - 1; index += 1) {
      failures.record({ kind: 'file', fileId: repeatedFile, reason: index })
    }
    failures.record({
      kind: 'directory',
      directoryId: directoryId('failed-directory'),
      reason: new Error('terminal directory failure'),
    })

    const summary = failures.snapshot()
    expect(summary.failures).toHaveLength(MAXIMUM_RETAINED_TRANSFER_FAILURES)
    expect(summary.failureCount).toBe(MILLION_FAILURE_OBSERVATIONS)
    expect(summary.omittedFailureCount).toBe(
      MILLION_FAILURE_OBSERVATIONS - MAXIMUM_RETAINED_TRANSFER_FAILURES,
    )
    expect(failures.hasDirectoryFailures).toBe(true)
    expect(jobOutcome('CompletedWithErrors', summary)).toMatchObject({
      status: 'CompletedWithErrors',
      failureCount: MILLION_FAILURE_OBSERVATIONS,
      omittedFailureCount: MILLION_FAILURE_OBSERVATIONS - MAXIMUM_RETAINED_TRANSFER_FAILURES,
    })
  })
})
