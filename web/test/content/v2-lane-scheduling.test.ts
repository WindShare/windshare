import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { V2BlockBroker } from '../../src/content/v2-broker'
import { V2LaneSet, type V2BlockDemand, type V2BlockLane, type V2BlockSchedulingObservation } from '../../src/content/v2-lane-set'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'
import type { V2BlockRouteEligibility } from '../../src/content/v2-route-policy'
import { LanePerformance, completionCost } from '../../src/content/scheduling/performance'
import { LaneExploration, probeDue, rescueDue } from '../../src/content/scheduling/exploration'

const BLOCK_BYTES = 1024
const descriptor: V2FileRevisionDescriptor = {
  shareInstance: new Uint8Array(16), shareInstanceId: 'share',
  fileId: new Uint8Array(16), fileIdText: 'file',
  fileRevision: new Uint8Array(16), fileRevisionText: 'revision',
  exactSize: BigInt(BLOCK_BYTES * 32), geometry: new FileGeometry(BigInt(BLOCK_BYTES * 32), BigInt(BLOCK_BYTES)),
}
const routes: V2BlockRouteEligibility = {
  active: true, allows: () => true, assertActive: () => undefined, subscribe: () => () => undefined,
}
function demand(index = 0n): V2BlockDemand {
  return { descriptor, leaseId: new Uint8Array(16), localBlockIndex: index }
}

class TimedLane implements V2BlockLane {
  readonly calls: bigint[] = []
  cancelled = 0
  closed = false
  readonly id: number
  milliseconds: number
  constructor(id: number, milliseconds: number) {
    this.id = id
    this.milliseconds = milliseconds
  }
  fetchBlock(input: V2BlockDemand, signal: AbortSignal): Promise<V2BlockRecord> {
    this.calls.push(input.localBlockIndex)
    return new Promise((resolve, reject) => {
      const abort = () => {
        clearTimeout(timer)
        this.cancelled += 1
        reject(signal.reason)
      }
      const timer = setTimeout(() => {
        signal.removeEventListener('abort', abort)
        resolve({ descriptor, localBlockIndex: input.localBlockIndex, data: new Uint8Array(BLOCK_BYTES) })
      }, this.milliseconds)
      signal.addEventListener('abort', abort, { once: true })
    })
  }
  close(): void { this.closed = true }
}

