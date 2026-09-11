import type { V2BlockRecord } from '../v2-records'

export const RANGE_READ_AHEAD_FACTOR = 4
export interface BlockConsumer {
  readonly distance: () => bigint
  readonly canDispatch: () => boolean
  readonly acquire: () => () => void
}
export const IMMEDIATE_BLOCK_CONSUMER: BlockConsumer = {
  distance: () => 0n, canDispatch: () => true, acquire: () => () => undefined,
}

/** Reservations include queued and completed blocks, not only network requests. */
export class RangeBufferBudget {
  readonly #maximum: number
  readonly #listeners = new Set<() => void>()
  #used = 0
  #nextListener = 1
  constructor(maximum: number) {
    if (!Number.isSafeInteger(maximum) || maximum <= 0) throw new RangeError('Range buffer budget must be positive')
    this.#maximum = maximum
  }
  reserve(bytes: number): (() => void) | undefined {
    if (!Number.isSafeInteger(bytes) || bytes <= 0 || bytes > this.#maximum) {
      throw new RangeError('Block exceeds the range buffer budget')
    }
    if (this.#used + bytes > this.#maximum) return undefined
    this.#used += bytes
    let released = false
    return () => {
      if (released) return
      released = true
      this.#used -= bytes
      // Rotate the first offer so one long download cannot reclaim every freed
      // byte before a newer preview or file can reserve its output frontier.
      const listeners = [...this.#listeners]
      const first = this.#nextListener++ % Math.max(1, listeners.length)
      for (let index = 0; index < listeners.length; index += 1) listeners[(first + index) % listeners.length]!()
    }
  }
  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }
}

/** Network slots refill on completion; output order only consumes the bounded read-ahead budget. */
interface BlockWindowOptions {
  readonly first: bigint
  readonly end: bigint
  readonly parallel: number
  readonly budget: RangeBufferBudget
  readonly bytes: (index: bigint) => number
  readonly read: (index: bigint, consumer: BlockConsumer) => Promise<V2BlockRecord>
}

export class OrderedBlockWindow {
  readonly #pending = new Map<bigint, { readonly result: Promise<V2BlockRecord>; readonly release: () => void }>()
  readonly #options: BlockWindowOptions
  readonly #unsubscribe: () => void
  #scheduled: bigint
  #emitted: bigint
  #active = 0
  #closed = false
  #failure: { readonly reason: unknown } | undefined
  #wake: (() => void) | undefined

  constructor(options: BlockWindowOptions) {
    this.#options = options
    this.#scheduled = options.first
    this.#emitted = options.first
    this.#unsubscribe = options.budget.subscribe(() => this.#fill())
    this.#fill()
  }

  async next(signal: AbortSignal): Promise<V2BlockRecord> {
    while (true) {
      signal.throwIfAborted()
      if (this.#failure !== undefined) throw this.#failure.reason
      const entry = this.#pending.get(this.#emitted)
      if (entry !== undefined) return entry.result
      await new Promise<void>((resolve, reject) => {
        const abort = () => { this.#wake = undefined; reject(signal.reason) }
        this.#wake = () => { signal.removeEventListener('abort', abort); resolve() }
        signal.addEventListener('abort', abort, { once: true })
        if (signal.aborted) abort()
      })
    }
  }

  consumed(): void {
    const entry = this.#pending.get(this.#emitted)
    if (entry === undefined) throw new Error('Output frontier has no reserved block')
    this.#pending.delete(this.#emitted++)
    entry.release()
    this.#fill()
  }

  async close(): Promise<void> {
    this.#closed = true
    this.#unsubscribe()
    await Promise.allSettled([...this.#pending.values()].map(entry => entry.result))
    for (const entry of this.#pending.values()) entry.release()
    this.#pending.clear()
  }

  #fill(): void {
    if (this.#closed || this.#failure !== undefined) return
    try {
      while (this.#scheduled < this.#options.end &&
        this.#pending.size < this.#options.parallel * RANGE_READ_AHEAD_FACTOR) {
        const index = this.#scheduled
        const release = this.#options.budget.reserve(this.#options.bytes(index))
        if (release === undefined) return
        const consumer: BlockConsumer = {
          distance: () => index > this.#emitted ? index - this.#emitted : 0n,
          canDispatch: () => this.#active < this.#options.parallel,
          acquire: () => {
            this.#active += 1
            return () => { this.#active -= 1 }
          },
        }
        let result: Promise<V2BlockRecord>
        try { result = this.#options.read(index, consumer) } catch (error) { release(); throw error }
        // Read-ahead failures must be observed immediately even while output waits on an earlier block.
        result.catch(() => undefined)
        this.#pending.set(index, { result, release })
        this.#scheduled += 1n
        this.#wake?.()
        this.#wake = undefined
      }
    } catch (reason) {
      this.#failure = { reason }
      this.#wake?.()
      this.#wake = undefined
    }
  }
}
