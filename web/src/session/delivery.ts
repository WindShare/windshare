import type { ChunkIndex } from '../contracts/selection'
import type { BlockSink, DeliveryOrder } from '../contracts/sink'
import type { CompactDemand } from './demand'

/** Serializes sink access and owns the bounded ordered-delivery buffer. */
export class BlockDelivery {
  readonly #sink: BlockSink
  readonly #demand: CompactDemand
  readonly #order: DeliveryOrder
  readonly #onProgress: () => void
  readonly #onFailure: (reason: unknown) => void
  readonly #buffered = new Map<ChunkIndex, Uint8Array>()
  readonly #writing = new Set<ChunkIndex>()
  #maxBuffered = 0
  #orderedDrainQueued = false
  #stopped = false
  #failure: unknown
  #workTail = Promise.resolve()

  constructor(
    sink: BlockSink,
    demand: CompactDemand,
    order: DeliveryOrder,
    onProgress: () => void,
    onFailure: (reason: unknown) => void,
  ) {
    this.#sink = sink
    this.#demand = demand
    this.#order = order
    this.#onProgress = onProgress
    this.#onFailure = onFailure
  }

  get order(): DeliveryOrder {
    return this.#order
  }

  get bufferedCount(): number {
    return this.#buffered.size
  }

  get maxBufferedCount(): number {
    return this.#maxBuffered
  }

  get pending(): boolean {
    return this.#buffered.size > 0 || this.#writing.size > 0
  }

  unavailable(index: ChunkIndex): boolean {
    return this.#buffered.has(index) || this.#writing.has(index)
  }

  accept(index: ChunkIndex, plaintext: Uint8Array): void {
    if (this.#stopped) {
      return
    }
    const owned = plaintext.slice()
    if (this.order === 'ascending') {
      this.#buffered.set(index, owned)
      this.#maxBuffered = Math.max(this.#maxBuffered, this.#buffered.size)
      this.#queueOrderedDrain()
      return
    }
    this.#queueUnordered(index, owned)
  }

  async finalize(): Promise<void> {
    await this.#workTail
    if (this.#stopped) {
      throw this.#failure ?? new Error('block delivery stopped before finalization')
    }
    await this.#sink.finalize()
  }

  async abort(reason: unknown): Promise<void> {
    this.#stopped = true
    await this.#workTail
    this.#buffered.clear()
    this.#writing.clear()
    await this.#sink.abort(reason)
  }

  #queueUnordered(index: ChunkIndex, plaintext: Uint8Array): void {
    this.#writing.add(index)
    this.#queue(async () => {
      let progressed = false
      try {
        if (!this.#stopped) {
          await this.#sink.writeBlock(index, plaintext)
          progressed = true
        }
      } finally {
        this.#writing.delete(index)
      }
      if (progressed && !this.#stopped) {
        this.#onProgress()
      }
    })
  }

  #queueOrderedDrain(): void {
    if (this.#orderedDrainQueued || this.#stopped) {
      return
    }
    this.#orderedDrainQueued = true
    this.#queue(async () => {
      let progressed = false
      try {
        while (!this.#stopped) {
          const head = this.#demand.orderedHead
          if (head === undefined) {
            return
          }
          const plaintext = this.#buffered.get(head)
          if (plaintext === undefined) {
            return
          }
          this.#buffered.delete(head)
          this.#writing.add(head)
          try {
            await this.#sink.writeBlock(head, plaintext)
          } catch (error) {
            this.#fail(error)
            return
          } finally {
            this.#writing.delete(head)
          }
          this.#demand.advanceOrdered(head)
          progressed = true
        }
      } finally {
        this.#orderedDrainQueued = false
        if (progressed && !this.#stopped) {
          this.#onProgress()
        }
      }
    })
  }

  #queue(operation: () => Promise<void>): void {
    const queued = this.#workTail.then(operation)
    this.#workTail = queued.catch((error: unknown) => this.#fail(error))
  }

  #fail(reason: unknown): void {
    if (this.#stopped) {
      return
    }
    this.#stopped = true
    this.#failure = reason
    this.#onFailure(reason)
  }
}
