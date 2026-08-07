import { describe, expect, it, vi } from 'vitest'

import type { V2CatalogClient } from '../../src/catalog/v2-client'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { V2RevisionReader } from '../../src/content/v2-session-services'
import type { TransferTraceEvent, TransferTraceEventName } from '../../src/transfer/intent'
import type { OutputSession } from '../../src/transfer/output-session'
import {
  TransferJob,
  V2OutputSettlementTimeoutError,
} from '../../src/transfer/v2-job'
import { settleFailedV2Output } from '../../src/transfer/settlement/v2-output'
import {
  identity,
  identityText,
  outputAuthority,
  terminalBoundaryOutput,
  withTimeout,
} from './v2-job-fixture'

describe('v2 output settlement evidence', () => {
  it('reports discarded output only after failed retention falls back to verified cleanup', async () => {
    const retentionFailure = new Error('retention failed')
    const events: string[] = []
    const session: OutputSession = {
      ...terminalBoundaryOutput(),
      suspendJob: async () => {
        events.push('suspend')
        throw retentionFailure
      },
      abortJob: async () => { events.push('abort') },
    }

    const result = await failingJob(session).run()

    expect(result.outcome.status).toBe('Aborted')
    expect(result.settlement).toEqual({ kind: 'Discarded', retentionFailure })
    expect(events).toEqual(['suspend', 'abort'])
  })

  it('surfaces ambiguous output ownership when both retention and cleanup fail', async () => {
    const retentionFailure = new Error('retention failed')
    const cleanupFailure = new Error('cleanup failed')
    const traces: TransferTraceEvent[] = []
    const session: OutputSession = {
      ...terminalBoundaryOutput(),
      suspendJob: vi.fn(async () => { throw retentionFailure }),
      abortJob: vi.fn(async () => { throw cleanupFailure }),
    }

    const result = await failingJob(session, (event) => traces.push(event)).run()

    expect(result.outcome.status).toBe('NeedsAttention')
    expect(result.settlement).toEqual({
      kind: 'NeedsAttention',
      failure: { requested: 'retain', retentionFailure, cleanupFailure },
    })
    expect(traceNames(traces)).toContain('job-needs-attention')
  })

  it('bounds a collaborator that never returns without racing cleanup against it', async () => {
    const abortJob = vi.fn(async () => undefined)
    const session: OutputSession = {
      ...terminalBoundaryOutput(),
      suspendJob: () => new Promise<void>(() => undefined),
      abortJob,
    }

    const result = await withTimeout(
      failingJob(session, undefined, 10).run(),
      500,
      'terminal output settlement exceeded its external test bound',
    )

    expect(result.outcome.status).toBe('NeedsAttention')
    expect(result.settlement.kind).toBe('NeedsAttention')
    if (result.settlement.kind !== 'NeedsAttention') throw new Error('test settlement lost its discriminant')
    expect(result.settlement.failure.retentionFailure).toBeInstanceOf(V2OutputSettlementTimeoutError)
    expect(result.settlement.failure.cleanupFailure).toBe(result.settlement.failure.retentionFailure)
    expect(abortJob).not.toHaveBeenCalled()
  })

  it('never replenishes the shared settlement budget when an injected clock rolls back', async () => {
    vi.useFakeTimers()
    try {
      const retentionFailure = new Error('retention failed')
      const readings = [100, 150, 0]
      let index = 0
      const session: OutputSession = {
        ...terminalBoundaryOutput(),
        suspendJob: async () => { throw retentionFailure },
        abortJob: () => new Promise<void>(() => undefined),
      }
      const settlementTask = settleFailedV2Output({
        output: session,
        authority: {} as never,
        reason: new Error('transfer failed'),
        preferRetention: true,
        timeoutMilliseconds: 100,
        clock: { now: () => readings[index++] ?? 0 },
      })
      const settled = vi.fn()
      settlementTask.then(settled)

      await vi.advanceTimersByTimeAsync(49)
      expect(settled).not.toHaveBeenCalled()
      await vi.advanceTimersByTimeAsync(1)

      const settlement = await settlementTask
      expect(settlement.kind).toBe('NeedsAttention')
      if (settlement.kind !== 'NeedsAttention') throw new Error('test settlement lost its discriminant')
      expect(settlement.failure.retentionFailure).toBe(retentionFailure)
      expect(settlement.failure.cleanupFailure).toBeInstanceOf(V2OutputSettlementTimeoutError)
    } finally {
      vi.useRealTimers()
    }
  })
})

function failingJob(
  session: OutputSession,
  onTrace?: (event: TransferTraceEvent) => void,
  outputSettlementTimeoutMilliseconds?: number,
): TransferJob {
  const failure = new Error('catalog root unavailable')
  return new TransferJob({
    descriptor: {
      shareInstance: identity(1), syntheticRoot: identity(2), syntheticRootId: identityText(2), chunkSize: 1,
    } as never,
    catalog: {
      loadDirectory: async () => { throw failure },
    } as unknown as V2CatalogClient,
    selection: new V2SelectionPolicy(),
    revisions: {} as V2RevisionReader,
    broker: {} as V2BlockRangeReader,
    lanes: { size: 1 },
    output: outputAuthority(session),
    ...(onTrace === undefined ? {} : { onTrace }),
    ...(outputSettlementTimeoutMilliseconds === undefined ? {} : { outputSettlementTimeoutMilliseconds }),
  })
}

function traceNames(events: readonly TransferTraceEvent[]): readonly TransferTraceEventName[] {
  return events.map((event) => event.name)
}
