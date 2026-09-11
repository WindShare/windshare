import { afterEach, describe, expect, it, vi } from 'vitest'
import { ReceiverPathActivity, RECENT_CONTENT_WINDOW_MILLISECONDS, type ReceiverPathActivitySnapshot } from '../../src/receiver/path-activity'
import type { V2ContentLaneAdmissionObservation } from '../../src/connectivity/v2-receiver-policy'
import type { V2BlockRouteObservation } from '../../src/content/v2-lane-set'
import { connectedChannelCount, presentReceiverPathActivity } from '../../src/ui/connection/path-presentation'

afterEach(() => vi.useRealTimers())

const DIRECT = { laneId: 3, laneEpoch: 2, route: 'direct' } as const
const RELAY = { laneId: 1, laneEpoch: 0, route: 'application-relay' } as const
const TURN = { laneId: 5, laneEpoch: 1, route: 'turn' } as const

function fetched(lane: V2ContentLaneAdmissionObservation, usefulBytes = 12): V2BlockRouteObservation {
  return { ...lane, dispatchSequence: 1, fileId: 'file', localBlockIndex: 0n, usefulBytes }
}

function fixture() {
  const activity = new ReceiverPathActivity(() => Date.now())
  const facts: ReceiverPathActivitySnapshot[] = []
  activity.subscribe(fact => facts.push(fact))
  activity.generationInstalled(1)
  return { activity, facts, latest: () => facts.at(-1)! }
}

describe('receiver path activity', () => {
  it('tracks every admitted channel and expires each channel independently without per-block rendering', async () => {
    vi.useFakeTimers()
    const { activity, facts, latest } = fixture()
    activity.admitted(1, DIRECT)
    activity.admitted(1, TURN)
    activity.admitted(1, RELAY)
    expect(latest().lanes).toEqual([RELAY, DIRECT, TURN].map(lane => ({ ...lane, recentContent: false })))
    expect(connectedChannelCount(latest())).toBe('3 channels connected')
    expect(presentReceiverPathActivity(latest())).toBe('Direct connected')
    expect(Object.isFrozen(latest().lanes)).toBe(true)
    expect(Object.isFrozen(latest().lanes[0])).toBe(true)

    const idle = latest()
    activity.fetched(1, fetched(RELAY, 0))
    expect(latest()).toBe(idle)
    activity.fetched(1, fetched(RELAY))
    expect(latest().lanes.map(lane => lane.recentContent)).toEqual([true, false, false])
    await vi.advanceTimersByTimeAsync(1_000)
    activity.fetched(1, fetched(DIRECT))
    const updates = facts.length
    activity.fetched(1, fetched(DIRECT))
    activity.admitted(1, DIRECT)
    expect(facts).toHaveLength(updates)
    expect(presentReceiverPathActivity(latest())).toBe('Received directly and through relay in the last 5 seconds')
    await vi.advanceTimersByTimeAsync(RECENT_CONTENT_WINDOW_MILLISECONDS - 1_000)
    expect(latest().lanes.map(lane => lane.recentContent)).toEqual([false, true, false])
    await vi.advanceTimersByTimeAsync(1_000)
    expect(latest()).toEqual(idle)
    expect(vi.getTimerCount()).toBe(0)
    activity.close()
  })

  it('rejects stale epochs, retired generations and unadmitted content without reviving old channels', () => {
    vi.useFakeTimers()
    const { activity, latest } = fixture()
    activity.admitted(1, DIRECT)
    activity.fetched(1, fetched(DIRECT))
    const replacement = { ...DIRECT, laneEpoch: 3 }
    activity.admitted(1, replacement)
    activity.detached(1, DIRECT)
    activity.fetched(1, fetched(DIRECT))
    activity.fetched(1, fetched({ ...replacement, route: 'turn' }))
    activity.fetched(1, fetched(RELAY))
    expect(latest().lanes).toEqual([{ ...replacement, recentContent: false }])
    activity.generationInstalled(1)
    expect(latest().lanes).toHaveLength(1)
    activity.fetched(1, fetched(replacement))
    activity.generationRetired(1)
    expect(latest().lanes).toEqual([])
    expect(vi.getTimerCount()).toBe(0)
    activity.admitted(1, replacement)
    activity.fetched(1, fetched(replacement))
    expect(latest().lanes).toEqual([])
    activity.generationInstalled(2)
    activity.admitted(2, DIRECT)
    activity.generationRetired(1)
    activity.detached(1, DIRECT)
    activity.fetched(1, fetched(DIRECT))
    expect(latest().lanes).toEqual([{ ...DIRECT, recentContent: false }])
    activity.fetched(2, fetched(DIRECT))
    activity.generationInstalled(3)
    expect(latest().lanes).toEqual([])
    expect(vi.getTimerCount()).toBe(0)
    activity.close()
  })

  it('removes only the disconnected channel and never labels TURN traffic as direct', () => {
    vi.useFakeTimers()
    const { activity, latest } = fixture()
    activity.admitted(1, DIRECT)
    activity.admitted(1, TURN)
    activity.fetched(1, fetched(DIRECT))
    activity.fetched(1, fetched(TURN))
    activity.detached(1, DIRECT)
    expect(latest().lanes).toEqual([{ ...TURN, recentContent: true }])
    expect(connectedChannelCount(latest())).toBe('1 channel connected')
    expect(presentReceiverPathActivity(latest())).toBe('Received through relay in the last 5 seconds')
    activity.detached(1, TURN)
    expect(connectedChannelCount(latest())).toBe('0 channels connected')
    expect(presentReceiverPathActivity(latest())).toBeNull()
    expect(vi.getTimerCount()).toBe(0)
    activity.close()
  })

  it('keeps observers passive and closes subscriptions and expiry timers', async () => {
    vi.useFakeTimers()
    const { activity, facts, latest } = fixture()
    expect(() => activity.subscribe(() => { throw new Error('observer failed') })).not.toThrow()
    const observer = vi.fn()
    const unsubscribe = activity.subscribe(observer)
    activity.admitted(1, DIRECT)
    activity.fetched(1, fetched(DIRECT))
    unsubscribe()
    const count = observer.mock.calls.length
    activity.close()
    expect(latest().lanes).toEqual([])
    const updates = facts.length
    activity.generationInstalled(2)
    activity.admitted(2, DIRECT)
    activity.fetched(2, fetched(DIRECT))
    activity.detached(2, DIRECT)
    activity.close()
    const closedObserver = vi.fn()
    activity.subscribe(closedObserver)()
    await vi.advanceTimersByTimeAsync(RECENT_CONTENT_WINDOW_MILLISECONDS)
    expect(facts).toHaveLength(updates)
    expect(observer).toHaveBeenCalledTimes(count)
    expect(closedObserver).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
  })
})
