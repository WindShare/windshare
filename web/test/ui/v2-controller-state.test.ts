import { describe, expect, it } from 'vitest'

import { outputFault, FaultScope, OutputFaultCode } from '../../src/transfer/fault'
import {
  COMPLETED_JOB_SETTLEMENT,
  needsAttentionJobSettlement,
  pausedJobSettlement,
} from '../../src/transfer/output-session'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  jobOutcome,
  type JobOutcomeStatus,
} from '../../src/transfer/outcome'
import type { TransferJobResult } from '../../src/transfer/v2-job'
import { transferTerminalSnapshot } from '../../src/ui/v2-controller-state'
import type { V2ReceiverSnapshot } from '../../src/ui/v2-model'

describe('v2 controller terminal presentation', () => {
  it('presents a transient pause as retry from byte zero', () => {
    const snapshot = transferTerminalSnapshot(
      baseSnapshot(),
      result('Paused', pausedJobSettlement('None')),
    )

    expect(snapshot.phase).toBe('retry-ready')
    expect(snapshot.status).toContain('byte zero')
  })

  it('presents a durable pause as retained resumable state', () => {
    const snapshot = transferTerminalSnapshot(
      baseSnapshot(),
      result('Paused', pausedJobSettlement('ProcessRestart')),
    )

    expect(snapshot.phase).toBe('paused')
    expect(snapshot.status).toContain('checkpoints were retained')
  })

  it('keeps any ambiguous output boundary in needs-attention state', () => {
    const snapshot = transferTerminalSnapshot(
      baseSnapshot(),
      result('Succeeded', needsAttentionJobSettlement(outputFault(
        FaultScope.OutputPause,
        OutputFaultCode.MutationAmbiguous,
      ))),
    )

    expect(snapshot.phase).toBe('needs-attention')
    expect(snapshot.status).toContain('manual review')
  })

  it('reports publication that won a pause race as completed', () => {
    const snapshot = transferTerminalSnapshot(
      baseSnapshot(),
      result('Paused', COMPLETED_JOB_SETTLEMENT),
    )

    expect(snapshot.phase).toBe('completed')
    expect(snapshot.status).toContain('completed before the pause')
  })
})

function baseSnapshot(): V2ReceiverSnapshot {
  return {
    phase: 'transferring',
    status: 'Receiving…',
  } as unknown as V2ReceiverSnapshot
}

function result(
  status: JobOutcomeStatus,
  settlement: TransferJobResult['settlement'],
): TransferJobResult {
  return {
    outcome: jobOutcome(status, EMPTY_TRANSFER_FAILURE_SUMMARY),
    settlement,
  } as unknown as TransferJobResult
}
