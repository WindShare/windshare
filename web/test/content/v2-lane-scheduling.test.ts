import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { V2BlockBroker, type V2BlockPriority } from '../../src/content/v2-broker'
import { V2LaneSet, type V2BlockDemand, type V2BlockLane, type V2BlockSchedulingObservation } from '../../src/content/v2-lane-set'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'
import type { V2BlockRouteEligibility } from '../../src/content/v2-route-policy'
import { LanePerformance, completionCost } from '../../src/content/scheduling/performance'
import { LaneRescues, PROBE_INTERVAL_MILLISECONDS, rescueDue } from '../../src/content/scheduling/exploration'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'

const BLOCK_BYTES = 1024
const BLOCK_COUNT = 96
const descriptor: V2FileRevisionDescriptor = {
  shareInstance: new Uint8Array(16), shareInstanceId: 'share',
  fileId: new Uint8Array(16), fileIdText: 'file',
  fileRevision: new Uint8Array(16), fileRevisionText: 'revision',
  exactSize: BigInt(BLOCK_BYTES * BLOCK_COUNT),
  geometry: new FileGeometry(BigInt(BLOCK_BYTES * BLOCK_COUNT), BigInt(BLOCK_BYTES)),
}
const routes: V2BlockRouteEligibility = {
  active: true, allows: () => true, assertActive: () => undefined, subscribe: () => () => undefined,
}
function demand(index = 0n): V2BlockDemand {
  return { descriptor, leaseId: new Uint8Array(16), localBlockIndex: index }
}
function record(input: V2BlockDemand): V2BlockRecord {
  return { descriptor: input.descriptor, localBlockIndex: input.localBlockIndex, data: new Uint8Array(BLOCK_BYTES).fill(Number(input.localBlockIndex)) }
}

