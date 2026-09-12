export const INITIAL_BLOCK_MILLISECONDS = 250
export const MINIMUM_SAMPLE_MILLISECONDS = 1
export const RELAY_COST_FACTOR = 1.1
const SAMPLE_WEIGHT = 0.25
const MEASUREMENT_WINDOW_MILLISECONDS = 1000
const MAXIMUM_ESTIMATE_MILLISECONDS = 60 * 60 * 1000

/** Busy-interval payload throughput counts overlapping work only once. */
export class LanePerformance {
  pendingBytes = 0
  bytesPerSecond = 0
  hasSuccessfulSample = false
  lastAttempt: number | undefined
  #busySince = 0
  #completedBytes = 0
  #pendingBlocks = 0

  begin(now: number, bytes: number): void {
    if (this.pendingBytes === 0) {
      this.#busySince = now
      this.#completedBytes = 0
    }
    this.#pendingBlocks += 1
    this.pendingBytes += bytes
    this.lastAttempt = now
  }

  complete(now: number, bytes: number, successful: boolean): void {
    if (successful && bytes > 0) {
      this.#completedBytes += bytes
      const elapsed = Math.max(now - this.#busySince, MINIMUM_SAMPLE_MILLISECONDS)
      if (!this.hasSuccessfulSample || elapsed >= MEASUREMENT_WINDOW_MILLISECONDS || bytes >= this.pendingBytes) {
        const sample = this.#completedBytes * 1000 / elapsed
        // Reduce admission immediately on congestion, but smooth short capacity bursts.
        this.bytesPerSecond = this.bytesPerSecond === 0 || sample < this.bytesPerSecond
          ? sample
          : this.bytesPerSecond + SAMPLE_WEIGHT * (sample - this.bytesPerSecond)
        this.hasSuccessfulSample = true
      }
      if (elapsed >= MEASUREMENT_WINDOW_MILLISECONDS) {
        this.#busySince = now
        this.#completedBytes = 0
      }
    }
    this.#pendingBlocks -= 1
    this.pendingBytes -= Math.min(this.pendingBytes, bytes)
  }

  /** A lost race bounds speed without misclassifying caller cancellation as failure. */
  superseded(bytes: number, elapsed: number): void {
    if (bytes <= 0 || elapsed < MINIMUM_SAMPLE_MILLISECONDS) return
    const upperBound = bytes * 1000 / elapsed
    if (this.bytesPerSecond === 0 || upperBound < this.bytesPerSecond) {
      this.bytesPerSecond = upperBound
    }
  }

  estimateQueueMilliseconds(): number {
    // Only complete blocks expose progress. Charge outstanding reservations until
    // they settle; a new request's size must never redefine this existing work.
    const milliseconds = this.bytesPerSecond > 0
      ? this.pendingBytes * 1000 / this.bytesPerSecond
      : this.#pendingBlocks * INITIAL_BLOCK_MILLISECONDS
    return Math.min(MAXIMUM_ESTIMATE_MILLISECONDS, milliseconds)
  }

  estimateCompletionMilliseconds(bytes: number): number {
    const service = this.bytesPerSecond > 0
      ? Math.max(bytes, 1) * 1000 / this.bytesPerSecond
      : INITIAL_BLOCK_MILLISECONDS
    return Math.min(MAXIMUM_ESTIMATE_MILLISECONDS,
      Math.max(MINIMUM_SAMPLE_MILLISECONDS, this.estimateQueueMilliseconds() + service))
  }
}

export function completionCost(milliseconds: number, relayed: boolean): number {
  return milliseconds * (relayed ? RELAY_COST_FACTOR : 1)
}