describe('content completion scheduling', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(0) })
  afterEach(() => { vi.useRealTimers() })

  it('keeps a fast ordered window moving when an unmeasured slow relay is added', async () => {
    const decisions: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: (value) => decisions.push(value) })
    const direct = new TimedLane(1, 10)
    const relay = new TimedLane(2, 1000)
    lanes.add(direct, 'direct')
    const warm = lanes.fetch(demand(), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(10)
    await warm
    lanes.add(relay, 'application-relay')
    const broker = new V2BlockBroker(lanes)
    const emitted: bigint[] = []
    const started = Date.now()
    const read = (async () => {
      for await (const slice of broker.readRouteAuthorizedRange(
        descriptor, demand().leaseId, { start: 0n, end: BigInt(BLOCK_BYTES * 16) },
        { routes, maximumParallel: 4 },
      )) emitted.push(slice.offset)
    })()
    await vi.advanceTimersByTimeAsync(40)
    await read
    expect(Date.now() - started).toBe(40)
    expect(emitted).toEqual(Array.from({ length: 16 }, (_, index) => BigInt(index * BLOCK_BYTES)))
    expect(relay.calls).toHaveLength(1)
    expect(relay.cancelled).toBe(1)
    expect(relay.closed).toBe(false)
    expect(decisions.filter((value) => value.route === 'application-relay').map((value) => value.purpose)).toEqual(['probe'])
    broker.close()
    lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rescues an already dispatched slow prefix when a direct path appears', async () => {
    const decisions: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: (value) => decisions.push(value) })
    const relay = new TimedLane(1, 10_000)
    lanes.add(relay, 'application-relay')
    const read = lanes.fetch(demand(), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(10)
    const direct = new TimedLane(2, 5)
    lanes.add(direct, 'direct')
    // Train the new path using a separately authorized read without allowing relay work.
    const directOnly = { ...routes, allows: (route: string) => route === 'direct' }
    const warm = lanes.fetch(demand(1n), directOnly, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(5)
    await warm
    await vi.advanceTimersByTimeAsync(90)
    await read
    expect(relay.cancelled).toBe(1)
    expect(decisions.filter((value) => value.localBlockIndex === 0n).map((value) => value.purpose)).toEqual(['content', 'rescue'])
    lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('learns a faster relay through a bounded probe and uses it for subsequent content', async () => {
    const decisions: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ now: Date.now, onBlockScheduled: (value) => decisions.push(value) })
    const direct = new TimedLane(1, 100)
    lanes.add(direct, 'direct')
    const warm = lanes.fetch(demand(), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(100)
    await warm
    const relay = new TimedLane(2, 5)
    lanes.add(relay, 'application-relay')
    const sampled = lanes.fetch(demand(1n), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(5)
    await sampled
    const read = lanes.fetch(demand(2n), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(5)
    await read
    expect(decisions.filter((value) => value.route === 'application-relay').map((value) => value.purpose)).toEqual(['probe', 'content'])
    lanes.close()
  })

  it('does not let a failed standby probe abort healthy primary content', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    lanes.add(new TimedLane(1, 10), 'direct')
    const warm = lanes.fetch(demand(), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(10)
    await warm
    const probe = vi.fn(async () => { throw new Error('standby content unavailable') })
    lanes.add({ id: 2, fetchBlock: probe }, 'application-relay')
    const read = lanes.fetch(demand(1n), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(10)
    await expect(read).resolves.toMatchObject({ localBlockIndex: 1n })
    expect(probe).toHaveBeenCalledOnce()
    await lanes.waitForLeaseIdle(demand().leaseId)
    lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('never probes or rescues on a forbidden route and stops on caller cancellation', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    const direct = new TimedLane(1, 10_000)
    const relay = new TimedLane(2, 5)
    lanes.add(direct, 'direct')
    lanes.add(relay, 'application-relay')
    const controller = new AbortController()
    const read = lanes.fetch(demand(), { ...routes, allows: (route) => route === 'direct' }, controller.signal)
    const rejected = expect(read).rejects.toMatchObject({ name: 'AbortError' })
    await vi.advanceTimersByTimeAsync(1000)
    controller.abort(new DOMException('caller stopped', 'AbortError'))
    await rejected
    expect(relay.calls).toEqual([])
    expect(direct.cancelled).toBe(1)
    lanes.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('closes active reads without leaving delayed rescue timers behind', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    const direct = new TimedLane(1, 10_000)
    lanes.add(direct, 'direct')
    const read = lanes.fetch(demand(), routes, new AbortController().signal)
    const rejection = expect(read).rejects.toThrow('LaneSet is closed')
    lanes.close()
    await rejection
    await lanes.waitForLeaseIdle(demand().leaseId)
    expect(direct.cancelled).toBe(1)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('retains lease ownership until a losing probe actually settles', async () => {
    const lanes = new V2LaneSet({ now: Date.now })
    lanes.add(new TimedLane(1, 10), 'direct')
    const warm = lanes.fetch(demand(), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(10)
    await warm
    let finishProbe!: (record: V2BlockRecord) => void
    let probeSignal!: AbortSignal
    lanes.add({
      id: 2,
      fetchBlock: (_input, signal) => {
        probeSignal = signal
        return new Promise((resolve) => { finishProbe = resolve })
      },
    }, 'application-relay')
    const broker = new V2BlockBroker(lanes)
    const read = broker.readBlock(demand(1n), { routes })
    await vi.advanceTimersByTimeAsync(10)
    await read
    expect(probeSignal.aborted).toBe(true)
    let idle = false
    const barrier = broker.waitForLeaseIdle(demand().leaseId).then(() => { idle = true })
    await vi.advanceTimersByTimeAsync(0)
    expect(idle).toBe(false)
    finishProbe({ descriptor, localBlockIndex: 1n, data: new Uint8Array(BLOCK_BYTES) })
    await barrier
    expect(idle).toBe(true)
    broker.close()
    lanes.close()
  })

  it('isolates scheduling observers from content and its lease lifetime', async () => {
    const lanes = new V2LaneSet({
      now: Date.now,
      onBlockScheduled: () => { throw new Error('observer failed') },
    })
    lanes.add(new TimedLane(1, 5), 'direct')
    const read = lanes.fetch(demand(), routes, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(5)
    await expect(read).resolves.toMatchObject({ localBlockIndex: 0n })
    await lanes.waitForLeaseIdle(demand().leaseId)
    lanes.close()
  })

  it('reduces admission when a continuously busy path slows down', () => {
    const performance = new LanePerformance()
    performance.begin(0, 1024)
    performance.begin(0, 1024)
    performance.complete(1000, 1024, true)
    performance.begin(1000, 1024)
    performance.complete(11_000, 1024, true)
    expect(performance.bytesPerSecond).toBeCloseTo(102.4)
    expect(performance.pendingBytes).toBe(1024)
  })

  it('separates overlapping throughput, canceled samples, and exploration budgets', () => {
    const performance = new LanePerformance()
    performance.begin(0, BLOCK_BYTES)
    performance.begin(0, BLOCK_BYTES)
    performance.complete(1000, BLOCK_BYTES, true)
    expect(performance.bytesPerSecond).toBe(BLOCK_BYTES)
    expect(performance.estimate(BLOCK_BYTES)).toBe(2000)
    performance.complete(2000, BLOCK_BYTES, true)
    expect(performance.bytesPerSecond).toBe(BLOCK_BYTES)
    performance.begin(1_000_000, BLOCK_BYTES)
    performance.complete(1_001_000, BLOCK_BYTES, false)
    expect(performance.bytesPerSecond).toBe(BLOCK_BYTES)
    expect(completionCost(100, true)).toBeLessThan(completionCost(200, false))
    const unmeasured = new LanePerformance()
    unmeasured.superseded(BLOCK_BYTES, 100)
    expect(unmeasured.hasSuccessfulSample).toBe(false)
    expect(probeDue(unmeasured, 0)).toBe(true)
    const budget = new LaneExploration()
    expect(budget.acquire('probe', 0)).toBe(true)
    expect(budget.acquire('probe', 0)).toBe(false)
    expect(budget.acquire('rescue', 0)).toBe(true)
    expect(budget.acquire('rescue', 0)).toBe(false)
    budget.release('probe')
    budget.release('rescue')
    expect(budget.acquire('probe', 4999)).toBe(false)
    expect(budget.acquire('probe', 5000)).toBe(true)
    expect(rescueDue(99, 1000, 1)).toBe(false)
    expect(rescueDue(100, 1000, 1)).toBe(true)
  })
})
