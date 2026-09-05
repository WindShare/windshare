import { afterEach, describe, expect, it, vi } from 'vitest'
import { ReceiverPathActivity, type ReceiverPathActivitySnapshot } from '../../src/receiver/path-activity'
import type { V2BlockRouteObservation } from '../../src/content/v2-lane-set'
import { presentReceiverPathActivity } from '../../src/ui/connection/path-presentation'

afterEach(() => vi.useRealTimers())
function fetched(route: V2BlockRouteObservation['route'], usefulBytes = 12): V2BlockRouteObservation {
  return { dispatchSequence: 1, laneId: 3, laneEpoch: 2, route, fileId: 'file', localBlockIndex: 0n, usefulBytes }
}

describe('receiver path activity', () => {
  it('separates admitted direct evidence from useful recent relay/direct content and expires silence', async () => {
    vi.useFakeTimers()
    const activity = new ReceiverPathActivity(() => Date.now())
    const facts: ReceiverPathActivitySnapshot[] = []
    activity.generationInstalled(1)
    activity.subscribe((fact) => facts.push(fact))
    activity.admitted(1, { laneId: 3, laneEpoch: 2, route: 'direct' })
    expect(facts.at(-1)).toEqual({ directConnected: true, content: 'idle' })
    expect(presentReceiverPathActivity(facts.at(-1)!)).toBe('Direct connected')
    activity.fetched(1, fetched('application-relay', 0))
    expect(facts.at(-1)?.content).toBe('idle')
    activity.fetched(1, fetched('application-relay'))
    expect(facts.at(-1)?.content).toBe('relay')
    await vi.advanceTimersByTimeAsync(1_000)
    activity.fetched(1, fetched('direct'))
    expect(facts.at(-1)?.content).toBe('parallel')
    expect(presentReceiverPathActivity(facts.at(-1)!)).toBe('Receiving directly and through relay')
    await vi.advanceTimersByTimeAsync(4_000)
    expect(facts.at(-1)?.content).toBe('direct')
    await vi.advanceTimersByTimeAsync(1_000)
    expect(facts.at(-1)).toEqual({ directConnected: true, content: 'idle' })
    expect(vi.getTimerCount()).toBe(0)
    activity.close()
  })

  it('never calls TURN direct and rejects stale epoch and retired generation facts', () => {
    const activity = new ReceiverPathActivity()
    let latest: ReceiverPathActivitySnapshot | undefined
    activity.subscribe((fact) => { latest = fact })
    activity.generationInstalled(1)
    activity.admitted(1, { laneId: 3, laneEpoch: 2, route: 'turn' })
    expect(latest?.directConnected).toBe(false)
    activity.admitted(1, { laneId: 3, laneEpoch: 3, route: 'direct' })
    activity.detached(1, { laneId: 3, laneEpoch: 2, route: 'direct' })
    expect(latest?.directConnected).toBe(true)
    activity.generationRetired(1)
    activity.admitted(1, { laneId: 3, laneEpoch: 4, route: 'direct' })
    expect(latest?.directConnected).toBe(false)
    activity.generationInstalled(2)
    activity.admitted(1, { laneId: 3, laneEpoch: 3, route: 'direct' })
    activity.fetched(1, fetched('direct'))
    expect(latest).toEqual({ directConnected: false, content: 'idle' })
    activity.fetched(2, fetched('turn'))
    expect(latest).toEqual({ directConnected: false, content: 'relay' })
    activity.close()
  })
})
