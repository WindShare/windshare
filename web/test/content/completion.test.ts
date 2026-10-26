import { describe, expect, it } from 'vitest'
import {
  estimateCompletion, estimateQueueMilliseconds, fitsReadAhead, INITIAL_BLOCK_MILLISECONDS,
  type DeliveryEstimate, type DeliveryLoad,
} from '../../src/content/scheduling/completion'

// Browser forecasts can evolve with local output constraints without changing
// the native receiver or an interoperability fixture.
describe('browser delivery forecasts', () => {
  const block = 1000
  const maximumEstimateMs = 60 * 60 * 1000
  const cases: {
    name: string; load: DeliveryLoad; bytes: number; queueMs: number; expected: DeliveryEstimate
  }[] = [
    {
      name: 'cold idle lanes have no measured capacity',
      load: { remainingBytes: 0, pendingResponses: 0, sample: { kind: 'unknown' } },
      bytes: block, queueMs: 0, expected: { evidence: 'unknown', milliseconds: INITIAL_BLOCK_MILLISECONDS },
    },
    {
      name: 'cold queues count outstanding responses',
      load: { remainingBytes: 5000, pendingResponses: 2, sample: { kind: 'unknown' } },
      bytes: block, queueMs: 2 * INITIAL_BLOCK_MILLISECONDS,
      expected: { evidence: 'unknown', milliseconds: 3 * INITIAL_BLOCK_MILLISECONDS },
    },
    {
      name: 'receipt reduces remaining network work',
      load: { remainingBytes: block, pendingResponses: 2,
        sample: { kind: 'recent', bytesPerSecond: 5000, startupMilliseconds: 100 } },
      bytes: block, queueMs: 200, expected: { evidence: 'recent', milliseconds: 400 },
    },
    {
      name: 'response startup overlaps a short queue',
      load: { remainingBytes: block, pendingResponses: 1,
        sample: { kind: 'recent', bytesPerSecond: 10000, startupMilliseconds: 200 } },
      bytes: block, queueMs: 100, expected: { evidence: 'recent', milliseconds: 300 },
    },
    {
      name: 'queued transmission hides response startup',
      load: { remainingBytes: 5000, pendingResponses: 5,
        sample: { kind: 'recent', bytesPerSecond: 10000, startupMilliseconds: 200 } },
      bytes: block, queueMs: 500, expected: { evidence: 'recent', milliseconds: 600 },
    },
    {
      name: 'authentication does not occupy the network queue',
      load: { remainingBytes: 0, pendingResponses: 2,
        sample: { kind: 'recent', bytesPerSecond: 10000, startupMilliseconds: 200 } },
      bytes: block, queueMs: 0, expected: { evidence: 'recent', milliseconds: 300 },
    },
    {
      name: 'stale evidence remains explicit',
      load: { remainingBytes: 0, pendingResponses: 0,
        sample: { kind: 'stale', bytesPerSecond: 1000, startupMilliseconds: 0 } },
      bytes: block, queueMs: 0, expected: { evidence: 'stale', milliseconds: 1000 },
    },
    {
      name: 'very slow forecasts remain bounded',
      load: { remainingBytes: 1024 * 1024, pendingResponses: 1,
        sample: { kind: 'recent', bytesPerSecond: 1e-6, startupMilliseconds: 0 } },
      bytes: 1, queueMs: maximumEstimateMs, expected: { evidence: 'recent', milliseconds: maximumEstimateMs },
    },
  ]
  it.each(cases)('$name', ({ load, bytes, queueMs, expected }) => {
    expect(estimateCompletion(load, bytes)).toEqual(expected)
    expect(estimateQueueMilliseconds(load)).toBe(queueMs)
  })
})

describe('browser read-ahead admission', () => {
  const block = 1000, aggregateRate = 5000
  const cases: { name: string; estimate: DeliveryEstimate; distance: bigint; fits: boolean }[] = [
    { name: 'frontiers have no read-ahead slack', estimate: { evidence: 'recent', milliseconds: 100 }, distance: 0n, fits: false },
    { name: 'independent capacity finishes before consumption', estimate: { evidence: 'recent', milliseconds: 500 }, distance: 4n, fits: true },
    { name: 'slow spare lanes would create the next output hole', estimate: { evidence: 'recent', milliseconds: 500 }, distance: 1n, fits: false },
    { name: 'bootstrap is not proven capacity', estimate: { evidence: 'unknown', milliseconds: INITIAL_BLOCK_MILLISECONDS }, distance: 4n, fits: false },
    { name: 'stale capacity cannot fill read-ahead', estimate: { evidence: 'stale', milliseconds: 100 }, distance: 4n, fits: false },
  ]
  it.each(cases)('$name', ({ estimate, distance, fits }) => {
    expect(fitsReadAhead(estimate, block, distance, aggregateRate)).toBe(fits)
  })
})
