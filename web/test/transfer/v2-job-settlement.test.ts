import { describe, expect, it } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { V2TransferFailureSettlementError } from '../../src/transfer/settlement/v2-output'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity,
  planAuthorityFixture,
  readerFixture,
  receiveIntentFixture,
  selectOnlyFile,
  transferJobFixture,
} from './v2-job-fixture'

describe('v2 plan settlement', () => {
  it('settles DirectTree progressively as a partial directory after a file-local failure', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.worker.failureCount).toBe(1)
    expect(result.lifecycle.kind).toBe('partial-directory')
    expect(plans.settlements).toEqual(['direct-tree:CompletedWithErrors'])
    expect(plans.pauses).toEqual([])
  })

  it('pauses a complete artifact instead of calling settlement after the same file-local failure', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.lifecycle.kind).toBe('restart-required')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
  })

  it.each([
    {
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      lifecycle: 'partial-directory',
      settlement: 'direct-tree:CompletedWithErrors',
    },
    {
      planKind: 'direct-atomic',
      artifactKind: 'zip-archive',
      lifecycle: 'restart-required',
      settlement: undefined,
    },
  ] as const)(
    'isolates a child finalization failure without allowing an incomplete $planKind artifact to publish',
    async ({ planKind, artifactKind, lifecycle, settlement }) => {
      const root = identity(2)
      const child = identity(3)
      const file = fileEntry(identity(11), 'payload.bin', 2n)
      const selection = new V2SelectionPolicy(true)
      const catalog = catalogFixture([
        { id: root, entries: [directoryEntry(child, 'child'), file] },
        { id: child, entries: [] },
      ])
      const readers = readerFixture([file])
      const plans = planAuthorityFixture({ failDirectoryFinalizePath: 'child' })
      const intent = await receiveIntentFixture({ planKind, artifactKind, selection })

      const result = await transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
      }).run()

      expect(result.worker.status).toBe('CompletedWithErrors')
      expect(result.lifecycle.kind).toBe(lifecycle)
      expect(plans.output.commits).toEqual([file.idText])
      expect(plans.settlements).toEqual(settlement === undefined ? [] : [settlement])
      expect(plans.pauses).toEqual(planKind === 'direct-tree' ? [] : [planKind])
    },
  )

  it('keeps successful worker evidence distinct when terminal publication settlement fails', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const plans = planAuthorityFixture({ failSettlement: true })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(result.lifecycle.kind).toBe('restart-required')
    expect(result.abortReason).toEqual(expect.any(Error))
    expect(plans.settlements).toEqual(['direct-atomic:Succeeded'])
    expect(plans.pauses).toEqual(['direct-atomic'])
    expect(plans.output.commits).toEqual([file.idText])
  })

  it('bounds a plan pause and records target ownership as unknown', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture({ hangPause: true })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await withExternalBound(transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
      outputSettlementTimeoutMilliseconds: 10,
    }).run(), 500)

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.lifecycle.kind).toBe('needs-attention')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
    expect(plans.unknownSettlements).toEqual(['direct-atomic'])
    expect(plans.pauseSignals).toHaveLength(1)
    expect(plans.pauseSignals[0]?.aborted).toBe(true)
  })

  it('records ownership unknown when an adapter reports a lifecycle illegal for the plan stage', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture({ invalidPauseLifecycle: true })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.lifecycle.kind).toBe('needs-attention')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
    expect(plans.unknownSettlements).toEqual(['direct-atomic'])
  })

  it('preserves the transfer failure when both output settlement paths fail', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture({ failPause: true, failUnknownSettlement: true })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    let thrown: unknown
    try {
      await transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
      }).run()
    } catch (error) {
      thrown = error
    }

    expect(thrown).toBeInstanceOf(V2TransferFailureSettlementError)
    const failure = thrown as V2TransferFailureSettlementError
    expect(failure.transferFailure).toBe(failure.trigger)
    expect(failure.message).toBe('Transfer failed before output settlement completed')
    expect(failure.cause).toBeUndefined()
    expect(failure.settlementFailures).toHaveLength(2)
    expect(failure.settlementFailures).toEqual([
      expect.objectContaining({ fact: expect.objectContaining({ stage: 'settlement' }) }),
      expect.objectContaining({ fact: expect.objectContaining({ stage: 'settlement' }) }),
    ])
    expect(JSON.stringify(failure.settlementFailures)).not.toContain('fixture')
  })
})

async function withExternalBound<T>(operation: Promise<T>, milliseconds: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  const timeout = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => reject(new Error('settlement exceeded external test bound')), milliseconds)
  })
  try {
    return await Promise.race([operation, timeout])
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}
