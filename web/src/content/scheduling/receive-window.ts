import { EVIDENCE_LIFETIME_MILLISECONDS, estimateCompletion, estimateQueueMilliseconds, type DeliveryEstimate, type DeliveryLoad, type DeliveryProgress } from './completion'

const RECEIVE_AHEAD_MILLISECONDS = 250
const MINIMUM_SAMPLE_MILLISECONDS = 1
const SAMPLE_WINDOW_MILLISECONDS = 250
const SAMPLE_WEIGHT = 0.25

export interface BlockReceiveAdmission {
  readonly waitedMilliseconds: number
  readonly unfinishedBytes: number
  readonly aheadBytes: number
  readonly bytesPerSecond: number
}

export interface BlockReceivePermit {
  readonly admission: BlockReceiveAdmission
  progress(): DeliveryProgress
  receive(objectBytes: number): void
  close(): void
}

interface IncomingResponse {
  readonly started: number
  remaining: number
  first: boolean
  lastReceiptAt: number | undefined
}

/**
 * A physical lane needs enough unfinished data to hide response latency, not a
 * fixed number of large competing records. Receipt replenishes the pipeline
 * before authentication/output completion, so fast or distant peers stay busy.
 */
export class BlockReceiveWindow {
  readonly #now: () => number
  readonly #listeners = new Set<() => void>()
  readonly #responses = new Set<IncomingResponse>()
  #streamingRate = false
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

  load(now = this.#now()): DeliveryLoad {
    const pendingResponses = [...this.#responses].filter(response => response.remaining > 0).length
    if (this.#bytesPerSecond <= 0 || this.#lastReceiptAt === undefined) {
      return { remainingBytes: this.#remainingBytes, pendingResponses, sample: { kind: 'unknown' } }
    }
    const lifetime = Math.max(EVIDENCE_LIFETIME_MILLISECONDS, 2 * this.#responseMilliseconds)
    return { remainingBytes: this.#remainingBytes, pendingResponses, sample: {
      kind: now - this.#lastReceiptAt <= lifetime ? 'recent' : 'stale',
      bytesPerSecond: this.#bytesPerSecond,
      // A whole-response sample already includes latency. Only inter-fragment
      // throughput can be combined with a separate response-delay estimate.
      startupMilliseconds: this.#streamingRate ? this.#responseMilliseconds : 0,
    } }
  }

  estimateCompletion(bytes: number): DeliveryEstimate { return estimateCompletion(this.load(), bytes) }
  estimateQueueMilliseconds(): number { return estimateQueueMilliseconds(this.load()) }

  #progress(response: IncomingResponse): DeliveryProgress {
    const now = this.#now()
    const elapsedMilliseconds = Math.max(0, now - response.started)
    const load = this.load(now)
    let remainingBytes = 0, pendingResponses = 0
    // Later admissions cannot move an existing request's predicted finish.
    for (const earlier of this.#responses) {
      if (earlier === response) break
      remainingBytes += earlier.remaining
      if (earlier.remaining > 0) pendingResponses++
    }
    const sample = load.sample.kind === 'unknown' ? load.sample : { ...load.sample,
      startupMilliseconds: response.first ? Math.max(0, load.sample.startupMilliseconds - elapsedMilliseconds) : 0 }
    return {
      remainingBytes: response.remaining, elapsedMilliseconds,
      silentMilliseconds: Math.max(0, now - (response.lastReceiptAt ?? response.started)),
      remaining: estimateCompletion({ remainingBytes, pendingResponses, sample }, response.remaining),
    }
  }

  #sampleDelivery(response: IncomingResponse, objectBytes: number, now: number): void {
    const streaming = this.#receiving > 0 || (this.#lastReceiptAt !== undefined &&
      now - this.#lastReceiptAt < this.#responseMilliseconds)
    this.#lastReceiptAt = now
    if (!streaming) {
      this.#sampleSince = now
      this.#sampleBytes = 0
    }
    if (response.first) {
      // The minimum observed response delay avoids charging queue contention
      // as extra bandwidth-delay capacity and admitting still more work.
      const delay = Math.max(MINIMUM_SAMPLE_MILLISECONDS, now - response.started)
      this.#responseMilliseconds = this.#responseMilliseconds === 0
        ? delay : Math.min(this.#responseMilliseconds, delay)
      response.first = false
    }
    const retainStreamingRate = !streaming && this.#streamingRate && response.remaining > 0
    this.#streamingRate = streaming || retainStreamingRate
    this.#sampleBytes += objectBytes
    const elapsed = Math.max(MINIMUM_SAMPLE_MILLISECONDS, now - this.#sampleSince)
    // Response delay controls how far to prefetch; inter-fragment delivery
    // measures capacity. Mixing them makes distant fast lanes look slow.
    const rate = streaming ? this.#sampleBytes * 1000 / elapsed
      : objectBytes * 1000 / Math.max(MINIMUM_SAMPLE_MILLISECONDS, now - response.started)
    // The first fragment includes a new response delay. It cannot refute an
    // established streaming rate until another fragment measures service.
    if (!retainStreamingRate) this.#bytesPerSecond = this.#bytesPerSecond === 0 || rate < this.#bytesPerSecond
      ? rate : this.#bytesPerSecond + SAMPLE_WEIGHT * (rate - this.#bytesPerSecond)
    if (!streaming || elapsed >= SAMPLE_WINDOW_MILLISECONDS) {
      this.#sampleSince = now
      this.#sampleBytes = 0
    }
  }

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
    // Even an active lane may have gone silent. Old evidence can admit one
    // revalidation, but cannot refill a pipeline before receipt confirms supply.
    if (this.load(started).sample.kind === 'stale') {
      this.#bytesPerSecond = 0
      this.#responseMilliseconds = 0
      this.#streamingRate = false
    }
    if (this.#active === 0) {
      this.#sampleSince = started
      this.#sampleBytes = 0
    }
    this.#active += 1
    this.#remainingBytes += bytes
    const response: IncomingResponse = { started, remaining: bytes, first: true, lastReceiptAt: undefined }
    this.#responses.add(response)
    let receiving = false
    let closed = false
    return {
      admission: Object.freeze({
        waitedMilliseconds: started - queuedAt, unfinishedBytes: this.#remainingBytes,
        aheadBytes: this.#bytesPerSecond === 0 ? 0 : ahead, bytesPerSecond: this.#bytesPerSecond,
      }),
      progress: () => this.#progress(response),
      receive: objectBytes => {
        if (closed || !Number.isSafeInteger(objectBytes) || objectBytes <= 0) return
        const wasAvailable = this.available
        const now = this.#now()
        const received = Math.min(response.remaining, objectBytes)
        response.remaining -= received
        if (received > 0) response.lastReceiptAt = now
        this.#remainingBytes -= received
        this.#sampleDelivery(response, objectBytes, now)
        if (response.remaining > 0 && !receiving) { receiving = true; this.#receiving += 1 }
        if (response.remaining === 0 && receiving) { receiving = false; this.#receiving -= 1 }
        if (!wasAvailable && this.available) this.#notifyAvailable()
      },
      close: () => {
        if (closed) return
        const wasAvailable = this.available
        closed = true
        this.#active -= 1
        if (receiving) this.#receiving -= 1
        this.#remainingBytes -= response.remaining
        this.#responses.delete(response)
        if (!wasAvailable && this.available) this.#notifyAvailable()
      },
    }
  }
}
