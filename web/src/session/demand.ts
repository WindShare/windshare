import {
  createChunkIndex,
  type ChunkIndex,
  type ChunkSet,
} from '../contracts/selection'
import type { ChunkAvailability } from '../contracts/sink'
import { ORDERED_REORDER_BLOCKS } from './model'

/** Interval traversal stays compact while active/retry state stays window-bounded. */
export class CompactDemand {
  readonly #chunks: ChunkSet
  readonly #availability: ChunkAvailability
  readonly #ordered: boolean
  readonly #retry: ChunkIndex[] = []
  readonly #orderedWindow: ChunkIndex[] = []
  #rangeOffset = 0
  #scanAt = 0
  #exhausted = false

  constructor(chunks: ChunkSet, availability: ChunkAvailability, ordered: boolean) {
    this.#chunks = chunks
    this.#availability = availability
    this.#ordered = ordered
  }

  get exhausted(): boolean {
    return this.#exhausted
  }

  get retryCount(): number {
    return this.#retry.length
  }

  get orderedCount(): number {
    return this.#orderedWindow.length
  }

  get orderedHead(): ChunkIndex | undefined {
    return this.#orderedWindow[0]
  }

  start(): void {
    if (this.#ordered) {
      this.#fillOrderedWindow()
    }
  }

  take(wanted: number, unavailable: (index: ChunkIndex) => boolean): ChunkIndex[] {
    if (this.#ordered) {
      return this.#takeOrdered(wanted, unavailable)
    }
    const indices: ChunkIndex[] = []
    while (indices.length < wanted && this.#retry.length > 0) {
      const index = this.#retry.shift()
      if (index !== undefined && !this.#availability.has(index)) {
        indices.push(index)
      }
    }
    while (indices.length < wanted) {
      const index = this.#nextMissing()
      if (index === undefined) {
        break
      }
      indices.push(index)
    }
    return indices
  }

  retry(index: ChunkIndex): void {
    let low = 0
    let high = this.#retry.length
    while (low < high) {
      const middle = low + Math.floor((high - low) / 2)
      const value = this.#retry[middle]
      if (value !== undefined && value < index) {
        low = middle + 1
      } else {
        high = middle
      }
    }
    if (this.#retry[low] !== index) {
      this.#retry.splice(low, 0, index)
    }
  }

  advanceOrdered(index: ChunkIndex): void {
    if (!this.#ordered || this.#orderedWindow[0] !== index) {
      throw new Error(`block ${index} is not the ordered demand head`)
    }
    this.#orderedWindow.shift()
    this.#fillOrderedWindow()
  }

  #takeOrdered(
    wanted: number,
    unavailable: (index: ChunkIndex) => boolean,
  ): ChunkIndex[] {
    const indices: ChunkIndex[] = []
    for (const index of this.#orderedWindow) {
      if (indices.length === wanted) {
        break
      }
      if (!unavailable(index)) {
        indices.push(index)
      }
    }
    return indices
  }

  #nextMissing(): ChunkIndex | undefined {
    const ranges = this.#chunks.ranges
    while (this.#rangeOffset < ranges.length) {
      const range = ranges[this.#rangeOffset]
      if (range === undefined) {
        break
      }
      const first = Math.max(this.#scanAt, range.first)
      for (let candidate = first; candidate < range.end; candidate += 1) {
        this.#scanAt = candidate + 1
        const index = createChunkIndex(candidate)
        if (!this.#availability.has(index)) {
          return index
        }
      }
      this.#rangeOffset += 1
      this.#scanAt = 0
    }
    this.#exhausted = true
    return undefined
  }

  #fillOrderedWindow(): void {
    while (this.#orderedWindow.length < ORDERED_REORDER_BLOCKS) {
      const index = this.#nextMissing()
      if (index === undefined) {
        return
      }
      this.#orderedWindow.push(index)
    }
  }
}
