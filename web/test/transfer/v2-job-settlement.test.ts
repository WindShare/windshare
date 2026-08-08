import { describe, expect, it, vi } from 'vitest'

import type { V2CatalogClient } from '../../src/catalog/v2-client'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { V2RevisionReader } from '../../src/content/v2-session-services'
import {
  JobSettlementKind,
  pausedJobSettlement,
  type OutputSession,
  type V2OutputAuthority,
} from '../../src/transfer/output-session'
import type { TransferTraceEvent, TransferTraceEventName } from '../../src/transfer/intent'
import { TransferJob } from '../../src/transfer/v2-job'
import { pauseFailedV2Output } from '../../src/transfer/settlement/v2-output'
import {
  identity,
  identityText,
  outputAuthority,
  terminalBoundaryOutput,
  withTimeout,
} from './v2-job-fixture'

describe('v2 output pause evidence', () => {
  it('retains a durable output at the backend-reported stable cut', async () => {
    const pauseJob = vi.fn(async () => pausedJobSettlement('ProcessRestart'))
    const session: OutputSession = {
      ...terminalBoundaryOutput(),
      pauseJob,
    }

    const result = await failingJob(session).run()

    expect(result.outcome.status).toBe('Paused')
    expect(result.settlement).toEqual({
      kind: JobSettlementKind.Paused,
      durability: 'ProcessRestart',
    })
    expect(pauseJob).toHaveBeenCalledOnce()
  })

  it('surfaces ambiguous output ownership without attempting destructive fallback', async () => {
    const traces: TransferTraceEvent[] = []
    const pauseJob = vi.fn(async () => {
      throw new Error('stable cut failed')
    })
    const session: OutputSession = {
      ...terminalBoundaryOutput(),
      pauseJob,
    }

    const result = await failingJob(session, (event) => traces.push(event)).run()

    expect(result.outcome.status).toBe('Paused')
    expect(result.settlement.kind).toBe(JobSettlementKind.NeedsAttention)
    if (result.settlement.kind !== JobSettlementKind.NeedsAttention) {
      throw new Error('test settlement lost its discriminant')
    }
    expect(result.settlement.fault).toMatchObject({
      scope: 'output-pause',
      code: 'mutation-ambiguous',
    })
    expect(pauseJob).toHaveBeenCalledOnce()
    expect(traceNames(traces)).toContain('job-needs-attention')
  })

  it('bounds a collaborator that never reaches a stable cut', async () => {
    const pauseJob = vi.fn(() => new Promise<never>(() => undefined))
    const session: OutputSession = {
      ...terminalBoundaryOutput(),
      pauseJob,
    }

    const result = await withTimeout(
      failingJob(session, undefined, 10).run(),
      500,
      'terminal output pause exceeded its external test bound',
    )

    expect(result.outcome.status).toBe('Paused')
    expect(result.settlement.kind).toBe(JobSettlementKind.NeedsAttention)
    expect(pauseJob).toHaveBeenCalledOnce()
  })

  it('abandons only an unopened capability and reports no resumable durability', async () => {
    const abort = vi.fn(async () => undefined)
    const authority = {
      confirmOutput: async () => {
        throw new Error('not used')
      },
      openOutput: async () => {
        throw new Error('not used')
      },
      abort,
    } as V2OutputAuthority
    const reason = new Error('output confirmation failed')

    const settlement = await pauseFailedV2Output({
      authority,
      reason,
      timeoutMilliseconds: 100,
    })

    expect(settlement).toEqual({
      kind: JobSettlementKind.Paused,
      durability: 'None',
    })
    expect(abort).toHaveBeenCalledWith(reason)
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
