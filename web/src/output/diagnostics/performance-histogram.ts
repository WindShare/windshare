import {
  PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS,
  type PerformanceHistogramSnapshot,
} from '../../diagnostics/trace/transfer-payload'

export const PERFORMANCE_UINT64_MAX = 0xffff_ffff_ffff_ffffn

export class BoundedMillisecondsHistogram {
  readonly #buckets = Array<bigint>(PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS.length + 1).fill(0n)
  readonly #onOverflow: () => void
  #sampleCount = 0n
  #totalMilliseconds = 0n
  #maximumMilliseconds = 0n

  constructor(onOverflow: () => void) {
    this.#onOverflow = onOverflow
  }

  observe(milliseconds: number): void {
    const value = requirePerformanceMilliseconds(milliseconds, 'performance duration')
    const asBigint = BigInt(value)
    const bucketIndex = PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS.findIndex(
      bound => value <= bound,
    )
    const index = bucketIndex < 0
      ? PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS.length
      : bucketIndex
    this.#buckets[index] = this.#add(this.#buckets[index]!, 1n)
    this.#sampleCount = this.#add(this.#sampleCount, 1n)
    this.#totalMilliseconds = this.#add(this.#totalMilliseconds, asBigint)
    this.#maximumMilliseconds = this.#maximumMilliseconds > asBigint
      ? this.#maximumMilliseconds
      : asBigint
  }

  snapshot(): PerformanceHistogramSnapshot {
    return Object.freeze({
      upperBoundsMilliseconds: PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS,
      bucketCounts: Object.freeze([...this.#buckets]),
      sampleCount: this.#sampleCount,
      totalMilliseconds: this.#totalMilliseconds,
      maximumMilliseconds: this.#maximumMilliseconds,
    })
  }

  #add(value: bigint, delta: bigint): bigint {
    if (value > PERFORMANCE_UINT64_MAX - delta) {
      this.#onOverflow()
      return PERFORMANCE_UINT64_MAX
    }
    return value + delta
  }
}

export function requirePerformanceMilliseconds(value: number, field: string): number {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new RangeError(`${field} must be a non-negative safe integer`)
  }
  return value
}

export function saturatingPerformanceAdd(
  value: bigint,
  delta: bigint,
  onOverflow: () => void,
): bigint {
  if (delta < 0n) throw new RangeError('performance counter delta must not be negative')
  if (value > PERFORMANCE_UINT64_MAX - delta) {
    onOverflow()
    return PERFORMANCE_UINT64_MAX
  }
  return value + delta
}
