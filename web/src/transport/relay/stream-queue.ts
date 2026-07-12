export type QueueIngressResult = 'accepted' | 'closed' | 'overflow'

/** A highWaterMark-zero stream keeps the protocol queue as the sole byte owner. */
export class BoundedStreamQueue<T> {
  readonly #ordinaryLimit: number
  readonly #queue: T[] = []
  readonly stream: ReadableStream<T>
  #controller: ReadableStreamDefaultController<T> | undefined
  #pullWaiting = false
  #closed = false
  #cancelled = false

  constructor(ordinaryLimit: number, onCancel?: (reason: unknown) => void) {
    this.#ordinaryLimit = ordinaryLimit
    this.stream = new ReadableStream<T>(
      {
        start: (controller) => {
          this.#controller = controller
        },
        pull: () => {
          this.#pullWaiting = true
          this.#drain()
        },
        cancel: (reason) => {
          this.#discard()
          onCancel?.(reason)
        },
      },
      { highWaterMark: 0 },
    )
  }

  get bufferedCount(): number {
    return this.#queue.length
  }

  push(item: T): QueueIngressResult {
    if (this.#closed) {
      return 'closed'
    }
    if (this.#queue.length >= this.#ordinaryLimit) {
      return 'overflow'
    }
    this.#queue.push(item)
    this.#drain()
    return 'accepted'
  }

  pushTerminal(item: T): QueueIngressResult {
    if (this.#closed) {
      return 'closed'
    }
    // Ordinary traffic cannot consume this extra slot, so saturation never hides
    // the lifecycle boundary from a stalled consumer.
    if (this.#queue.length > this.#ordinaryLimit) {
      return 'overflow'
    }
    this.#queue.push(item)
    this.#closed = true
    this.#drain()
    return 'accepted'
  }

  close(): void {
    if (this.#closed) {
      return
    }
    this.#closed = true
    this.#drain()
  }

  #discard(): void {
    this.#queue.length = 0
    this.#closed = true
    this.#cancelled = true
    this.#pullWaiting = false
  }

  #drain(): void {
    if (!this.#pullWaiting) {
      return
    }
    const item = this.#queue.shift()
    if (item !== undefined) {
      this.#pullWaiting = false
      this.#controller?.enqueue(item)
      return
    }
    if (!this.#cancelled) {
      if (!this.#closed) {
        return
      }
      this.#pullWaiting = false
      this.#controller?.close()
    }
  }
}
