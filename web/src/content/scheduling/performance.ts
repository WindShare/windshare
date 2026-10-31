import { MINIMUM_SAMPLE_MILLISECONDS } from './completion'

const SAMPLE_WEIGHT = 0.25
const MEASUREMENT_WINDOW_MILLISECONDS = 1000

/** Busy-interval payload throughput counts overlapping work only once. */
export class LanePerformance {
  /** Unsettled plaintext reservations; received fragments remain here until authentication/settlement. */
  pendingBytes = 0
  bytesPerSecond = 0
  hasSuccessfulSample = false
  #successfulBlocks = 0
  lastAttempt: number | undefined
  #busySince = 0
  #completedBytes = 0
  #activeSince = 0
  #busyMilliseconds = 0

  get successfulBlocks(): number { return this.#successfulBlocks }

  begin(now: number, bytes: number): void {
    if (this.pendingBytes === 0) {
      this.#busySince = now
      this.#activeSince = now
      this.#completedBytes = 0
    }
    this.pendingBytes += bytes
    this.lastAttempt = now
  }

  complete(now: number, bytes: number, successful: boolean): void {
    const wasBusy = this.pendingBytes > 0
    if (successful && bytes > 0) {
      this.#successfulBlocks++
      this.#completedBytes += bytes
      const elapsed = Math.max(now - this.#busySince, MINIMUM_SAMPLE_MILLISECONDS)
      if (!this.hasSuccessfulSample || elapsed >= MEASUREMENT_WINDOW_MILLISECONDS || bytes >= this.pendingBytes) {
        const sample = this.#completedBytes * 1000 / elapsed
        // Keep completed-goodput diagnostics responsive to sustained drops while smoothing bursts.
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
    this.pendingBytes -= Math.min(this.pendingBytes, bytes)
    if (this.pendingBytes === 0 && wasBusy) {
      this.#busyMilliseconds += Math.max(0, now - this.#activeSince)
    }
  }

  /** Overlapping requests share busy time; output pauses do not consume it. */
  busyMilliseconds(now: number): number {
    return this.#busyMilliseconds + (this.pendingBytes > 0 ? Math.max(0, now - this.#activeSince) : 0)
  }

  /** A lost race bounds speed without misclassifying caller cancellation as failure. */
  superseded(bytes: number, elapsed: number): void {
    if (bytes <= 0 || elapsed < MINIMUM_SAMPLE_MILLISECONDS) return
    const upperBound = bytes * 1000 / elapsed
    if (this.bytesPerSecond === 0 || upperBound < this.bytesPerSecond) {
      this.bytesPerSecond = upperBound
    }
  }

}
