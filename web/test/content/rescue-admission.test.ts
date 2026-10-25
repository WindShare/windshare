import { afterEach, describe, expect, it, vi } from 'vitest'
import { ContentAdmissions, type ContentAdmission } from '../../src/content/scheduling/admission'
import { BlockReceiveWindow } from '../../src/content/scheduling/receive-window'
import { LanePerformance } from '../../src/content/scheduling/performance'
import { LaneSampling } from '../../src/content/scheduling/sampling'
import { FileGeometry } from '../../src/content/geometry'
import type { ContentLane, ContentWork } from '../../src/content/scheduling/selection'

const BLOCK = 1024 * 1024
const descriptor = { shareInstance: new Uint8Array(16), shareInstanceId: 'share',
  fileId: new Uint8Array(16), fileIdText: 'file', fileRevision: new Uint8Array(16),
  fileRevisionText: 'revision', exactSize: BigInt(4 * BLOCK), geometry: new FileGeometry(BigInt(4 * BLOCK), BigInt(BLOCK)) }
const routes = { active: true, allows: () => true, assertActive() {}, subscribe: () => () => {} }
const flush = async () => { for (let step = 0; step < 8; step++) await Promise.resolve() }

function fixture() {
  vi.useFakeTimers(); vi.setSystemTime(0)
  const lane = (id: number, route: ContentLane['route']): ContentLane => ({
    lane: { id, async fetchBlock() { throw new Error('Admission cannot fetch') } }, laneEpoch: 0, route,
    performance: new LanePerformance(), sampling: new LaneSampling(), receive: new BlockReceiveWindow(Date.now),
    unsubscribeReceive() {}, inflight: 0, failed: false,
  })
  const fast = lane(1, 'turn'), unknown = lane(2, 'application-relay')
  const sample = fast.receive.tryAcquire(BLOCK)!
  vi.setSystemTime(100); sample.receive(BLOCK); sample.close()
  const backlog = fast.receive.tryAcquire(4 * BLOCK)!
  const admissions = new ContentAdmissions(() => [fast, unknown], Date.now)
  const unsubscribe = fast.receive.subscribeAvailable(() => admissions.wake())
  const controller = new AbortController()
  const work = (index: number): ContentWork => ({
    demand: { descriptor, leaseId: new Uint8Array(16), localBlockIndex: BigInt(index) },
    routes, signal: controller.signal, priority: 'download', distance: () => BigInt(index),
  })
  return { fast, unknown, backlog, admissions, controller, work, unsubscribe }
}
afterEach(() => vi.useRealTimers())

describe('frontier rescue admission', () => {
  it('waits for the best evidence and takes released credit before read-ahead', async () => {
    const f = fixture()
    const ordinary = f.admissions.request(f.work(1), f.controller.signal, lane => lane === f.fast).catch(() => undefined)
    let acquired = 0, released = 0, selected: ContentAdmission | undefined
    const rescue = f.admissions.requestRescue({ ...f.work(0), capacity: {
      readiness: () => 'ready', lastServed: () => 0,
      acquire: () => { acquired++; return () => { released++ } },
    } }, f.controller.signal, () => true, () => 'no-progress')
      .then(value => { selected = value; return value })
    await flush()
    expect(selected).toBeUndefined()
    expect(acquired).toBe(0)
    expect(f.unknown.inflight).toBe(0)
    expect(vi.getTimerCount()).toBe(1)

    vi.setSystemTime(600)
    f.backlog.receive(2.5 * BLOCK)
    const admitted = await rescue
    expect(admitted.lane).toBe(f.fast)
    expect(admitted.purpose).toBe('rescue')
    expect(acquired).toBe(1)
    expect(f.fast.inflight).toBe(1)
    f.controller.abort()
    expect(await ordinary).toBeUndefined()
    admitted.close('cancelled'); f.backlog.close(); f.unsubscribe()
    await flush()
    expect(released).toBe(1)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rechecks rescue justification when capacity arrives and cancels without a reservation', async () => {
    const f = fixture()
    let due = true, acquired = false
    const pending = f.admissions.requestRescue({ ...f.work(0), capacity: {
      readiness: () => 'ready', lastServed: () => 0,
      acquire: () => { acquired = true; return () => {} },
    } }, f.controller.signal, () => true, () => due ? 'no-progress' : undefined).catch(() => undefined)
    await flush()
    due = false
    f.backlog.close()
    await flush()
    expect(acquired).toBe(false)
    expect(vi.getTimerCount()).toBe(1)
    f.controller.abort(); await pending; f.unsubscribe()
    await flush()
    expect(vi.getTimerCount()).toBe(0)
  })
})
