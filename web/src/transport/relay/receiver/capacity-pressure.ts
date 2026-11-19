import type { CapacityPressure } from '../../../contracts/capacity'

export class RelayCapacityPressure implements CapacityPressure {
  readonly #until = [0, 0] as [number, number]
  readonly #listeners = new Set<() => void>()
  #cleared = false
  #closed = false

  get cleared(): boolean { return this.#cleared }

  get remainingMilliseconds(): number {
    return Math.max(0, Math.max(...this.#until) - performance.now())
  }

  subscribeClearance(listener: () => void): () => void {
    if (!this.#closed) this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  update(direction: 0 | 1, milliseconds: number): void {
    if (milliseconds > 0) {
      this.#until[direction] = performance.now() + milliseconds
      this.#cleared = false
      return
    }
    if (this.#until[direction] === 0) return
    this.#until[direction] = 0
    // Fetching a block uses both directions. Silence or a release on just one
    // direction cannot supersede an advisory that still applies to the other.
    if (this.remainingMilliseconds > 0 || this.#cleared) return
    this.#cleared = true
    for (const listener of this.#listeners) listener()
  }

  close(): void {
    this.#closed = true
    this.#cleared = false
    this.#until.fill(0)
    this.#listeners.clear()
  }
}
