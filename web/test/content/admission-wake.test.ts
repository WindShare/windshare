import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ContentAdmissions, type ContentAdmission } from '../../src/content/scheduling/admission'
import { BlockReceiveWindow } from '../../src/content/scheduling/receive-window'
import { LanePerformance } from '../../src/content/scheduling/performance'
import { LaneSampling, SAMPLE_ATTEMPT_BUDGET } from '../../src/content/scheduling/sampling'
import { PROBE_INTERVAL_MILLISECONDS } from '../../src/content/scheduling/exploration'
import type { ContentLane, ContentWork } from '../../src/content/scheduling/selection'
import type { V2BlockRouteEligibility } from '../../src/content/v2-route-policy'
import { FileGeometry } from '../../src/content/geometry'

const BLOCK_BYTES = 1024 * 1024
const routes: V2BlockRouteEligibility = {
  active: true, allows: () => true, assertActive() {}, subscribe: () => () => {},
}
function work(index: number, signal: AbortSignal): ContentWork {
  return {
    demand: {
      descriptor: { shareInstance: new Uint8Array(16), shareInstanceId: 'share',
        fileId: new Uint8Array(16), fileIdText: 'file', fileRevision: new Uint8Array(16),
        fileRevisionText: 'revision', exactSize: BigInt(16 * BLOCK_BYTES),
        geometry: new FileGeometry(BigInt(16 * BLOCK_BYTES), BigInt(BLOCK_BYTES)) },
      leaseId: new Uint8Array(16), localBlockIndex: BigInt(index),
    },
    routes, signal, priority: 'download', distance: () => BigInt(index),
  }
}
function lane(id: number, route: ContentLane['route'], rate: number): ContentLane {
  const performance = new LanePerformance()
  performance.bytesPerSecond = rate
  performance.hasSuccessfulSample = true
  performance.lastAttempt = Date.now()
  return { lane: { id, fetchBlock: async () => { throw new Error('Admission cannot fetch content') } },
    laneEpoch: 0, route, performance, sampling: new LaneSampling(), receive: new BlockReceiveWindow(Date.now),
    unsubscribeReceive() {}, inflight: 0, failed: false }
}
async function flush() { for (let step = 0; step < 8; step++) await Promise.resolve() }
function fixture() {
  const lanes = [lane(1, 'direct', 10 * BLOCK_BYTES), lane(2, 'application-relay', BLOCK_BYTES)]
  const admissions = new ContentAdmissions(() => lanes, Date.now)
  const controller = new AbortController()
  const request = (input: ContentWork) => admissions.request(input, input.signal, () => true)
  return { lanes, admissions, controller, request }
}

