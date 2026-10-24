import { describe, expect, it } from 'vitest'
import { LaneSampling, SAMPLE_ATTEMPT_BUDGET, SAMPLE_BYTE_BUDGET, SAMPLE_LIFETIME_MILLISECONDS } from '../../src/content/scheduling/sampling'

describe('bounded sampling evidence', () => {
  it('separates cancellation from observed capacity and counts overlapping busy time once', () => {
    const sampling = new LaneSampling()
    sampling.offered(100, 0)
    sampling.offered(100, 50)
    sampling.settled(100, 100, 'completed')
    expect(sampling.snapshot(150).busyMilliseconds).toBe(150)
    sampling.settled(100, 200, 'cancelled')
    expect(sampling.snapshot(5000)).toEqual({
      offeredBytes: 200, settledBytes: 200, completedBytes: 100, completedAttempts: 1,
      failedAttempts: 0, supersededAttempts: 0, cancelledAttempts: 1, busyMilliseconds: 200,
    })
  })

  it('publishes whole milliseconds for lossless diagnostic counters', () => {
    const sampling = new LaneSampling()
    sampling.offered(1, 0.25)
    sampling.settled(1, 10.75, 'completed')
    expect(sampling.snapshot(11).busyMilliseconds).toBe(10)
  })

  it('bounds accelerated sampling by bytes, attempts, wall time and withdrawal', () => {
    const sampling = new LaneSampling()
    expect(sampling.available(1, 0)).toBe(false)
    sampling.start(0)
    sampling.offered(SAMPLE_BYTE_BUDGET, 0)
    expect(sampling.available(1, 1)).toBe(false)
    sampling.settled(SAMPLE_BYTE_BUDGET, 1, 'failed')
    sampling.start(2)
    for (let attempt = 0; attempt < SAMPLE_ATTEMPT_BUDGET; attempt++) {
      expect(sampling.available(1, 2)).toBe(true)
      sampling.offered(1, 2)
      sampling.settled(1, 3, 'superseded')
    }
    expect(sampling.available(1, 3)).toBe(false)
    sampling.start(4)
    expect(sampling.available(1, 4 + SAMPLE_LIFETIME_MILLISECONDS)).toBe(false)
    sampling.start(5)
    sampling.stop()
    expect(sampling.available(1, 5)).toBe(false)
    const evidence = sampling.snapshot(6)
    expect(evidence.failedAttempts).toBe(1)
    expect(evidence.supersededAttempts).toBe(SAMPLE_ATTEMPT_BUDGET)
  })
})
