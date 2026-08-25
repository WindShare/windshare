import { describe, expect, it } from 'vitest'
import type { DomainTraceSource, TraceObserver } from '../../src/diagnostics/trace/ports'
import type { ReceiveIntent } from '../../src/transfer/intent'
import type { TransferTraceEvent } from '../../src/transfer/job/contract'
import { V2TransferObservers } from '../../src/transfer/job/observers'

describe('transfer observer separation', () => {
  it('delivers product progress while an absent trace observer builds no payload', () => {
    let current: TraceObserver<TransferTraceEvent> | undefined
    const source: DomainTraceSource<TransferTraceEvent> = {
      get current() {
        return current
      },
    }
    const progress: unknown[] = []
    const observers = new V2TransferObservers({
      intent: minimalIntent(),
      transferJobId: 'product-job-id',
      lanes: { size: 2 },
      onProgress: value => { progress.push(value) },
      trace: source,
    })

    observers.progress({
      measure: {
        discovery: 'open',
        discoveredFiles: 1,
        discoveredBytes: 2n,
        sizeClass: 'unknown',
      },
      writtenBytes: 1n,
      recoverableBytes: 0n,
      completedFiles: 0,
      completedBytes: 0n,
      fileErrors: 0,
      selectionErrors: 0,
      failedDirectories: 0,
      capacityWaitingFiles: 0,
      capacityAccumulatedWaitMilliseconds: 125,
      capacityWaitAttempts: 2,
      capacityWaitVisible: false,
    })

    let payloadFieldRead = false
    expect(() => observers.directoryAdmitted({
      get admittedDirectoryCount(): never {
        payloadFieldRead = true
        throw new Error('trace payload was built while disabled')
      },
      get layoutClass(): never {
        payloadFieldRead = true
        throw new Error('trace payload was built while disabled')
      },
    })).not.toThrow()
    expect(progress).toEqual([
      expect.objectContaining({
        recoverableBytes: 0n,
        capacityWaitingFiles: 0,
        capacityAccumulatedWaitMilliseconds: 125,
        capacityWaitAttempts: 2,
        capacityWaitVisible: false,
      }),
    ])
    expect(payloadFieldRead).toBe(false)

    const events: TransferTraceEvent[] = []
    current = event => { events.push(event) }
    observers.intentFrozen('original-file')
    expect(events).toEqual([
      expect.objectContaining({
        name: 'receive_transition',
        transition: 'intent_frozen',
      }),
    ])

    current = () => {
      throw new Error('trace consumer failed')
    }
    expect(() => observers.materializationStarted()).not.toThrow()
    expect(progress).toHaveLength(1)
  })
})

function minimalIntent(): ReceiveIntent {
  return {
    artifact: { kind: 'original-file' },
    plan: { kind: 'direct-atomic' },
  } as ReceiveIntent
}
