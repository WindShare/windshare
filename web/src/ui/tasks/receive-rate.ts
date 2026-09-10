const MILLISECONDS_PER_SECOND = 1000
const RATE_WINDOW_MILLISECONDS = 5000
const MINIMUM_ETA_WINDOW_MILLISECONDS = 3000
const MAXIMUM_ETA_RATE_RATIO = 2n

interface ReceiptSample {
  readonly at: number
  readonly bytes: bigint
}

export interface ReceiveRate {
  readonly bytesPerSecond: bigint
  readonly remainingSeconds: number | null
}

/** Rates use only new receipt; reusing retained bytes must not inflate throughput or ETA. */
export class ReceiveRateSampler {
  readonly #samples: ReceiptSample[]

  constructor(at: number, bytes: bigint) {
    this.#samples = [{ at, bytes }]
  }

  sample(at: number, bytes: bigint, remainingBytes: bigint | null): ReceiveRate | null {
    const previous = this.#samples.at(-1)!
    if (at <= previous.at) return null
    if (bytes < previous.bytes) {
      this.#samples.splice(0, this.#samples.length, { at, bytes })
      return null
    }
    this.#samples.push({ at, bytes })
    while (this.#samples.length > 2 && this.#samples[1]!.at <= at - RATE_WINDOW_MILLISECONDS) {
      this.#samples.shift()
    }
    const first = this.#samples[0]!
    const elapsed = at - first.at
    const rate = byteRate(bytes - first.bytes, elapsed)
    return {
      bytesPerSecond: rate,
      remainingSeconds: this.#remainingSeconds(remainingBytes, rate, elapsed),
    }
  }

  #remainingSeconds(remaining: bigint | null, rate: bigint, elapsed: number): number | null {
    if (remaining === null || remaining <= 0n || rate === 0n || elapsed < MINIMUM_ETA_WINDOW_MILLISECONDS) {
      return null
    }
    const intervals = this.#samples.slice(1).map((sample, index) => {
      const previous = this.#samples[index]!
      return byteRate(sample.bytes - previous.bytes, sample.at - previous.at)
    })
    const minimum = intervals.reduce((left, right) => left < right ? left : right)
    const maximum = intervals.reduce((left, right) => left > right ? left : right)
    // An idle interval or a rapidly changing rate cannot support a useful whole-task estimate.
    if (minimum === 0n || maximum > minimum * MAXIMUM_ETA_RATE_RATIO) return null
    const seconds = (remaining + rate - 1n) / rate
    return seconds <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(seconds) : null
  }
}

function byteRate(bytes: bigint, milliseconds: number): bigint {
  return bytes * BigInt(MILLISECONDS_PER_SECOND) / BigInt(Math.max(1, Math.round(milliseconds)))
}
