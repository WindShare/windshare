import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { vi } from 'vitest'
import { LaneRequests, type LaneRequestKind, type RequestSchedulingObservation } from '../../src/content/scheduling/requests'
import { LanePerformance } from '../../src/content/scheduling/performance'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'
import type { V2BlockRouteEligibility } from '../../src/content/v2-route-policy'

const DIRECT_ID = 2
const RELAY_ID = 1
const DIRECT_MS = 2
const RELAY_MS = 1000
const CONTENT_BLOCK_BYTES = 1024
const ALL_ROUTES: V2BlockRouteEligibility = {
  active: true, allows: () => true, assertActive: () => undefined, subscribe: () => () => undefined,
}
function pending<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((accept, decline) => { resolve = accept; reject = decline })
  return { promise, resolve, reject }
}
function router(observations: RequestSchedulingObservation[] = []): LaneRequests {
  const requests = new LaneRequests({ now: Date.now, observe: value => observations.push(value) })
  requests.add({ id: RELAY_ID, epoch: 0, route: 'application-relay' })
  requests.add({ id: DIRECT_ID, epoch: 1, route: 'direct' })
  return requests
}
function reply(laneId: number): Promise<number> {
  return new Promise(resolve => setTimeout(() => resolve(laneId), laneId === DIRECT_ID ? DIRECT_MS : RELAY_MS))
}

