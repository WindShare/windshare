export const SAMPLE_LIFETIME_MILLISECONDS = 12_000
export const SAMPLE_BYTE_BUDGET = 8 * 1024 * 1024
export const SAMPLE_ATTEMPT_BUDGET = 8

export type SamplingOutcome = 'completed' | 'failed' | 'superseded' | 'cancelled'
/** Settlement and successful bytes stay distinct so cancellation cannot manufacture capacity evidence. */
export interface SamplingEvidence {
  readonly offeredBytes: number
  readonly settledBytes: number
  readonly completedBytes: number
  readonly completedAttempts: number
  readonly failedAttempts: number
  readonly supersededAttempts: number
  readonly cancelledAttempts: number
  readonly busyMilliseconds: number
}

/** A sample borrows ordinary work and capacity; it never creates a private queue. */
export class LaneSampling {
  #offeredBytes = 0
  #settledBytes = 0
  #completedBytes = 0
  #completedAttempts = 0
  #failedAttempts = 0
  #supersededAttempts = 0
  #cancelledAttempts = 0
  #pending = 0
  #busySince = 0
  #busyMilliseconds = 0
  #lease: { expires: number; bytes: number; attempts: number } | undefined

  start(now: number): void {
    this.#lease = { expires: now + SAMPLE_LIFETIME_MILLISECONDS, bytes: SAMPLE_BYTE_BUDGET, attempts: SAMPLE_ATTEMPT_BUDGET }
  }

  stop(): void { this.#lease = undefined }

  available(bytes: number, now: number): boolean {
    const lease = this.#lease
    return lease !== undefined && now < lease.expires && lease.attempts > 0 && bytes <= lease.bytes
  }

  offered(bytes: number, now: number): void {
    this.#offeredBytes += bytes
    if (this.#pending++ === 0) this.#busySince = now
    // Natural allocation consumes the same allowance as accelerated probes.
    if (this.#lease !== undefined) {
      this.#lease.bytes = Math.max(0, this.#lease.bytes - bytes)
      this.#lease.attempts = Math.max(0, this.#lease.attempts - 1)
    }
  }

  settled(bytes: number, now: number, outcome: SamplingOutcome): void {
    this.#settledBytes += bytes
    if (outcome === 'completed') this.#completedBytes += bytes
    switch (outcome) {
      case 'completed': this.#completedAttempts++; break
      case 'failed': this.#failedAttempts++; break
      case 'superseded': this.#supersededAttempts++; break
      case 'cancelled': this.#cancelledAttempts++; break
    }
    if (--this.#pending === 0) this.#busyMilliseconds += Math.max(0, now - this.#busySince)
  }

  snapshot(now: number): SamplingEvidence {
    return {
      offeredBytes: this.#offeredBytes, settledBytes: this.#settledBytes, completedBytes: this.#completedBytes,
      completedAttempts: this.#completedAttempts, failedAttempts: this.#failedAttempts,
      supersededAttempts: this.#supersededAttempts, cancelledAttempts: this.#cancelledAttempts,
      busyMilliseconds: Math.floor(this.#busyMilliseconds + (this.#pending > 0 ? Math.max(0, now - this.#busySince) : 0)),
    }
  }
}
