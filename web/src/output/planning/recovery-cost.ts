export const RECOVERY_RATE_WINDOW_MILLISECONDS = 5_000
export const RECOVERY_RATE_MINIMUM_WINDOW_MILLISECONDS = 3_000
export const RECOVERY_RATE_MAXIMUM_AGE_MILLISECONDS = 10_000
export const RECOVERY_RATE_MAXIMUM_SAMPLES = 128
export const RECOVERY_RECEIPT_BUCKET_MILLISECONDS = 250
export const RECOVERY_LOCAL_MINIMUM_SAMPLE_BYTES = 1_048_576n
const MAXIMUM_RELIABLE_RATE_RATIO = 2n
const MILLISECONDS_PER_SECOND = 1_000n

export interface RecoveryCostSnapshot {
  readonly receivedBytesPerSecond: bigint | null
  readonly copiedBytesPerSecond: bigint | null
  readonly flushMilliseconds: number | null
}

interface ReceiptSample { readonly atMilliseconds: number; readonly newReceivedBytes: bigint }

/** Only successful new receipt and completed local work are evidence; retained bytes are not throughput. */
export class RecoveryCostObserver {
  readonly #receipts: ReceiptSample[] = []
  #latestReceipt: ReceiptSample | undefined
  readonly #copyRates: bigint[] = []
  readonly #flushDurations: number[] = []

  observeReceipt(sample: ReceiptSample): void {
    if (!Number.isSafeInteger(sample.atMilliseconds) || sample.atMilliseconds < 0 ||
        sample.newReceivedBytes < 0n) throw new TypeError('Invalid new-receipt observation')
    const previous = this.#latestReceipt
    if (previous !== undefined && sample.atMilliseconds < previous.atMilliseconds) return
    if (previous !== undefined && sample.newReceivedBytes < previous.newReceivedBytes) this.#receipts.length = 0
    this.#latestReceipt = Object.freeze({ ...sample })
    const anchor = this.#receipts.at(-1)
    // Time-spaced cumulative anchors retain the elapsed horizon regardless of callback frequency.
    // The latest endpoint includes writes coalesced inside the unfinished time bucket.
    if (anchor === undefined || sample.atMilliseconds - anchor.atMilliseconds >= RECOVERY_RECEIPT_BUCKET_MILLISECONDS) {
      this.#receipts.push(this.#latestReceipt)
    }
    while (this.#receipts.length > 2 &&
      this.#receipts[1]!.atMilliseconds <= sample.atMilliseconds - RECOVERY_RATE_WINDOW_MILLISECONDS) {
      this.#receipts.shift()
    }
  }

  observeCopy(input: Readonly<{ bytes: bigint; durationMilliseconds: number }>): void {
    if (!validDuration(input.durationMilliseconds) || input.bytes < RECOVERY_LOCAL_MINIMUM_SAMPLE_BYTES) return
    this.#copyRates.push(rate(input.bytes, input.durationMilliseconds))
    if (this.#copyRates.length > RECOVERY_RATE_MAXIMUM_SAMPLES) this.#copyRates.shift()
  }

  observeFlush(input: Readonly<{ bytes: bigint; durationMilliseconds: number }>): void {
    if (!validDuration(input.durationMilliseconds) || input.bytes <= 0n) return
    this.#flushDurations.push(input.durationMilliseconds)
    if (this.#flushDurations.length > RECOVERY_RATE_MAXIMUM_SAMPLES) this.#flushDurations.shift()
  }

  snapshot(atMilliseconds: number): RecoveryCostSnapshot {
    return Object.freeze({
      receivedBytesPerSecond: this.#receiptRate(atMilliseconds),
      copiedBytesPerSecond: reliableRate(this.#copyRates),
      flushMilliseconds: this.#flushDurations.length === 0 ? null : Math.max(...this.#flushDurations),
    })
  }

  #receiptRate(atMilliseconds: number): bigint | null {
    const first = this.#receipts[0]
    const last = this.#latestReceipt
    if (first === undefined || last === undefined || atMilliseconds < last.atMilliseconds ||
        atMilliseconds - last.atMilliseconds > RECOVERY_RATE_MAXIMUM_AGE_MILLISECONDS ||
        last.atMilliseconds - first.atMilliseconds < RECOVERY_RATE_MINIMUM_WINDOW_MILLISECONDS) return null
    const intervals = this.#receipts.slice(1).map((sample, index) => {
      const previous = this.#receipts[index]!
      return rate(sample.newReceivedBytes - previous.newReceivedBytes,
        sample.atMilliseconds - previous.atMilliseconds)
    })
    if (reliableRate(intervals) === null) return null
    return rate(last.newReceivedBytes - first.newReceivedBytes, last.atMilliseconds - first.atMilliseconds)
  }
}

function reliableRate(rates: readonly bigint[]): bigint | null {
  if (rates.length === 0) return null
  const minimum = rates.reduce((left, right) => left < right ? left : right)
  const maximum = rates.reduce((left, right) => left > right ? left : right)
  return minimum > 0n && maximum <= minimum * MAXIMUM_RELIABLE_RATE_RATIO ? minimum : null
}

function validDuration(value: number): boolean { return Number.isFinite(value) && value > 0 }

function rate(bytes: bigint, milliseconds: number): bigint {
  return bytes * MILLISECONDS_PER_SECOND / BigInt(Math.max(1, Math.ceil(milliseconds)))
}