/** All requests share a lane's finite bandwidth; more in-flight blocks cannot manufacture capacity. */
class BandwidthLane implements V2BlockLane {
  readonly id: number
  readonly millisecondsPerBlock: number
  readonly calls: bigint[] = []
  readonly delivered: bigint[] = []
  cancelled = 0
  #tail = 0
  constructor(id: number, millisecondsPerBlock: number) {
    this.id = id
    this.millisecondsPerBlock = millisecondsPerBlock
  }
  fetchBlock(input: V2BlockDemand, signal: AbortSignal): Promise<V2BlockRecord> {
    this.calls.push(input.localBlockIndex)
    const due = Math.max(Date.now(), this.#tail) + this.millisecondsPerBlock
    this.#tail = due
    return new Promise((resolve, reject) => {
      const abort = () => { clearTimeout(timer); this.cancelled += 1; reject(signal.reason) }
      const timer = setTimeout(() => {
        signal.removeEventListener('abort', abort)
        this.delivered.push(input.localBlockIndex)
        resolve(record(input))
      }, due - Date.now())
      signal.addEventListener('abort', abort, { once: true })
    })
  }
}
async function download(broker: V2BlockBroker, count: number, maximumParallel = 8): Promise<bigint[]> {
  const emitted: bigint[] = []
  for await (const slice of broker.readRouteAuthorizedRange(
    descriptor, demand().leaseId, { start: 0n, end: BigInt(BLOCK_BYTES * count) },
    { routes, maximumParallel },
  )) {
    expect(slice.data[0]).toBe(Number(slice.offset / BigInt(BLOCK_BYTES)))
    emitted.push(slice.offset)
  }
  return emitted
}
async function warm(lanes: V2LaneSet, route: 'direct' | 'application-relay', milliseconds: number): Promise<void> {
  const sample = lanes.fetch(demand(), { ...routes, allows: value => value === route }, new AbortController().signal)
  await vi.advanceTimersByTimeAsync(milliseconds)
  await sample
}

async function downloadFiles(broker: V2BlockBroker, count: number, priority: V2BlockPriority = 'download'): Promise<string[]> {
  let next = 0
  const finished: string[] = []
  await Promise.all(Array.from({ length: Math.min(count, 8) }, async () => {
    while (next < count) {
      const file = { ...descriptor, fileIdText: priority + '-' + next++, exactSize: BigInt(BLOCK_BYTES),
        geometry: new FileGeometry(BigInt(BLOCK_BYTES), BigInt(BLOCK_BYTES)) }
      for await (const slice of broker.readRouteAuthorizedRange(
        file, demand().leaseId, { start: 0n, end: file.exactSize }, { routes, priority },
      )) {
        expect(slice.data).toHaveLength(BLOCK_BYTES)
        finished.push(file.fileIdText)
      }
    }
  }))
  return finished
}

describe('independent file allocation', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(0) })
  afterEach(() => vi.useRealTimers())

  it('learns a lane attached during single-block file downloads and uses its independent capacity', async () => {
    const observations: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: value => observations.push(value) })
    const direct = new BandwidthLane(1, 10)
    const relay = new BandwidthLane(2, 15)
    lanes.add(direct, 'direct')
    await warm(lanes, 'direct', 10)
    const broker = new V2BlockBroker(lanes)
    const started = Date.now()
    let elapsed = 0
    const transfer = downloadFiles(broker, 800).then(files => { elapsed = Date.now() - started; return files })
    await vi.advanceTimersByTimeAsync(100)
    lanes.add(relay, 'application-relay')
    await vi.advanceTimersByTimeAsync(5000)
    const files = await transfer
    expect(files).toHaveLength(800)
    expect(new Set(files).size).toBe(800)
    expect(elapsed).toBeLessThan(5000)
    expect(relay.delivered.length).toBeGreaterThan(100)
    expect(observations.some(value => value.purpose === 'probe' && value.localBlockIndex === 0n)).toBe(true)
    expect(observations.find(value => value.fileId === 'download-0')?.laneId).toBe(direct.id)
    expect(observations.find(value => value.fileId === 'download-1')?.laneId).toBe(direct.id)
    broker.close(); lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('bounds slow-lane exploration across a sustained file batch', async () => {
    const observations: (V2BlockSchedulingObservation & { at: number })[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: value => observations.push({ ...value, at: Date.now() }) })
    const relay = new BandwidthLane(2, 10_000)
    lanes.add(new BandwidthLane(1, 10), 'direct')
    await warm(lanes, 'direct', 10)
    lanes.add(relay, 'application-relay')
    const broker = new V2BlockBroker(lanes)
    const started = Date.now()
    let elapsed = 0
    const transfer = downloadFiles(broker, 1200).then(files => { elapsed = Date.now() - started; return files })
    await vi.advanceTimersByTimeAsync(13_000)
    expect(await transfer).toHaveLength(1200)
    expect(elapsed).toBeLessThan(12_200)
    const probes = observations.filter(value => value.purpose === 'probe')
    expect(probes).toHaveLength(3)
    for (let index = 1; index < probes.length; index += 1) {
      expect(probes[index]!.at - probes[index - 1]!.at).toBeGreaterThanOrEqual(PROBE_INTERVAL_MILLISECONDS)
    }
    expect(probes.every(value => value.pendingBytes === BLOCK_BYTES)).toBe(true)
    expect(relay.cancelled).toBe(3)
    broker.close(); lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rescues a slow sampled file before it becomes the batch completion tail', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    const relay = new BandwidthLane(2, 10_000)
    lanes.add(new BandwidthLane(1, 10), 'direct')
    await warm(lanes, 'direct', 10)
    lanes.add(relay, 'application-relay')
    const broker = new V2BlockBroker(lanes)
    let elapsed = 0
    const started = Date.now()
    const transfer = downloadFiles(broker, 8).then(files => { elapsed = Date.now() - started; return files })
    await vi.advanceTimersByTimeAsync(100)
    expect(await transfer).toHaveLength(8)
    expect(elapsed).toBeLessThanOrEqual(100)
    expect(relay.cancelled).toBe(1)
    expect(vi.getTimerCount()).toBe(0)
    await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MILLISECONDS)
    expect(relay.calls).toHaveLength(1)
    broker.close(); lanes.close()
  })

  it('keeps concurrent preview frontiers on the measured fast path', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    const relay = new BandwidthLane(2, 10_000)
    lanes.add(new BandwidthLane(1, 10), 'direct')
    await warm(lanes, 'direct', 10)
    lanes.add(relay, 'application-relay')
    const broker = new V2BlockBroker(lanes)
    const transfer = downloadFiles(broker, 16, 'preview')
    await vi.advanceTimersByTimeAsync(160)
    expect(await transfer).toHaveLength(16)
    expect(relay.calls).toHaveLength(0)
    broker.close(); lanes.close()
  })

  it('does not use the only queued file as a probe while another read is active', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    const relay = new BandwidthLane(2, 10_000)
    lanes.add(new BandwidthLane(1, 10), 'direct')
    await warm(lanes, 'direct', 10)
    lanes.add(relay, 'application-relay')
    const active = lanes.fetch(demand(), routes, new AbortController().signal)
    const broker = new V2BlockBroker(lanes)
    const transfer = downloadFiles(broker, 1)
    await vi.advanceTimersByTimeAsync(20)
    expect(await transfer).toHaveLength(1)
    await active
    expect(relay.calls).toHaveLength(0)
    broker.close(); lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })
})

