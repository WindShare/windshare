import { AsyncBoundedQueue } from '../job/scheduler'

export interface DiscoverySchedulingObservation {
  readonly queue: 'generations' | 'zip_members'
  readonly decision: 'waiting' | 'resumed' | 'cancelled' | 'complete'
  readonly pendingItems: number
  readonly metadataBytes: bigint
  readonly maximumItems: number
  readonly maximumMetadataBytes: bigint
}

/** Observes bounded lookahead without giving diagnostics authority over admission. */
export class DiscoveryQueue<T> extends AsyncBoundedQueue<T> {
  readonly #queue: DiscoverySchedulingObservation['queue']
  readonly #maximumItems: number
  readonly #maximumMetadataBytes: bigint
  readonly #observe: ((event: DiscoverySchedulingObservation) => void) | undefined
  #completed = false

  constructor(input: Readonly<{
    queue: DiscoverySchedulingObservation['queue']
    maximumItems: number
    maximumMetadataBytes: bigint
    weight: (item: T) => bigint
    observe?: (event: DiscoverySchedulingObservation) => void
  }>) {
    super(input.maximumItems, input.maximumMetadataBytes, input.weight)
    this.#queue = input.queue
    this.#maximumItems = input.maximumItems
    this.#maximumMetadataBytes = input.maximumMetadataBytes
    this.#observe = input.observe
  }

  override async push(item: T, signal: AbortSignal): Promise<void> {
    if (this.tryPush(item, signal)) return
    this.#emit('waiting')
    try {
      await super.push(item, signal)
      this.#emit('resumed')
    } catch (error) {
      this.#emit('cancelled')
      throw error
    }
  }

  override close(): void {
    super.close()
    if (this.#completed) return
    this.#completed = true
    this.#emit('complete')
  }

  #emit(decision: DiscoverySchedulingObservation['decision']): void {
    try {
      this.#observe?.({
        queue: this.#queue, decision, ...this.snapshot(),
        maximumItems: this.#maximumItems, maximumMetadataBytes: this.#maximumMetadataBytes,
      })
    } catch {
      // Optional tracing must not stall catalog discovery or file transfer.
    }
  }
}