describe('admission wake ownership', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(0) })
  afterEach(() => vi.useRealTimers())

  it('gives a new path bounded real work before the periodic probe deadline', async () => {
    const { lanes, request, controller } = fixture()
    const primary = await request(work(0, controller.signal))
    lanes[1]!.sampling.start(Date.now())
    for (let index = 1; index <= SAMPLE_ATTEMPT_BUDGET; index++) {
      const sample = await request(work(index, controller.signal))
      expect(sample.purpose).toBe('sample')
      expect(sample.lane.lane.id).toBe(2)
      await vi.advanceTimersByTimeAsync(1000)
      sample.close('completed')
    }
    expect(lanes[1]!.sampling.snapshot(Date.now()).completedAttempts).toBe(SAMPLE_ATTEMPT_BUDGET)
    let admitted = false
    const pending = request(work(9, controller.signal)).then(() => { admitted = true }).catch(() => {})
    await vi.advanceTimersByTimeAsync(100)
    expect(admitted).toBe(false)
    controller.abort()
    await pending
    primary.close('completed')
    await flush()
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(['preview', 'route', 'consumer'] as const)('sampling preserves %s authority', async constraint => {
    const { lanes, request, controller } = fixture()
    const primary = await request(work(0, controller.signal))
    lanes[1]!.sampling.start(Date.now())
    let candidate: ContentWork = work(1, controller.signal)
    if (constraint === 'preview') candidate = { ...candidate, priority: 'preview', distance: () => 0n }
    if (constraint === 'route') candidate = { ...candidate, routes: { ...routes, allows: route => route === 'direct' } }
    if (constraint === 'consumer') candidate = { ...candidate, capacity: {
      readiness: () => 'consumer', lastServed: () => 0, acquire: () => { throw new Error('Blocked consumer acquired capacity') },
    } }
    let admitted = false
    const pending = request(candidate).then(() => { admitted = true }).catch(() => {})
    await vi.advanceTimersByTimeAsync(1000)
    expect(admitted).toBe(false)
    expect(lanes[1]!.sampling.snapshot(Date.now()).offeredBytes).toBe(0)
    controller.abort()
    await pending
    primary.close('cancelled')
  })

  it('probes at the eligible deadline without receipts and preserves the global probe interval', async () => {
    const { lanes, request, controller } = fixture()
    const primary = await request(work(0, controller.signal))
    let sampled: ContentAdmission | undefined
    const pending = request(work(1, controller.signal)).then(value => { sampled = value; return value })
    await flush()
    expect(vi.getTimerCount()).toBe(1)
    await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MILLISECONDS - 1)
    expect(sampled).toBeUndefined()
    await vi.advanceTimersByTimeAsync(1)
    const probe = await pending
    expect(probe.lane.lane.id).toBe(2)
    expect(probe.purpose).toBe('probe')
    expect(vi.getTimerCount()).toBe(0)
    probe.close('completed')
    lanes.splice(1, 1, lane(3, 'turn', BLOCK_BYTES))
    lanes[1]!.performance.lastAttempt = undefined
    sampled = undefined
    const next = request(work(2, controller.signal)).then(value => { sampled = value; return value })
    await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MILLISECONDS - 1)
    expect(sampled).toBeUndefined()
    await vi.advanceTimersByTimeAsync(1)
    const nextProbe = await next
    expect(nextProbe.lane.lane.id).toBe(3)
    expect(nextProbe.purpose).toBe('probe')
    nextProbe.close('completed'); primary.close('completed')
    await flush()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('uses capacity release events without polling or delaying ready work', async () => {
    const { request, admissions, controller } = fixture()
    const primary = await request(work(0, controller.signal))
    let available = false
    let admitted = false
    const pending = request({ ...work(1, controller.signal), capacity: {
      lastServed: () => 0, readiness: () => available ? 'ready' : 'consumer', acquire: () => () => {},
    } }).then(value => { admitted = true; return value })
    await flush()
    expect(vi.getTimerCount()).toBe(0)
    await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MILLISECONDS)
    expect(admitted).toBe(false)
    available = true
    admissions.wake()
    const probe = await pending
    expect(probe.purpose).toBe('probe')
    expect(vi.getTimerCount()).toBe(0)
    probe.close('completed'); primary.close('completed')
  })

  it('recomputes an armed deadline when route authority changes', async () => {
    const { request, controller } = fixture()
    const primary = await request(work(0, controller.signal))
    let relayAllowed = true
    const listeners = new Set<() => void>()
    const mutableRoutes: V2BlockRouteEligibility = { ...routes,
      allows: route => route === 'direct' || relayAllowed,
      subscribe: listener => { listeners.add(listener); return () => { listeners.delete(listener) } },
    }
    const pending = request({ ...work(1, controller.signal), routes: mutableRoutes })
    await flush()
    expect(vi.getTimerCount()).toBe(1)
    relayAllowed = false
    for (const listener of listeners) listener()
    await flush()
    expect(vi.getTimerCount()).toBe(0)
    await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MILLISECONDS)
    relayAllowed = true
    for (const listener of listeners) listener()
    const probe = await pending
    expect(probe.purpose).toBe('probe')
    expect(listeners.size).toBe(0)
    probe.close('completed'); primary.close('completed')
  })

  it.each(['cancel', 'remove-lane'] as const)('retires the deadline on %s without an orphan timer', async action => {
    const { request, lanes, admissions, controller } = fixture()
    const primary = await request(work(0, controller.signal))
    const pending = request(work(1, controller.signal)).catch(() => undefined)
    await flush()
    expect(vi.getTimerCount()).toBe(1)
    if (action === 'cancel') controller.abort()
    else { lanes.splice(1); admissions.wake() }
    await flush()
    expect(vi.getTimerCount()).toBe(0)
    controller.abort()
    await pending
    primary.close('cancelled')
    await flush()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('coalesces reentrant admission wakes into one pending deadline', async () => {
    const { request, admissions, controller } = fixture()
    const first = request({ ...work(0, controller.signal), capacity: {
      lastServed: () => 0, readiness: () => 'ready',
      acquire: () => { admissions.wake(); return () => {} },
    } })
    const pending = request(work(1, controller.signal)).catch(() => undefined)
    const primary = await first
    await flush()
    expect(vi.getTimerCount()).toBe(1)
    controller.abort()
    await pending
    primary.close('cancelled')
    await flush()
    expect(vi.getTimerCount()).toBe(0)
  })
})