describe('lane completion estimates', () => {
  it('charges each cold reservation independently of the next block size and releases cancellations', () => {
    const performance = new LanePerformance()
    const largerBlock = BLOCK_BYTES * 64
    performance.begin(0, BLOCK_BYTES)
    performance.begin(0, largerBlock)
    expect(performance.estimateQueueMilliseconds()).toBe(500)
    expect(performance.estimateCompletionMilliseconds(1)).toBe(750)
    expect(performance.estimateCompletionMilliseconds(largerBlock)).toBe(750)
    expect(performance.bytesPerSecond).toBe(0)
    performance.complete(100, largerBlock, false)
    expect(performance.estimateQueueMilliseconds()).toBe(250)
    performance.complete(100, BLOCK_BYTES, false)
    expect(performance.estimateQueueMilliseconds()).toBe(0)
    expect(performance.estimateCompletionMilliseconds(BLOCK_BYTES)).toBe(250)
  })

  it('uses observed bandwidth for queued bytes while pricing the new block separately', () => {
    const performance = new LanePerformance()
    performance.begin(0, BLOCK_BYTES)
    performance.complete(10, BLOCK_BYTES, true)
    performance.begin(20, BLOCK_BYTES * 3)
    performance.begin(20, BLOCK_BYTES)
    expect(performance.estimateQueueMilliseconds()).toBe(40)
    expect(performance.estimateCompletionMilliseconds(BLOCK_BYTES)).toBe(50)
    performance.complete(25, BLOCK_BYTES, false)
    expect(performance.estimateQueueMilliseconds()).toBe(30)
    performance.complete(50, BLOCK_BYTES * 3, true)
    expect(performance.estimateQueueMilliseconds()).toBe(0)
    expect(performance.estimateCompletionMilliseconds(BLOCK_BYTES)).toBe(10)
  })
})

