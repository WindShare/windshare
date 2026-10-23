import { describe, expect, it } from 'vitest'
import { V2BlockBroker } from '../../src/content/v2-broker'
import { V2LaneSet, type V2BlockDemand, type V2BlockReadObserver } from '../../src/content/v2-lane-set'
import { FileGeometry } from '../../src/content/geometry'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'
import type { V2BlockRouteEligibility, V2BlockTransportRoute } from '../../src/content/v2-route-policy'

const MIB = 1024 * 1024
const leaseId = new Uint8Array(16)
const routes = (...allowed: V2BlockTransportRoute[]): V2BlockRouteEligibility => ({
  active: true, allows: route => allowed.includes(route), assertActive() {}, subscribe: () => () => {},
})
const direct = routes('direct')
const relay = routes('application-relay')
function descriptor(name: string, blocks = 1, bytes = MIB): V2FileRevisionDescriptor {
  const size = BigInt(blocks * bytes)
  return { shareInstance: new Uint8Array(16), shareInstanceId: 'share', fileId: new Uint8Array(16),
    fileIdText: name, fileRevision: new Uint8Array(16), fileRevisionText: 'revision',
    exactSize: size, geometry: new FileGeometry(size, BigInt(bytes)) }
}
function demand(file: V2FileRevisionDescriptor, index = 0n): V2BlockDemand {
  return { descriptor: file, leaseId, localBlockIndex: index }
}
async function flush() { for (let step = 0; step < 48; step++) await Promise.resolve() }
class Lane {
  readonly id = 1
  readonly calls: string[] = []
  readonly pending = new Map<string, () => void>()
  fetchBlock(input: V2BlockDemand, signal: AbortSignal, observer?: V2BlockReadObserver): Promise<V2BlockRecord> {
    const key = input.descriptor.fileIdText + ':' + input.localBlockIndex
    const range = input.descriptor.geometry.blockPlaintext(input.localBlockIndex)
    this.calls.push(key)
    return new Promise((resolve, reject) => {
      const abort = () => { this.pending.delete(key); reject(signal.reason) }
      signal.addEventListener('abort', abort, { once: true })
      this.pending.set(key, () => {
        signal.removeEventListener('abort', abort)
        this.pending.delete(key)
        const data = new Uint8Array(Number(range.end - range.start))
        observer?.received(data.length)
        resolve({ descriptor: input.descriptor, localBlockIndex: input.localBlockIndex, data })
      })
    })
  }
  complete(key: string) {
    const finish = this.pending.get(key)
    expect(finish, 'admitted ' + key).toBeDefined()
    finish!()
  }
}

describe('receiver allocation ownership', () => {
  it('reports the cold attempt estimate captured before reserving its bytes', async () => {
    const expected: number[] = []
    const lanes = new V2LaneSet({ now: () => 1,
      onBlockScheduled: observation => expected.push(observation.expectedMilliseconds) })
    const lane = new Lane()
    lanes.add(lane, 'direct')
    const broker = new V2BlockBroker(lanes)
    try {
      const read = broker.readBlock(demand(descriptor('cold')), { routes: direct })
      await flush()
      expect(expected).toEqual([250])
      lane.complete('cold:0')
      await read
      expect(expected.every(milliseconds => milliseconds === 250)).toBe(true)
    } finally {
      broker.close(); lanes.close()
    }
  })

  it('does not attach a new consumer to an abandoned attempt that is still settling', async () => {
    const lanes = new V2LaneSet()
    const replies: Array<() => void> = []
    lanes.add({ id: 1, fetchBlock: input => new Promise(resolve => replies.push(() => resolve({
      descriptor: input.descriptor, localBlockIndex: input.localBlockIndex, data: new Uint8Array(MIB),
    }))) }, 'direct')
    const broker = new V2BlockBroker(lanes)
    const block = demand(descriptor('same'))
    const controller = new AbortController()
    const leaving = broker.readBlock(block, { routes: direct, signal: controller.signal })
    const rejection = expect(leaving).rejects.toMatchObject({ name: 'AbortError' })
    await flush()
    controller.abort()
    await rejection
    const replacement = broker.readBlock(block, { routes: direct })
    await flush()
    expect(replies).toHaveLength(1)
    replies[0]!()
    await flush()
    expect(replies).toHaveLength(2)
    replies[1]!()
    expect((await replacement).data).toHaveLength(MIB)
    await broker.waitForLeaseIdle(leaseId)
    broker.close(); lanes.close()
  })

  it('admits a later preview immediately after the cold request instead of eight privately queued downloads', async () => {
    const lanes = new V2LaneSet({ now: () => 1 })
    const lane = new Lane()
    lanes.add(lane, 'direct')
    const broker = new V2BlockBroker(lanes)
    const bulk = descriptor('bulk', 8)
    const controller = new AbortController()
    const downloads = Array.from({ length: 8 }, (_, index) =>
      broker.readBlock(demand(bulk, BigInt(index)), { routes: direct, signal: controller.signal }).catch(() => undefined))
    await flush()
    expect(lane.calls).toEqual(['bulk:0'])
    const preview = broker.readBlock(demand(descriptor('preview')), { routes: direct, priority: 'preview' })
    await flush()
    lane.complete('bulk:0')
    await flush()
    expect(lane.calls[1]).toBe('preview:0')
    lane.complete('preview:0')
    await preview
    controller.abort()
    await Promise.all(downloads)
    await broker.waitForLeaseIdle(leaseId)
    broker.close(); lanes.close()
  })

  it('does not charge dispatch slots to work whose authorized route is absent', async () => {
    const lanes = new V2LaneSet()
    const lane = new Lane()
    lanes.add(lane, 'direct')
    const broker = new V2BlockBroker(lanes, { maximumUpstreamReads: 1 })
    const controller = new AbortController()
    const waiting = broker.readBlock(demand(descriptor('waiting')), { routes: relay, signal: controller.signal }).catch(() => undefined)
    await flush()
    const ready = broker.readBlock(demand(descriptor('ready')), { routes: direct })
    await flush()
    expect(lane.calls).toEqual(['ready:0'])
    lane.complete('ready:0')
    await ready
    controller.abort(); await waiting
    broker.close(); lanes.close()
  })

  it('keeps one supported 4 MiB preview frontier available while bulk output is stalled', async () => {
    const lanes = new V2LaneSet({ now: () => 1 })
    const lane = new Lane()
    lanes.add(lane, 'direct')
    const broker = new V2BlockBroker(lanes, { maximumRangeBufferBytes: 8 * MIB })
    const file = descriptor('bulk', 8, 4 * MIB)
    const bulk = broker.readRouteAuthorizedRange(file, leaseId, { start: 0n, end: file.exactSize },
      { routes: direct })
    const first = bulk.next()
    await flush()
    lane.complete('bulk:0')
    await first; await flush()
    expect(lane.calls).toEqual(['bulk:0'])
    const image = descriptor('preview', 1, 4 * MIB)
    const preview = broker.readRouteAuthorizedRange(image, leaseId, { start: 0n, end: image.exactSize },
      { routes: direct, priority: 'preview' })
    const ready = preview.next()
    await flush()
    expect(lane.calls).toEqual(['bulk:0', 'preview:0'])
    lane.complete('preview:0')
    expect((await ready).value?.data.length).toBe(4 * MIB)
    await preview.return(undefined)
    await bulk.return(undefined)
    broker.close(); lanes.close()
  })
})
