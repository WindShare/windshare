import { describe, expect, it } from 'vitest'

import {
  bindTransferExecutionLimits,
  transferJobLimits,
  type TransferExecutionLimits,
} from '../../src/transfer/job/limits'
import { OutputResourceBudget } from '../../src/transfer/job/scheduler'
import {
  OutputBudgetExceededError,
  disabledOutputExecutionProfile,
  type OutputExecutionProfile,
} from '../../src/transfer/output-session'
import type { TransferJobOptions } from '../../src/transfer/job/contract'

describe('late output execution-profile binding', () => {
  it('uses the output profile when the caller omits a bound', () => {
    const limits = transferJobLimits({} as TransferJobOptions)

    expect(bindTransferExecutionLimits(limits, disabledOutputExecutionProfile(8)))
      .toMatchObject({ concurrentFiles: 8 })
  })

  it('uses a smaller explicit caller bound', () => {
    const limits = transferJobLimits({ maximumConcurrentFiles: 3 } as TransferJobOptions)

    expect(bindTransferExecutionLimits(limits, disabledOutputExecutionProfile(8)))
      .toMatchObject({ concurrentFiles: 3 })
  })

  it('rejects caller and output limits above the absolute safety ceiling', () => {
    expect(() => transferJobLimits({ maximumConcurrentFiles: 33 } as TransferJobOptions))
      .toThrow(RangeError)
    const oversized: OutputExecutionProfile = {
      ...disabledOutputExecutionProfile(1),
      maximumConcurrentFilePipelines: 33,
    }

    expect(() => bindTransferExecutionLimits(
      transferJobLimits({} as TransferJobOptions),
      oversized,
    )).toThrow(/invalid concurrent file-pipeline limit/)
  })
})

describe('output resource budgets', () => {
  it('bounds active files and releases queued admission progressively', async () => {
    const budget = new OutputResourceBudget(resourceLimits({ concurrentFiles: 2 }))
    const signal = new AbortController().signal
    const first = await budget.acquireFile(signal)
    const second = await budget.acquireFile(signal)
    let thirdAdmitted = false
    const third = budget.acquireFile(signal).then((lease) => {
      thirdAdmitted = true
      return lease
    })

    await Promise.resolve()
    expect(thirdAdmitted).toBe(false)
    expect(budget.snapshot()).toMatchObject({
      activeFiles: 2,
      peakActiveFiles: 2,
      queuedAdmissions: 1,
    })

    first.release()
    const thirdLease = await third
    expect(budget.snapshot().activeFiles).toBe(2)
    second.release()
    thirdLease.release()
    expect(budget.snapshot().activeFiles).toBe(0)
  })

  it('charges outstanding writes and JS buffers against independent byte ceilings', async () => {
    const budget = new OutputResourceBudget(resourceLimits({
      maximumOutstandingWriteBytes: 4n,
      maximumBufferedBytes: 5n,
    }))
    const signal = new AbortController().signal
    const firstStarted = deferred()
    const releaseFirst = deferred()
    const secondStarted = deferred()
    const first = budget.runWrite(3n, signal, async () => {
      firstStarted.resolve()
      await releaseFirst.promise
    })
    await firstStarted.promise
    const second = budget.runWrite(2n, signal, async () => { secondStarted.resolve() })

    await Promise.resolve()
    expect(budget.snapshot()).toMatchObject({
      outstandingWriteBytes: 3n,
      bufferedBytes: 3n,
      queuedAdmissions: 1,
    })
    releaseFirst.resolve()
    await secondStarted.promise
    await Promise.all([first, second])

    expect(budget.snapshot()).toMatchObject({
      outstandingWriteBytes: 0n,
      bufferedBytes: 0n,
      peakOutstandingWriteBytes: 3n,
      peakBufferedBytes: 3n,
    })
    await expect(budget.runWrite(6n, signal, async () => undefined))
      .rejects.toBeInstanceOf(OutputBudgetExceededError)
  })
})

function resourceLimits(overrides: Partial<TransferExecutionLimits>): TransferExecutionLimits {
  return Object.freeze({
    concurrentFiles: 2,
    maximumOutstandingWriteBytes: 8n,
    maximumBufferedBytes: 8n,
    ...overrides,
  })
}

function deferred(): Readonly<{ promise: Promise<void>; resolve: () => void }> {
  let resolve!: () => void
  const promise = new Promise<void>((complete) => { resolve = complete })
  return { promise, resolve }
}