describe('latency-sensitive request routing', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(0) })
  afterEach(() => vi.useRealTimers())

  it('keeps revision opens and releases on the fast path instead of alternating by request kind', async () => {
    const observed: RequestSchedulingObservation[] = []
    const requests = router(observed)
    for (let file = 0; file < 20; file += 1) {
      for (const kind of ['open_revisions', 'release_lease'] as const) {
        const request = requests.run({ kind }, route => reply(route.laneId))
        await vi.advanceTimersByTimeAsync(DIRECT_MS)
        expect(await request).toBe(DIRECT_ID)
      }
    }
    expect(Date.now()).toBe(80)
    expect(observed.filter(value => value.transition === 'dispatched').every(value => value.laneId === DIRECT_ID)).toBe(true)
    requests.close()
  })

  it.each<LaneRequestKind>(['open_revisions', 'list_children', 'renew_lease', 'release_lease'])(
    'prices one cold content block as one reservation for %s', async kind => {
      const observed: RequestSchedulingObservation[] = []
      const content = new LanePerformance()
      content.begin(0, CONTENT_BLOCK_BYTES)
      const requests = new LaneRequests({ now: Date.now, observe: value => observed.push(value) })
      requests.add({ id: DIRECT_ID, epoch: 0, route: 'direct', content })
      await requests.run({ kind }, async () => undefined)
      expect(observed[0]?.expectedMilliseconds).toBe(275)
      requests.close()
    },
  )

  it('keeps a cold content queue competitive with a measured slower relay', async () => {
    const requests = new LaneRequests({ now: Date.now })
    requests.add({ id: RELAY_ID, epoch: 0, route: 'application-relay' })
    const relaySample = requests.run({ kind: 'open_revisions' }, () =>
      new Promise(resolve => setTimeout(resolve, 400)))
    await vi.advanceTimersByTimeAsync(400)
    await relaySample
    const content = new LanePerformance()
    content.begin(Date.now(), CONTENT_BLOCK_BYTES)
    requests.add({ id: DIRECT_ID, epoch: 0, route: 'direct', content })
    expect(await requests.run({ kind: 'open_revisions' }, async route => route.laneId)).toBe(DIRECT_ID)
    content.begin(Date.now(), CONTENT_BLOCK_BYTES)
    expect(await requests.run({ kind: 'open_revisions' }, async route => route.laneId)).toBe(RELAY_ID)
    requests.close()
  })

  it('reserves concurrent requests before another caller makes its decision', async () => {
    const observed: RequestSchedulingObservation[] = []
    const requests = new LaneRequests({ now: Date.now, observe: value => observed.push(value) })
    requests.add({ id: 1, epoch: 1, route: 'direct' })
    requests.add({ id: 2, epoch: 1, route: 'direct' })
    const deferred = pending<number>()
    const first = requests.run({ kind: 'open_revisions' }, () => deferred.promise)
    const second = requests.run({ kind: 'open_revisions' }, async route => route.laneId)
    expect(await second).toBe(2)
    deferred.resolve(1)
    await first
    expect(observed.filter(value => value.transition === 'dispatched').map(value => value.pendingRequests)).toEqual([1, 1])
    requests.close()
  })

  it('learns a faster relay and isolates directory scan latency from revision latency', async () => {
    const requests = router()
    const slow = requests.run({ kind: 'open_revisions' }, route =>
      new Promise<number>(resolve => setTimeout(() => resolve(route.laneId), RELAY_MS)))
    await vi.advanceTimersByTimeAsync(RELAY_MS)
    await slow
    expect(await requests.run({ kind: 'open_revisions' }, async route => route.laneId)).toBe(RELAY_ID)
    expect(await requests.run({ kind: 'list_children' }, async route => route.laneId)).toBe(DIRECT_ID)
    requests.close()
  })

  it('honors route authority and waits for an allowed lane without polling', async () => {
    const requests = new LaneRequests()
    requests.add({ id: 1, epoch: 0, route: 'application-relay' })
    const directOnly = { ...ALL_ROUTES, allows: (route: string) => route === 'direct' }
    let invoked = false
    const waiting = requests.run({ kind: 'open_revisions', routes: directOnly }, async route => {
      invoked = true
      return route.laneId
    })
    await Promise.resolve()
    expect(invoked).toBe(false)
    expect(vi.getTimerCount()).toBe(0)
    requests.add({ id: 2, epoch: 2, route: 'direct' })
    expect(await waiting).toBe(2)
    requests.close()
  })

  it('does not send on a retired reservation or charge completion to its replacement', async () => {
    const requests = router()
    const deferred = pending<number>()
    const first = requests.run({ kind: 'open_revisions' }, () => deferred.promise)
    await Promise.resolve()
    requests.remove(DIRECT_ID, 1)
    requests.add({ id: DIRECT_ID, epoch: 2, route: 'direct' })
    expect(await requests.run({ kind: 'open_revisions' }, async route => route.laneId)).toBe(DIRECT_ID)
    deferred.reject(new V2SessionRuntimeError('lane', 'old lane closed'))
    await expect(first).rejects.toMatchObject({ scope: 'lane' })
    expect(await requests.run({ kind: 'open_revisions' }, async route => route.laneId)).toBe(DIRECT_ID)
    requests.close()
  })

  it('reselects if the chosen lane detaches before dispatch', async () => {
    const requests = router()
    const result = requests.run({ kind: 'open_revisions' }, async route => route.laneId)
    requests.remove(DIRECT_ID, 1)
    expect(await result).toBe(RELAY_ID)
    requests.close()
  })

  it('rechecks changed route authority before dispatch and releases the unused reservation', async () => {
    const observed: RequestSchedulingObservation[] = []
    const requests = router(observed)
    let directAllowed = true
    const routes = { ...ALL_ROUTES, allows: (route: string) => route !== 'direct' || directAllowed }
    const result = requests.run({ kind: 'open_revisions', routes }, async route => route.laneId)
    directAllowed = false
    expect(await result).toBe(RELAY_ID)
    directAllowed = true
    const directOnly = { ...routes, allows: (route: string) => route === 'direct' }
    expect(await requests.run({ kind: 'open_revisions', routes: directOnly }, async route => route.laneId)).toBe(DIRECT_ID)
    expect(observed.filter(value => value.laneId === DIRECT_ID && value.transition === 'dispatched'))
      .toMatchObject([{ pendingRequests: 1 }])
    requests.close()
  })

  it('cleans up route subscriptions that admit a lane synchronously', async () => {
    const requests = new LaneRequests()
    let unsubscribed = 0
    const routes = { ...ALL_ROUTES, subscribe: (wake: () => void) => {
      requests.add({ id: DIRECT_ID, epoch: 1, route: 'direct' })
      wake()
      return () => { unsubscribed += 1 }
    } }
    expect(await requests.run({ kind: 'open_revisions', routes }, async route => route.laneId)).toBe(DIRECT_ID)
    expect(unsubscribed).toBe(1)
    requests.close()
  })

  it('releases failed reservations and never duplicates an uncertain lease-creating operation', async () => {
    const requests = router()
    let calls = 0
    const failure = new V2SessionRuntimeError('lane', 'send uncertain')
    await expect(requests.run({ kind: 'open_revisions' }, async () => { calls += 1; throw failure })).rejects.toBe(failure)
    expect(calls).toBe(1)
    expect(await requests.run({ kind: 'open_revisions' }, async route => route.laneId)).toBe(RELAY_ID)
    requests.close()
  })

  it('cancels waiting and active requests and isolates diagnostic failures', async () => {
    const requests = new LaneRequests({ observe: () => { throw new Error('diagnostic failure') } })
    const cancel = new AbortController()
    const waiting = requests.run({ kind: 'open_revisions', signal: cancel.signal }, async () => 0)
    const rejection = expect(waiting).rejects.toMatchObject({ name: 'AbortError' })
    cancel.abort()
    await rejection
    requests.add({ id: DIRECT_ID, epoch: 1, route: 'direct' })
    const active = requests.run({ kind: 'renew_lease' }, route => new Promise((_, reject) => {
      route.signal.addEventListener('abort', () => reject(route.signal.reason), { once: true })
    }))
    await Promise.resolve()
    requests.close()
    await expect(active).rejects.toThrow('Request lanes are closed')
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each<LaneRequestKind>(['open_revisions', 'list_children', 'renew_lease', 'release_lease'])(
    'captures route, cost, and outcome for %s', async kind => {
      const observed: RequestSchedulingObservation[] = []
      const requests = router(observed)
      expect(await requests.run({ kind }, async route => route.laneId)).toBe(DIRECT_ID)
      expect(observed).toMatchObject([
        { sequence: 1, kind, laneId: DIRECT_ID, laneEpoch: 1, route: 'direct', transition: 'dispatched', pendingRequests: 1 },
        { sequence: 1, kind, laneId: DIRECT_ID, transition: 'completed' },
      ])
      requests.close()
    },
  )
})
