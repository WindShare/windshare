import type { FrameChannel } from '../contracts/channel'

/** Tracks asynchronous channel ownership so session completion leaves no pumps behind. */
export class ChannelSettlement {
  readonly #readers = new Set<Promise<void>>()
  readonly #closes = new Set<Promise<void>>()

  trackReader(reader: Promise<void>): void {
    this.#readers.add(reader)
    reader.finally(() => this.#readers.delete(reader)).catch(() => undefined)
  }

  close(channel: FrameChannel): void {
    const operation = Promise.resolve()
      .then(() => channel.close())
      .catch(() => undefined)
    this.#closes.add(operation)
    operation.finally(() => this.#closes.delete(operation)).catch(() => undefined)
  }

  async settle(): Promise<void> {
    while (this.#readers.size > 0 || this.#closes.size > 0) {
      await Promise.allSettled([...this.#readers, ...this.#closes])
    }
  }
}
