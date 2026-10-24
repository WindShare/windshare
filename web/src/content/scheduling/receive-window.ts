const RECEIVE_AHEAD_MILLISECONDS = 250
const MINIMUM_SAMPLE_MILLISECONDS = 1
const SAMPLE_WINDOW_MILLISECONDS = 250
const SAMPLE_WEIGHT = 0.25
const IDLE_ESTIMATE_MILLISECONDS = 1000

export interface BlockReceiveAdmission {
  readonly waitedMilliseconds: number
  readonly unfinishedBytes: number
  readonly aheadBytes: number
  readonly bytesPerSecond: number
}

export interface BlockReceivePermit {
  readonly admission: BlockReceiveAdmission
  receive(objectBytes: number): void
  close(): void
}

/**
 * A physical lane needs enough unfinished data to hide response latency, not a
 * fixed number of large competing records. Receipt replenishes the pipeline
 * before authentication/output completion, so fast or distant peers stay busy.
 */
export class BlockReceiveWindow {
  readonly #now: () => number
  readonly #listeners = new Set<() => void>()
  #active = 0
  #receiving = 0
  #remainingBytes = 0
  #bytesPerSecond = 0
  #responseMilliseconds = 0
  #sampleSince = 0
  #sampleBytes = 0
  #lastReceiptAt: number | undefined

  constructor(now: () => number = () => performance.now()) {
    this.#now = now
  }

  get available(): boolean {
    const ahead = this.#bytesPerSecond * (this.#responseMilliseconds + RECEIVE_AHEAD_MILLISECONDS) / 1000
    // Cold lanes need one real request, but waiting descriptors must remain in
    // the receiver's allocation queue so a later frontier can overtake them.
    return this.#active === 0 || (this.#bytesPerSecond > 0 && this.#remainingBytes <= ahead)
  }

  get unfinishedBytes(): number { return this.#remainingBytes }

  tryAcquire(bytes: number, queuedAt = this.#now()): BlockReceivePermit | undefined {
    if (!Number.isSafeInteger(bytes) || bytes <= 0) throw new RangeError('Block admission requires positive bytes')
    if (!this.available) return undefined
    const ahead = this.#bytesPerSecond * (this.#responseMilliseconds + RECEIVE_AHEAD_MILLISECONDS) / 1000
    return this.#begin(bytes, queuedAt, ahead)
  }

  /** A waiter drains available capacity; only a blocked-to-ready edge needs a new turn. */
  subscribeAvailable(listener: () => void): () => void {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  #notifyAvailable(): void {
    for (const listener of this.#listeners) listener()
  }

  #begin(bytes: number, queuedAt: number, ahead: number): BlockReceivePermit {
    const started = this.#now()
    if (this.#active === 0) {
      if (started - this.#sampleSince >= IDLE_ESTIMATE_MILLISECONDS) {
        this.#bytesPerSecond = 0
        this.#responseMilliseconds = 0
      }
      this.#sampleSince = started
      this.#sampleBytes = 0
    }
    this.#active += 1
    this.#remainingBytes += bytes
    let remaining = bytes
    let first = true
    let receiving = false
    let closed = false
    return {
      admission: Object.freeze({
        waitedMilliseconds: started - queuedAt, unfinishedBytes: this.#remainingBytes,
        aheadBytes: this.#bytesPerSecond === 0 ? 0 : ahead, bytesPerSecond: this.#bytesPerSecond,
      }),
      receive: objectBytes => {
        if (closed || !Number.isSafeInteger(objectBytes) || objectBytes <= 0) return
        const wasAvailable = this.available
        const now = this.#now()
        const streaming = this.#receiving > 0 || (this.#lastReceiptAt !== undefined &&
          now - this.#lastReceiptAt < this.#responseMilliseconds)
        this.#lastReceiptAt = now
        if (!streaming) {
          this.#sampleSince = now
          this.#sampleBytes = 0
        }
        if (first) {
          // The minimum observed response delay avoids charging queue contention
          // as extra bandwidth-delay capacity and admitting still more work.
          const response = Math.max(MINIMUM_SAMPLE_MILLISECONDS, now - started)
          this.#responseMilliseconds = this.#responseMilliseconds === 0
            ? response : Math.min(this.#responseMilliseconds, response)
          first = false
        }
        const received = Math.min(remaining, objectBytes)
        remaining -= received
        this.#remainingBytes -= received
        this.#sampleBytes += objectBytes
        const elapsed = Math.max(MINIMUM_SAMPLE_MILLISECONDS, now - this.#sampleSince)
        // Response delay controls how far to prefetch; inter-fragment delivery
        // measures capacity. Mixing them makes distant fast lanes look slow.
        const rate = streaming ? this.#sampleBytes * 1000 / elapsed
          : objectBytes * 1000 / Math.max(MINIMUM_SAMPLE_MILLISECONDS, now - started)
        this.#bytesPerSecond = this.#bytesPerSecond === 0 || rate < this.#bytesPerSecond
          ? rate : this.#bytesPerSecond + SAMPLE_WEIGHT * (rate - this.#bytesPerSecond)
        if (!streaming || elapsed >= SAMPLE_WINDOW_MILLISECONDS) {
          this.#sampleSince = now
          this.#sampleBytes = 0
        }
        if (remaining > 0 && !receiving) { receiving = true; this.#receiving += 1 }
        if (remaining === 0 && receiving) { receiving = false; this.#receiving -= 1 }
        if (!wasAvailable && this.available) this.#notifyAvailable()
      },
      close: () => {
        if (closed) return
        const wasAvailable = this.available
        closed = true
        this.#active -= 1
        if (receiving) this.#receiving -= 1
        this.#remainingBytes -= remaining
        if (!wasAvailable && this.available) this.#notifyAvailable()
      },
    }
  }
}
