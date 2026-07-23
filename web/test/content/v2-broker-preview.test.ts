import { describe, expect, it } from 'vitest'

import { V2ConnectivityRouteAuthority } from '../../src/connectivity/v2-receiver-policy'
import { byteRange, FileGeometry } from '../../src/content/geometry'
import {
  V2BlockBroker,
  V2LaneSet,
  type V2BlockDemand,
  type V2BlockLane,
  type V2BlockRouteEligibility,
} from '../../src/content/v2-broker'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'

const ALL_ROUTES: V2BlockRouteEligibility = Object.freeze({
  active: true,
  allows: () => true,
  assertActive: () => undefined,
  subscribe: () => () => undefined,
})

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function revision(exactSize = 40n, blockSize = 10n): V2FileRevisionDescriptor {
  return Object.freeze({
    shareInstance: identity(1),
    shareInstanceId: 'share',
    fileId: identity(2),
    fileIdText: 'file',
    fileRevision: identity(3),
    fileRevisionText: 'revision',
    exactSize,
    geometry: new FileGeometry(exactSize, blockSize),
  })
}

interface LaneCall {
  readonly demand: V2BlockDemand
  readonly signal: AbortSignal
  readonly resolve: (record: V2BlockRecord) => void
}

class ImmediateRecordingLane implements V2BlockLane {
  readonly id: number
  readonly calls: V2BlockDemand[] = []

  constructor(id: number) {
    this.id = id
  }

  async fetchBlock(demand: V2BlockDemand, signal: AbortSignal): Promise<V2BlockRecord> {
    signal.throwIfAborted()
    this.calls.push(demand)
    const block = demand.descriptor.geometry.blockPlaintext(demand.localBlockIndex)
    return Object.freeze({
      descriptor: demand.descriptor,
      localBlockIndex: demand.localBlockIndex,
      data: new Uint8Array(Number(block.end - block.start)).fill(this.id),
    })
  }
}

class ControlledLane implements V2BlockLane {
  readonly id = 1
  readonly calls: LaneCall[] = []

  fetchBlock(demand: V2BlockDemand, signal: AbortSignal): Promise<V2BlockRecord> {
    return new Promise((resolve) => this.calls.push({ demand, signal, resolve }))
  }

  complete(callIndex: number): void {
    const call = this.calls[callIndex]
    if (call === undefined) throw new Error('missing controlled lane call')
    const block = call.demand.descriptor.geometry.blockPlaintext(call.demand.localBlockIndex)
    const length = Number(block.end - block.start)
    call.resolve(Object.freeze({
      descriptor: call.demand.descriptor,
      localBlockIndex: call.demand.localBlockIndex,
      data: new Uint8Array(length).fill(Number(call.demand.localBlockIndex) + 1),
    }))
  }
}

function brokerWith(lane: ControlledLane, options: ConstructorParameters<typeof V2BlockBroker>[1] = {}) {
  const lanes = new V2LaneSet()
  lanes.add(lane, 'relay')
  return new V2BlockBroker(lanes, options)
}

async function turn(): Promise<void> {
  await Promise.resolve()
  await Promise.resolve()
}

