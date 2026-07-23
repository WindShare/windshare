import { describe, expect, it, vi } from 'vitest'

import {
  R8_PERFORMANCE_SAMPLE_COUNT,
  R8_TREND_PREFIX,
  r8PerformanceSampleCount,
  reportR8Trend,
  summarizeR8Metric,
} from './r8-trend'

describe('R8 trend evidence', () => {
  it('keeps ordinary correctness runs single-sample and evidence runs exactly five-sample', () => {
    expect(r8PerformanceSampleCount({})).toBe(1)
    expect(r8PerformanceSampleCount({ WINDSHARE_R8_PERFORMANCE_SAMPLES: '5' }))
      .toBe(R8_PERFORMANCE_SAMPLE_COUNT)
    expect(() => r8PerformanceSampleCount({ WINDSHARE_R8_PERFORMANCE_SAMPLES: '4' }))
      .toThrow(/must be 5/u)
  })

  it('preserves rounded raw observations and uses nearest-rank percentiles', () => {
    expect(summarizeR8Metric([4.4444, 1.1111, 5.5555, 2.2222, 3.3333])).toEqual({
      raw: [4.444, 1.111, 5.556, 2.222, 3.333],
      p50: 3.333,
      p95: 5.556,
    })
  })

  it.each([
    { samples: [] },
    { samples: [-1] },
    { samples: [Number.NaN] },
    { samples: [Number.POSITIVE_INFINITY] },
  ])(
    'rejects a non-evidentiary metric sample set %#',
    ({ samples }) => {
      expect(() => summarizeR8Metric(samples)).toThrow(/finite non-negative/u)
    },
  )

  it('emits a self-describing single-line JSON record', () => {
    const log = vi.spyOn(console, 'log').mockImplementation(() => undefined)
    reportR8Trend({
      browser: 'test-engine',
      scenario: 'test-scenario',
      workload: { samples: 5 },
      capabilities: { timer: true },
      unavailable: {},
      metrics: { elapsedMilliseconds: summarizeR8Metric([1, 2, 3, 4, 5]) },
    })

    expect(log).toHaveBeenCalledOnce()
    const line = String(log.mock.calls[0]?.[0])
    expect(line.startsWith(R8_TREND_PREFIX)).toBe(true)
    expect(JSON.parse(line.slice(R8_TREND_PREFIX.length))).toMatchObject({
      schema: 1,
      statistics: { percentileMethod: 'nearest-rank', decimalPlaces: 3 },
      metrics: { elapsedMilliseconds: { raw: [1, 2, 3, 4, 5], p50: 3, p95: 5 } },
    })
  })
})
