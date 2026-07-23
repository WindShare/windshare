export const R8_PERFORMANCE_SAMPLE_COUNT = 5
export const R8_TREND_PREFIX = 'WINDSHARE_R8_TREND '

export interface R8MetricSummary {
  readonly raw: readonly number[]
  readonly p50: number
  readonly p95: number
}

export function r8PerformanceSampleCount(
  environment: Readonly<Record<string, string | undefined>> = process.env,
): number {
  const configured = environment.WINDSHARE_R8_PERFORMANCE_SAMPLES
  if (configured === undefined) return 1
  if (configured !== String(R8_PERFORMANCE_SAMPLE_COUNT)) {
    throw new Error(
      `WINDSHARE_R8_PERFORMANCE_SAMPLES must be ${R8_PERFORMANCE_SAMPLE_COUNT}`,
    )
  }
  return R8_PERFORMANCE_SAMPLE_COUNT
}

export function summarizeR8Metric(values: readonly number[]): R8MetricSummary {
  if (values.length === 0 || values.some((value) => !Number.isFinite(value) || value < 0)) {
    throw new TypeError('R8 trend metrics require finite non-negative samples')
  }
  const raw = values.map(roundMetric)
  const sorted = [...raw].sort((left, right) => left - right)
  return Object.freeze({
    raw: Object.freeze(raw),
    p50: nearestRank(sorted, 0.5),
    p95: nearestRank(sorted, 0.95),
  })
}

export function reportR8Trend(record: {
  readonly browser: string
  readonly scenario: string
  readonly workload: Readonly<Record<string, unknown>>
  readonly capabilities: Readonly<Record<string, boolean>>
  readonly unavailable: Readonly<Record<string, string>>
  readonly metrics: Readonly<Record<string, R8MetricSummary>>
}): void {
  console.log(`${R8_TREND_PREFIX}${JSON.stringify({
    schema: 1,
    statistics: { percentileMethod: 'nearest-rank', decimalPlaces: 3 },
    ...record,
  })}`)
}

function nearestRank(sorted: readonly number[], percentile: number): number {
  const index = Math.max(0, Math.ceil(percentile * sorted.length) - 1)
  const value = sorted[index]
  if (value === undefined) throw new Error('R8 percentile sample disappeared')
  return value
}

function roundMetric(value: number): number {
  return Math.round(value * 1_000) / 1_000
}
