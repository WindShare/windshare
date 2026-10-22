import { V2_MAXIMUM_CHUNK_BYTES } from '../../catalog/v2-records'
import type { V2BlockRecord } from '../v2-records'
import type { ContentPriority } from './selection'

export const RANGE_READ_AHEAD_FACTOR = 4

export interface BlockConsumer {
  readonly lastServed: () => number
  readonly distance: () => bigint
  readonly canDispatch: () => boolean
  readonly acquire: () => (() => void) | undefined
  readonly ready: (signal: AbortSignal) => Promise<void>
}
export const IMMEDIATE_BLOCK_CONSUMER: BlockConsumer = {
  lastServed: () => 0, distance: () => 0n, canDispatch: () => true, acquire: () => () => undefined,
  ready: async signal => { signal.throwIfAborted() },
}

/** Output holdings cannot borrow the interactive frontier: a sink may never resume. */
export class RangeBufferBudget {
  readonly #maximum: number
  readonly #interactive: number
  readonly #listeners = new Set<() => void>()
  #used = 0
  #turn = 0

  nextTurn(): number { return ++this.#turn }

  constructor(maximum: number, interactive = V2_MAXIMUM_CHUNK_BYTES) {
    if (!Number.isSafeInteger(maximum) || maximum <= 0) throw new RangeError('Range buffer budget must be positive')
    this.#maximum = maximum
    this.#interactive = Math.min(interactive, Math.floor(maximum / 2))
  }

  available(bytes: number, priority: ContentPriority): boolean {
    if (!Number.isSafeInteger(bytes) || bytes <= 0 || bytes > this.#maximum) {
      throw new RangeError('Block exceeds the range buffer budget')
    }
    const ceiling = priority === 'preview' ? this.#maximum : Math.max(bytes, this.#maximum - this.#interactive)
    return this.#used + bytes <= ceiling
  }

  reserve(bytes: number, priority: ContentPriority): (() => void) | undefined {
    if (!this.available(bytes, priority)) return undefined
    this.#used += bytes
    let released = false
    return () => {
      if (released) return
      released = true
      this.#used -= bytes
      for (const listener of this.#listeners) listener()
    }
  }

  wake(): void { queueMicrotask(() => { for (const listener of this.#listeners) listener() }) }

  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }
}

interface BlockWindowOptions {
  readonly first: bigint
  readonly end: bigint
  readonly parallel: number
  readonly priority: ContentPriority
  readonly budget: RangeBufferBudget
  readonly bytes: (index: bigint) => number
  readonly read: (index: bigint, consumer: BlockConsumer) => Promise<V2BlockRecord>
}

interface BufferedBlock {
  readonly result: Promise<V2BlockRecord>
  readonly release: () => void
}

/** Descriptors are cheap; byte ownership starts only when the allocator admits work. */
export class OrderedBlockWindow {
  readonly #pending = new Map<bigint, BufferedBlock>()
  readonly #options: BlockWindowOptions
  #scheduled: bigint
  #emitted: bigint
  #admitting: bigint
  #active = 0
  #lastServed = 0
  #closed = false

  constructor(options: BlockWindowOptions) {
    this.#options = options
    this.#scheduled = options.first
    this.#emitted = options.first
    this.#admitting = options.first
    this.#fill()
  }

  async next(signal: AbortSignal): Promise<V2BlockRecord> {
    signal.throwIfAborted()
    const entry = this.#pending.get(this.#emitted)
    if (entry === undefined) throw new Error('Output frontier has no queued block')
    return entry.result
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
    await Promise.allSettled([...this.#pending.values()].map(entry => entry.result))
    for (const entry of this.#pending.values()) entry.release()
    this.#pending.clear()
  }

  #fill(): void {
    while (!this.#closed && this.#scheduled < this.#options.end &&
      this.#pending.size < this.#options.parallel * RANGE_READ_AHEAD_FACTOR) {
      const index = this.#scheduled++
      const bytes = this.#options.bytes(index)
      // Reject impossible geometry before publishing a descriptor that could
      // otherwise wait forever for capacity.
      this.#options.budget.available(bytes, this.#options.priority)
      let release: (() => void) | undefined
      const reserve = () => {
        if (release !== undefined) return true
        // Cached or shared later blocks must not consume the capacity needed
        // by this output's first unadmitted block.
        if (index !== this.#admitting) return false
        release = this.#options.budget.reserve(bytes, this.#options.priority)
        if (release === undefined) return false
        this.#admitting++
        this.#options.budget.wake()
        return true
      }
      const consumer: BlockConsumer = {
        lastServed: () => this.#lastServed,
        distance: () => index > this.#emitted ? index - this.#emitted : 0n,
        canDispatch: () => (release !== undefined || index === this.#admitting) &&
          this.#active < this.#options.parallel &&
          (release !== undefined || this.#options.budget.available(bytes, this.#options.priority)),
        acquire: () => {
          if (this.#active >= this.#options.parallel || !reserve()) return undefined
          this.#active++
          this.#lastServed = this.#options.budget.nextTurn()
          let finished = false
          return () => { if (!finished) { finished = true; this.#active-- } }
        },
        ready: signal => this.#ready(reserve, signal),
      }
      const result = this.#options.read(index, consumer)
      result.catch(() => undefined)
      this.#pending.set(index, { result, release: () => { release?.(); release = undefined } })
    }
  }

  async #ready(reserve: () => boolean, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    if (reserve()) return
    await new Promise<void>((resolve, reject) => {
      const finish = (error?: unknown) => {
        unsubscribe()
        signal.removeEventListener('abort', abort)
        if (error !== undefined) reject(error)
        else resolve()
      }
      const abort = () => finish(signal.reason)
      const unsubscribe = this.#options.budget.subscribe(() => { if (reserve()) finish() })
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
      else if (reserve()) finish()
    })
  }
}