describe('v2 preview/download block broker', () => {
  it('keeps a shared upstream read alive when preview leaves but download remains', async () => {
    const activeLeases = new Set([10, 20, 30])
    const lane = new ControlledLane()
    const broker = brokerWith(lane, {
      maximumUpstreamReads: 1,
      validateDemand: (demand) => activeLeases.has(demand.leaseId[0] ?? -1)
        ? undefined
        : new Error('lease inactive'),
    })
    const descriptor = revision()
    const previewAbort = new AbortController()
    const preview = broker.readBlock(
      { descriptor, leaseId: identity(10), localBlockIndex: 0n },
      { routes: ALL_ROUTES, priority: 'preview', signal: previewAbort.signal },
    )
    const download = broker.readBlock(
      { descriptor, leaseId: identity(20), localBlockIndex: 0n },
      { routes: ALL_ROUTES, priority: 'download' },
    )
    await turn()
    expect(lane.calls).toHaveLength(1)

    previewAbort.abort(new DOMException('preview closed', 'AbortError'))
    await expect(preview).rejects.toMatchObject({ name: 'AbortError' })
    expect(lane.calls[0]?.signal.aborted).toBe(false)
    let leaseIdle = false
    const leaseBarrier = broker.waitForLeaseIdle(identity(10)).then(() => { leaseIdle = true })
    await turn()
    expect(leaseIdle).toBe(false)
    lane.complete(0)
    await leaseBarrier
    await expect(download).resolves.toMatchObject({ localBlockIndex: 0n })

    await expect(broker.readBlock({
      descriptor,
      leaseId: identity(30),
      localBlockIndex: 0n,
    }, { routes: ALL_ROUTES, priority: 'preview' })).resolves.toMatchObject({ localBlockIndex: 0n })
    expect(lane.calls).toHaveLength(1)

    activeLeases.delete(30)
    await expect(broker.readBlock({
      descriptor,
      leaseId: identity(30),
      localBlockIndex: 0n,
    }, { routes: ALL_ROUTES })).rejects.toThrow('lease inactive')
  })

  it('abandons a preview-only read and cannot let its late result poison the cache', async () => {
    const lane = new ControlledLane()
    const broker = brokerWith(lane, { maximumUpstreamReads: 1 })
    const descriptor = revision()
    const controller = new AbortController()
    const preview = broker.readBlock(
      { descriptor, leaseId: identity(4), localBlockIndex: 0n },
      { routes: ALL_ROUTES, priority: 'preview', signal: controller.signal },
    )
    await turn()
    controller.abort(new DOMException('preview closed', 'AbortError'))
    await expect(preview).rejects.toMatchObject({ name: 'AbortError' })
    expect(lane.calls[0]?.signal.aborted).toBe(true)
    let leaseIdle = false
    const leaseBarrier = broker.waitForLeaseIdle(identity(4)).then(() => { leaseIdle = true })
    await turn()
    expect(leaseIdle).toBe(false)
    lane.complete(0)
    await leaseBarrier
    await turn()

    const replacement = broker.readBlock(
      { descriptor, leaseId: identity(5), localBlockIndex: 0n },
      { routes: ALL_ROUTES, priority: 'download' },
    )
    await turn()
    expect(lane.calls).toHaveLength(2)
    lane.complete(1)
    await expect(replacement).resolves.toMatchObject({ localBlockIndex: 0n })
  })

  it('reads only the blocks intersecting the requested file-local range', async () => {
    const lane = new ControlledLane()
    const broker = brokerWith(lane)
    const descriptor = revision()
    const collected = (async () => {
      const slices = []
      for await (const slice of broker.readRange(
        descriptor,
        identity(4),
        byteRange(11n, 19n),
        { routes: ALL_ROUTES, priority: 'preview' },
      )) slices.push(slice)
      return slices
    })()
    await turn()
    expect(lane.calls.map((call) => call.demand.localBlockIndex)).toEqual([1n])
    lane.complete(0)
    const slices = await collected
    expect(slices).toHaveLength(1)
    expect(slices[0]).toMatchObject({ offset: 11n })
    expect(slices[0]?.data).toEqual(new Uint8Array(8).fill(2))
  })

  it('cancels unread range work without aborting an overlapping download consumer', async () => {
    const lane = new ControlledLane()
    const broker = brokerWith(lane)
    const descriptor = revision()
    const download = broker.readBlock(
      { descriptor, leaseId: identity(5), localBlockIndex: 1n },
      { routes: ALL_ROUTES, priority: 'download' },
    )
    const iterator = broker.readRange(
      descriptor,
      identity(4),
      byteRange(0n, 30n),
      { routes: ALL_ROUTES, maximumParallel: 3, priority: 'preview' },
    )
    const first = iterator.next()
    await turn()
    const firstCall = lane.calls.findIndex((call) => call.demand.localBlockIndex === 0n)
    lane.complete(firstCall)
    await expect(first).resolves.toMatchObject({ done: false })

    await iterator.return(undefined)
    const shared = lane.calls.find((call) => call.demand.localBlockIndex === 1n)
    const abandoned = lane.calls.find((call) => call.demand.localBlockIndex === 2n)
    expect(shared?.signal.aborted).toBe(false)
    expect(abandoned?.signal.aborted).toBe(true)
    const sharedCall = lane.calls.indexOf(shared!)
    const abandonedCall = lane.calls.indexOf(abandoned!)
    lane.complete(sharedCall)
    await expect(download).resolves.toMatchObject({ localBlockIndex: 1n })
    lane.complete(abandonedCall)
    await turn()
  })

  it('enforces per-consumer routes for distinct work while coalescing the same BlockRef', async () => {
    const observations: Array<{ readonly route: string; readonly localBlockIndex: bigint }> = []
    const lanes = new V2LaneSet({
      onBlockFetched: ({ route, localBlockIndex }) => observations.push({ route, localBlockIndex }),
    })
    const relay = new ImmediateRecordingLane(1)
    lanes.add(relay, 'relay')
    const broker = new V2BlockBroker(lanes)
    const descriptor = revision()
    const largeRoutes = new V2ConnectivityRouteAuthority()
    const smallRoutes = new V2ConnectivityRouteAuthority()
    smallRoutes.admitRelay()

    const largeDistinct = broker.readBlock(
      { descriptor, leaseId: identity(10), localBlockIndex: 0n },
      { routes: largeRoutes, priority: 'download' },
    )
    await turn()
    expect(relay.calls).toHaveLength(0)

    await expect(broker.readBlock(
      { descriptor, leaseId: identity(20), localBlockIndex: 1n },
      { routes: smallRoutes, priority: 'download' },
    )).resolves.toMatchObject({ localBlockIndex: 1n })
    expect(relay.calls.map((call) => call.localBlockIndex)).toEqual([1n])

    const sharedLarge = broker.readBlock(
      { descriptor, leaseId: identity(30), localBlockIndex: 2n },
      { routes: largeRoutes, priority: 'download' },
    )
    const sharedSmall = broker.readBlock(
      { descriptor, leaseId: identity(40), localBlockIndex: 2n },
      { routes: smallRoutes, priority: 'preview' },
    )
    await expect(Promise.all([sharedLarge, sharedSmall])).resolves.toHaveLength(2)
    expect(relay.calls.map((call) => call.localBlockIndex)).toEqual([1n, 2n])

    const peer = new ImmediateRecordingLane(2)
    lanes.add(peer, 'peer')
    await expect(largeDistinct).resolves.toMatchObject({ localBlockIndex: 0n })
    expect(peer.calls.map((call) => call.localBlockIndex)).toEqual([0n])
    expect(observations).toEqual([
      { route: 'relay', localBlockIndex: 1n },
      { route: 'relay', localBlockIndex: 2n },
      { route: 'peer', localBlockIndex: 0n },
    ])
  })

  it('revokes a canceled consumer route before a queued upstream dispatch', async () => {
    const observations: Array<{ readonly route: string; readonly localBlockIndex: bigint }> = []
    const lanes = new V2LaneSet({
      onBlockFetched: ({ route, localBlockIndex }) => observations.push({ route, localBlockIndex }),
    })
    const relay = new ImmediateRecordingLane(1)
    lanes.add(relay, 'relay')
    const broker = new V2BlockBroker(lanes)
    const descriptor = revision()
    const largeRoutes = new V2ConnectivityRouteAuthority()
    const canceledRoutes = new V2ConnectivityRouteAuthority()
    canceledRoutes.admitRelay()
    const controller = new AbortController()

    const surviving = broker.readBlock(
      { descriptor, leaseId: identity(10), localBlockIndex: 3n },
      { routes: largeRoutes, priority: 'download' },
    )
    const canceled = broker.readBlock(
      { descriptor, leaseId: identity(20), localBlockIndex: 3n },
      { routes: canceledRoutes, priority: 'preview', signal: controller.signal },
    )
    controller.abort(new DOMException('consumer canceled', 'AbortError'))
    await expect(canceled).rejects.toMatchObject({ name: 'AbortError' })
    await turn()
    expect(relay.calls).toHaveLength(0)

    const peer = new ImmediateRecordingLane(2)
    lanes.add(peer, 'peer')
    await expect(surviving).resolves.toMatchObject({ localBlockIndex: 3n })
    expect(observations).toEqual([{ route: 'peer', localBlockIndex: 3n }])
  })

  it('schedules preview before download and prefetch while retaining bounded fairness', async () => {
    const lane = new ControlledLane()
    const broker = brokerWith(lane, { maximumUpstreamReads: 1 })
    const descriptor = revision()
    const reads = [
      broker.readBlock(
        { descriptor, leaseId: identity(4), localBlockIndex: 0n },
        { routes: ALL_ROUTES },
      ),
    ]
    await turn()
    reads.push(
      broker.readBlock(
        { descriptor, leaseId: identity(4), localBlockIndex: 1n },
        { routes: ALL_ROUTES, priority: 'prefetch' },
      ),
      broker.readBlock(
        { descriptor, leaseId: identity(4), localBlockIndex: 2n },
        { routes: ALL_ROUTES, priority: 'download' },
      ),
      broker.readBlock(
        { descriptor, leaseId: identity(4), localBlockIndex: 3n },
        { routes: ALL_ROUTES, priority: 'preview' },
      ),
    )
    await turn()

    for (let call = 0; call < 4; call += 1) {
      lane.complete(call)
      await turn()
    }
    await Promise.all(reads)
    expect(lane.calls.map((call) => call.demand.localBlockIndex)).toEqual([0n, 3n, 2n, 1n])
  })
})
