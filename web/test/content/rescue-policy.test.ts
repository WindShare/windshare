import { describe, expect, it } from 'vitest'
import { INITIAL_BLOCK_MILLISECONDS, type DeliveryEstimate, type DeliveryProgress } from '../../src/content/scheduling/completion'
import { MINIMUM_RESCUE_MILLISECONDS, rescueDecision, type RescueReason } from '../../src/content/scheduling/exploration'

describe('browser frontier rescue policy', () => {
  const cold: DeliveryEstimate = { evidence: 'unknown', milliseconds: INITIAL_BLOCK_MILLISECONDS }
  const recent = (milliseconds: number): DeliveryEstimate => ({ evidence: 'recent', milliseconds })
  const stale = (milliseconds: number): DeliveryEstimate => ({ evidence: 'stale', milliseconds })
  const cases: {
    name: string; progress: DeliveryProgress; initial: DeliveryEstimate
    alternative: DeliveryEstimate; reason?: RescueReason
  }[] = [
    {
      name: 'minimum delay protects transient startup',
      progress: { remainingBytes: 1000, elapsedMilliseconds: MINIMUM_RESCUE_MILLISECONDS - 1,
        silentMilliseconds: MINIMUM_RESCUE_MILLISECONDS - 1, remaining: cold },
      initial: cold, alternative: recent(1),
    },
    {
      name: 'measured alternatives help before bootstrap expires',
      progress: { remainingBytes: 1000, elapsedMilliseconds: MINIMUM_RESCUE_MILLISECONDS,
        silentMilliseconds: MINIMUM_RESCUE_MILLISECONDS, remaining: cold },
      initial: cold, alternative: recent(1), reason: 'faster-delivery',
    },
    {
      name: 'ongoing receipt can outlive the initial forecast',
      progress: { remainingBytes: 10, elapsedMilliseconds: 10000, silentMilliseconds: 10, remaining: recent(20) },
      initial: recent(100), alternative: recent(100),
    },
    {
      name: 'fully received blocks need no network rescue',
      progress: { remainingBytes: 0, elapsedMilliseconds: 5000, silentMilliseconds: 1000, remaining: recent(1) },
      initial: cold, alternative: recent(20),
    },
    {
      name: 'silent frontiers do not wait for twice an old forecast',
      progress: { remainingBytes: 131072, elapsedMilliseconds: 8824, silentMilliseconds: 8824, remaining: stale(250) },
      initial: recent(5056), alternative: recent(439), reason: 'no-progress',
    },
    {
      name: 'unknown alternatives cannot win by bootstrap score',
      progress: { remainingBytes: 1000, elapsedMilliseconds: 499, silentMilliseconds: 499, remaining: cold },
      initial: cold, alternative: cold,
    },
    {
      name: 'unknown alternatives retain bounded cold-start liveness',
      progress: { remainingBytes: 1000, elapsedMilliseconds: 500, silentMilliseconds: 500, remaining: cold },
      initial: cold, alternative: cold, reason: 'bootstrap-overdue',
    },
    {
      name: 'fresh progress prevents an unmeasured rescue',
      progress: { remainingBytes: 100, elapsedMilliseconds: 300, silentMilliseconds: 0, remaining: recent(100) },
      initial: recent(100), alternative: cold,
    },
    {
      name: 'startup gets one alternative delivery interval',
      progress: { remainingBytes: 1000, elapsedMilliseconds: 99, silentMilliseconds: 99, remaining: recent(1000) },
      initial: recent(1000), alternative: recent(100),
    },
    {
      name: 'old fast evidence can revalidate a slow frontier',
      progress: { remainingBytes: 1000, elapsedMilliseconds: 400, silentMilliseconds: 5, remaining: recent(1000) },
      initial: recent(1000), alternative: stale(100), reason: 'faster-delivery',
    },
    {
      name: 'stale speed is not counted at face value',
      progress: { remainingBytes: 1000, elapsedMilliseconds: 400, silentMilliseconds: 5, remaining: recent(300) },
      initial: recent(1000), alternative: stale(100),
    },
  ]
  it.each(cases)('$name', ({ progress, initial, alternative, reason }) => {
    expect(rescueDecision(progress, initial, alternative)).toBe(reason)
  })
})
