/** Receiver-owned capacity survives both network and ProtocolSession replacement. */
export class PeerAttemptBudget {
  readonly #attemptCapacity: number
  readonly #elapsedCapacity: number
  readonly #refillMilliseconds: number
  #attempts: number
  #elapsed: number
  #updatedAt: number | undefined

  constructor(attemptCapacity = 8, elapsedCapacity = 360_000, refillMilliseconds = 600_000) {
    if (![attemptCapacity, elapsedCapacity, refillMilliseconds].every(
      (value) => Number.isSafeInteger(value) && value > 0,
    )) throw new RangeError('Peer budget capacities must be positive integers')
    this.#attemptCapacity = attemptCapacity
    this.#elapsedCapacity = elapsedCapacity
    this.#refillMilliseconds = refillMilliseconds
    this.#attempts = attemptCapacity
    this.#elapsed = elapsedCapacity
  }

  available(now: number): { readonly attempts: number; readonly elapsedMilliseconds: number } {
    this.#refill(now)
    return Object.freeze({ attempts: Math.floor(this.#attempts), elapsedMilliseconds: this.#elapsed })
  }

  takeAttempt(now: number): boolean {
    this.#refill(now)
    if (this.#attempts < 1) return false
    this.#attempts -= 1
    return true
  }

  /** Reservation prevents concurrent paths from overspending the shared active-time budget. */
  reserveElapsed(now: number, maximumMilliseconds: number): {
    readonly milliseconds: number
    release(usedMilliseconds: number, completedAt: number): void
  } {
    this.#refill(now)
    const milliseconds = Math.min(maximumMilliseconds, this.#elapsed)
    this.#elapsed -= milliseconds
    let released = false
    return Object.freeze({
      milliseconds,
      release: (usedMilliseconds: number, completedAt: number) => {
        if (released) return
        released = true
        this.#refill(completedAt)
        this.#elapsed = Math.min(this.#elapsedCapacity,
          this.#elapsed + Math.max(0, milliseconds - usedMilliseconds))
      },
    })
  }

  nextAttemptDelay(now: number, elapsedNeeded = 1): number {
    this.#refill(now)
    return Math.ceil(Math.max(
      (1 - this.#attempts) * this.#refillMilliseconds / this.#attemptCapacity,
      (elapsedNeeded - this.#elapsed) * this.#refillMilliseconds / this.#elapsedCapacity,
      0,
    ))
  }

  #refill(now: number): void {
    if (!Number.isFinite(now) || (this.#updatedAt !== undefined && now < this.#updatedAt)) {
      throw new RangeError('Peer budget clock must be monotonic')
    }
    const elapsed = this.#updatedAt === undefined ? 0 : now - this.#updatedAt
    this.#updatedAt = now
    this.#attempts = Math.min(this.#attemptCapacity,
      this.#attempts + elapsed * this.#attemptCapacity / this.#refillMilliseconds)
    this.#elapsed = Math.min(this.#elapsedCapacity,
      this.#elapsed + elapsed * this.#elapsedCapacity / this.#refillMilliseconds)
  }
}
