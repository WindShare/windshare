export class NegotiationEventQueue<T> {
  readonly #items: T[] = []
  readonly #capacity: number
  readonly #overflow: () => T
  readonly #onOverflow: (overflow: T) => void
  #waiting: ((value: T) => void) | undefined
  #closed = false
  #overflowed = false

  constructor(capacity: number, overflow: () => T, onOverflow: (overflow: T) => void) {
    this.#capacity = capacity
    this.#overflow = overflow
    this.#onOverflow = onOverflow
  }

  push(value: T): void {
    if (this.#closed || this.#overflowed) {
      return
    }
    const waiting = this.#waiting
    if (waiting !== undefined) {
      this.#waiting = undefined
      waiting(value)
      return
    }
    if (this.#items.length >= this.#capacity) {
      const overflow = this.#overflow()
      this.#items.length = 0
      this.#items.push(overflow)
      this.#overflowed = true
      this.#onOverflow(overflow)
      return
    }
    this.#items.push(value)
  }

  next(): Promise<T> {
    const value = this.#items.shift()
    if (value !== undefined) {
      return Promise.resolve(value)
    }
    return new Promise<T>((resolve) => {
      this.#waiting = resolve
    })
  }

  close(): void {
    this.#closed = true
    this.#items.length = 0
  }
}
