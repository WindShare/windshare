import { describe, expect, it } from 'vitest'
import { BlockAttempts } from '../../src/content/scheduling/attempts'
import { LanePerformance } from '../../src/content/scheduling/performance'
import { LaneSampling } from '../../src/content/scheduling/sampling'
import { BlockReceiveWindow } from '../../src/content/scheduling/receive-window'
import type { ContentLane } from '../../src/content/scheduling/selection'

function lane(): ContentLane {
  return { lane: { id: 1, async fetchBlock() { throw new Error('Policy cannot fetch') } },
    laneEpoch: 0, route: 'direct', inflight: 0, failed: false, unsubscribeReceive() {},
    performance: new LanePerformance(), sampling: new LaneSampling(), receive: new BlockReceiveWindow(() => 0) }
}
function deliver(state: ContentLane) {
  state.performance.begin(0, 1)
  state.performance.complete(1, 1, true)
}

describe('block attempt exclusion', () => {
  it('does not duplicate an active attempt when another block succeeds', () => {
    const attempts = new BlockAttempts()
    const state = lane()
    attempts.started(state, 1)
    deliver(state)
    expect(attempts.allows(state)).toBe(false)
    attempts.failed(state)
    expect(attempts.allows(state)).toBe(false)
    deliver(state)
    expect(attempts.allows(state)).toBe(true)
    attempts.started(state, 3)
    attempts.failed(state)
    expect(attempts.allows(state)).toBe(false)
  })

  it('does not treat partial receipt or cancelled work as authenticated recovery', () => {
    const attempts = new BlockAttempts()
    const state = lane()
    attempts.started(state, 1)
    attempts.failed(state)
    const permit = state.receive.tryAcquire(1)!
    permit.receive(1)
    state.performance.begin(0, 1)
    state.performance.complete(1, 1, false)
    expect(attempts.allows(state)).toBe(false)
    permit.close()
  })

  it('isolates a broken diagnostic sink from retry authority', () => {
    const attempts = new BlockAttempts(() => { throw new Error('observer failed') })
    const state = lane()
    attempts.started(state, 1)
    attempts.failed(state)
    deliver(state)
    expect(() => attempts.started(state, 2)).not.toThrow()
    attempts.failed(state)
    expect(() => attempts.reportExhausted()).not.toThrow()
    expect(attempts.allows(state)).toBe(false)
  })
})
