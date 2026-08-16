import { describe, expect, it } from 'vitest'

import { byteRange } from '../../src/content/geometry'
import {
  V2BlockBroker,
  V2LaneSet,
  type V2BlockDemand,
  type V2BlockLane,
  type V2BlockRangeReader,
  type V2BlockRouteEligibility,
} from '../../src/content/v2-broker'
import type { V2BlockRecord } from '../../src/content/v2-records'
import type { TestOutput } from './v2-job-fixture'
import {
  catalogFixture,
  fileEntry,
  identity,
  planAuthorityFixture,
  readerFixture,
  receiveIntentFixture,
  selectOnlyFile,
  testOutput,
  transferJobFixture,
} from './v2-job-fixture'

const ALL_CONTENT_ROUTES: V2BlockRouteEligibility = Object.freeze({
  active: true,
  allows: () => true,
  assertActive: () => undefined,
  subscribe: () => () => undefined,
})

class DeferredTransferLane implements V2BlockLane {
  readonly id: number
  readonly calls: Array<Readonly<{
    demand: V2BlockDemand
    resolve: (record: V2BlockRecord) => void
  }>> = []

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(demand: V2BlockDemand): Promise<V2BlockRecord> {
    return new Promise((resolve) => this.calls.push({ demand, resolve }))
  }

  completeAll(): void {
    for (const call of this.calls) {
      const block = call.demand.descriptor.geometry.blockPlaintext(call.demand.localBlockIndex)
      call.resolve(Object.freeze({
        descriptor: call.demand.descriptor,
        localBlockIndex: call.demand.localBlockIndex,
        data: new Uint8Array(Number(block.end - block.start)).fill(this.id),
      }))
    }
  }
}

describe('v2 authenticated file transfer', () => {
  it('opens the authenticated revision through the adapter before output ownership exists', async () => {
    const events: string[] = []
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], events)
    const output = testOutput(events)
    const plans = planAuthorityFixture({ output })
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
    expect(result.lifecycle.kind).toBe('published')
    expect(events.slice(0, 4)).toEqual([
      'begin-request',
      `revision:${file.idText}`,
      'revision-opened',
      'transaction-created',
    ])
    expect(events.indexOf(`block:${file.idText}`)).toBeGreaterThan(events.indexOf('transaction-created'))
    expect(output.requests).toHaveLength(1)
    expect(output.requests[0]).toMatchObject({
      source: { fileId: file.idText },
      sourcePath: ['payload.bin'],
      artifactPath: ['payload.bin'],
      expectedSize: 4n,
    })
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.releases).toEqual([file.idText])
    expect(plans.settlements).toEqual(['direct-atomic:Succeeded'])
  })

  it('requests only missing authenticated ranges and still commits the whole file', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput([], {
      durability: 'ProcessRestart',
      initialRanges: [byteRange(0n, 2n)],
    })
    const plans = planAuthorityFixture({ output })
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
    expect(readers.blockRequests).toEqual([file.idText])
    expect(output.writes).toEqual([{ offset: 2n, bytes: 2 }])
    expect(output.commits).toEqual([file.idText])
    expect(readers.releases).toEqual([file.idText])
  })

  it('fills disjoint unaligned gaps without rewriting the durable middle range', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput([], {
      durability: 'ProcessRestart',
      initialRanges: [byteRange(1n, 3n)],
    })
    const plans = planAuthorityFixture({ output })
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
    expect(output.writes).toEqual([
      { offset: 0n, bytes: 1 },
      { offset: 3n, bytes: 1 },
    ])
    expect(readers.blockRequests).toEqual([file.idText, file.idText])
    expect(output.commits).toEqual([file.idText])
  })

  it('pipelines one large file across relay and peer lanes', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'parallel.bin', 8n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const lanes = new V2LaneSet()
    const relay = new DeferredTransferLane(1)
    const peer = new DeferredTransferLane(2)
    lanes.add(relay, 'relay')
    lanes.add(peer, 'peer')
    const blockBroker = new V2BlockBroker(lanes)
    const broker: V2BlockRangeReader = {
      readRange: (descriptor, leaseId, range, options = {}) =>
        blockBroker.readRouteAuthorizedRange(descriptor, leaseId, range, {
          ...options,
          routes: ALL_CONTENT_ROUTES,
        }),
    }
    const output = testOutput()
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker,
    }).run()
    await expect.poll(() => relay.calls.length + peer.calls.length).toBe(4)
    expect(relay.calls).toHaveLength(2)
    expect(peer.calls).toHaveLength(2)
    expect([...relay.calls, ...peer.calls]
      .map((call) => call.demand.localBlockIndex)
      .sort((left, right) => Number(left - right))).toEqual([0n, 1n, 2n, 3n])

    relay.completeAll()
    peer.completeAll()
    const result = await running
    blockBroker.close()
    lanes.close()

    expect(result.worker.status).toBe('Succeeded')
    expect(output.writes).toEqual([
      { offset: 0n, bytes: 2 },
      { offset: 2n, bytes: 2 },
      { offset: 4n, bytes: 2 },
      { offset: 6n, bytes: 2 },
    ])
  })

  it('commits an authenticated empty file without requesting content blocks', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'empty.bin', 0n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput()
    const plans = planAuthorityFixture({ output })
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
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).toEqual([])
    expect(output.writes).toEqual([])
    expect(output.commits).toEqual([file.idText])
    expect(readers.releases).toEqual([file.idText])
  })

  it.each([
    'direct-atomic',
    'workspace-then-publish',
    'portable-handoff',
  ] as const)('never seals or publishes %s after a selected revision failure', async (planKind) => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind,
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
    expect(result.lifecycle.kind).toBe(planKind === 'workspace-then-publish'
      ? 'resumable-receive'
      : 'restart-required')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual([planKind])
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).toEqual([])
    expect(plans.output.commits).toEqual([])
  })

  it('rejects an adapter that invokes the revision callback twice and releases the first lease', async () => {
    const events: string[] = []
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], events)
    const base = testOutput(events)
    const output: TestOutput = {
      ...base,
      beginFile: async (request, signal) => {
        await request.openRevision(signal)
        return base.beginFile(request, signal)
      },
    }
    const plans = planAuthorityFixture({ output })
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

    expect(result.worker.status).toBe('Paused')
    expect(result.lifecycle.kind).toBe('restart-required')
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).toEqual([])
    expect(readers.releases).toEqual([file.idText])
    expect(events).not.toContain('transaction-created')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
  })
})