describe('receiver content allocation', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(0) })
  afterEach(() => vi.useRealTimers())

  it.each([2, 3])('adds useful capacity from %i lanes by assigning different blocks', async laneCount => {
    const observations: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: value => observations.push(value) })
    const paths = Array.from({ length: laneCount }, (_, index) => new BandwidthLane(index + 1, 10 + index * 5))
    paths.forEach((lane, index) => lanes.add(lane, index === 0 ? 'direct' : 'application-relay'))
    const broker = new V2BlockBroker(lanes)
    let elapsed = 0
    const transfer = download(broker, BLOCK_COUNT).then(offsets => { elapsed = Date.now(); return offsets })
    await vi.advanceTimersByTimeAsync(1500)
    expect(await transfer).toHaveLength(BLOCK_COUNT)
    expect(elapsed).toBeLessThan(laneCount === 2 ? 700 : 580)
    expect(paths.every(path => path.delivered.length > 10)).toBe(true)
    const delivered = paths.flatMap(path => path.delivered)
    expect(new Set(delivered).size).toBe(BLOCK_COUNT)
    expect(delivered).toHaveLength(BLOCK_COUNT)
    broker.close(); lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('gives a newly attached lane an independent block that can finish after a faster lane', async () => {
    const observations: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: value => observations.push(value) })
    const direct = new BandwidthLane(1, 10)
    const relay = new BandwidthLane(2, 15)
    lanes.add(direct, 'direct')
    await warm(lanes, 'direct', 10)
    lanes.add(relay, 'application-relay')
    const broker = new V2BlockBroker(lanes)
    const transfer = download(broker, BLOCK_COUNT)
    await vi.advanceTimersByTimeAsync(1000)
    await transfer
    const probe = observations.find(value => value.purpose === 'probe')
    expect(probe?.localBlockIndex).toBeGreaterThan(0n)
    expect(relay.delivered).toContain(probe?.localBlockIndex)
    expect(relay.cancelled).toBe(0)
    expect(observations.filter(value => value.laneId === 2 && value.purpose === 'content').length).toBeGreaterThan(10)
    broker.close(); lanes.close()
  })

  it('rescues a slow exploratory block when output reaches it without filling an unbounded window', async () => {
    const observations: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: value => observations.push(value) })
    const direct = new BandwidthLane(1, 10)
    const relay = new BandwidthLane(2, 10_000)
    lanes.add(direct, 'direct')
    await warm(lanes, 'direct', 10)
    lanes.add(relay, 'application-relay')
    const broker = new V2BlockBroker(lanes, { maximumRangeBufferBytes: BLOCK_BYTES * 16 })
    const started = Date.now()
    let elapsed = 0
    const transfer = download(broker, 16, 4).then(value => { elapsed = Date.now() - started; return value })
    await vi.advanceTimersByTimeAsync(500)
    expect(await transfer).toHaveLength(16)
    expect(elapsed).toBeLessThanOrEqual(190)
    expect(relay.cancelled).toBe(1)
    expect(observations.some(value => value.purpose === 'probe' && value.laneId === 2)).toBe(true)
    expect(observations.some(value => value.purpose === 'rescue' && value.laneId === 1)).toBe(true)
    broker.close(); lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rescues already dispatched slow content when a direct path appears', async () => {
    const observations: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: value => observations.push(value) })
    const relay = new BandwidthLane(1, 10_000)
    lanes.add(relay, 'application-relay')
    const read = lanes.fetch(demand(), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(10)
    lanes.add(new BandwidthLane(2, 5), 'direct')
    await warm(lanes, 'direct', 5)
    await vi.advanceTimersByTimeAsync(50)
    await read
    expect(relay.cancelled).toBe(1)
    expect(observations.filter(value => value.localBlockIndex === 0n).map(value => value.purpose)).toContain('rescue')
    lanes.close()
  })

  it('retries a physically failed independent allocation without failing healthy content', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    lanes.add(new BandwidthLane(1, 10), 'direct')
    await warm(lanes, 'direct', 10)
    const failed = vi.fn(async () => { throw new V2SessionRuntimeError('lane', 'standby disconnected') })
    lanes.add({ id: 2, fetchBlock: failed }, 'application-relay')
    const broker = new V2BlockBroker(lanes)
    const transfer = download(broker, 16)
    await vi.advanceTimersByTimeAsync(500)
    expect(await transfer).toHaveLength(16)
    expect(failed).toHaveBeenCalledOnce()
    broker.close(); lanes.close()
  })

  it('does not explore forbidden routes or keep timers after caller cancellation', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    const relay = new BandwidthLane(2, 5)
    lanes.add(new BandwidthLane(1, 10_000), 'direct')
    lanes.add(relay, 'application-relay')
    const controller = new AbortController()
    const read = lanes.fetch(demand(), { ...routes, allows: route => route === 'direct' }, controller.signal)
    const rejection = expect(read).rejects.toMatchObject({ name: 'AbortError' })
    await vi.advanceTimersByTimeAsync(1000)
    controller.abort(new DOMException('caller stopped', 'AbortError'))
    await rejection
    expect(relay.calls).toEqual([])
    lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('holds lease authority until a canceled losing attempt actually settles', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    let finish!: (value: V2BlockRecord) => void
    let losingSignal!: AbortSignal
    lanes.add({ id: 1, fetchBlock: (_demand, signal) => {
      losingSignal = signal
      return new Promise(resolve => { finish = resolve })
    } }, 'application-relay')
    const read = lanes.fetch(demand(), routes, new AbortController().signal)
    lanes.add(new BandwidthLane(2, 2), 'direct')
    await warm(lanes, 'direct', 2)
    await vi.advanceTimersByTimeAsync(30)
    await read
    expect(losingSignal.aborted).toBe(true)
    let idle = false
    const barrier = lanes.waitForLeaseIdle(demand().leaseId).then(() => { idle = true })
    await vi.advanceTimersByTimeAsync(0)
    expect(idle).toBe(false)
    finish(record(demand()))
    await barrier
    lanes.close()
  })

  it('isolates observers from content and clears active reads on close', async () => {
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: () => { throw new Error('observer failed') } })
    lanes.add(new BandwidthLane(1, 5), 'direct')
    const read = lanes.fetch(demand(), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(5)
    await expect(read).resolves.toMatchObject({ localBlockIndex: 0n })
    const pending = lanes.fetch(demand(1n), routes, new AbortController().signal)
    const rejected = expect(pending).rejects.toThrow('LaneSet is closed')
    lanes.close()
    await rejected
    expect(vi.getTimerCount()).toBe(0)
  })

  it('counts shared bandwidth once and gives rescue its own bounded budget', () => {
    const performance = new LanePerformance()
    performance.begin(0, BLOCK_BYTES)
    performance.begin(0, BLOCK_BYTES)
    performance.complete(1000, BLOCK_BYTES, true)
    expect(performance.bytesPerSecond).toBe(BLOCK_BYTES)
    expect(performance.estimateCompletionMilliseconds(BLOCK_BYTES)).toBe(2000)
    performance.complete(2000, BLOCK_BYTES, true)
    performance.begin(1_000_000, BLOCK_BYTES)
    performance.complete(1_001_000, BLOCK_BYTES, false)
    expect(performance.bytesPerSecond).toBe(BLOCK_BYTES)
    expect(completionCost(100, true)).toBeLessThan(completionCost(200, false))
    const unmeasured = new LanePerformance()
    unmeasured.superseded(BLOCK_BYTES, 100)
    expect(unmeasured.hasSuccessfulSample).toBe(false)
    const budget = new LaneRescues()
    expect(budget.acquire()).toBe(true)
    expect(budget.acquire()).toBe(true)
    expect(budget.acquire()).toBe(false)
    budget.release()
    expect(budget.acquire()).toBe(true)
    expect(rescueDue(9, 1000, 1)).toBe(false)
    expect(rescueDue(10, 1000, 1)).toBe(true)
  })
})
