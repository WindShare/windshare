export type FrameIngressResult = 'accepted' | 'closed' | 'overflow'

/**
 * The protocol queue, rather than ReadableStream's implicit queue, owns the
 * bound so a stalled consumer cannot turn browser callbacks into unbounded RAM.
 */
export class FrameStream {
  readonly #ordinaryLimit: number
  readonly #queue: Uint8Array[] = []
  readonly #wake = new Set<() => void>()
  readonly stream: ReadableStream<Uint8Array>
  #closed = false

  constructor(ordinaryLimit: number, onCancel: (reason: unknown) => void) {
    this.#ordinaryLimit = ordinaryLimit
    this.stream = new ReadableStream<Uint8Array>(
      {
        pull: (controller) => this.#pull(controller),
        cancel: (reason) => {
          this.#discard()
          onCancel(reason)
        },
      },
      { highWaterMark: 0 },
    )
  }

  get bufferedCount(): number {
    return this.#queue.length
  }

  push(frame: Uint8Array): FrameIngressResult {
    if (this.#closed) {
      return 'closed'
    }
    if (this.#queue.length >= this.#ordinaryLimit) {
      return 'overflow'
    }
    this.#queue.push(frame)
    this.#notify()
    return 'accepted'
  }

  pushTerminal(frame: Uint8Array): FrameIngressResult {
    if (this.#closed) {
      return 'closed'
    }
    // The extra slot is reserved for the lifecycle boundary, so ordinary
    // saturation cannot suppress the peer's final frame.
    if (this.#queue.length > this.#ordinaryLimit) {
      return 'overflow'
    }
    this.#queue.push(frame)
    this.#closed = true
    this.#notify()
    return 'accepted'
  }

  close(): void {
    if (this.#closed) {
      return
    }
    this.#closed = true
    this.#notify()
  }

  #discard(): void {
    this.#queue.length = 0
    this.#closed = true
    this.#notify()
  }

  async #pull(controller: ReadableStreamDefaultController<Uint8Array>): Promise<void> {
    while (this.#queue.length === 0 && !this.#closed) {
      await new Promise<void>((resolve) => this.#wake.add(resolve))
    }
    const frame = this.#queue.shift()
    if (frame !== undefined) {
      controller.enqueue(frame)
      return
    }
    controller.close()
  }

  #notify(): void {
    const waiters = [...this.#wake]
    this.#wake.clear()
    waiters.forEach((resolve) => resolve())
  }
}
