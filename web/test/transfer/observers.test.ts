import { describe, expect, it } from 'vitest'
import type { DomainTraceSource, TraceObserver } from '../../src/diagnostics/trace/ports'
import type { ReceiveIntent } from '../../src/transfer/intent'
import type { TransferTraceEvent } from '../../src/transfer/job/contract'
import { V2TransferProgressLedger } from '../../src/transfer/progress/v2-ledger'
import { V2TransferObservers } from '../../src/transfer/job/observers'
import { observeTransferContent } from '../../src/transfer/progress/content'
import { byteRange } from '../../src/content/geometry'
import { fileEntry, identity, readerFixture } from './v2-job-fixture'

describe('transfer observer separation', () => {
  it('coalesces fragment bursts while retaining every byte and preserving output authority', async () => {
    let now = 0
    const ledger = new V2TransferProgressLedger()
    const values: Array<{ receivedObjectBytes: bigint; writtenBytes: bigint; recoverableBytes: bigint }> = []
    const observers = new V2TransferObservers({
      intent: minimalIntent(), transferJobId: 'receipt-job', lanes: { size: 1 },
      onProgress: value => values.push(value),
    })
    let snapshots = 0
    const snapshot = () => {
      snapshots++
      return ledger.snapshot({ discovery: 'complete', discoveredFiles: 1, discoveredBytes: 4n, sizeClass: 'small' })
    }
    const receipts: Array<(bytes: number) => void> = []
    const broker = observeTransferContent({
      readRange: async function* (_descriptor, _leaseId, range, options) {
        receipts.push(options!.onReceive!)
        yield { offset: range.start, data: new Uint8Array(Number(range.end - range.start)) }
      },
    }, {
      received: bytes => ledger.receiveObjectBytes(bytes),
      updated: () => observers.progress(snapshot()),
    }, () => now)
    const file = fileEntry(identity(11), 'receipt.bin', 4n)
    const revision = await readerFixture([file]).revisions.open(file.id)
    for (let index = 0; index < 2; index++) {
      await broker.readRange(revision.descriptor, revision.leaseId, byteRange(0n, 4n)).next()
    }
    expect(receipts[0]).toBe(receipts[1])
    for (let index = 0; index < 1000; index++) receipts[0]!(1)
    expect(snapshots).toBe(1)
    now = 250
    receipts[1]!(1)
    expect(values).toHaveLength(2)
    expect(values.at(-1)).toMatchObject({ receivedObjectBytes: 1001n, writtenBytes: 0n, recoverableBytes: 0n })
    ledger.acknowledgeWrite(4n)
    observers.progress(snapshot())
    expect(values.at(-1)).toMatchObject({ receivedObjectBytes: 1001n, writtenBytes: 4n, recoverableBytes: 0n })
    await revision.release()
  })


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
      phase: 'receiving',
      materializedBytes: 1n,
      receivedObjectBytes: 2n,
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
